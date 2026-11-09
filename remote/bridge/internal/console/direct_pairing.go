package console

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/directrelay"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/protocol"
)

const directRelayPort = 47832

// StartDirectRelay starts the dedicated TLS listener only when the user has
// previously opted in by requesting a direct pairing QR.
func (s *Server) StartDirectRelay() error {
	s.directLifecycleMu.Lock()
	defer s.directLifecycleMu.Unlock()
	if s.d.Store == nil || !s.d.Store.Get().DirectRelayEnabled {
		return nil
	}
	s.directMu.Lock()
	defer s.directMu.Unlock()
	if s.directHTTP != nil {
		return nil
	}
	if err := s.startDirectRelayLocked(s.d.Store.Get()); err != nil {
		s.directLastError = err.Error()
		return err
	}
	return nil
}

func (s *Server) startDirectRelayLocked(c config.Config) error {
	if s.d.Store == nil {
		return errors.New("direct relay requires the configuration store")
	}
	if !c.DirectRelayEnabled {
		return errors.New("direct relay is not enabled")
	}
	savedHosts := uniqueDirectHosts(c.DirectRelayHosts)
	if len(savedHosts) == 0 {
		return errors.New("direct relay has no saved local network addresses")
	}
	activeHosts := matchingDirectHosts(savedHosts, s.currentDirectHosts())
	if len(activeHosts) == 0 {
		return errors.New("none of the saved direct addresses are available on this computer")
	}
	identityPath := filepath.Join(filepath.Dir(s.d.Store.Path()), "direct-relay-identity.pem")
	identity, err := directrelay.LoadOrCreateTLSIdentity(identityPath, savedHosts)
	if err != nil {
		return err
	}
	listeners, boundHosts, port, err := openDirectListeners(s.directListenAddr, activeHosts)
	if err != nil {
		return err
	}
	secureURLs := func() []string {
		hosts := matchingDirectHosts(boundHosts, s.currentDirectHosts())
		urls := make([]string, 0, len(hosts))
		for _, host := range hosts {
			urls = append(urls, "wss://"+net.JoinHostPort(host, strconv.Itoa(port))+"/direct/ws")
		}
		return urls
	}
	var relay *directrelay.Relay
	relay = directrelay.New(directrelay.Options{
		Logger:            s.log,
		DeviceID:          c.DeviceID,
		DeviceName:        c.DeviceName,
		DeviceOS:          runtime.GOOS,
		CertificateSHA256: identity.Fingerprint,
		PairedPhoneID:     c.DirectPhoneID,
		PairedPhoneHash:   c.DirectPhoneHash,
		SecureURLs:        secureURLs,
		OnPaired: func(phoneID, phoneHash string) error {
			return s.d.Store.Update(func(next *config.Config) error {
				if !next.DirectRelayEnabled || next.DirectPhoneID != "" || next.DirectPhoneHash != "" {
					return errors.New("direct pairing state changed; scan a fresh QR")
				}
				next.DirectPhoneID, next.DirectPhoneHash = phoneID, phoneHash
				next.DirectAPI = nil
				return nil
			})
		},
		OnRevoked: func() error {
			return s.d.Store.Update(func(next *config.Config) error {
				if next.EffectiveRemoteMode() == "direct" {
					next.ActivateRemoteMode("none")
				}
				next.DirectPhoneID, next.DirectPhoneHash = "", ""
				next.DirectRelayEnabled = false
				next.DirectRelayHosts = nil
				next.DirectAPI = nil
				return nil
			})
		},
		OnDisconnected: func(target directrelay.EventTarget) {
			s.clearDirectEventSubscriptionsForTarget(relay, target)
		},
	})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !s.d.Store.Get().DirectRemoteEnabled() {
				http.Error(w, "direct mode is not active", http.StatusServiceUnavailable)
				return
			}
			relay.Handler().ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	s.directRelay = relay
	commandCtx, commandCancel := context.WithCancel(context.Background())
	s.directCommandCancel = commandCancel
	s.directHTTP = srv
	s.directListeners = listeners
	s.directHosts = boundHosts
	s.directFingerprint = identity.Fingerprint
	s.directStartedAt = time.Now()
	s.directLastError = ""
	go s.serveDirectRelay(srv, identity.Certificate, listeners...)
	go s.runDirectCommands(commandCtx, relay)
	s.log.Printf("direct pairing TLS listener started on %d for %d local addresses (Hub fallback disabled)", port, len(listeners))
	return nil
}

const directCommandConcurrency = 4

var directCommandTypes = map[string]struct{}{
	"workspace.projects":         {},
	"workspace.explorer":         {},
	"computer.management.status": {}, "computer.api.create": {}, "computer.agent.install": {},
	"workspace.project.preview": {},
	"workspace.artifacts":       {},
	"agents.status":             {}, "agents.api.set": {}, "projects.list": {}, "sessions.list": {}, "session.open": {},
	"session.start": {}, "session.send": {}, "session.continue": {}, "session.steer": {}, "session.interrupt": {},
	"session.revert":   {},
	"approval.respond": {}, "attachment.put": {}, "attachment.read": {}, "models.list": {}, "features.list": {},
	"session.context": {}, "session.compact": {}, "session.actions": {}, "session.action": {}, "sessions.archived": {},
	"session.workflows": {}, "session.workflow": {}, "session.side-question.catalog": {}, "session.side-question.ask": {},
	"session.side-question.open": {}, "session.side-question.read": {}, "session.side-question.send": {}, "session.side-question.delete": {}, "session.side-question.tools": {},
	"workspace.files.list": {}, "workspace.files.read": {}, "workspace.files.write": {}, "workspace.files.mutate": {}, "workspace.git.read": {}, "workspace.git.mutate": {},
	"workspace.preview.file": {}, "workspace.preview.open": {}, "workspace.preview.frame": {}, "workspace.preview.input": {}, "workspace.preview.close": {},
	"workspace.terminal.open": {}, "workspace.terminal.read": {}, "workspace.terminal.input": {}, "workspace.terminal.resize": {}, "workspace.terminal.close": {}, "workspace.project.files.list": {}, "workspace.project.files.read": {},
}

// These commands operate on an existing session. Their key must be fenced
// before Dispatch so concurrent events cannot enter the Hub queue first.
var directSessionScopedCommands = map[string]struct{}{
	"workspace.project.preview": {},
	"workspace.artifacts":       {},
	"session.open":              {}, "session.send": {}, "session.continue": {}, "session.steer": {}, "session.interrupt": {},
	"session.revert":   {},
	"approval.respond": {}, "attachment.read": {}, "session.context": {}, "session.compact": {}, "session.actions": {}, "session.action": {},
	"session.workflows": {}, "session.workflow": {}, "session.side-question.catalog": {}, "session.side-question.ask": {},
	"session.side-question.open": {}, "session.side-question.read": {}, "session.side-question.send": {}, "session.side-question.delete": {}, "session.side-question.tools": {},
	"workspace.files.list": {}, "workspace.files.read": {}, "workspace.files.write": {}, "workspace.files.mutate": {}, "workspace.git.read": {}, "workspace.git.mutate": {},
	"workspace.preview.file": {}, "workspace.preview.open": {}, "workspace.preview.frame": {}, "workspace.preview.input": {}, "workspace.preview.close": {},
	"workspace.terminal.open": {}, "workspace.terminal.read": {}, "workspace.terminal.input": {}, "workspace.terminal.resize": {}, "workspace.terminal.close": {},
}

func directCommandAllowed(command map[string]any) error {
	typ, _ := command["type"].(string)
	if directManagementCommand(typ) {
		_, err := parseDirectManagement(command)
		return err
	}
	if _, ok := directCommandTypes[typ]; !ok {
		return errors.New("当前直连不支持这个操作")
	}
	if raw, present := command["sessionKey"]; present {
		if _, ok := raw.(string); !ok {
			return errors.New("直连会话标识无效")
		}
	}
	if _, scoped := directSessionScopedCommands[typ]; scoped {
		if sessionKey, ok := command["sessionKey"].(string); !ok || strings.TrimSpace(sessionKey) == "" {
			return errors.New("直连会话操作缺少会话标识")
		}
	}
	// Only commands that do not inspect or mutate a conversation may omit an
	// explicit surface. All session, workspace, model and feature operations
	// must select the CLI surface; direct pairing never grants desktop-history
	// access implicitly.
	if typ != "agents.status" && typ != "projects.list" && typ != "agents.api.set" {
		if surface, present := command["controlSurface"]; !present || surface != "cli" {
			return errors.New("电脑直连当前只允许 CLI 会话，请明确选择 CLI 会话")
		}
	} else if surface, present := command["controlSurface"]; present && surface != "cli" {
		return errors.New("电脑直连当前只允许 CLI 会话，不会切换到桌面或历史记录通道")
	}
	if typ == "agents.api.set" {
		for field := range command {
			switch field {
			case "type", "agent", "accountId", "model", "sessionKey", "controlSurface":
			default:
				return errors.New("直连 API 切换参数无效")
			}
		}
		agent, _ := command["agent"].(string)
		if strings.HasSuffix(agent, "-desktop") || !config.ValidAgentFamily(agent) {
			return errors.New("电脑直连只允许切换 CLI Agent 的 API")
		}
		if _, ok := command["accountId"].(string); !ok {
			return errors.New("直连 API 标识无效")
		}
	}
	if _, present := command["historyIdentity"]; present {
		return errors.New("电脑直连不读取只读桌面历史")
	}
	// The direct relay is a CLI-only surface.  Reject the native Claude
	// Desktop entrypoint explicitly instead of relying on Dispatch to infer
	// the surface from the command's controlSurface field.
	if entrypoint, present := command["entrypoint"]; present {
		if value, ok := entrypoint.(string); !ok || strings.TrimSpace(value) != "" {
			return errors.New("电脑直连不启动原生桌面入口")
		}
	}
	if typ == "sessions.list" {
		tool, _ := command["tool"].(string)
		if strings.TrimSpace(tool) == "" {
			return errors.New("电脑直连会话列表必须明确选择 Agent")
		}
		client, present := command["client"]
		if tool == "claude" {
			if !present || client != "code" {
				return errors.New("电脑直连只允许读取 Claude Code 会话")
			}
		} else if present {
			return errors.New("电脑直连会话列表不接受其他 Agent 的 Claude 会话类型")
		}
	} else if client, present := command["client"]; present {
		tool, _ := command["tool"].(string)
		if tool != "claude" || client != "code" {
			return errors.New("电脑直连只允许 Claude Code 会话")
		}
	}
	if typ == "approval.respond" {
		if key, _ := command["sessionKey"].(string); key == "" {
			return errors.New("直连审批缺少会话归属")
		}
	}
	if typ == "session.side-question.tools" {
		for key := range command {
			if strings.EqualFold(key, "owner") || strings.EqualFold(key, "action") && key != "action" {
				return errors.New("临时工具会话不接受自定义归属或歧义操作")
			}
		}
		switch command["action"] {
		case "open", "read", "features", "send", "stop", "approve", "close":
		default:
			return errors.New("临时工具会话操作无效")
		}
	}
	return nil
}

func (s *Server) runDirectCommands(ctx context.Context, relay *directrelay.Relay) {
	if relay == nil {
		return
	}
	semaphore := make(chan struct{}, directCommandConcurrency)
	for {
		select {
		case <-ctx.Done():
			return
		case command := <-relay.Commands():
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			go func(command directrelay.Command) {
				defer func() { <-semaphore }()
				result, commandErr, admitted := relay.RunCurrentCommand(command, func(commandCtx context.Context, payload map[string]any) (any, error) {
					identity, pairingValid := command.BindingValidator()
					validate := func() error {
						if !s.d.Store.Get().DirectRemoteEnabled() {
							return errors.New("电脑直连当前未启用")
						}
						return pairingValid()
					}
					if err := validate(); err != nil {
						return nil, err
					}
					if err := directCommandAllowed(payload); err != nil {
						return nil, err
					}
					if directManagementCommand(commandType(payload)) {
						return s.dispatchDirectManagement(hubclient.WithDirectExecutionValidator(commandCtx, identity, validate), payload)
					}
					if s.d.Hub == nil {
						return nil, errors.New("本机 Agent 执行服务尚未就绪")
					}
					typ := commandType(payload)
					// Every existing-session command carries its stable key. Install
					// the privacy fence before Dispatch so events emitted while the
					// command is running cannot race to Hub first.
					if sessionKey, _ := payload["sessionKey"].(string); sessionKey != "" {
						if persistErr := s.setDirectEventSubscription(relay, command.EventTarget(), sessionKey); persistErr != nil {
							if unknownErr := s.markDirectStartUnknown(); unknownErr != nil {
								s.log.Printf("direct: failed to persist session privacy fence: %v", unknownErr)
							}
							return nil, fmt.Errorf("直连会话隐私栅栏无法保存：%w", persistErr)
						}
					}
					if typ == "session.start" {
						if err := s.beginDirectSessionStart(); err != nil {
							return nil, fmt.Errorf("无法安全登记直连会话：%w", err)
						}
					}
					deadlineCtx, cancel := context.WithTimeout(commandCtx, 90*time.Second)
					defer cancel()
					result, err := s.d.Hub.Dispatch(hubclient.WithDirectExecutionValidator(deadlineCtx, identity, validate), payload)
					var sessionKey string
					if err == nil && typ == "session.start" {
						// The phone cannot know this key until the command completes.
						// Its first live output is recovered by the follow-up snapshot.
						if response, ok := result.(map[string]any); ok {
							sessionKey, _ = response["sessionKey"].(string)
						}
					}
					if typ == "session.start" {
						unknownOutcome := err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || protocol.ExecutionErrorCode(err) == "command_delivery_uncertain")
						if err == nil && sessionKey == "" {
							// A successful start without its identity cannot be
							// safely associated with a Hub event. Keep the global
							// fence closed rather than risk mirroring its output.
							unknownOutcome = true
						}
						if persistErr := s.endDirectSessionStart(sessionKey, unknownOutcome); persistErr != nil {
							if err != nil {
								s.log.Printf("direct: failed to persist session-start fence: %v", persistErr)
							} else {
								err = fmt.Errorf("直连会话隐私栅栏无法保存：%w", persistErr)
								result = nil
							}
						} else if err == nil && sessionKey != "" {
							// The key is now durably fenced. The phone receives the
							// initial output through its follow-up history snapshot.
							if subscribeErr := s.setDirectEventSubscription(relay, command.EventTarget(), sessionKey); subscribeErr != nil {
								_ = s.markDirectStartUnknown()
								err = fmt.Errorf("直连会话事件订阅无法保存：%w", subscribeErr)
								result = nil
							}
						}
					}
					return result, err
				})
				if admitted {
					relay.SendResponse(command, result, commandErr)
				}
			}(command)
		}
	}
}

func openDirectListeners(override string, hosts []string) ([]net.Listener, []string, int, error) {
	port := directRelayPort
	if override != "" {
		ln, err := net.Listen("tcp", override)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("listen for direct pairing on %s: %w", override, err)
		}
		tcp, ok := ln.Addr().(*net.TCPAddr)
		if !ok {
			_ = ln.Close()
			return nil, nil, 0, errors.New("direct pairing listener did not return a TCP address")
		}
		port = tcp.Port
		boundHosts := uniqueDirectHosts(hosts)
		if host, _, err := net.SplitHostPort(override); err == nil && host != "" && host != "0.0.0.0" && host != "::" {
			// An explicit listener override is authoritative. It is commonly used
			// by local tests and loopback-only desktop deployments, where the
			// normal advertised-host filter intentionally rejects loopback.
			if parsed := net.ParseIP(strings.Trim(host, "[]")); parsed != nil {
				boundHosts = []string{parsed.String()}
			}
		}
		return []net.Listener{ln}, boundHosts, port, nil
	}
	var listeners []net.Listener
	var boundHosts []string
	for _, host := range uniqueDirectHosts(hosts) {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
		if err != nil {
			continue
		}
		listeners = append(listeners, ln)
		boundHosts = append(boundHosts, host)
	}
	if len(listeners) == 0 {
		return nil, nil, 0, errors.New("could not bind the direct TLS listener to any saved local address")
	}
	return listeners, boundHosts, port, nil
}

func (s *Server) serveDirectRelay(srv *http.Server, certificate tls.Certificate, listeners ...net.Listener) {
	var wg sync.WaitGroup
	for _, raw := range listeners {
		raw := raw
		wg.Add(1)
		go func() {
			defer wg.Done()
			listener := tls.NewListener(raw, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
			if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Printf("direct pairing listener on %s stopped: %v", raw.Addr(), err)
			}
			_ = raw.Close()
		}()
	}
	wg.Wait()
	s.directLifecycleMu.Lock()
	defer s.directLifecycleMu.Unlock()
	s.directMu.Lock()
	var relay *directrelay.Relay
	var cancel context.CancelFunc
	if s.directHTTP == srv {
		relay = s.directRelay
		cancel = s.directCommandCancel
		s.directRelay = nil
		s.directHTTP = nil
		s.directListeners = nil
		s.directCommandCancel = nil
		s.directLastError = "direct listener stopped unexpectedly"
	}
	s.directMu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.clearDirectEventSubscription()
	if relay != nil {
		_ = relay.Close()
	}
	s.clearDirectEventSubscription()
}

func (s *Server) currentDirectHosts() []string {
	if s.directHostProvider != nil {
		return uniqueDirectHosts(s.directHostProvider())
	}
	return localDirectHosts()
}

func uniqueDirectHosts(hosts []string) []string {
	seen := make(map[string]struct{}, len(hosts))
	result := make([]string, 0, len(hosts))
	for _, raw := range hosts {
		host := strings.TrimSpace(strings.Trim(raw, "[]"))
		if host == "" {
			continue
		}
		if ip := net.ParseIP(host); ip != nil {
			if !eligibleDirectIP(ip) {
				continue
			}
			host = ip.String()
		} else {
			continue
		}
		if _, ok := seen[host]; !ok {
			seen[host] = struct{}{}
			result = append(result, host)
		}
	}
	sort.Strings(result)
	return result
}

func eligibleDirectIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if ip.IsLoopback() {
		return false
	}
	if ip.IsPrivate() {
		return true
	}
	// Tailscale and several other mesh VPNs use RFC 6598 shared space.
	if v4 := ip.To4(); v4 != nil {
		return v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
	}
	return false
}

func localDirectHosts() []string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var hosts []string
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if !eligibleDirectIP(ip) {
				continue
			}
			hosts = append(hosts, ip.String())
		}
	}
	return uniqueDirectHosts(hosts)
}

func matchingDirectHosts(saved, available []string) []string {
	allowed := make(map[string]struct{}, len(uniqueDirectHosts(available)))
	for _, host := range uniqueDirectHosts(available) {
		allowed[host] = struct{}{}
	}
	var matched []string
	for _, host := range uniqueDirectHosts(saved) {
		if _, ok := allowed[host]; ok {
			matched = append(matched, host)
		}
	}
	return matched
}

func (s *Server) ensureDirectRelay() error {
	s.directLifecycleMu.Lock()
	defer s.directLifecycleMu.Unlock()
	if s.d.Store == nil {
		return errors.New("direct relay configuration is unavailable")
	}
	s.directMu.RLock()
	running := s.directHTTP != nil
	s.directMu.RUnlock()
	c := s.d.Store.Get()
	if running && c.DirectRemoteEnabled() {
		return nil
	}
	candidate := c.Clone()
	candidate.ActivateRemoteMode("direct")
	if len(candidate.DirectRelayHosts) == 0 {
		candidate.DirectRelayHosts = s.currentDirectHosts()
		if len(candidate.DirectRelayHosts) == 0 {
			return errors.New("no local network address is available for direct pairing")
		}
	}
	// Bind and validate locally first. Until the durable commit the handler
	// rejects direct traffic, so preparation cannot interrupt the old mode.
	if !running {
		s.directMu.Lock()
		err := s.startDirectRelayLocked(candidate)
		if err != nil {
			s.directLastError = err.Error()
		}
		s.directMu.Unlock()
		if err != nil {
			return err
		}
	}
	err := s.d.Store.Update(func(next *config.Config) error {
		if next.EffectiveRemoteMode() != c.EffectiveRemoteMode() || config.RemoteIdentity(*next) != config.RemoteIdentity(c) || next.DirectPhoneID != c.DirectPhoneID || next.DirectPhoneHash != c.DirectPhoneHash {
			return errors.New("远程方式已变化，请重试")
		}
		next.ActivateRemoteMode("direct")
		next.DirectRelayHosts = append([]string(nil), candidate.DirectRelayHosts...)
		return nil
	})
	if err != nil {
		if !running {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = s.closeDirectRelayLocked(ctx)
		}
		return err
	}
	return nil
}

func (s *Server) closeDirectRelay(ctx context.Context) error {
	s.directLifecycleMu.Lock()
	defer s.directLifecycleMu.Unlock()
	return s.closeDirectRelayLocked(ctx)
}

func (s *Server) closeDirectRelayLocked(ctx context.Context) error {
	s.directMu.Lock()
	relay, srv, listeners := s.directRelay, s.directHTTP, append([]net.Listener(nil), s.directListeners...)
	commandCancel := s.directCommandCancel
	s.directRelay, s.directHTTP, s.directListeners = nil, nil, nil
	s.directCommandCancel = nil
	s.directMu.Unlock()
	s.clearDirectEventSubscription()
	if commandCancel != nil {
		commandCancel()
	}
	var closeErr error
	if relay != nil {
		if err := relay.Close(); err != nil {
			closeErr = err
		}
	}
	// relay.Close drains admitted commands; clear once more in case a command
	// installed its target while cancellation was unwinding.
	s.clearDirectEventSubscription()
	if srv != nil {
		if err := srv.Shutdown(ctx); err != nil {
			_ = srv.Close()
			if closeErr == nil {
				closeErr = err
			}
		}
	}
	for _, ln := range listeners {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func (s *Server) handleDirectStatus(w http.ResponseWriter, _ *http.Request) {
	c := config.Config{}
	if s.d.Store != nil {
		c = s.d.Store.Get()
	}
	s.directMu.RLock()
	relay, running := s.directRelay, s.directHTTP != nil
	startedAt, fingerprint, lastError := s.directStartedAt, s.directFingerprint, s.directLastError
	hosts := matchingDirectHosts(s.directHosts, s.currentDirectHosts())
	port := directRelayPort
	if len(s.directListeners) > 0 {
		if tcp, ok := s.directListeners[0].Addr().(*net.TCPAddr); ok {
			port = tcp.Port
		}
	}
	s.directMu.RUnlock()
	if !running {
		hosts = nil
	}
	urls := make([]string, 0, len(hosts))
	for _, host := range hosts {
		urls = append(urls, "wss://"+net.JoinHostPort(host, strconv.Itoa(port))+"/direct/ws")
	}
	paired, connected := c.DirectPhoneID != "" && c.DirectPhoneHash != "", false
	if relay != nil {
		paired, connected = relay.IsPaired(), relay.IsConnected()
	}
	message := "direct pairing is disabled"
	if c.DirectRelayEnabled {
		message = lastError
		if running {
			message = "TLS 直连已启动；消息和命令只在手机与电脑之间传输，不会回退到 Salcara Hub。"
		}
		if message == "" {
			message = "direct pairing listener is not running"
		}
	}
	startedAtMS := int64(0)
	if !startedAt.IsZero() {
		startedAtMS = startedAt.UnixMilli()
	}
	writeJSON(w, map[string]any{
		"enabled": c.DirectRelayEnabled, "running": running, "paired": paired, "connected": connected,
		"pairingSupported": running && len(urls) > 0, "mobileSupported": true, "commandDispatch": running && s.d.Hub != nil,
		"port": port, "secureUrls": urls,
		"fingerprint": fingerprint, "startedAt": startedAtMS, "message": message,
	})
}

func (s *Server) handleDirectQR(w http.ResponseWriter, _ *http.Request) {
	s.remoteModeMu.Lock()
	defer s.remoteModeMu.Unlock()
	if c := s.d.Store.Get(); c.DirectPhoneID != "" && c.DirectPhoneHash != "" {
		writeErr(w, http.StatusConflict, "a phone is already paired; enable direct mode to reconnect")
		return
	}
	if err := s.ensureDirectRelay(); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "direct pairing could not start: "+err.Error())
		return
	}
	s.remoteModeSeq++
	s.directMu.Lock()
	relay := s.directRelay
	if relay == nil {
		s.directMu.Unlock()
		writeErr(w, http.StatusServiceUnavailable, "direct pairing listener is not available")
		return
	}
	payload, err := relay.GenerateQR()
	if err != nil {
		s.directMu.Unlock()
		if relay.IsPaired() {
			writeErr(w, http.StatusConflict, "a phone is already paired; revoke it before creating another QR")
			return
		}
		writeErr(w, http.StatusServiceUnavailable, "a secure direct pairing QR is not available")
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		s.directMu.Unlock()
		writeErr(w, http.StatusInternalServerError, "could not encode direct pairing QR")
		return
	}
	png, err := qrcode.Encode(string(encoded), qrcode.Medium, 384)
	if err != nil {
		s.directMu.Unlock()
		writeErr(w, http.StatusInternalServerError, "could not create direct pairing QR")
		return
	}
	s.directMu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"ok": true, "qrDataURL": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png), "expiresAt": payload["expiresAt"]})
}

func (s *Server) handleDirectRevoke(w http.ResponseWriter, _ *http.Request) {
	if s.d.Store == nil {
		writeErr(w, http.StatusServiceUnavailable, "direct pairing configuration is unavailable")
		return
	}
	s.remoteModeMu.Lock()
	defer s.remoteModeMu.Unlock()
	s.directLifecycleMu.Lock()
	defer s.directLifecycleMu.Unlock()
	s.directMu.Lock()
	relay, srv, listeners := s.directRelay, s.directHTTP, append([]net.Listener(nil), s.directListeners...)
	commandCancel := s.directCommandCancel
	var err error
	s.directRelay, s.directHTTP, s.directListeners = nil, nil, nil
	s.directCommandCancel = nil
	s.directMu.Unlock()
	s.clearDirectEventSubscription()
	if commandCancel != nil {
		commandCancel()
	}
	if relay != nil {
		err = relay.Revoke()
	} else if s.d.Store != nil {
		err = s.d.Store.Update(func(c *config.Config) error {
			if c.EffectiveRemoteMode() == "direct" {
				c.ActivateRemoteMode("none")
			}
			c.DirectPhoneID, c.DirectPhoneHash = "", ""
			c.DirectRelayEnabled = false
			c.DirectRelayHosts = nil
			c.DirectAPI = nil
			return nil
		})
	}
	if err != nil {
		s.directMu.Lock()
		s.directLastError = "direct pairing revocation was not persisted"
		s.directMu.Unlock()
	} else {
		s.directMu.Lock()
		s.directLastError = ""
		s.directMu.Unlock()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if relay != nil {
		_ = relay.Close()
	}
	// Revoke drains admitted commands before returning. Remove any target that
	// was registered after the first clear during cancellation.
	s.clearDirectEventSubscription()
	if srv != nil {
		if shutdownErr := srv.Shutdown(ctx); shutdownErr != nil {
			_ = srv.Close()
		}
	}
	for _, ln := range listeners {
		_ = ln.Close()
	}
	identityPath := filepath.Join(filepath.Dir(s.d.Store.Path()), "direct-relay-identity.pem")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "direct phone revocation was not persisted")
		return
	}
	s.remoteModeSeq++
	if removeErr := os.Remove(identityPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		s.log.Printf("direct pairing certificate retirement failed: %v", removeErr)
	}
	writeJSON(w, map[string]any{"ok": true, "message": "direct phone pairing and unused QR tokens were revoked"})
}

func commandType(command map[string]any) string {
	typ, _ := command["type"].(string)
	return typ
}

type directEventSubscription struct {
	relay  *directrelay.Relay
	target directrelay.EventTarget
}

func (s *Server) setDirectEventSubscription(relay *directrelay.Relay, target directrelay.EventTarget, sessionKey string) error {
	if relay == nil || sessionKey == "" {
		return errors.New("直连会话事件订阅无效")
	}
	s.directEventMu.Lock()
	defer s.directEventMu.Unlock()
	if s.directEventTargets == nil {
		s.directEventTargets = make(map[string]directEventSubscription)
	}
	if err := s.persistDirectSessionLocked(sessionKey); err != nil {
		return err
	}
	// Persist the privacy fence even if the phone disconnected while this
	// command was queued, but never attach a dead socket as an event target.
	// Validate before retiring any other generation: a delayed old command must
	// not erase subscriptions already installed by a newly authenticated phone.
	if !relay.IsCurrentEventTarget(target) {
		return errors.New("直连手机已断开，未执行会话操作")
	}
	// The relay accepts only one phone at a time. A new authenticated command
	// proves that this target is current; retire every old generation so stale
	// sessions cannot consume the bounded subscription table after reconnect.
	for key, existing := range s.directEventTargets {
		if existing.relay == relay && existing.target != target {
			delete(s.directEventTargets, key)
		}
	}
	if _, exists := s.directEventTargets[sessionKey]; !exists && len(s.directEventTargets) >= 4096 {
		return errors.New("直连会话事件订阅已达到安全上限")
	}
	s.directEventTargets[sessionKey] = directEventSubscription{relay: relay, target: target}
	return nil
}

func (s *Server) clearDirectEventSubscriptionsForTarget(relay *directrelay.Relay, target directrelay.EventTarget) {
	if relay == nil {
		return
	}
	s.directEventMu.Lock()
	for sessionKey, subscription := range s.directEventTargets {
		if subscription.relay == relay && subscription.target == target {
			delete(s.directEventTargets, sessionKey)
		}
	}
	s.directEventMu.Unlock()
}

func (s *Server) rememberDirectSessionLocked(sessionKey string) {
	if sessionKey == "" {
		return
	}
	if s.directSessionKeys == nil {
		s.directSessionKeys = make(map[string]struct{})
	}
	s.directSessionKeys[sessionKey] = struct{}{}
}

func (s *Server) rememberDirectSession(sessionKey string) error {
	if sessionKey == "" {
		return nil
	}
	if len(sessionKey) > 512 || strings.TrimSpace(sessionKey) == "" || strings.ContainsAny(sessionKey, "\x00\r\n") {
		return errors.New("直连会话标识无效，已阻止同步")
	}
	// Keep the Hub publisher behind the write lock until the durable write and
	// in-memory fence both succeed. A concurrent open of the same session must
	// not bypass an in-flight registration.
	s.directEventMu.Lock()
	defer s.directEventMu.Unlock()
	return s.persistDirectSessionLocked(sessionKey)
}

func (s *Server) persistDirectSessionLocked(sessionKey string) error {
	if len(sessionKey) > 512 || strings.TrimSpace(sessionKey) == "" || strings.ContainsAny(sessionKey, "\x00\r\n") {
		return errors.New("直连会话标识无效，已阻止同步")
	}
	if _, known := s.directSessionKeys[sessionKey]; known {
		return nil
	}
	if len(s.directSessionKeys) >= 4096 {
		return errors.New("直连会话隐私栅栏已达到安全上限")
	}
	if s.d.Store != nil {
		if err := s.d.Store.Update(func(c *config.Config) error {
			for _, knownKey := range c.DirectSessionKeys {
				if knownKey == sessionKey {
					return nil
				}
			}
			// Session keys are generated by the local agent, but keep the durable
			// fence bounded so a long-lived installation cannot grow its config
			// without limit.
			if len(c.DirectSessionKeys) >= 4096 {
				return errors.New("直连会话隐私栅栏已达到安全上限")
			}
			c.DirectSessionKeys = append(c.DirectSessionKeys, sessionKey)
			return nil
		}); err != nil {
			return err
		}
	}
	s.rememberDirectSessionLocked(sessionKey)
	return nil
}

func (s *Server) beginDirectSessionStart() error {
	// Close the in-memory Hub fence before touching disk. PublishHubEvent holds
	// the matching read lock through enqueue, so no event can pass during the
	// persistence window.
	s.directEventMu.Lock()
	s.directStartsInFlight++
	s.directEventMu.Unlock()
	if s.d.Store != nil {
		if err := s.d.Store.Update(func(c *config.Config) error {
			if c.DirectSessionStartsPending >= 1024 {
				return errors.New("直连会话启动数量超过安全限制")
			}
			c.DirectSessionStartsPending++
			return nil
		}); err != nil {
			s.directEventMu.Lock()
			if s.directStartsInFlight > 0 {
				s.directStartsInFlight--
			}
			s.directEventMu.Unlock()
			return err
		}
	}
	return nil
}

func (s *Server) endDirectSessionStart(sessionKey string, unknown bool) error {
	if sessionKey != "" && (len(sessionKey) > 512 || strings.TrimSpace(sessionKey) == "" || strings.ContainsAny(sessionKey, "\x00\r\n")) {
		sessionKey = ""
		unknown = true
	}
	if s.d.Store != nil {
		if err := s.d.Store.Update(func(c *config.Config) error {
			if sessionKey != "" {
				found := false
				for _, knownKey := range c.DirectSessionKeys {
					if knownKey == sessionKey {
						found = true
						break
					}
				}
				if !found {
					if len(c.DirectSessionKeys) >= 4096 {
						unknown = true
					} else {
						c.DirectSessionKeys = append(c.DirectSessionKeys, sessionKey)
					}
				}
			}
			if unknown {
				c.DirectSessionFenceUnknown = true
			}
			if c.DirectSessionStartsPending > 0 {
				c.DirectSessionStartsPending--
			}
			return nil
		}); err != nil {
			s.directEventMu.Lock()
			if s.directStartsInFlight > 0 {
				s.directStartsInFlight--
			}
			s.directStartUnknown = true
			if sessionKey != "" {
				s.rememberDirectSessionLocked(sessionKey)
			}
			s.directEventMu.Unlock()
			// Leave the persisted pending counter untouched. Startup will recover
			// it as a durable unknown-session fence if this process exits now.
			return err
		}
	}
	s.directEventMu.Lock()
	if s.directStartsInFlight > 0 {
		s.directStartsInFlight--
	}
	if sessionKey != "" {
		s.rememberDirectSessionLocked(sessionKey)
	}
	if unknown {
		s.directStartUnknown = true
	}
	s.directEventMu.Unlock()
	return nil
}

func (s *Server) markDirectStartUnknown() error {
	s.directEventMu.Lock()
	s.directStartUnknown = true
	s.directEventMu.Unlock()
	if s.d.Store == nil {
		return nil
	}
	return s.d.Store.Update(func(c *config.Config) error {
		c.DirectSessionFenceUnknown = true
		return nil
	})
}

func (s *Server) clearDirectEventSubscription() {
	s.directEventMu.Lock()
	s.directEventTargets = make(map[string]directEventSubscription)
	s.directEventMu.Unlock()
}

func (s *Server) broadcastDirectEvent(ev protocol.Event) {
	if ev.SessionKey == "" {
		return
	}
	s.directEventMu.RLock()
	subscription, ok := s.directEventTargets[ev.SessionKey]
	s.directEventMu.RUnlock()
	if !ok || subscription.relay == nil {
		return
	}
	if ev.DeviceID == "" && s.d.Store != nil {
		ev.DeviceID = s.d.Store.Get().DeviceID
	}
	if !subscription.relay.SendEvent(subscription.target, ev.Type, ev) {
		s.directEventMu.Lock()
		if current, exists := s.directEventTargets[ev.SessionKey]; exists && current == subscription {
			delete(s.directEventTargets, ev.SessionKey)
		}
		s.directEventMu.Unlock()
	}
}

// IsDirectSessionEvent identifies an event that must not cross the Hub
// transport. This includes every session ever opened or created over P2P in
// this process, even after the active subscription is cleared. While a direct
// session.start is in flight, the generated key is not yet available, so all
// session events fail closed until the key is registered. An uncertain start
// outcome keeps that process-wide session-event fence closed.
func (s *Server) IsDirectSessionEvent(ev protocol.Event) bool {
	if ev.SessionKey == "" {
		return false
	}
	s.directEventMu.RLock()
	defer s.directEventMu.RUnlock()
	if s.directStartsInFlight > 0 || s.directStartUnknown {
		return true
	}
	_, direct := s.directSessionKeys[ev.SessionKey]
	return direct
}
