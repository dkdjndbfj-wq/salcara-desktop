package console

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"reflect"
	"salcara/bridge/internal/config"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type apiListener struct {
	listener net.Listener
	server   *http.Server
	port     int
}
type apiListenerManager struct {
	mu        sync.Mutex
	entries   []apiListener
	settings  config.CodexAPINetwork
	lastError string
	binding   string
	closed    bool
}
type apiListenerContext struct{}
type apiListenerPermit struct {
	manager *apiListenerManager
	epoch   string
	binding string
	port    int
}

func (s *Server) initAPINetwork() {
	s.apiNetwork = &apiListenerManager{}
	s.apiService.AllowRemote = func(r *http.Request, c config.Config) bool {
		p, ok := r.Context().Value(apiListenerContext{}).(apiListenerPermit)
		if !ok || p.manager != s.apiNetwork || p.epoch != c.CodexAPI.AuthEpoch || c.CodexAPI.Network.Scope() != "lan" {
			return false
		}
		m := s.apiNetwork
		m.mu.Lock()
		live := !m.closed && m.binding == p.binding && p.binding != ""
		m.mu.Unlock()
		return live
	}
	if s.d.Store != nil {
		s.d.Store.OnChange(func(_, c config.Config) {
			m := s.apiNetwork
			m.mu.Lock()
			var retired []apiListener
			if !c.CodexAPIActive() || !reflect.DeepEqual(m.settings, c.CodexAPI.Network) {
				retired, m.entries = m.entries, nil
				m.binding = ""
			}
			m.mu.Unlock()
			retireAPIListeners(retired, false)
		})
	}
}
func retireAPIListeners(entries []apiListener, force bool) {
	for _, entry := range entries {
		_ = entry.listener.Close()
		go func(entry apiListener) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := entry.server.Shutdown(ctx); err != nil && force {
				_ = entry.server.Close()
			}
		}(entry)
	}
}
func (s *Server) CloseAPIListeners() {
	s.closeOfficialUsageMaintenance()
	if s.apiNetwork == nil {
		return
	}
	m := s.apiNetwork
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	s.apiService.CloseAdmission()
	retired := m.entries
	m.entries = nil
	m.binding = ""
	m.mu.Unlock()
	retireAPIListeners(retired, true)
	if err := s.closeCodexServiceProfile(); err != nil {
		s.log.Printf("Codex service shutdown: %s", err.Error())
	}
}
func (s *Server) syncAPIListeners() error {
	if s.apiNetwork == nil || s.d.Store == nil {
		return nil
	}
	m := s.apiNetwork
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("API 监听服务已关闭")
	}
	c := s.d.Store.Get()
	if !c.CodexAPIActive() {
		return nil
	}
	n := c.CodexAPI.Network
	if config.ValidateCodexAPINetwork(n) != nil {
		return errors.New("API 网络设置无效")
	}
	for _, p := range n.Ports {
		if p == s.d.Port {
			return errors.New("API 专用端口不能使用控制台端口")
		}
	}
	if reflect.DeepEqual(m.settings, n) && len(m.entries) == len(n.Ports) {
		return nil
	}
	var opened []apiListener
	binding := config.RandomToken(24)
	bind := "127.0.0.1"
	if n.Scope() == "lan" {
		bind = "0.0.0.0"
	}
	for _, p := range n.Ports {
		ln, err := net.Listen("tcp4", net.JoinHostPort(bind, strconv.Itoa(p)))
		if err != nil {
			for _, entry := range opened {
				_ = entry.listener.Close()
			}
			m.lastError = "API 端口 " + strconv.Itoa(p) + " 无法监听，请检查占用"
			return errors.New(m.lastError)
		}
		server := &http.Server{Handler: s.apiListenerHandler(p, n, binding), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 64 << 10}
		opened = append(opened, apiListener{listener: ln, server: server, port: p})
	}
	latest := s.d.Store.Get()
	if !latest.CodexAPIActive() || latest.CodexAPI.AuthEpoch != c.CodexAPI.AuthEpoch {
		for _, entry := range opened {
			_ = entry.listener.Close()
		}
		return errCodexAPIRuntimeChanged
	}
	old := m.entries
	m.entries = opened
	m.settings = n
	m.lastError = ""
	m.binding = binding
	retireAPIListeners(old, false)
	for _, entry := range opened {
		go func(entry apiListener) { _ = entry.server.Serve(entry.listener) }(entry)
	}
	return nil
}
func listenerHostAllowed(host string, port int, scope string) bool {
	name, p, err := net.SplitHostPort(host)
	if err != nil || p != strconv.Itoa(port) {
		return false
	}
	if name == "localhost" || name == "127.0.0.1" {
		return true
	}
	ip := net.ParseIP(name)
	if scope != "lan" || ip == nil || !ip.IsPrivate() {
		return false
	}
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, address := range addresses {
		local, _, err := net.ParseCIDR(address.String())
		if err == nil && local.Equal(ip) {
			return true
		}
	}
	return false
}
func localAPIHosts() []string {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return []string{}
	}
	seen := map[string]bool{}
	for _, address := range addresses {
		ip, _, err := net.ParseCIDR(address.String())
		if err == nil && ip.To4() != nil && ip.IsPrivate() {
			seen[ip.String()] = true
		}
	}
	out := make([]string, 0, len(seen))
	for ip := range seen {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}
func (s *Server) apiListenerHandler(port int, n config.CodexAPINetwork, binding string) http.Handler {
	return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := s.apiNetwork
		m.mu.Lock()
		live := !m.closed && binding != "" && m.binding == binding
		m.mu.Unlock()
		c := s.d.Store.Get()
		if !live || !c.CodexAPIActive() || !reflect.DeepEqual(n, c.CodexAPI.Network) {
			writeErr(w, 503, "API 监听授权已停止")
			return
		}
		if !listenerHostAllowed(r.Host, port, n.Scope()) || r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeErr(w, 403, "API 来源无效")
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if err != nil || ip == nil || !ip.IsLoopback() && (n.Scope() != "lan" || !ip.IsPrivate()) {
			writeErr(w, 403, "API 来源无效")
			return
		}
		path := r.URL.Path
		native := strings.HasPrefix(path, "/v1beta/models") || path == "/api/chat" || path == "/api/generate" || path == "/api/tags"
		if !native && !strings.HasPrefix(path, "/v1/") && !strings.HasPrefix(path, "/backend-api/codex/") && !strings.HasPrefix(path, "/codex-api/") {
			writeErr(w, 404, "API 路由不存在")
			return
		}
		request := r.Clone(context.WithValue(r.Context(), apiListenerContext{}, apiListenerPermit{manager: s.apiNetwork, epoch: c.CodexAPI.AuthEpoch, binding: binding, port: port}))
		if native || strings.HasPrefix(path, "/v1/") {
			u := *r.URL
			u.Path = "/codex-api" + path
			request.URL = &u
		}
		s.apiService.ServeHTTP(w, request)
	}))
}
func apiNetworkRevision(epoch string) string {
	digest := sha256.Sum256([]byte("salcara-api-network/" + epoch))
	return hex.EncodeToString(digest[:])
}
func (s *Server) apiNetworkRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/local/codex-api/network", s.handleAPINetwork)
	m.HandleFunc("POST /api/local/codex-api/network", s.handleAPINetwork)
}
func (s *Server) handleAPINetwork(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if !s.codexProfileMu.TryLock() {
			writeErr(w, http.StatusConflict, "API 服务或 Codex 配置正在更新")
			return
		}
		defer s.codexProfileMu.Unlock()
		if s.apiServiceClosed() {
			writeErr(w, http.StatusServiceUnavailable, "API 监听服务已关闭")
			return
		}
	}
	if r.URL.RawQuery != "" {
		writeErr(w, 400, "网络设置不接收查询参数")
		return
	}
	before := s.d.Store.Get()
	if r.Method == http.MethodPost {
		var in struct {
			BindScope  string `json:"bindScope"`
			Ports      []int  `json:"ports"`
			ClientHost string `json:"clientHost"`
			Revision   string `json:"revision"`
		}
		if readReverseJSON(r, &in, 8192) != nil {
			writeErr(w, 400, "网络设置格式无效")
			return
		}
		settings := config.CodexAPINetwork{BindScope: in.BindScope, Ports: in.Ports, ClientHost: in.ClientHost}
		if config.ValidateCodexAPINetwork(settings) != nil {
			writeErr(w, 400, "请选择访问范围、IPv4 主机和最多 16 个不同端口")
			return
		}
		if settings.Host() != "localhost" && settings.Host() != "127.0.0.1" && !listenerHostAllowed(net.JoinHostPort(settings.Host(), "1"), 1, settings.Scope()) {
			writeErr(w, 400, "客户端主机须为本机当前的局域网 IPv4 地址")
			return
		}
		if in.Revision != apiNetworkRevision(before.CodexAPI.AuthEpoch) {
			writeErr(w, 409, "网络设置已变化，请刷新后重试")
			return
		}
		for _, p := range in.Ports {
			if p == s.d.Port {
				writeErr(w, 400, "专用端口不能与控制台相同")
				return
			}
		}
		epoch, ok := s.d.Store.RetireCodexAPIServiceRuntime(before.CodexAPI.AuthEpoch)
		if !ok {
			writeErr(w, 409, "API 授权已变化")
			return
		}
		if err := s.d.Store.Update(func(c *config.Config) error {
			if c.CodexAPI.AuthEpoch != epoch {
				return errCodexAPIRuntimeChanged
			}
			c.CodexAPI.Network = settings
			c.CodexAPI.Enabled = before.CodexAPI.Enabled
			if before.CodexAPIActive() {
				c.CodexAPI.RuntimeLease = c.GatewayRuntimeKey
			}
			return nil
		}); err != nil {
			writeErr(w, 500, "网络设置未保存；API 服务已在本次进程停止")
			return
		}
		if err := s.syncAPIListeners(); err != nil {
			current := s.d.Store.Get()
			_, _ = s.d.Store.RetireCodexAPIServiceRuntime(current.CodexAPI.AuthEpoch)
			writeErr(w, 503, err.Error())
			return
		}
	}
	c := s.d.Store.Get()
	endpoints := []string{}
	for _, p := range c.CodexAPI.Network.Ports {
		endpoints = append(endpoints, "http://"+net.JoinHostPort(c.CodexAPI.Network.Host(), strconv.Itoa(p))+"/v1")
	}
	m := s.apiNetwork
	m.mu.Lock()
	active := len(m.entries)
	lastError := m.lastError
	m.mu.Unlock()
	writeJSON(w, map[string]any{"bindScope": c.CodexAPI.Network.Scope(), "ports": c.CodexAPI.Network.Ports, "clientHost": c.CodexAPI.Network.Host(), "revision": apiNetworkRevision(c.CodexAPI.AuthEpoch), "baseUrls": endpoints, "listeningPorts": active, "enabled": c.CodexAPI.Enabled, "requiresStart": c.CodexAPI.Enabled && !c.CodexAPIActive(), "running": c.CodexAPIActive(), "lastError": lastError, "localHosts": localAPIHosts(), "wsl": map[string]any{"mirroredLocalhostSupported": true, "natRequiresLAN": true, "hostAddressRequired": true}})
}
