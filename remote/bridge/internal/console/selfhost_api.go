package console

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubserver"
)

// ──────────────────────────────────────────────────────────────
// Self-hosted Hub management API
// These endpoints let the Bridge console UI start/stop an embedded Hub
// and report its status.
// ──────────────────────────────────────────────────────────────

// HubManager wraps an embedded hubserver.Hub that can be optionally started
// alongside Bridge.  Nil means the embedded Hub is not running.
type HubManager struct {
	hub           *hubserver.Hub
	server        *http.Server
	listener      net.Listener
	cancel        context.CancelFunc
	started       time.Time
	addr          string
	mode          string
	revision      string
	bases         []string
	stopping      bool // guarded by Server.hubMu
	serveReturned chan struct{}
}

// readyListener lets Start wait until net/http has entered its accept loop
// before the manager becomes visible to status and stop requests.
type readyListener struct {
	net.Listener
	ready chan struct{}
	once  sync.Once
}

func (l *readyListener) Accept() (net.Conn, error) {
	l.once.Do(func() { close(l.ready) })
	return l.Listener.Accept()
}

func (s *Server) handleHubStatus(w http.ResponseWriter, _ *http.Request) {
	s.hubMu.RLock()
	mgr := s.embeddedHub
	if mgr == nil || mgr.hub == nil {
		s.hubMu.RUnlock()
		writeJSON(w, map[string]any{
			"running": false,
			"addr":    "",
			"devices": 0,
			"phones":  0,
		})
		return
	}
	stopping := mgr.stopping
	hub := mgr.hub
	addr := mgr.addr
	started := mgr.started
	mode := mgr.mode
	revision := mgr.revision
	bases := append([]string(nil), mgr.bases...)
	s.hubMu.RUnlock()
	if stopping {
		writeJSON(w, map[string]any{
			"running":  false,
			"stopping": true,
			"addr":     addr,
			"mode":     mode,
			"revision": revision,
			"bases":    bases,
			"devices":  hub.DeviceCount(),
			"phones":   hub.PhoneCount(),
		})
		return
	}
	writeJSON(w, map[string]any{
		"running":   true,
		"addr":      addr,
		"mode":      mode,
		"revision":  revision,
		"bases":     bases,
		"startedAt": started.UnixMilli(),
		"devices":   hub.DeviceCount(),
		"phones":    hub.PhoneCount(),
	})
}

type startHubRequest struct {
	Addr        string   `json:"addr"`     // loopback only, e.g. "127.0.0.1:8765"
	Mode        string   `json:"mode"`     // "open" or "restricted"
	Revision    string   `json:"revision"` // station policy revision
	Bases       []string `json:"bases"`    // allowed API bases (restricted mode)
	ConfirmOpen bool     `json:"confirmOpen"`
}

func normalizeHubStartRequest(req *startHubRequest) error {
	if req.Addr == "" {
		req.Addr = "127.0.0.1:8765"
	}
	host, _, err := net.SplitHostPort(req.Addr)
	parsedHost := net.ParseIP(host)
	if err != nil || parsedHost == nil || !parsedHost.IsLoopback() {
		return errors.New("内置 Hub 仅允许绑定 127.0.0.1 或 [::1]；局域网访问需要 HTTPS 入口")
	}
	if req.Mode == "" {
		req.Mode = "restricted"
	}
	if req.Mode != "open" && req.Mode != "restricted" {
		return errors.New("Hub 模式无效")
	}
	if req.Mode == "open" {
		if !req.ConfirmOpen {
			return errors.New("开放模式需要明确确认")
		}
		// Open mode means all API bases. Keeping a restricted base list beside
		// it creates a misleading policy because Hub treats an empty list as the
		// allow-all switch.
		if len(req.Bases) != 0 {
			return errors.New("开放模式不能同时指定受限 API 地址")
		}
	} else {
		if len(req.Bases) == 0 || len(req.Bases) > 32 {
			return errors.New("受限模式需填写 1 到 32 个有效 API 地址")
		}
		seen := make(map[string]struct{}, len(req.Bases))
		bases := make([]string, 0, len(req.Bases))
		for _, raw := range req.Bases {
			base, err := config.NormalizeStationAPIBase(strings.TrimSpace(raw))
			if err != nil {
				return errors.New("受限模式包含无效的 API 地址")
			}
			if _, ok := seen[base]; ok {
				return errors.New("受限模式不能重复填写 API 地址")
			}
			seen[base] = struct{}{}
			bases = append(bases, base)
		}
		req.Bases = bases
	}
	revision := strings.TrimSpace(req.Revision)
	if revision == "" {
		revision = "self-hosted-v1"
	}
	if len(revision) > 128 || strings.ContainsAny(revision, "\r\n\x00") {
		return errors.New("策略版本无效")
	}
	req.Revision = revision
	return nil
}

func (s *Server) handleHubStart(w http.ResponseWriter, r *http.Request) {
	var req startHubRequest
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式有误")
		return
	}
	if err := normalizeHubStartRequest(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	s.hubMu.Lock()
	if s.embeddedHub != nil {
		s.hubMu.Unlock()
		writeErr(w, http.StatusConflict, "内置 Hub 正在运行或停止，请稍后重试")
		return
	}
	ln, err := net.Listen("tcp", req.Addr)
	if err != nil {
		s.hubMu.Unlock()
		s.log.Printf("embedded hub listen on %s failed: %v", req.Addr, err)
		writeErr(w, http.StatusConflict, "内置 Hub 监听失败，请检查地址和端口是否可用")
		return
	}
	readyLn := &readyListener{Listener: ln, ready: make(chan struct{})}

	hub := hubserver.New(hubserver.Options{
		Logger:          s.log,
		StationMode:     req.Mode,
		StationRevision: req.Revision,
		APIBases:        req.Bases,
	})

	hubCtx, cancel := context.WithCancel(context.Background())
	srv := &http.Server{
		Handler: hub.Handler(),
		// Every request, including long-lived Bridge SSE streams, inherits the
		// embedded Hub lifecycle. Cancelling this context during stop lets
		// handlers leave immediately instead of making Shutdown wait for an
		// otherwise healthy stream forever.
		BaseContext:       func(net.Listener) context.Context { return hubCtx },
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	addr := ln.Addr().String()
	mgr := &HubManager{
		hub: hub, server: srv, listener: ln, cancel: cancel,
		started: time.Now(), addr: addr, mode: req.Mode,
		revision: req.Revision, bases: append([]string(nil), req.Bases...),
		serveReturned: make(chan struct{}),
	}
	s.embeddedHub = mgr
	s.embeddedHubServer = srv
	go s.serveEmbeddedHub(mgr, readyLn)
	select {
	case <-readyLn.ready:
		// A listener can fail immediately after entering Accept. Prefer its
		// terminal state over reporting a Hub that has already exited.
		select {
		case <-mgr.serveReturned:
			_ = srv.Close()
			_ = ln.Close()
			cancel()
			if s.embeddedHub == mgr {
				s.embeddedHub = nil
				s.embeddedHubServer = nil
			}
			s.hubMu.Unlock()
			writeErr(w, http.StatusInternalServerError, "内置 Hub 启动失败")
			return
		default:
		}
	case <-mgr.serveReturned:
		_ = srv.Close()
		_ = ln.Close()
		cancel()
		if s.embeddedHub == mgr {
			s.embeddedHub = nil
			s.embeddedHubServer = nil
		}
		s.hubMu.Unlock()
		writeErr(w, http.StatusInternalServerError, "内置 Hub 启动失败")
		return
	case <-r.Context().Done():
		// The request may be abandoned while Serve is getting ready. Do not
		// leave a listener running without a caller that received its address.
		_ = srv.Close()
		_ = ln.Close()
		cancel()
		s.embeddedHub = nil
		s.embeddedHubServer = nil
		s.hubMu.Unlock()
		return
	}
	s.hubMu.Unlock()
	go hub.RunGC(hubCtx)

	s.log.Printf("embedded hub started on %s (mode=%s)", addr, req.Mode)
	writeJSON(w, map[string]any{"ok": true, "addr": addr, "mode": req.Mode, "revision": req.Revision, "bases": req.Bases})
}

func (s *Server) serveEmbeddedHub(mgr *HubManager, listener net.Listener) {
	err := mgr.server.Serve(listener)
	close(mgr.serveReturned)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		s.log.Printf("embedded hub stopped unexpectedly: %v", err)
	}
	mgr.cancel()
	s.hubMu.Lock()
	// A graceful stop returns from Serve as soon as the listener closes, while
	// in-flight handlers may still be draining. Keep the manager published in
	// that window so status reports stopping and a second start cannot race the
	// old listener. stopEmbeddedHub removes it after Shutdown completes.
	stopping := mgr.stopping
	if s.embeddedHub == mgr && !stopping {
		s.embeddedHub = nil
		s.embeddedHubServer = nil
	}
	s.hubMu.Unlock()
}

func (s *Server) stopEmbeddedHub(ctx context.Context) error {
	s.hubMu.Lock()
	mgr := s.embeddedHub
	if mgr == nil {
		s.hubMu.Unlock()
		return nil
	}
	if mgr.stopping {
		s.hubMu.Unlock()
		return errors.New("内置 Hub 正在停止")
	}
	mgr.stopping = true
	s.hubMu.Unlock()

	// Cancel request contexts before asking net/http to drain. This is required
	// for SSE and other long-lived handlers that otherwise never observe the
	// listener close on their own.
	mgr.cancel()
	err := mgr.server.Shutdown(ctx)
	if err != nil {
		_ = mgr.server.Close()
	}
	// Shutdown only closes listeners known to net/http. Close the underlying
	// listener too so stop remains safe if called at the edge of Serve startup.
	_ = mgr.listener.Close()
	s.hubMu.Lock()
	if s.embeddedHub == mgr {
		s.embeddedHub = nil
		s.embeddedHubServer = nil
	}
	s.hubMu.Unlock()
	return err
}

// CloseSelfHostServices shuts down the optional Hub, direct pairing listener,
// and its background GC loop.
// It is safe to call more than once during normal exit and test cleanup.
func (s *Server) CloseSelfHostServices() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.closeDirectRelay(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.log.Printf("direct pairing shutdown: %v", err)
	}
	if err := s.stopEmbeddedHub(ctx); err != nil && !errors.Is(err, context.Canceled) {
		s.log.Printf("embedded hub shutdown: %v", err)
	}
}

func (s *Server) handleHubStop(w http.ResponseWriter, _ *http.Request) {
	s.hubMu.RLock()
	running := s.embeddedHub != nil
	s.hubMu.RUnlock()
	if !running {
		writeErr(w, http.StatusNotFound, "内置 Hub 当前未运行")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.stopEmbeddedHub(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "内置 Hub 停止失败")
		return
	}

	s.log.Printf("embedded hub stopped by user")
	writeJSON(w, map[string]any{"ok": true})
}

// ──────────────────────────────────────────────────────────────
// Route registration — call from routes(api) in server.go
// ──────────────────────────────────────────────────────────────

func (s *Server) registerSelfHostRoutes(api *http.ServeMux) {
	// Embedded Hub
	api.HandleFunc("GET /api/local/hub/status", s.handleHubStatus)
	api.HandleFunc("POST /api/local/hub/start", s.handleHubStart)
	api.HandleFunc("POST /api/local/hub/stop", s.handleHubStop)

	// P2P direct relay
	api.HandleFunc("GET /api/local/direct/status", s.handleDirectStatus)
	api.HandleFunc("POST /api/local/direct/qr", s.handleDirectQR)
	api.HandleFunc("POST /api/local/direct/revoke", s.handleDirectRevoke)
}
