package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"multifrp/internal/control"
	"multifrp/internal/engine"
)

func TestComposeTunnelLifecycle(t *testing.T) {
	origin := os.Getenv("MULTIFRP_TEST_ORIGIN")
	if origin == "" {
		t.Skip("set MULTIFRP_TEST_ORIGIN to run with Compose")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	remote, err := control.NewRemote(origin, control.Enrollment{})
	if err != nil {
		t.Fatal(err)
	}
	var session *http.Cookie
	admin := func(path string, payload, result any) {
		t.Helper()
		method := http.MethodGet
		var body io.Reader
		if payload != nil {
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			method, body = http.MethodPost, bytes.NewReader(raw)
		}
		request, err := http.NewRequestWithContext(ctx, method, origin+"/api/admin/"+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if payload != nil {
			request.Header.Set("Content-Type", "application/json")
		}
		if session != nil {
			request.AddCookie(session)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode > 299 {
			data, _ := io.ReadAll(response.Body)
			t.Fatalf("%s: %d %s", path, response.StatusCode, data)
		}
		if path == "login" {
			cookies := response.Cookies()
			if len(cookies) != 1 {
				t.Fatal("administrator session cookie missing")
			}
			session = cookies[0]
		}
		if result != nil {
			if err := json.NewDecoder(response.Body).Decode(result); err != nil {
				t.Fatal(err)
			}
		}
	}
	admin("login", map[string]string{"user": "admin", "password": os.Getenv("MULTIFRP_TEST_PASSWORD")}, nil)
	admin("domain", map[string]string{"domain": "localhost"}, nil)
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		for {
			conn, err := backend.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	localPort := backend.Addr().(*net.TCPAddr).Port
	prepare := func(name string) (control.Remote, control.Tunnel) {
		t.Helper()
		var invite struct{ Code string }
		admin("invites", struct{}{}, &invite)
		credentials, err := remote.Enroll(ctx, invite.Code, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := remote.Enroll(ctx, invite.Code, name); err == nil {
			t.Fatal("single use invite was accepted twice")
		}
		owner := remote
		owner.Credentials = credentials
		if err := owner.Submit(ctx, control.Tunnel{Name: name, LocalIP: "127.0.0.1", LocalPort: localPort}); err != nil {
			t.Fatal(err)
		}
		state, err := owner.State(ctx)
		if err != nil || len(state.Tunnels) != 1 || state.Tunnels[0].State != "pending" || state.Tunnels[0].Address != "" {
			t.Fatalf("unexpected pending state: %+v %v", state, err)
		}
		admin("tunnels/"+state.Tunnels[0].ID, map[string]string{"action": "approve"}, nil)
		state, err = owner.State(ctx)
		if err != nil || len(state.Tunnels) != 1 || state.Tunnels[0].Address != "localhost:"+strconv.Itoa(state.Tunnels[0].Port) {
			t.Fatalf("unexpected approved state: %+v %v", state, err)
		}
		service, err := engine.New(owner, state.Tunnels)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { defer close(done); _ = service.Run(ctx) }()
		t.Cleanup(func() { service.Close(); <-done })
		return owner, state.Tunnels[0]
	}
	first, a := prepare("compose-smoke-active")
	second, b := prepare("compose-smoke-idle")
	connect := func(tunnel control.Tunnel) net.Conn {
		t.Helper()
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
			conn, err := net.DialTimeout("tcp", tunnel.Address, time.Second)
			if err == nil {
				_ = conn.SetDeadline(time.Now().Add(time.Second))
				if _, err = conn.Write([]byte("hello")); err == nil {
					buffer := make([]byte, 5)
					if _, err = io.ReadFull(conn, buffer); err == nil && string(buffer) == "hello" {
						return conn
					}
				}
				conn.Close()
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Fatalf("tunnel %s did not pass data", tunnel.ID)
		return nil
	}
	conn := connect(a)
	defer conn.Close()
	payload := bytes.Repeat([]byte{0x6a}, 64<<10)
	buffer := make([]byte, len(payload))
	var live, today bool
	for deadline := time.Now().Add(14 * time.Second); time.Now().Before(deadline) && (!live || !today); {
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(conn, buffer); err != nil || !bytes.Equal(payload, buffer) {
			t.Fatalf("forward: %v", err)
		}
		var dashboard struct{ Tunnels []tunnelView }
		admin("state", nil, &dashboard)
		for _, row := range dashboard.Tunnels {
			switch row.ID {
			case a.ID:
				live = live || (row.Online && row.Rate.In > 0 && row.Rate.Out > 0)
				today = today || (row.Today.In >= uint64(len(payload)) && row.Today.Out >= uint64(len(payload)))
			case b.ID:
				if row.Rate != (rate{}) || row.Today != (traffic{}) {
					t.Fatal("idle tunnel is receiving the other tunnel's counts")
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !live || !today {
		t.Fatal("active connection did not update live rate and daily bytes")
	}
	var history struct{ Total traffic }
	admin("tunnels/"+a.ID+"/history?range=7d", nil, &history)
	if history.Total.In == 0 || history.Total.Out == 0 {
		t.Fatal("historical counts are absent")
	}
	admin("tunnels/"+a.ID, map[string]string{"action": "disable"}, nil)
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(buffer); err == nil {
		t.Fatal("disabled tunnel connection remained open")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("disabled tunnel did not disconnect immediately")
	}
	state, err := first.State(ctx)
	if err != nil || state.Tunnels[0].Enabled {
		t.Fatalf("disabled tunnel remains enabled: %v", err)
	}
	other := connect(b)
	defer other.Close()
	admin("devices/"+second.Credentials.DeviceID+"/revoke", struct{}{}, nil)
	var revoked control.RemoteError
	if _, err := second.State(ctx); !errors.As(err, &revoked) || revoked.Code != 403 {
		t.Fatalf("revoked credential still usable: %v", err)
	}
	t.Logf("verified both tunnels, active traffic and history persisted under %s", a.ID)
}
