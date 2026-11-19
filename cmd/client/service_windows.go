package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/fatedier/frp/client"
	frplog "github.com/fatedier/frp/pkg/util/log"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sys/windows/svc"
	"multifrp/internal/control"
	"multifrp/internal/engine"
)

const serviceName = "MultiFRP"
const pipePath = `\\.\pipe\MultiFRP.Control`

type settings struct {
	Origin      string
	Credentials control.Enrollment
	Disabled    map[string]bool
}

type pipeRequest struct {
	Action             string
	Origin, Code, Name string
	Tunnel             control.Tunnel
}

type pipeResponse struct {
	Error, Origin, DeviceID, Status, Log string
	Tunnels                              []control.Tunnel
}

type daemon struct {
	db       *bolt.DB
	logPath  string
	mu       sync.RWMutex
	config   settings
	snapshot control.DeviceState
	status   string
	changed  chan struct{}
}

type serviceHandler struct{ sid string }

func (s serviceHandler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending, WaitHint: 5000}
	path := filepath.Join(installDir(), "data")
	db, err := bolt.Open(filepath.Join(path, "client.db"), 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return false, 1
	}
	defer db.Close()
	d := &daemon{db: db, logPath: filepath.Join(path, "frpc.log"), changed: make(chan struct{}, 1), status: "等待连接", config: settings{Disabled: map[string]bool{}}}
	if err := d.load(); err != nil {
		return false, 2
	}
	frplog.InitLogger(d.logPath, "info", 3, true)
	acl := "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;LS)(A;;0x12019b;;;" + s.sid + ")"
	listener, err := winio.ListenPipe(pipePath, &winio.PipeConfig{SecurityDescriptor: acl})
	if err != nil {
		return false, 3
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var workers sync.WaitGroup
	workers.Go(func() { d.accept(ctx, listener, cancel) })
	workers.Go(func() { d.syncLoop(ctx) })
	current := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	status <- current
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				status <- current
			case svc.Stop, svc.Shutdown:
				cancel()
			}
		}
	}
	status <- svc.Status{State: svc.StopPending, WaitHint: 12000}
	_ = listener.Close()
	workers.Wait()
	return false, 0
}

var configBucket = []byte("client")
var configKey = []byte("settings")

func (d *daemon) load() error {
	return d.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(configBucket)
		if bucket == nil {
			return nil
		}
		err := json.Unmarshal(bucket.Get(configKey), &d.config)
		if d.config.Disabled == nil {
			d.config.Disabled = map[string]bool{}
		}
		return err
	})
}

func (d *daemon) save(next settings) error {
	data, err := json.Marshal(next)
	if err != nil {
		return err
	}
	err = d.db.Update(func(tx *bolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists(configBucket)
		if err != nil {
			return err
		}
		return bucket.Put(configKey, data)
	})
	if err == nil {
		d.config = next
	}
	return err
}

func (d *daemon) wake() {
	select {
	case d.changed <- struct{}{}:
	default:
	}
}

func (d *daemon) accept(ctx context.Context, listener net.Listener, cancel context.CancelFunc) {
	var clients sync.WaitGroup
	defer clients.Wait()
	limit := make(chan struct{}, 16)
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		select {
		case limit <- struct{}{}:
			clients.Go(func() {
				defer func() { <-limit; conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(12 * time.Second))
				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stop()
				var request pipeRequest
				if json.NewDecoder(io.LimitReader(conn, 65536)).Decode(&request) != nil {
					return
				}
				response := d.command(ctx, request)
				_ = json.NewEncoder(conn).Encode(response)
				if request.Action == "quit" {
					cancel()
				}
			})
		default:
			_ = conn.Close()
		}
	}
}

func (d *daemon) command(ctx context.Context, request pipeRequest) pipeResponse {
	var err error
	switch request.Action {
	case "status", "quit":
	case "logs":
		return pipeResponse{Log: tailLog(d.logPath)}
	case "enroll":
		err = d.enroll(ctx, request)
	case "submit":
		d.mu.RLock()
		config := d.config
		d.mu.RUnlock()
		if config.Credentials.Token == "" {
			err = errors.New("请先绑定设备")
			break
		}
		var remote control.Remote
		remote, err = control.NewRemote(config.Origin, config.Credentials)
		if err == nil {
			err = remote.Submit(ctx, request.Tunnel)
		}
		if err == nil {
			d.wake()
		}
	case "toggle":
		d.mu.Lock()
		found := false
		for _, t := range d.snapshot.Tunnels {
			if t.ID == request.Tunnel.ID && t.State == "approved" && t.Enabled {
				found = true
				break
			}
		}
		if found {
			next := d.config
			next.Disabled = maps.Clone(d.config.Disabled)
			if next.Disabled[request.Tunnel.ID] {
				delete(next.Disabled, request.Tunnel.ID)
			} else {
				next.Disabled[request.Tunnel.ID] = true
			}
			err = d.save(next)
		} else {
			err = errors.New("隧道当前不可操作")
		}
		d.mu.Unlock()
		if err == nil {
			d.wake()
		}
	default:
		err = errors.New("未知客户端操作")
	}
	if err != nil {
		return pipeResponse{Error: err.Error()}
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	result := pipeResponse{Origin: d.config.Origin, DeviceID: d.config.Credentials.DeviceID, Status: d.status}
	for _, row := range d.snapshot.Tunnels {
		if row.Enabled && d.config.Disabled[row.ID] {
			row.State, row.Online = "paused", false
		}
		result.Tunnels = append(result.Tunnels, row)
	}
	return result
}

func (d *daemon) enroll(ctx context.Context, request pipeRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.config.Credentials.Token != "" && d.status != "设备授权已撤销" {
		return errors.New("当前设备已绑定")
	}
	remote, err := control.NewRemote(request.Origin, control.Enrollment{})
	if err != nil {
		return err
	}
	credentials, err := remote.Enroll(ctx, request.Code, request.Name)
	if err != nil {
		return err
	}
	if err := d.save(settings{Origin: remote.Origin.String(), Credentials: credentials, Disabled: map[string]bool{}}); err != nil {
		return err
	}
	d.status = "设备已绑定，正在同步"
	d.wake()
	return nil
}

func (d *daemon) syncLoop(ctx context.Context) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var runtime tunnelRuntime
	defer runtime.stop()
	for ctx.Err() == nil {
		d.syncTunnels(ctx, &runtime)
		select {
		case <-ctx.Done():
			return
		case <-runtime.finished:
			runtime = tunnelRuntime{}
		case <-ticker.C:
		case <-d.changed:
		}
	}
}

type tunnelRuntime struct {
	frpc     *client.Service
	finished chan struct{}
	applied  []control.Tunnel
}

func (runtime *tunnelRuntime) stop() {
	if runtime.frpc != nil {
		runtime.frpc.Close()
		<-runtime.finished
		*runtime = tunnelRuntime{}
	}
}

func (runtime *tunnelRuntime) apply(ctx context.Context, remote control.Remote, next []control.Tunnel) error {
	if len(next) == 0 {
		runtime.stop()
		return nil
	}
	if runtime.frpc == nil {
		service, err := engine.New(remote, next)
		if err != nil {
			return err
		}
		done := make(chan struct{})
		*runtime = tunnelRuntime{frpc: service, finished: done, applied: next}
		go func() { defer close(done); _ = service.Run(ctx) }()
		return nil
	}
	if !slices.Equal(runtime.applied, next) {
		if err := engine.Update(runtime.frpc, next); err != nil {
			return err
		}
		runtime.applied = next
	}
	return nil
}

func (d *daemon) syncTunnels(ctx context.Context, runtime *tunnelRuntime) {
	d.mu.RLock()
	config, previous := d.config, d.snapshot
	d.mu.RUnlock()
	if config.Credentials.Token == "" {
		d.updateStatus("等待绑定设备", nil)
		return
	}
	remote, err := control.NewRemote(config.Origin, config.Credentials)
	if err != nil {
		d.updateStatus(err.Error(), nil)
		return
	}
	state, err := remote.State(ctx)
	if ctx.Err() != nil {
		return
	}
	var remoteErr control.RemoteError
	switch {
	case errors.As(err, &remoteErr) && remoteErr.Code == 403:
		runtime.stop()
		d.updateStatus("设备授权已撤销", &control.DeviceState{})
		return
	case err != nil:
		d.updateStatus("正在重试连接管理端 · "+err.Error(), nil)
		if runtime.frpc == nil {
			return
		}
		state = previous
	default:
		d.updateStatus("管理端已连接", &state)
	}

	// Disabling a tunnel locally takes effect even if the management API is unavailable.
	d.mu.RLock()
	disabled := d.config.Disabled
	d.mu.RUnlock()
	var tunnels []control.Tunnel
	for _, t := range state.Tunnels {
		if t.State == "approved" && t.Enabled && !disabled[t.ID] {
			tunnels = append(tunnels, control.Tunnel{ID: t.ID, State: t.State, Enabled: true,
				LocalIP: t.LocalIP, LocalPort: t.LocalPort, Port: t.Port})
		}
	}
	slices.SortFunc(tunnels, func(a, b control.Tunnel) int { return strings.Compare(a.ID, b.ID) })
	if err := runtime.apply(ctx, remote, tunnels); err != nil {
		d.updateStatus("隧道启动失败 · "+err.Error(), nil)
	}
}

func (d *daemon) updateStatus(status string, state *control.DeviceState) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status = status
	if state != nil {
		d.snapshot = *state
	}
}

func tailLog(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return "暂无隧道日志"
	}
	defer file.Close()
	if info, err := file.Stat(); err == nil && info.Size() > 45000 {
		_, _ = file.Seek(-45000, io.SeekEnd)
	}
	content, _ := io.ReadAll(io.LimitReader(file, 45000))
	return string(content)
}
