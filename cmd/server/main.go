package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/fatedier/frp/pkg/config/types"
	frpconfig "github.com/fatedier/frp/pkg/config/v1"
	plugin "github.com/fatedier/frp/pkg/plugin/server"
	frpnet "github.com/fatedier/frp/pkg/util/net"
	"github.com/fatedier/frp/server"
)

//go:embed web
var webFiles embed.FS

const frpAddr = "127.0.0.1:7000"
const dashboardAddr = "127.0.0.1:7500"

func randomID(size int) string {
	value := make([]byte, size)
	rand.Read(value)
	return hex.EncodeToString(value)
}

func env(key, value string) string {
	if actual := os.Getenv(key); actual != "" {
		return actual
	}
	return value
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context) error {
	adminPassword := os.Getenv("MULTIFRP_ADMIN_PASSWORD")
	domain := strings.ToLower(env("MULTIFRP_PUBLIC_DOMAIN", os.Getenv("MULTIFRP_HOST")))
	if len(adminPassword) < 16 || !validDomain(domain) {
		return errors.New("configure a public domain and an administrator password of at least 16 bytes")
	}
	first, firstErr := strconv.Atoi(env("MULTIFRP_PORT_FIRST", "30000"))
	last, lastErr := strconv.Atoi(env("MULTIFRP_PORT_LAST", "30099"))
	if firstErr != nil || lastErr != nil || first < 20000 || last > 65535 || last < first {
		return errors.New("the tunnel port range must be within 20000–65535")
	}
	zone, err := time.LoadLocation(env("MULTIFRP_TIMEZONE", "Asia/Shanghai"))
	if err != nil {
		return err
	}
	db, data, err := openStore(env("MULTIFRP_DB", "data/multifrp.db"), domain)
	if err != nil {
		return err
	}
	defer db.Close()
	for _, t := range data.Tunnels {
		if t.Port != 0 && (t.Port < first || t.Port > last) {
			return fmt.Errorf("tunnel %s uses port %d outside the configured range", t.Name, t.Port)
		}
	}
	g := newGateway(os.Getenv("MULTIFRP_TUNNEL_BIND"), first, last)
	defer g.close()
	if err := g.reconcile(data, func() error { return nil }); err != nil {
		return err
	}
	a := &app{db: db, data: data, gateway: g, adminUser: env("MULTIFRP_ADMIN_USER", "admin"), adminPassword: digest(adminPassword),
		secureCookies: os.Getenv("MULTIFRP_INSECURE_LOCAL") != "1", zone: zone, sessions: map[string]time.Time{}, online: map[string]bool{}, rates: map[string]rate{},
		runKey: make([]byte, 32)}
	rand.Read(a.runKey)
	pluginListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer pluginListener.Close()
	privateMux := http.NewServeMux()
	privateMux.HandleFunc("POST /authorize", a.authorizeFRP)
	privateHTTP := &http.Server{Handler: privateMux, ReadHeaderTimeout: 5 * time.Second}
	defer privateHTTP.Close()
	go privateHTTP.Serve(pluginListener)
	frpsConfig := &frpconfig.ServerConfig{BindAddr: "127.0.0.1", BindPort: 7000, ProxyBindAddr: "127.0.0.1",
		WebServer:         frpconfig.WebServerConfig{Addr: "127.0.0.1", Port: 7500},
		AllowPorts:        []types.PortsRange{{Start: firstPrivatePort, End: lastPrivatePort}},
		MaxPortsPerClient: 32,
		HTTPPlugins:       []frpconfig.HTTPPluginOptions{{Name: "multifrp", Addr: pluginListener.Addr().String(), Path: "/authorize", Ops: []string{plugin.OpLogin, plugin.OpNewProxy, plugin.OpPing, plugin.OpNewWorkConn}}},
	}
	if err := frpsConfig.Complete(); err != nil {
		return err
	}
	frps, err := server.NewService(frpsConfig)
	if err != nil {
		return err
	}
	defer frps.Close()
	go frps.Run(ctx)
	samplerCtx, stopSampler := context.WithCancel(context.Background())
	samplerDone := make(chan struct{})
	go func() { defer close(samplerDone); a.sampleLoop(samplerCtx) }()
	defer func() { g.close(); stopSampler(); <-samplerDone }()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, true) })
	frpProxy := httputil.NewSingleHostReverseProxy(&url.URL{Scheme: "http", Host: frpAddr})
	mux.HandleFunc("GET "+frpnet.FrpWebsocketPath, func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			w.WriteHeader(http.StatusUpgradeRequired)
			return
		}
		frpProxy.ServeHTTP(w, r)
	})
	mux.HandleFunc("POST /api/admin/login", a.login)
	mux.HandleFunc("POST /api/admin/logout", a.admin(a.logout))
	mux.HandleFunc("GET /api/admin/state", a.admin(a.dashboard))
	mux.HandleFunc("POST /api/admin/invites", a.admin(a.invite))
	mux.HandleFunc("POST /api/admin/invites/{id}/remark", a.admin(a.updateInviteRemark))
	mux.HandleFunc("POST /api/admin/domain", a.admin(a.domain))
	mux.HandleFunc("POST /api/admin/devices/{id}/revoke", a.admin(a.revoke))
	mux.HandleFunc("POST /api/admin/tunnels/{id}", a.admin(a.setTunnel))
	mux.HandleFunc("GET /api/admin/tunnels/{id}/history", a.admin(a.history))
	mux.HandleFunc("POST /api/client/enroll", a.enroll)
	mux.HandleFunc("GET /api/client/state", a.clientState)
	mux.HandleFunc("POST /api/client/tunnels", a.requestTunnel)
	assets, err := fs.Sub(webFiles, "web")
	if err != nil {
		return err
	}
	mux.Handle("GET /", http.FileServerFS(assets))
	httpServer := &http.Server{Addr: env("MULTIFRP_HTTP_BIND", "127.0.0.1:8080"), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'")
			mux.ServeHTTP(w, r)
		}),
	}
	listenErr := make(chan error, 1)
	go func() { listenErr <- httpServer.ListenAndServe() }()
	select {
	case err := <-listenErr:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

func (a *app) authorizeFRP(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Op      string
		Content json.RawMessage
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	if err := decoder.Decode(&request); err != nil {
		w.WriteHeader(400)
		return
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	response := plugin.Response{Reject: true, RejectReason: "device or tunnel not authorized"}
	switch request.Op {
	case plugin.OpLogin:
		var login plugin.LoginContent
		if json.Unmarshal(request.Content, &login) == nil && a.verifyLocked(login.User, login.Metas["deviceToken"]) {
			// Tie reconnected sessions to the authenticated device and keep work connection IDs private.
			mac := hmac.New(sha256.New, a.runKey)
			_, _ = mac.Write([]byte(login.User))
			login.RunID = hex.EncodeToString(mac.Sum(nil)[:16])
			login.ClientID = login.User
			response = plugin.Response{Content: login}
		}
	case plugin.OpPing, plugin.OpNewWorkConn:
		var payload struct{ User plugin.UserInfo }
		if json.Unmarshal(request.Content, &payload) == nil && a.verifyLocked(payload.User.User, payload.User.Metas["deviceToken"]) {
			response = plugin.Response{Unchange: true}
		}
	case plugin.OpNewProxy:
		var proxy plugin.NewProxyContent
		if json.Unmarshal(request.Content, &proxy) != nil || !a.verifyLocked(proxy.User.User, proxy.User.Metas["deviceToken"]) {
			break
		}
		owner, id, _ := strings.Cut(proxy.ProxyName, ".")
		tunnel, ok := a.data.Tunnels[id]
		if ok && owner == proxy.User.User && tunnel.DeviceID == owner && tunnel.State == "approved" && tunnel.Enabled && proxy.ProxyType == "tcp" && proxy.Group == "" && proxy.RemotePort == tunnel.Port {
			if privatePort := a.gateway.privatePort(id); privatePort != 0 {
				proxy.RemotePort = privatePort
				response = plugin.Response{Content: proxy}
			}
		}
	}
	respond(w, 200, response)
}

func (a *app) pollFRP(ctx context.Context, client *http.Client) {
	request, err := http.NewRequestWithContext(ctx, "GET", "http://"+dashboardAddr+"/api/proxy/tcp", nil)
	if err != nil {
		return
	}
	online := map[string]bool{}
	response, err := client.Do(request)
	if err == nil {
		defer response.Body.Close()
		var result struct {
			Proxies []struct{ Name, Status string }
		}
		if response.StatusCode == 200 && json.NewDecoder(response.Body).Decode(&result) == nil {
			for _, proxy := range result.Proxies {
				if proxy.Status == "online" {
					online[proxy.Name] = true
				}
			}
		}
	}
	a.mu.Lock()
	a.online = online
	a.mu.Unlock()
}

func (a *app) sampleLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	client := &http.Client{Timeout: time.Second}
	previous := time.Now()
	pruned := time.Time{}
	pending := map[time.Time]map[string]traffic{}
	flush := func(now time.Time) {
		samples := a.gateway.sample()
		elapsed := now.Sub(previous).Seconds()
		previous = now
		minute := now.Truncate(time.Minute)
		if pending[minute] == nil {
			pending[minute] = map[string]traffic{}
		}
		rates := map[string]rate{}
		for id, delta := range samples {
			old := pending[minute][id]
			pending[minute][id] = traffic{old.In + delta.In, old.Out + delta.Out}
			if elapsed > 0 {
				rates[id] = rate{float64(delta.In) * 8 / elapsed, float64(delta.Out) * 8 / elapsed}
			}
		}
		a.mu.Lock()
		a.rates = rates
		a.mu.Unlock()
		for at, values := range pending {
			prune := now.Sub(pruned) >= time.Hour && at == minute
			if len(values) > 0 || prune {
				if err := addTraffic(a.db, at, values, prune); err != nil {
					log.Printf("save traffic: %v", err)
					continue
				}
				if prune {
					pruned = now
				}
			}
			delete(pending, at)
		}
	}
	for {
		select {
		case now := <-ticker.C:
			flush(now)
			a.pollFRP(ctx, client)
		case <-ctx.Done():
			flush(time.Now())
			return
		}
	}
}
