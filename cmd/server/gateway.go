package main

import (
	"context"
	"fmt"
	"io"
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
	meter       *meter
	connections map[*connection]context.CancelFunc
}

type connection struct{ client net.Conn }

type gateway struct {
	first, last int
	listeners   []net.Listener
	mu          sync.Mutex
	routes      map[int]*route
	meters      map[string]*meter
	workers     sync.WaitGroup
	closed      bool
}

const firstPrivatePort = 12000

func newGateway(host string, first, last int) (*gateway, error) {
	g := &gateway{first: first, last: last, routes: map[int]*route{}, meters: map[string]*meter{}}
	for port := first; port <= last; port++ {
		listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			g.close()
			return nil, fmt.Errorf("bind public tunnel port %d: %w", port, err)
		}
		g.listeners = append(g.listeners, listener)
		g.workers.Go(func() { g.accept(listener, port) })
	}
	return g, nil
}

func (g *gateway) privatePort(publicPort int) int { return firstPrivatePort + publicPort - g.first }

func (g *gateway) reconcile(data state) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	for port, previous := range g.routes {
		tunnel := data.Tunnels[previous.id]
		if tunnel.Port == port && tunnel.State == "approved" && tunnel.Enabled && !data.Devices[tunnel.DeviceID].Revoked {
			continue
		}
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
		g.routes[tunnel.Port] = &route{id: id, meter: g.meters[id], connections: map[*connection]context.CancelFunc{}}
	}
}

func (g *gateway) accept(listener net.Listener, port int) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		g.mu.Lock()
		route := g.routes[port]
		if g.closed || route == nil || len(route.connections) >= 128 {
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
			g.forward(ctx, client, port, route.meter)
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
	upstream, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(g.privatePort(port))))
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
	for _, listener := range g.listeners {
		_ = listener.Close()
	}
	for _, route := range g.routes {
		for entry, cancel := range route.connections {
			cancel()
			_ = entry.client.Close()
		}
	}
	g.mu.Unlock()
	g.workers.Wait()
}

func proxyName(t control.Tunnel) string { return t.DeviceID + "." + t.ID }
