package config

import (
	"errors"
	"net"
)

// Extra listeners carry only the authenticated API, never the console.
// Empty settings retain the historical console-mounted loopback endpoint.
type CodexAPINetwork struct {
	BindScope  string `json:"bindScope,omitempty"`
	Ports      []int  `json:"ports,omitempty"`
	ClientHost string `json:"clientHost,omitempty"`
}

func (n CodexAPINetwork) Scope() string {
	if n.BindScope == "" {
		return "loopback"
	}
	return n.BindScope
}
func (n CodexAPINetwork) Host() string {
	if n.ClientHost == "" {
		return "127.0.0.1"
	}
	return n.ClientHost
}
func ValidateCodexAPINetwork(n CodexAPINetwork) error {
	invalid := errors.New("API 网络设置无效")
	if n.Scope() != "loopback" && n.Scope() != "lan" || len(n.Ports) > 16 {
		return invalid
	}
	seen := map[int]bool{}
	for _, p := range n.Ports {
		if p < 1 || p > 65535 || seen[p] {
			return invalid
		}
		seen[p] = true
	}
	host := n.Host()
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() == nil || ip.IsLoopback() && !ip.Equal(net.ParseIP("127.0.0.1")) || !ip.IsLoopback() && !ip.IsPrivate() || n.Scope() == "loopback" && !ip.IsLoopback() {
		return invalid
	}
	return nil
}
