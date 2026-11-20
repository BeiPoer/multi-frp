package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"multifrp/internal/control"
)

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestGatewayOnDemandAndRollback(t *testing.T) {
	g := newGateway("127.0.0.1", 30000, 40000)
	defer g.close()
	data := state{Devices: map[string]device{}, Tunnels: map[string]control.Tunnel{}}
	save := func() error { return nil }
	if err := g.reconcile(data, save); err != nil {
		t.Fatal(err)
	}
	if len(g.routes) != 0 || len(g.privatePorts) != 0 {
		t.Fatal("empty pool allocated resources")
	}
	port := freePort(t)
	blocker, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	tunnel := control.Tunnel{ID: "one", DeviceID: "device", Port: port, State: "approved", Enabled: true}
	data.Tunnels[tunnel.ID] = tunnel
	saved := false
	if err := g.reconcile(data, func() error { saved = true; return nil }); err == nil || saved {
		t.Fatal("occupied port was committed")
	}
	blocker.Close()
	diskErr := errors.New("disk full")
	if err := g.reconcile(data, func() error { return diskErr }); !errors.Is(err, diskErr) {
		t.Fatalf("save: %v", err)
	}
	assertPortFree(t, port)
	if len(g.privatePorts) != 0 {
		t.Fatal("failed change leaked a private port")
	}
	if err := g.reconcile(data, save); err != nil {
		t.Fatal(err)
	}
	private := g.privatePort(tunnel.ID)
	if private < firstPrivatePort || private > lastPrivatePort {
		t.Fatal("invalid private port")
	}
	if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil {
		l.Close()
		t.Fatal("enabled port not reserved")
	}
	tunnel.Enabled = false
	data.Tunnels[tunnel.ID] = tunnel
	if err := g.reconcile(data, func() error { return diskErr }); !errors.Is(err, diskErr) {
		t.Fatal(err)
	}
	if len(g.routes) != 1 {
		t.Fatal("failed save disabled an existing route")
	}
	if err := g.reconcile(data, save); err != nil {
		t.Fatal(err)
	}
	assertPortFree(t, port)
	tunnel.Enabled = true
	data.Tunnels[tunnel.ID] = tunnel
	blocker, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.reconcile(data, save); err == nil {
		t.Fatal("re-enabled an occupied port")
	}
	blocker.Close()
	if err := g.reconcile(data, save); err != nil {
		t.Fatal(err)
	}
	if g.privatePort(tunnel.ID) != private {
		t.Fatal("re-enable changed internal destination")
	}
	g.close()
	g = newGateway("127.0.0.1", 30000, 40000)
	defer g.close()
	if err := g.reconcile(data, save); err != nil {
		t.Fatal(err)
	}
	if len(g.routes) != 1 {
		t.Fatal("restart did not restore enabled tunnel")
	}
	data.Devices["device"] = device{Revoked: true}
	if err := g.reconcile(data, save); err != nil {
		t.Fatal(err)
	}
	assertPortFree(t, port)
}

func assertPortFree(t *testing.T, port int) {
	t.Helper()
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("port %d not released: %v", port, err)
	}
	l.Close()
}

func TestApprovalConflictDoesNotPersist(t *testing.T) {
	db, data, err := openStore(filepath.Join(t.TempDir(), "state.db"), "localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port
	g := newGateway("127.0.0.1", port, port)
	defer g.close()
	data.Tunnels["one"] = control.Tunnel{ID: "one", State: "pending"}
	a := &app{db: db, data: data, gateway: g}
	approve := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/", strings.NewReader(`{"Action":"approve"}`))
		r.Header.Set("Content-Type", "application/json")
		r.SetPathValue("id", "one")
		w := httptest.NewRecorder()
		a.setTunnel(w, r)
		return w
	}
	if w := approve(); w.Code != 409 {
		t.Fatalf("conflict status %d: %s", w.Code, w.Body.String())
	}
	if a.data.Tunnels["one"].State != "pending" {
		t.Fatal("failed approval changed state")
	}
	blocker.Close()
	if w := approve(); w.Code != 200 {
		t.Fatalf("approval status %d: %s", w.Code, w.Body.String())
	}
	if a.data.Tunnels["one"].Port != port {
		t.Fatal("approval did not assign port")
	}
}

func TestAdminPublicPortChange(t *testing.T) {
	db, data, err := openStore(filepath.Join(t.TempDir(), "state.db"), "localhost")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	busy := blocker.Addr().(*net.TCPAddr).Port
	old, next := freePort(t), freePort(t)
	for old == next {
		next = freePort(t)
	}
	g := newGateway("127.0.0.1", min(old, next, busy), max(old, next, busy))
	defer g.close()
	data.Tunnels["one"] = control.Tunnel{ID: "one", DeviceID: "device-one", State: "pending"}
	data.Tunnels["two"] = control.Tunnel{ID: "two", DeviceID: "device-two", State: "pending"}
	a := &app{db: db, data: data, gateway: g}
	change := func(id, payload string, status int) {
		t.Helper()
		r := httptest.NewRequest("POST", "/", strings.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		r.SetPathValue("id", id)
		w := httptest.NewRecorder()
		a.setTunnel(w, r)
		if w.Code != status {
			t.Fatalf("tunnel %s %s: got %d, want %d: %s", id, payload, w.Code, status, w.Body)
		}
	}
	choice := func(action string, port int) string { return fmt.Sprintf(`{"action":%q,"port":%d}`, action, port) }
	change("one", choice("approve", old), 200)
	first := g.privatePort("one")
	change("one", `{"action":"delete"}`, 409)
	change("two", `{"action":"delete"}`, 409)
	change("one", `{"action":"change-port"}`, 400)
	change("one", choice("change-port", 0), 400)
	change("one", choice("change-port", busy), 409)
	change("two", choice("approve", old), 409)
	if a.data.Tunnels["one"].Port != old || g.routes[old] == nil {
		t.Fatal("a rejected change interrupted the old route")
	}
	change("one", choice("change-port", next), 200)
	assertPortFree(t, old)
	if a.data.Tunnels["one"].Port != next || g.routes[next] == nil || g.privatePort("one") != first {
		t.Fatal("changing the public port lost the tunnel's private listener assignment")
	}
	change("two", choice("approve", old), 200)
	if g.privatePort("two") == 0 || g.privatePort("two") == first || g.routes[old].id != "two" {
		t.Fatal("the reused public port must never forward to the previous owner's FRP listener")
	}
	change("two", `{"action":"disable"}`, 200)
	change("two", choice("change-port", next), 409)
	change("two", choice("change-port", busy), 200)
	if a.data.Tunnels["two"].Enabled || a.data.Tunnels["two"].Port != busy || g.routes[busy] != nil {
		t.Fatal("editing a disabled tunnel unexpectedly opened its public entry")
	}
	change("two", `{"action":"enable"}`, 409)
	change("two", `{"action":"delete"}`, 200)
	change("two", `{"action":"delete"}`, 404)
}

func TestServerTunnelLifecycle(t *testing.T) {
	origin := fmt.Sprintf("http://127.0.0.1:%d", freePort(t))
	t.Setenv("MULTIFRP_HOST", "localhost")
	t.Setenv("MULTIFRP_PUBLIC_DOMAIN", "localhost")
	t.Setenv("MULTIFRP_ADMIN_USER", "admin")
	t.Setenv("MULTIFRP_ADMIN_PASSWORD", "integration-password-123")
	t.Setenv("MULTIFRP_DB", filepath.Join(t.TempDir(), "server.db"))
	t.Setenv("MULTIFRP_HTTP_BIND", strings.TrimPrefix(origin, "http://"))
	t.Setenv("MULTIFRP_TUNNEL_BIND", "127.0.0.1")
	t.Setenv("MULTIFRP_PORT_FIRST", "30000")
	t.Setenv("MULTIFRP_PORT_LAST", "40000")
	t.Setenv("MULTIFRP_INSECURE_LOCAL", "1")
	t.Setenv("MULTIFRP_TEST_ORIGIN", origin)
	t.Setenv("MULTIFRP_TEST_PASSWORD", "integration-password-123")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("server did not stop")
		}
	})
	client := &http.Client{Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		response, err := client.Get(origin + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == 200 {
				ready = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		t.Fatal("server did not become healthy")
	}
	assertPortFree(t, 30000)
	assertPortFree(t, 40000)
	TestComposeTunnelLifecycle(t)
	assertPortFree(t, 30000)
	assertPortFree(t, 30001)
	assertPortFree(t, 40000)
}
