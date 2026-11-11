// Package console is the local web console on 127.0.0.1:47831 (embedded single-page app + JSON API).
//
// Protection: every request must carry Host 127.0.0.1:<port> or localhost:<port> (blocks DNS rebinding).
// The page load sets a random per-run token as an HttpOnly SameSite=Strict cookie, and /api/* requires that
// cookie plus a same-origin Origin header (when the browser sends one), so other websites can neither read
// nor drive the API. /internal/agents/ (used by the bridge's own helper processes) skips the cookie but is
// still loopback-only and checks its own token inside the agents package.
package console

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/accounthealth"
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/agentwatchdog"
	"salcara/bridge/internal/apiservice"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/directrelay"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/pluginhook"
	"salcara/bridge/internal/prober"
	"salcara/bridge/internal/protocol"
	"salcara/bridge/internal/toolcfg"
	"salcara/bridge/internal/usageledger"
)

//go:embed web
var webFS embed.FS

// CookieName holds the per-run console token.
const CookieName = "salcara_console"

// Deps are what the console needs from the rest of the program.
type Deps struct {
	Store           *config.Store
	Hub             *hubclient.Client
	Version         string
	Port            int
	LogPath         string
	Exe             string
	Dir             string // config / log folder (for "打开文件夹")
	Logger          *log.Logger
	Quit            func()
	Local           *launcher.Service
	ClaudeDesktop   *launcher.ClaudeDesktop3PService
	Companion       CompanionInstaller
	CodexAccounts   *codexaccounts.Service
	OfficialGateway func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error)
	OfficialImages  func(context.Context, config.LocalAccount, []byte, bool, string, func() bool) (*http.Response, error)
}

// CompanionInstaller is local-only. Installation alone never connects to a
// desktop or controls a chat; a separate explicitly authorized lease is required.
type CompanionInstaller interface {
	Preview(context.Context) (desktopcompanion.Status, error)
	Install(context.Context) (desktopcompanion.Status, error)
	PreviewUninstall(context.Context) (desktopcompanion.Status, error)
	Uninstall(context.Context) (desktopcompanion.Status, error)
}

// Server is the console.
type Server struct {
	pluginHooks *pluginhook.Hooks
	d           Deps
	token       string
	log         *log.Logger

	mgrMu sync.RWMutex
	mgr   agents.Manager

	subMu sync.Mutex
	subs  map[chan sseMsg]struct{}

	pendMu           sync.Mutex
	pending          map[string]protocol.Event
	activityRevision uint64
	activeSessions   map[string]string

	byMu      sync.Mutex
	byDesktop map[string]time.Time

	usageMu                  sync.Mutex
	usageAt                  time.Time
	usageKey                 string
	usageData                map[string]any
	usageErr                 string
	nativeMu                 sync.Mutex
	nativeAt                 time.Time
	nativeState              desktopcompanion.NativeConnection
	nativeFlight             *nativeStatusFlight
	nativeEpoch              uint64
	modelProbeMu             sync.Mutex
	modelProbeSeq            map[string]uint64
	modelProbeNext           uint64
	accountTestMu            sync.Mutex
	accountTests             map[string]bool
	accountTestHTTP          *http.Client
	remoteDiscoveryMu        sync.Mutex
	remoteEnrollmentMu       sync.Mutex
	remoteModeMu             sync.Mutex
	remoteModeSeq            uint64
	remoteDiscoverySeq       uint64
	remoteDiscoveryLatest    map[string]uint64
	launchModelProbe         func(context.Context, config.LocalAccount) ([]string, error)
	pairMu                   sync.Mutex
	pairPNG                  []byte
	pairExpires              int64
	pairIdentity             string
	accountPool              *gateway.AccountPool
	apiService               *apiservice.Service
	apiNetwork               *apiListenerManager
	officialUsageMaintenance *officialUsageMaintenance
	usageLedger              *usageledger.Ledger
	accountCostMu            sync.Mutex
	accountCostSamples       map[string]accountCostSample
	usageOverview            *usageOverviewCache
	accountUsage             *accountUsageCache
	managedLaunchMu          sync.Mutex
	managedLaunchPreviews    map[string]managedLaunchPreview
	agentChangeMu            sync.Mutex
	agentChangeIDMu          sync.Mutex
	agentChangeIDs           map[string]struct{}
	nativeConfigurationStop  func(context.Context, launcher.Plan) error // fake-only stop boundary in tests
	codexProfileMu           sync.Mutex
	codexProfile             *toolcfg.CodexServiceProfile
	backgroundMu             sync.Mutex
	proberStarted            bool
	watchdogStarted          bool
	watchdog                 *agentwatchdog.Watchdog

	// Self-hosted Hub (optional embedded relay server)
	hubMu             sync.RWMutex
	embeddedHub       *HubManager
	embeddedHubServer *http.Server

	// P2P direct relay (optional LAN/VPN direct connection mode)
	directLifecycleMu   sync.Mutex
	directMu            sync.RWMutex
	directRelay         *directrelay.Relay
	directCommandCancel context.CancelFunc
	directEventMu       sync.RWMutex
	directEventTargets  map[string]directEventSubscription
	// directSessionKeys is a process-lifetime privacy fence. A session that
	// was ever created or opened through the authenticated P2P transport must
	// never be mirrored through Hub later, even after the phone disconnects,
	// switches sessions, or is revoked. directEventTargets routes live events
	// per session; this set controls whether Hub may see them at all.
	directSessionKeys map[string]struct{}
	// directStartsInFlight closes the small window between admitting a direct
	// session.start and learning its generated key. Events are suppressed from
	// Hub while that window is open so a fast agent cannot leak its first frame.
	// It is intentionally fail-closed and may briefly suppress unrelated live
	// session events while the local start call is completing.
	directStartsInFlight int
	// directStartUnknown is set when a direct start returns an uncertain error.
	// The newly-created key cannot be proven, so all session events remain
	// Hub-private for the rest of this process rather than risking a leak.
	directStartUnknown bool
	directHTTP         *http.Server
	directListeners    []net.Listener
	directHosts        []string
	directFingerprint  string
	directStartedAt    time.Time
	directLastError    string
	directListenAddr   string // test seam; production uses the dedicated direct port
	directHostProvider func() []string
}

// nativeStatusFlight coalesces the expensive local desktop probe. The cache is
// only for console presentation; Hub dispatch and API changes always perform
// their own authoritative lease validation.
type nativeStatusFlight struct{ done chan struct{} }

type sseMsg struct {
	event string
	data  []byte
}

// New creates the console server.
func New(d Deps) *Server {
	if d.Logger == nil {
		d.Logger = log.New(io.Discard, "", 0)
	}
	if d.Local == nil {
		dir := d.Dir
		if dir == "" && d.Store != nil {
			dir = filepath.Dir(d.Store.Path())
		}
		d.Local = launcher.New(dir)
	}
	if d.ClaudeDesktop == nil {
		d.ClaudeDesktop = launcher.NewClaudeDesktop3P(d.Local)
	}
	apiService := apiservice.New(func() config.Config {
		if d.Store == nil {
			return config.Config{}
		}
		return d.Store.Get()
	})
	apiService.Official = d.OfficialGateway
	apiService.OfficialImages = d.OfficialImages
	ledger := newUsageLedger(d)
	if ledger != nil {
		apiService.Observe = func(o usageledger.Observation) {
			ledger.Append(o)
			// Update LastUsedAt for the account that served this request.
			// Fire-and-forget: a write failure here must never block the response path.
			if o.AccountID != "" && o.HTTPStatus >= 200 && o.HTTPStatus < 300 && d.Store != nil {
				go func(accountID string) {
					now := time.Now().UnixMilli()
					_ = d.Store.Update(func(c *config.Config) error {
						for i := range c.LocalAccounts {
							if c.LocalAccounts[i].ID == accountID {
								c.LocalAccounts[i].LastUsedAt = now
								return nil
							}
						}
						return nil
					})
				}(o.AccountID)
			}
		}
		apiService.BeginUsage = ledger.BeginAccountUsage
	}
	if d.Hub != nil {
		apiService.ModelCatalog = d.Hub.OfficialModelCatalog
	}
	s := &Server{
		d:                  d,
		pluginHooks:        pluginhook.FromEnv(),
		token:              config.RandomToken(24),
		log:                d.Logger,
		subs:               map[chan sseMsg]struct{}{},
		pending:            map[string]protocol.Event{},
		activeSessions:     map[string]string{},
		accountPool:        gateway.NewAccountPool(),
		apiService:         apiService,
		usageLedger:        ledger,
		usageOverview:      newUsageOverviewCache(d.Store),
		accountUsage:       newAccountUsageCache(),
		directSessionKeys:  map[string]struct{}{},
		directEventTargets: map[string]directEventSubscription{},

		byDesktop:     map[string]time.Time{},
		modelProbeSeq: map[string]uint64{},
	}
	if s.d.Store != nil {
		persisted := s.d.Store.Get()
		for _, sessionKey := range persisted.DirectSessionKeys {
			if strings.TrimSpace(sessionKey) != "" {
				s.directSessionKeys[sessionKey] = struct{}{}
			}
		}
		s.directStartUnknown = persisted.DirectSessionFenceUnknown || persisted.DirectSessionStartsPending > 0
		if persisted.DirectSessionStartsPending > 0 && !persisted.DirectSessionFenceUnknown {
			// A process restart during session.start leaves its generated session
			// key unknowable. Persist the conservative fence before accepting new
			// events so future restarts remain fail-closed too.
			if err := s.d.Store.Update(func(c *config.Config) error {
				c.DirectSessionFenceUnknown = true
				c.DirectSessionStartsPending = 0
				return nil
			}); err != nil {
				s.log.Printf("direct: failed to persist recovered unknown session-start fence: %v", err)
			}
		}
	}
	apiService.Plugins = s.pluginHooks
	s.initAPINetwork()
	s.initOfficialUsageMaintenance()
	return s
}

// startProber launches the background account health prober. Call this once
// after New() and after a context is available (e.g. from run() in main.go):
//
//	cons.StartProber(ctx)
func (s *Server) StartProber(ctx context.Context) {
	if s.apiService == nil || s.apiService.Health == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.backgroundMu.Lock()
	if s.proberStarted {
		s.backgroundMu.Unlock()
		return
	}
	s.proberStarted = true
	s.backgroundMu.Unlock()
	p := prober.New(prober.Options{
		Health: s.apiService.Health,
		Accounts: func() []config.LocalAccount {
			if s.d.Store == nil {
				return nil
			}
			return s.d.Store.Get().LocalAccounts
		},
		Client:   s.apiService.Client,
		Interval: 5 * time.Minute,
		OnResult: func(accountID, baseURL string, kind accounthealth.ErrorKind, latency time.Duration) {
			if kind != accounthealth.OK {
				s.log.Printf("prober: account %s (%s) %s latency=%dms", accountID, baseURL, kind, latency.Milliseconds())
			}
		},
	})
	go p.Run(ctx)
}

// StartWatchdog launches the background stalled-agent watchdog. Call this once
// after SetManager() and after a context is available:
//
//	cons.StartWatchdog(ctx)
//
// The watchdog detects agent processes that are alive but have stopped
// producing events for longer than 10 minutes and requests an exact-turn
// interrupt. The agent adapter publishes the terminal event only after it
// confirms that turn stopped; uncertain outcomes are published as warnings.
func (s *Server) StartWatchdog(ctx context.Context) {
	m := s.manager()
	if m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.backgroundMu.Lock()
	if s.watchdogStarted {
		s.backgroundMu.Unlock()
		return
	}
	s.watchdogStarted = true
	s.backgroundMu.Unlock()
	w := agentwatchdog.New(agentwatchdog.Options{
		Manager: m,
		Logger:  s.log,
		EmitEvent: func(ev protocol.Event) {
			s.Broadcast(ev)
			s.PublishHubEvent(ev)
		},
	})
	s.backgroundMu.Lock()
	s.watchdog = w
	s.backgroundMu.Unlock()
	go w.Run(ctx)
}

// PublishHubEvent is the single outbound Hub event boundary. Direct P2P
// session events must be kept on the authenticated direct socket even when an
// event originates in a background producer such as the stalled-agent
// watchdog. Keeping the guard here prevents future producers from bypassing
// the privacy fence by calling Hub.Push directly.
func (s *Server) PublishHubEvent(ev protocol.Event) {
	if s.d.Hub == nil {
		return
	}
	// Local write-conflict bookkeeping must see Direct completion too. This
	// observation cannot enqueue, decorate, persist or upload the private event.
	s.d.Hub.ObserveLocalEvent(ev)
	// Hold the read lock through queue insertion. Registering a direct session
	// takes the write lock, so a concurrent session.open/start cannot pass the
	// fence check and then enqueue the same event to Hub afterward.
	s.directEventMu.RLock()
	if ev.SessionKey != "" {
		if s.directStartsInFlight > 0 || s.directStartUnknown {
			s.directEventMu.RUnlock()
			return
		}
		if _, direct := s.directSessionKeys[ev.SessionKey]; direct {
			s.directEventMu.RUnlock()
			return
		}
	}
	s.d.Hub.Push(ev)
	s.directEventMu.RUnlock()
}

// SetManager attaches the agents manager.
func (s *Server) SetManager(m agents.Manager) {
	s.mgrMu.Lock()
	s.mgr = m
	s.mgrMu.Unlock()
}

func (s *Server) manager() agents.Manager {
	s.mgrMu.RLock()
	defer s.mgrMu.RUnlock()
	return s.mgr
}

// Token is the per-run cookie value (exposed for tests).
func (s *Server) Token() string { return s.token }

// Handler returns the root HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(static))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			w.Header().Set("Cache-Control", "no-cache")
			files.ServeHTTP(w, r)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: CookieName, Value: s.token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		b, err := webFS.ReadFile("web/index.html")
		if err != nil {
			http.Error(w, "missing index", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"service": "salcara-bridge", "version": s.d.Version})
	})
	mux.Handle("/internal/agents/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopback(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		m := s.manager()
		if m == nil {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		m.LocalHandler().ServeHTTP(w, r)
	}))
	api := http.NewServeMux()
	s.routes(api)
	s.pluginRoutes(api)
	s.usageLedgerRoutes(api)
	s.usageLedgerQueryRoutes(api)
	api.HandleFunc("GET /api/local/accounts/usage", s.handleAccountUsage)
	api.HandleFunc("GET /api/local/accounts/health", s.handleAccountHealth)
	api.HandleFunc("POST /api/local/accounts/health/reset", s.handleAccountHealthReset)
	mux.Handle("/api/", s.requireAuth(api))
	accountPool := s.accountPool
	if accountPool == nil {
		accountPool = gateway.NewAccountPool()
		s.accountPool = accountPool
	}
	if accounts := s.d.CodexAccounts; accounts != nil {
		accountPool.SetRoutingProbe(func(credentialRef string, identityEpoch uint64, model string) gateway.PoolRoutingObservation {
			observation := accounts.PoolRouting(credentialRef, identityEpoch, model)
			return gateway.PoolRoutingObservation{
				Current: observation.Current, QuotaFresh: observation.QuotaFresh, QuotaAllowed: observation.QuotaAllowed,
				RemainingPercent: observation.RemainingPercent, PlanFresh: observation.PlanFresh, PlanFamily: observation.PlanFamily,
				SubscriptionKnown: observation.SubscriptionKnown, SubscriptionActiveUntil: observation.SubscriptionActiveUntil,
			}
		})
	} else {
		accountPool.SetRoutingProbe(nil)
	}
	s.apiService.Pool = accountPool
	localGateway := gateway.New(func() string { return s.d.Store.Get().GatewayKey }, func(target string) (config.LocalAccount, bool) {
		return s.d.Store.Get().GatewaySourceAccount(target)
	})
	localGateway.Plugins = s.pluginHooks
	localGateway.AuthorizationRevision = func(target string) string { return s.d.Store.Get().GatewayAuthorizationRevision(target) }
	if s.usageLedger != nil {
		localGateway.Observe = s.observeUsage
		localGateway.BeginUsage = s.usageLedger.BeginAccountUsage
	}
	localGateway.KeyForTarget = func(target string) string {
		c := s.d.Store.Get()
		if strings.HasPrefix(target, "direct-") {
			return c.DirectGatewayCredential(target)
		}
		if strings.HasPrefix(target, "remote-") {
			return c.RemoteGatewayCredential(target)
		}
		// A gateway-backed local tool must present the lease minted by this
		// Bridge process.  Do not accept the durable GatewayKey after restart:
		// an old Codex/Claude profile or another local program must fail closed
		// until the user explicitly reapplies/launches it through Salcara.
		return c.LocalGatewayCredential(target)
	}
	localGateway.Pool = accountPool
	localGateway.Official = s.d.OfficialGateway
	localGateway.PoolEnabled = func(target string) bool {
		c := s.d.Store.Get()
		if strings.HasPrefix(target, "remote-") || strings.HasPrefix(target, "direct-") {
			return false // A proof for A does not authorize substituting member B.
		}
		selected, ok := c.AuthorizedAppliedToolAccount(target)
		return ok && selected.PoolEnabled
	}
	localGateway.Accounts = func(target string) []config.LocalAccount {
		c := s.d.Store.Get()
		family := target
		family = strings.TrimPrefix(family, "direct-")
		if strings.HasPrefix(family, "remote-") {
			family = strings.TrimPrefix(family, "remote-")
		}
		accounts := make([]config.LocalAccount, 0, len(c.LocalAccounts))
		for _, account := range c.LocalAccounts {
			accounts = append(accounts, c.APIForTool(account, family))
		}
		return accounts
	}
	localGateway.PoolGroup = func(target string) string {
		c := s.d.Store.Get()
		family := target
		if strings.HasPrefix(family, "direct-") {
			if selected, ok := c.DirectToolAccount(strings.TrimPrefix(family, "direct-")); ok {
				return selected.PoolGroup
			}
			return ""
		}
		if strings.HasPrefix(family, "remote-") {
			family = strings.TrimPrefix(family, "remote-")
			if selected, ok := c.RemoteToolAccount(family); ok {
				return selected.PoolGroup
			}
			if selected, ok := c.AuthorizedAppliedToolAccount(family); ok {
				return selected.PoolGroup
			}
			return ""
		}
		if selected, ok := c.AuthorizedAppliedToolAccount(target); ok {
			return selected.PoolGroup
		}
		return ""
	}
	localGateway.RefreshModels = func(ctx context.Context, target string, applied config.LocalAccount) ([]string, error) {
		models, err := launcher.Models(ctx, applied)
		if err != nil {
			return nil, err
		}
		models = visibleAccountModels(applied, models)
		if len(models) == 0 {
			return nil, errors.New("当前 API 的模型均被本卡的允许/拒绝规则过滤，请调整规则后刷新")
		}
		err = s.d.Store.Update(func(c *config.Config) error {
			current, valid := c.LocalAccount(applied.ID)
			if !valid {
				return errors.New("API 已切换，未应用旧模型目录")
			}
			current = c.APIForTool(current, target)
			if config.AccountFingerprint(current) != config.AccountFingerprint(applied) {
				return errors.New("API 已切换，未应用旧模型目录")
			}
			if current.ModelsManual != applied.ModelsManual || strings.Join(current.Models, "\x00") != strings.Join(applied.Models, "\x00") {
				return errors.New("模型目录已编辑，未应用旧结果")
			}
			for i := range c.LocalAccounts {
				if c.LocalAccounts[i].ID == applied.ID {
					if current.ModelsManual {
						models = visibleAccountModels(current, current.Models)
					} else {
						c.LocalAccounts[i].Models = models
					}
					return nil
				}
			}
			return errors.New("API 已移除")
		})
		return models, err
	}
	mux.Handle("/gateway/", localGateway)
	mux.Handle("/codex-api/", s.apiService)
	mux.Handle("/backend-api/codex/", s.apiService)

	return s.hostGuard(securityHeaders(mux))
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

// allowedHost reports whether the Host header names this console.
func (s *Server) allowedHost(host string) bool {
	p := strconv.Itoa(s.d.Port)
	return host == "127.0.0.1:"+p || host == "localhost:"+p
}

func (s *Server) allowedOrigin(origin string) bool {
	p := strconv.Itoa(s.d.Port)
	return origin == "http://127.0.0.1:"+p || origin == "http://localhost:"+p
}

func (s *Server) hostGuard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.allowedHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (s *Server) requireAuth(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && !s.allowedOrigin(o) {
			writeErr(w, http.StatusForbidden, "来源不被允许")
			return
		}
		if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
			writeErr(w, http.StatusForbidden, "来源不被允许")
			return
		}
		c, err := r.Cookie(CookieName)
		if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "控制台会话已过期，请刷新页面")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				writeErr(w, http.StatusUnsupportedMediaType, "需要 JSON")
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ---- live updates ----

// Broadcast forwards an agent event to every open console page and tracks pending approvals.
func (s *Server) Broadcast(ev protocol.Event) {
	if s.d.Hub != nil {
		ev = s.d.Hub.ProjectSessionEvent(ev)
	}
	s.backgroundMu.Lock()
	watchdog := s.watchdog
	s.backgroundMu.Unlock()
	if watchdog != nil {
		watchdog.ObserveEvent(ev)
	}
	if ev.TS == 0 {
		ev.TS = time.Now().UnixMilli()
	}
	s.publishPluginEvent(ev)
	s.broadcastDirectEvent(ev)
	if s.publishActivity(ev) {
		return
	}
	b, _ := json.Marshal(ev)
	s.publish(sseMsg{event: "event", data: b})
}

func (s *Server) markDesktop(id string) {
	s.byMu.Lock()
	defer s.byMu.Unlock()
	for k, t := range s.byDesktop {
		if time.Since(t) > 10*time.Minute {
			delete(s.byDesktop, k)
		}
	}
	s.byDesktop[id] = time.Now()
}

// Attribute marks approvals answered in this console as resolved "by desktop". The agents report every
// Respond() as coming from the phone, so the core passes each event through here before fan-out.
func (s *Server) Attribute(ev protocol.Event) protocol.Event {
	if ev.Type != "approval.resolved" {
		return ev
	}
	s.byMu.Lock()
	if _, ok := s.byDesktop[ev.ApprovalID]; ok {
		delete(s.byDesktop, ev.ApprovalID)
		ev.By = "desktop"
	}
	s.byMu.Unlock()
	return ev
}

// BroadcastStatus pushes a hub status change.
func (s *Server) BroadcastStatus(st hubclient.Status) {
	if st.Pairing != nil && st.Pairing.PendingExpiresAt == 0 {
		s.clearQR()
	}
	b, _ := json.Marshal(st)
	s.publish(sseMsg{event: "status", data: b})
}

func (s *Server) publish(m sseMsg) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- m:
		default: // slow page; it will resync on reconnect
		}
	}
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	ch := make(chan sseMsg, 256)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	defer func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	if s.d.Hub != nil {
		b, _ := json.Marshal(s.d.Hub.Status())
		writeSSE(w, "status", b)
	}
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-ch:
			writeSSE(w, m.event, m.data)
			fl.Flush()
		case <-ping.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func writeSSE(w io.Writer, event string, data []byte) {
	_, _ = io.WriteString(w, "event: "+event+"\ndata: ")
	_, _ = w.Write(data)
	_, _ = io.WriteString(w, "\n\n")
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return errors.New("请求格式有误")
	}
	return nil
}

func ctxTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}
