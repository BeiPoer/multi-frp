package control

// Tunnel is the configuration shared by the server and Windows client.
type Tunnel struct {
	ID        string `json:"id"`
	DeviceID  string `json:"deviceID"`
	Name      string `json:"name"`
	LocalIP   string `json:"localIP"`
	LocalPort int    `json:"localPort"`
	Port      int    `json:"port"`
	State     string `json:"state"`
	Enabled   bool   `json:"enabled"`
	Online    bool   `json:"online"`
	Address   string `json:"address"`
}

type DeviceState struct {
	DeviceID string   `json:"deviceID"`
	Tunnels  []Tunnel `json:"tunnels"`
}

type Enrollment struct {
	DeviceID string `json:"deviceID"`
	Token    string `json:"token"`
}
