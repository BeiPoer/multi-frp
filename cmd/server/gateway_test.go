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
	private := g.privatePort(port)
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
	if g.privatePort(port) != private {
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
}
