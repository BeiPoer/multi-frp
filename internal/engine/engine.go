package engine

import (
	"fmt"
	"strconv"

	"github.com/fatedier/frp/client"
	"github.com/fatedier/frp/pkg/config/source"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"multifrp/internal/control"
)

func New(remote control.Remote, tunnels []control.Tunnel) (*client.Service, error) {
	port := 443
	protocol := "wss"
	if remote.Origin.Scheme == "http" {
		port, protocol = 80, "websocket"
	}
	if raw := remote.Origin.Port(); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 65535 {
			return nil, fmt.Errorf("服务器端口无效")
		}
		port = value
	}
	config := &v1.ClientCommonConfig{
		ServerAddr: remote.Origin.Hostname(), ServerPort: port, User: remote.Credentials.DeviceID, ClientID: remote.Credentials.DeviceID,
		LoginFailExit: new(false), Metadatas: map[string]string{"deviceToken": remote.Credentials.Token},
		Transport: v1.ClientTransportConfig{Protocol: protocol, WireProtocol: "v2", TCPMux: new(true), HeartbeatInterval: 10, HeartbeatTimeout: 30,
			TLS: v1.TLSClientConfig{Enable: new(protocol == "wss"), TLSConfig: v1.TLSConfig{UseSystemCa: true}}},
	}
	if err := config.Complete(); err != nil {
		return nil, err
	}
	src := source.NewConfigSource()
	if err := src.ReplaceAll(proxies(tunnels), nil); err != nil {
		return nil, err
	}
	return client.NewService(client.ServiceOptions{Common: config, ConfigSourceAggregator: source.NewAggregator(src)})
}

func Update(service *client.Service, tunnels []control.Tunnel) error {
	return service.UpdateAllConfigurer(proxies(tunnels), nil)
}

func proxies(tunnels []control.Tunnel) []v1.ProxyConfigurer {
	result := make([]v1.ProxyConfigurer, 0, len(tunnels))
	for _, t := range tunnels {
		if t.State != "approved" || !t.Enabled {
			continue
		}
		proxy := &v1.TCPProxyConfig{ProxyBaseConfig: v1.ProxyBaseConfig{
			Name: t.ID, Type: "tcp", ProxyBackend: v1.ProxyBackend{LocalIP: t.LocalIP, LocalPort: t.LocalPort},
		}, RemotePort: t.Port}
		proxy.Complete()
		result = append(result, proxy)
	}
	return result
}
