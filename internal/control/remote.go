package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Remote struct {
	Origin      *url.URL
	Credentials Enrollment
}

var httpClient = &http.Client{
	Timeout:       8 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type RemoteError struct {
	Code    int
	Message string
}

func (e RemoteError) Error() string { return e.Message }

func NewRemote(raw string, credentials Enrollment) (Remote, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	origin, err := url.Parse(raw)
	if err != nil || origin.Hostname() == "" || origin.User != nil || origin.RawQuery != "" || origin.Fragment != "" || (origin.Path != "" && origin.Path != "/") {
		return Remote{}, errors.New("请输入管理端地址，例如 https://manage.example.com")
	}
	ip := net.ParseIP(origin.Hostname())
	if origin.Scheme != "https" && !(origin.Scheme == "http" && (origin.Hostname() == "localhost" || ip.IsLoopback())) {
		return Remote{}, errors.New("管理端地址需要使用 HTTPS")
	}
	origin.Path, origin.RawPath = "", ""
	return Remote{Origin: origin, Credentials: credentials}, nil
}

func (r Remote) request(ctx context.Context, path string, payload, result any) error {
	method := http.MethodGet
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		method, body = http.MethodPost, bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, r.Origin.String()+"/api/client/"+path, body)
	if err != nil {
		return err
	}
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if r.Credentials.Token != "" {
		request.Header.Set("Authorization", "Bearer "+r.Credentials.DeviceID+"."+r.Credentials.Token)
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8<<20))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var result struct{ Error string }
		_ = decoder.Decode(&result)
		if result.Error == "" {
			result.Error = fmt.Sprintf("管理端返回 HTTP %d", response.StatusCode)
		}
		return RemoteError{Code: response.StatusCode, Message: result.Error}
	}
	return decoder.Decode(result)
}

func (r Remote) Enroll(ctx context.Context, code, name string) (Enrollment, error) {
	var result Enrollment
	err := r.request(ctx, "enroll", map[string]string{"code": code, "name": name}, &result)
	return result, err
}

func (r Remote) State(ctx context.Context) (DeviceState, error) {
	var result DeviceState
	err := r.request(ctx, "state", nil, &result)
	return result, err
}

func (r Remote) Submit(ctx context.Context, tunnel Tunnel) error {
	return r.request(ctx, "tunnels", tunnel, new(Tunnel))
}
