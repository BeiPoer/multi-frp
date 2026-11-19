package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"multifrp/internal/control"
)

type meter struct {
	in, out atomic.Uint64
}

type route struct {
	id          string
	listener    net.Listener
	closed      bool
	meter       *meter
	connections map[*connection]context.CancelFunc
}

type connection struct{ client net.Conn }

type gateway struct {
	first, last int
	host        string
	// Keep assignments across disable/re-enable until old FRP proxies have gone away.
	privatePorts map[int]int
	mu           sync.Mutex
	routes       map[int]*route
	meters       map[string]*meter
	workers      sync.WaitGroup
	closed       bool
}

const firstPrivatePort = 12000
const lastPrivatePort = 19999

func newGateway(host string, first, last int) *gateway {
	return &gateway{first: first, last: last, host: host, privatePorts: map[int]int{}, routes: map[int]*route{}, meters: map[string]*meter{}}
}

func (g *gateway) privatePort(publicPort int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.privatePorts[publicPort]
}

// Reserve listeners before saving; a failed bind or save must leave existing routes intact.
func (g *gateway) reconcile(data state, save func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return fmt.Errorf("gateway is closed")
	}
	added := map[int]net.Listener{}
	defer func() {
		for _, listener := range added {
			_ = listener.Close()
		}
	}()
	privatePorts := maps.Clone(g.privatePorts)
	used := map[int]bool{}
	for _, port := range privatePorts {
		used[port] = true
	}
	for _, tunnel := range data.Tunnels {
		if tunnel.State != "approved" || !tunnel.Enabled || data.Devices[tunnel.DeviceID].Revoked || g.routes[tunnel.Port] != nil {
			continue
		}
		listener, err := net.Listen("tcp", net.JoinHostPort(g.host, strconv.Itoa(tunnel.Port)))
		if err != nil {
			return failure{409, fmt.Sprintf("公网端口 %d 无法监听，请检查是否已被占用：%v", tunnel.Port, err)}
		}
		added[tunnel.Port] = listener
		if privatePorts[tunnel.Port] == 0 {
			for port := firstPrivatePort; port <= lastPrivatePort; port++ {
				if used[port] {
					continue
				}
				probe, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
				if err != nil {
					continue
				}
				_ = probe.Close()
				privatePorts[tunnel.Port], used[port] = port, true
				break
			}
			if privatePorts[tunnel.Port] == 0 {
				return failure{409, "内部转发端口已耗尽"}
			}
		}
	}
	if err := save(); err != nil {
		return err
	}
	g.privatePorts = privatePorts
	for port, previous := range g.routes {
		tunnel := data.Tunnels[previous.id]
		if tunnel.Port == port && tunnel.State == "approved" && tunnel.Enabled && !data.Devices[tunnel.DeviceID].Revoked {
			continue
		}
		previous.closed = true
		_ = previous.listener.Close()
		for entry, cancel := range previous.connections {
			cancel()
			_ = entry.client.Close()
		}
		delete(g.routes, port)
	}
	for id, tunnel := range data.Tunnels {
		if tunnel.State != "approved" || !tunnel.Enabled || data.Devices[tunnel.DeviceID].Revoked || g.routes[tunnel.Port] != nil {
			continue
		}
		if g.meters[id] == nil {
			g.meters[id] = &meter{}
		}
		r := &route{id: id, listener: added[tunnel.Port], meter: g.meters[id], connections: map[*connection]context.CancelFunc{}}
		g.routes[tunnel.Port] = r
		delete(added, tunnel.Port)
		privatePort := g.privatePorts[tunnel.Port]
		g.workers.Go(func() { g.accept(r, privatePort) })
	}
	return nil
}

func (g *gateway) accept(route *route, privatePort int) {
	for {
		client, err := route.listener.Accept()
		if err != nil {
			return
		}
		g.mu.Lock()
		if g.closed || route.closed || len(route.connections) >= 128 {
			g.mu.Unlock()
			_ = client.Close()
			continue
		}
		entry := &connection{client}
		ctx, cancel := context.WithCancel(context.Background())
		route.connections[entry] = cancel
		g.workers.Go(func() {
			defer func() {
				cancel()
				_ = client.Close()
				g.mu.Lock()
				delete(route.connections, entry)
				g.mu.Unlock()
			}()
			g.forward(ctx, client, privatePort, route.meter)
		})
		g.mu.Unlock()
	}
}

type countingWriter struct {
	dst     io.Writer
	counter *atomic.Uint64
}

func (w countingWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.counter.Add(uint64(n))
	return n, err
}

func (g *gateway) forward(ctx context.Context, client net.Conn, port int, m *meter) {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	upstream, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return
	}
	defer upstream.Close()
	defer context.AfterFunc(ctx, func() { _ = upstream.Close() })()
	var copyWorkers sync.WaitGroup
	forward := func(dst, src net.Conn, counter *atomic.Uint64) {
		if _, err := io.Copy(countingWriter{dst, counter}, src); err != nil {
			_ = dst.Close()
			_ = src.Close()
			return
		}
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}
	copyWorkers.Go(func() { forward(upstream, client, &m.in) })
	forward(client, upstream, &m.out)
	copyWorkers.Wait()
}

func (g *gateway) sample() map[string]traffic {
	g.mu.Lock()
	defer g.mu.Unlock()
	result := make(map[string]traffic)
	for id, m := range g.meters {
		value := traffic{m.in.Swap(0), m.out.Swap(0)}
		if value != (traffic{}) {
			result[id] = value
		}
	}
	return result
}

func (g *gateway) close() {
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		g.workers.Wait()
		return
	}
	g.closed = true
	for _, route := range g.routes {
		_ = route.listener.Close()
		for entry, cancel := range route.connections {
			cancel()
			_ = entry.client.Close()
		}
	}
	g.mu.Unlock()
	g.workers.Wait()
}

func proxyName(t control.Tunnel) string { return t.DeviceID + "." + t.ID }
