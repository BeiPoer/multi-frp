package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
	"multifrp/internal/control"
)

type app struct {
	mu                       sync.RWMutex
	db                       *bolt.DB
	data                     state
	gateway                  *gateway
	adminUser, adminPassword string
	sessions                 map[string]time.Time
	secureCookies            bool
	runKey                   []byte
	zone                     *time.Location
	online                   map[string]bool
	rates                    map[string]rate
}

type rate struct {
	In  float64 `json:"in"`
	Out float64 `json:"out"`
}

type tunnelView struct {
	control.Tunnel
	Device string  `json:"device"`
	Rate   rate    `json:"rate"`
	Today  traffic `json:"today"`
}

type failure struct {
	status  int
	message string
}

func (e failure) Error() string { return e.message }

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, err error) {
	var expected failure
	if errors.As(err, &expected) {
		respond(w, expected.status, map[string]string{"error": expected.message})
		return
	}
	respond(w, 500, map[string]string{"error": "服务器无法保存或读取数据"})
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	if strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
		fail(w, failure{415, "请求必须为 JSON"})
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
	if decoder.Decode(value) != nil || decoder.Decode(new(any)) != io.EOF {
		fail(w, failure{400, "请求格式无效"})
		return false
	}
	return true
}

func (a *app) changeLocked(change func(*state) error) error {
	next := state{a.data.Domain, maps.Clone(a.data.Devices), maps.Clone(a.data.Invites), maps.Clone(a.data.Tunnels)}
	if err := change(&next); err != nil {
		return err
	}
	if err := a.gateway.reconcile(next, func() error { return saveState(a.db, next) }); err != nil {
		return err
	}
	a.data = next
	return nil
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	var request struct{ User, Password string }
	if !decode(w, r, &request) {
		return
	}
	validUser := equal(digest(request.User), digest(a.adminUser))
	validPassword := equal(digest(request.Password), a.adminPassword)
	if !validUser || !validPassword {
		fail(w, failure{401, "用户名或密码错误"})
		return
	}
	token := randomID(32)
	expires := time.Now().Add(12 * time.Hour)
	a.mu.Lock()
	for key, expiry := range a.sessions {
		if expiry.Before(time.Now()) {
			delete(a.sessions, key)
		}
	}
	if len(a.sessions) >= 128 {
		clear(a.sessions)
	}
	a.sessions[token] = expires
	a.mu.Unlock()
	a.sessionCookie(w, token, expires, 0)
	respond(w, 200, true)
}

func (a *app) sessionCookie(w http.ResponseWriter, token string, expires time.Time, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: "multifrp_session", Value: token, Path: "/api/admin", HttpOnly: true,
		Secure: a.secureCookies, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: maxAge})
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("multifrp_session"); err == nil {
		a.mu.Lock()
		delete(a.sessions, cookie.Value)
		a.mu.Unlock()
	}
	a.sessionCookie(w, "", time.Unix(1, 0), -1)
	respond(w, 200, true)
}

func (a *app) admin(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("multifrp_session")
		if err == nil {
			a.mu.RLock()
			valid := a.sessions[cookie.Value].After(time.Now())
			a.mu.RUnlock()
			if valid {
				handler(w, r)
				return
			}
		}
		fail(w, failure{401, "请先登录"})
	}
}

func (a *app) enroll(w http.ResponseWriter, r *http.Request) {
	var request struct{ Code, Name string }
	if !decode(w, r, &request) {
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	request.Code = strings.TrimSpace(request.Code)
	if request.Name == "" || len(request.Name) > 120 || len(request.Code) != 32 {
		fail(w, failure{400, "请输入有效的邀请码和设备名称"})
		return
	}
	result := control.Enrollment{DeviceID: randomID(12), Token: randomID(32)}
	a.mu.Lock()
	err := a.changeLocked(func(s *state) error {
		key := digest(request.Code)
		if !s.Invites[key].After(time.Now()) {
			return failure{403, "邀请码无效、已使用或已过期"}
		}
		delete(s.Invites, key)
		s.Devices[result.DeviceID] = device{ID: result.DeviceID, Name: request.Name, TokenHash: digest(result.Token)}
		return nil
	})
	a.mu.Unlock()
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, 201, result)
}

func (a *app) verifyLocked(id, token string) bool {
	device, ok := a.data.Devices[id]
	return ok && !device.Revoked && len(token) == 64 && equal(device.TokenHash, digest(token))
}

func credentials(r *http.Request) (string, string) {
	header, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return "", ""
	}
	id, token, _ := strings.Cut(header, ".")
	return id, token
}

func (a *app) decorate(tunnel control.Tunnel) control.Tunnel {
	tunnel.Online = a.online[proxyName(tunnel)] && tunnel.Enabled && tunnel.State == "approved" && !a.data.Devices[tunnel.DeviceID].Revoked
	if tunnel.Port != 0 {
		tunnel.Address = net.JoinHostPort(a.data.Domain, strconv.Itoa(tunnel.Port))
	}
	return tunnel
}

func (a *app) clientState(w http.ResponseWriter, r *http.Request) {
	id, token := credentials(r)
	a.mu.RLock()
	if !a.verifyLocked(id, token) {
		a.mu.RUnlock()
		fail(w, failure{403, "设备授权无效或已撤销"})
		return
	}
	result := control.DeviceState{DeviceID: id, Tunnels: []control.Tunnel{}}
	for _, tunnel := range a.data.Tunnels {
		if tunnel.DeviceID == id {
			result.Tunnels = append(result.Tunnels, a.decorate(tunnel))
		}
	}
	a.mu.RUnlock()
	slices.SortFunc(result.Tunnels, func(x, y control.Tunnel) int { return strings.Compare(x.Name, y.Name) })
	respond(w, 200, result)
}

func (a *app) requestTunnel(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Name, LocalIP string
		LocalPort     int
	}
	if !decode(w, r, &request) {
		return
	}
	id, token := credentials(r)
	request.Name = strings.TrimSpace(request.Name)
	ip := net.ParseIP(strings.TrimSpace(request.LocalIP))
	if request.Name == "" || len(request.Name) > 120 || ip == nil || ip.IsUnspecified() || ip.IsMulticast() || request.LocalPort < 1 || request.LocalPort > 65535 {
		fail(w, failure{400, "请输入名称、有效本机 IP 和 1–65535 的端口"})
		return
	}
	tunnel := control.Tunnel{ID: randomID(12), DeviceID: id, Name: request.Name, LocalIP: ip.String(), LocalPort: request.LocalPort, State: "pending"}
	a.mu.Lock()
	err := a.changeLocked(func(s *state) error {
		if !a.verifyLocked(id, token) {
			return failure{403, "设备授权无效或已撤销"}
		}
		count := 0
		for _, old := range s.Tunnels {
			if old.DeviceID == id && old.State == "pending" {
				count++
			}
		}
		if count >= 32 {
			return failure{409, "待审批申请已达上限"}
		}
		s.Tunnels[tunnel.ID] = tunnel
		return nil
	})
	a.mu.Unlock()
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, 201, tunnel)
}

func (a *app) invite(w http.ResponseWriter, r *http.Request) {
	var request struct{}
	if !decode(w, r, &request) {
		return
	}
	code := randomID(16)
	now := time.Now()
	expires := now.Add(24 * time.Hour)
	a.mu.Lock()
	err := a.changeLocked(func(s *state) error {
		for key, expiry := range s.Invites {
			if !expiry.After(now) {
				delete(s.Invites, key)
			}
		}
		if len(s.Invites) >= 256 {
			return failure{409, "未使用的邀请码已达上限"}
		}
		s.Invites[digest(code)] = expires
		return nil
	})
	a.mu.Unlock()
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, 201, map[string]any{"code": code, "expires": expires})
}

var domainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func validDomain(domain string) bool {
	if net.ParseIP(domain) != nil {
		return true
	}
	if len(domain) > 253 || domain == "" {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if !domainLabel.MatchString(label) {
			return false
		}
	}
	return true
}

func (a *app) domain(w http.ResponseWriter, r *http.Request) {
	var request struct{ Domain string }
	if !decode(w, r, &request) {
		return
	}
	domain := strings.ToLower(strings.TrimSpace(request.Domain))
	if !validDomain(domain) {
		fail(w, failure{400, "请输入域名或 IP 地址"})
		return
	}
	a.mu.Lock()
	err := a.changeLocked(func(s *state) error { s.Domain = domain; return nil })
	a.mu.Unlock()
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, 200, true)
}

func (a *app) revoke(w http.ResponseWriter, r *http.Request) {
	var request struct{}
	if !decode(w, r, &request) {
		return
	}
	id := r.PathValue("id")
	a.mu.Lock()
	err := a.changeLocked(func(s *state) error {
		device, ok := s.Devices[id]
		if !ok {
			return failure{404, "设备不存在"}
		}
		device.Revoked = true
		device.TokenHash = ""
		s.Devices[id] = device
		for key, tunnel := range s.Tunnels {
			if tunnel.DeviceID != id {
				continue
			}
			tunnel.Enabled = false
			if tunnel.State == "pending" {
				tunnel.State = "rejected"
			}
			s.Tunnels[key] = tunnel
		}
		return nil
	})
	a.mu.Unlock()
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, 200, true)
}

func (a *app) setTunnel(w http.ResponseWriter, r *http.Request) {
	var request struct{ Action string }
	if !decode(w, r, &request) {
		return
	}
	id := r.PathValue("id")
	a.mu.Lock()
	err := a.changeLocked(func(s *state) error {
		tunnel, ok := s.Tunnels[id]
		if !ok {
			return failure{404, "隧道不存在"}
		}
		switch request.Action {
		case "approve":
			if tunnel.State != "pending" || s.Devices[tunnel.DeviceID].Revoked {
				return failure{409, "此隧道无法批准"}
			}
			used := make(map[int]bool)
			for _, item := range s.Tunnels {
				if item.Port != 0 {
					used[item.Port] = true
				}
			}
			for port := a.gateway.first; port <= a.gateway.last; port++ {
				if !used[port] {
					tunnel.Port = port
					break
				}
			}
			if tunnel.Port == 0 {
				return failure{409, "端口池已满"}
			}
			tunnel.State, tunnel.Enabled = "approved", true
		case "reject":
			if tunnel.State != "pending" {
				return failure{409, "此隧道无法拒绝"}
			}
			tunnel.State = "rejected"
		case "enable", "disable":
			if tunnel.State != "approved" || s.Devices[tunnel.DeviceID].Revoked {
				return failure{409, "此隧道无法修改"}
			}
			tunnel.Enabled = request.Action == "enable"
		default:
			return failure{400, "未知操作"}
		}
		s.Tunnels[id] = tunnel
		return nil
	})
	a.mu.Unlock()
	if err != nil {
		fail(w, err)
		return
	}
	respond(w, 200, true)
}

func (a *app) dashboard(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	domain := a.data.Domain
	devices := []device{}
	tunnels := []tunnelView{}
	for _, item := range a.data.Devices {
		item.TokenHash = ""
		devices = append(devices, item)
	}
	for id, tunnel := range a.data.Tunnels {
		tunnels = append(tunnels, tunnelView{Tunnel: a.decorate(tunnel), Device: a.data.Devices[tunnel.DeviceID].Name, Rate: a.rates[id]})
	}
	a.mu.RUnlock()
	now := time.Now().In(a.zone)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, a.zone)
	err := a.db.View(func(tx *bolt.Tx) error {
		root := tx.Bucket(trafficBucket)
		for i := range tunnels {
			tunnels[i].Today, _ = queryTraffic(root.Bucket([]byte(tunnels[i].ID)), today, 0)
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	slices.SortFunc(devices, func(x, y device) int { return strings.Compare(x.Name, y.Name) })
	slices.SortFunc(tunnels, func(x, y tunnelView) int { return strings.Compare(x.Name, y.Name) })
	respond(w, 200, map[string]any{"domain": domain, "devices": devices, "tunnels": tunnels, "timezone": a.zone.String()})
}

func (a *app) history(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.mu.RLock()
	_, ok := a.data.Tunnels[id]
	a.mu.RUnlock()
	if !ok {
		fail(w, failure{404, "隧道不存在"})
		return
	}
	step, count := 15*time.Minute, 96
	if r.URL.Query().Get("range") == "7d" {
		step, count = time.Hour, 168
	}
	end := time.Now().Truncate(step)
	start := end.Add(-time.Duration(count-1) * step)
	var total traffic
	var recorded []trafficPoint
	err := a.db.View(func(tx *bolt.Tx) error {
		total, recorded = queryTraffic(tx.Bucket(trafficBucket).Bucket([]byte(id)), start, step)
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	points := make([]trafficPoint, count)
	for i := range points {
		points[i].Time = start.Add(time.Duration(i) * step).Unix()
	}
	for _, point := range recorded {
		index := int((point.Time - start.Unix()) / int64(step.Seconds()))
		if index >= 0 && index < len(points) {
			points[index] = point
		}
	}
	respond(w, 200, map[string]any{"total": total, "points": points, "seconds": step.Seconds(), "timezone": a.zone.String()})
}
