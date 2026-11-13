// Package hubclient keeps this computer registered and online at the Salcara hub (PROTOCOL.md §2): it
// registers the device, holds the /bridge/stream SSE connection open, executes commands from the phone and
// uploads agent events in batches.
package hubclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/hubingress"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/protocol"
)

// Connection states shown in the console.
const (
	StateNotLoggedIn        = "not_logged_in" // 未登录
	StateConnecting         = "connecting"    // 连接中
	StateConnected          = "connected"     // 已连接
	StateInvalidKey         = "invalid_key"   // legacy remote authorization failure
	StateDeviceAuthRequired = "device_auth_required"
	StateSourceUnauthorized = "source_unauthorized"
	StatePairRequired       = "pair_required"
	StateRemoteBlocked      = "remote_blocked"
)

// Status is a snapshot of the hub connection.
type Status struct {
	State     string                       `json:"state"`
	Error     string                       `json:"error,omitempty"`
	ErrorCode string                       `json:"errorCode,omitempty"`
	ClockSkew *hubingress.ClockSkewDetails `json:"clockSkew,omitempty"`
	Since     int64                        `json:"since"` // ms
	HubURL    string                       `json:"hubUrl"`
	DeviceID  string                       `json:"deviceId"`
	Queued    int                          `json:"queued"`
	LastEvent int64                        `json:"lastEventSent,omitempty"`
	Pairing   *PairStatus                  `json:"pairing,omitempty"`
	Recovery  RecoveryStatus               `json:"recovery"`
}

// RecoveryStatus describes local message storage, not remote task completion.
// It contains no private paths, message bodies, credentials or raw disk errors.
type RecoveryStatus struct {
	State           string `json:"state"` // ready, degraded, blocked, unavailable
	QueuedBytes     int    `json:"queuedBytes"`
	HistoryRequired bool   `json:"historyRequired"`
}

type PairStatus struct {
	DeviceID         string `json:"deviceId"`
	Paired           bool   `json:"paired"`
	Revision         int64  `json:"revision"`
	PendingExpiresAt int64  `json:"pendingExpiresAt"`
}

// Options configure a Client. Zero durations use the production defaults.
type Options struct {
	Store   *config.Store
	Version string
	Logger  *log.Logger
	HTTP    *http.Client // for normal requests; the stream uses its own no-timeout client based on Transport

	HeartbeatTimeout time.Duration // no data for this long → reconnect (60s)
	BackoffMin       time.Duration // 1s
	BackoffMax       time.Duration // 60s
	FlushInterval    time.Duration // 1s, cumulative message snapshots are coalesced
	FlushBatch       int           // 50
	QueueCap         int           // 5000
	QueueBytes       int           // 8 MiB conservative retained/wire budget
	ReregisterEvery  time.Duration // 5m
	ToolCheckEvery   time.Duration // 60s
	StandbyEvery     time.Duration // 15s; short polls to already paired alternate stations

	OnStatus func(Status) // called on every state change
	// NavigateDesktop only opens an existing native desktop chat. It does not send a turn.
	NavigateDesktop func(context.Context, string) error
	// DiscoverTools is a read-only executable inventory, injectable for tests.
	// Paths and package identities returned by it never leave agents.status.
	DiscoverTools func(context.Context, map[string]string) []launcher.Tool
	// Desktop is the explicitly activated experimental native companion only.
	// Nil or expired never falls back to a CLI manager.
	Desktop Desktop
	// ClaudeDesktopHistory resolves the current verified 3P namespace afresh.
	// It is intentionally separate from native Desktop and the CLI manager.
	ClaudeDesktopHistory func(context.Context) (ReadOnlyDesktopHistory, error)
}

type Desktop interface {
	NativeStatus(context.Context) (desktopcompanion.NativeConnection, error)
	NativeList(context.Context) ([]protocol.SessionInfo, error)
	NativeOpen(context.Context, string) (protocol.SessionInfo, []protocol.Event, error)
	NativeSend(context.Context, string, string, string) error
}

// Client is the hub connection.
type Client struct {
	o   Options
	log *log.Logger

	mgrMu                    sync.RWMutex
	mgr                      agents.Manager
	mgrReady                 atomic.Bool // avoids making read-only status wait for manager startup
	workspaceMu              sync.Mutex
	workspaceSessions        map[string]workspaceLease // sessionKey -> last verified workspace state
	workspaceStartObserver   *workspaceStartObservation
	workspaceScanned         bool       // a successful metadata scan for the current manager
	taskConfigMu             sync.Mutex // serialize phone task admission with API changes
	sourceGrantMu            sync.Mutex // one acquisition at a time; never holds taskConfigMu across network
	workspaceFilesMu         sync.Mutex // bounded, on-demand project file operations
	projectlessMu            sync.Mutex
	projectlessLoaded        bool
	projectlessErr           error
	projectlessRecords       map[string]projectlessRecord
	terminalMu               sync.Mutex
	terminals                map[string]*workspaceTerminal
	previewMu                sync.Mutex
	previews                 map[string]*workspacePreview
	projectPreviewMu         sync.Mutex
	projectPreviews          map[string]*workspaceProjectPreview
	projectPreviewAgentEpoch atomic.Uint64
	updatePrepared           bool      // protected by taskConfigMu; updater closes task admission before quitting
	updateUntil              time.Time // prepare lease; abandoned installers cannot block new tasks forever
	updateCommitted          bool
	updatePolicy             atomic.Pointer[UpdatePolicy] // publisher policy survives installer cancellation
	modelCatalogMu           sync.Mutex
	modelCatalog             map[string]verifiedModelCatalog
	modelCatalogEpoch        uint64
	featureCatalog           featureCatalogCache
	sideQuestions            sideQuestionCache
	cooperationTickets       map[string]*cooperationTicket
	cooperationEpoch         atomic.Uint64

	mu                   sync.Mutex
	phoneMu              sync.Mutex
	revokeCleanupMu      sync.Mutex // serialize durable conditional station cleanup
	revokeWake           chan struct{}
	uploadAuthorization  string
	nativeAdmissions     map[*nativeAdmission]struct{} // pending HTTP admissions, not running desktop tasks; protected by mu
	lastPairRevision     int64
	pairRevisionIdentity string
	status               Status
	cancelConn           context.CancelFunc
	connectionEpoch      uint64 // protected by mu; invalidates delayed stream handshakes
	lastFP               string
	lastReg              time.Time
	tools                []protocol.Tool
	toolsAt              time.Time

	wake  chan struct{}
	regCh chan struct{}

	qmu              sync.Mutex
	durable          *durableStore
	durableErr       error
	outboxWriteErr   error // qmu; cleared only by a complete persisted snapshot
	outboxSequence   uint64
	eventScope       atomic.Pointer[eventUploadScope]
	queueEventScope  string // qmu; immutable scope of persisted queue and batch
	attachmentMu     sync.Mutex
	attachmentStores map[string]*attachmentStore
	// stationSwitching is set while a remote.station.switch transaction is
	// validating and committing.  It is guarded by qmu when changed/read by
	// the queue path so flushLoop cannot take a batch after the switch has
	// declared the queue idle.  Push still accepts late events; the final
	// switch check then safely aborts the handover instead of losing them.
	stationSwitching atomic.Bool
	// A committed handover or an ordinary Kick has a short interval before
	// the station sends pair.status. Retain events for the durable phone
	// generation instead of treating unknown authorization as a revoke.
	handoverAwaitingAuth atomic.Bool
	queue                []protocol.Event
	queueBytes           int
	inFlight             int             // immutable prefix currently being uploaded; never coalesce it
	inFlightBatchID      string          // stable across HTTP retries until this prefix is acknowledged
	inFlightPayload      json.RawMessage // frozen wire bytes, including gap-notice timestamps
	inFlightRecovery     map[string]recoveryMark
	queueConfig          config.Config
	recoveryGeneration   uint64
	recovery             map[string]recoveryMark
	recoveryAll          bool
	recoveryOverflow     uint64
	recoverySeen         map[string]uint64
	recoverySeenOrder    []string
	flushNow             chan struct{}
	lastSent             atomic.Int64

	streamHTTP *http.Client
}

var errUnauthorized = errors.New("远程连接授权被拒绝")

// New creates a client; call SetManager before Run.
func New(o Options) *Client {
	if o.HeartbeatTimeout == 0 {
		o.HeartbeatTimeout = 60 * time.Second
	}
	if o.BackoffMin == 0 {
		o.BackoffMin = time.Second
	}
	if o.BackoffMax == 0 {
		o.BackoffMax = 60 * time.Second
	}
	if o.FlushInterval == 0 {
		o.FlushInterval = time.Second
	}
	if o.FlushBatch == 0 {
		o.FlushBatch = 50
	}
	if o.QueueCap <= 0 {
		o.QueueCap = 5000
	}
	if o.QueueBytes <= 0 {
		o.QueueBytes = defaultQueueBytes
	}
	if o.ReregisterEvery == 0 {
		o.ReregisterEvery = 5 * time.Minute
	}
	if o.ToolCheckEvery == 0 {
		o.ToolCheckEvery = 60 * time.Second
	}
	if o.StandbyEvery <= 0 {
		o.StandbyEvery = 15 * time.Second
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	clientCopy := *o.HTTP
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	o.HTTP = &clientCopy
	if o.Logger == nil {
		o.Logger = log.New(io.Discard, "", 0)
	}
	tr := o.HTTP.Transport
	if tr == nil {
		tr = &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 30 * time.Second}
	}
	cacheDir := ""
	if o.Store != nil {
		cacheDir = filepath.Join(filepath.Dir(o.Store.Path()), "ingress-cache")
	}
	tr = hubingress.New(tr, cacheDir)
	o.HTTP.Transport = tr
	c := &Client{
		o:          o,
		log:        o.Logger,
		wake:       make(chan struct{}, 1),
		regCh:      make(chan struct{}, 1),
		flushNow:   make(chan struct{}, 1),
		revokeWake: make(chan struct{}, 1),
		streamHTTP: &http.Client{Transport: tr, CheckRedirect: o.HTTP.CheckRedirect},
	}
	c.status = Status{State: StateNotLoggedIn, Since: nowMS()}
	c.restoreUpdatePolicy()
	c.lastPairRevision = -1
	c.workspaceSessions = make(map[string]workspaceLease)
	if o.Store != nil {
		c.durable, c.durableErr = openDurableStore(filepath.Join(filepath.Dir(o.Store.Path()), "remote-store"))
		if c.durableErr == nil {
			c.durableErr = c.restoreOutbox()
			c.syncOutboxAttachmentPinsLocked()
		}
		if c.durableErr != nil {
			c.log.Printf("hub: durable storage unavailable: %v", c.durableErr)
		}
		if o.Store.Get().PhoneHash != "" && (len(c.queue) > 0 || len(c.recovery) > 0) {
			c.handoverAwaitingAuth.Store(true)
		}
	}
	c.watchCooperationSource()
	if o.Store != nil {
		o.Store.OnChange(func(old, current config.Config) {
			if old.EffectiveRemoteMode() != current.EffectiveRemoteMode() {
				c.Kick()
			}
		})
	}
	return c
}

// SetManager attaches the agents manager (commands fail with an error until it is set).
func (c *Client) SetManager(m agents.Manager) {
	c.projectPreviewAgentEpoch.Add(1)
	c.cooperationEpoch.Add(1)
	c.featureCatalog.clear()
	c.workspaceMu.Lock()
	c.workspaceSessions = make(map[string]workspaceLease)
	c.workspaceScanned = false
	c.workspaceMu.Unlock()
	c.mgrMu.Lock()
	c.mgr = m
	c.mgrMu.Unlock()
	c.mgrReady.Store(m != nil)
}

func (c *Client) manager() agents.Manager {
	c.mgrMu.RLock()
	defer c.mgrMu.RUnlock()
	return c.mgr
}

// Status returns the current connection status.
func (c *Client) Status() Status {
	// Keep the lock order consistent with Push/flushLoop. Those paths hold
	// qmu while checking canUpload (which takes mu); taking mu first here could
	// deadlock exactly when a status refresh races an event upload.
	c.qmu.Lock()
	queued := len(c.queue)
	recovery := RecoveryStatus{State: "ready", QueuedBytes: c.queueBytes, HistoryRequired: c.recoveryAll || len(c.recovery) != 0}
	if c.durableErr != nil {
		recovery.State = "blocked"
	} else if c.durable == nil {
		recovery.State = "unavailable"
	} else if c.outboxWriteErr != nil {
		recovery.State = "degraded"
	}
	c.qmu.Unlock()
	c.mu.Lock()
	s := c.status
	if s.ClockSkew != nil {
		copy := *s.ClockSkew
		s.ClockSkew = &copy
	}
	c.mu.Unlock()
	cfg := c.o.Store.Get()
	s.HubURL = cfg.EffectiveHubURL()
	s.DeviceID = cfg.DeviceID
	s.Queued = queued
	s.LastEvent = c.lastSent.Load()
	s.Recovery = recovery
	return s
}

func (c *Client) durableReady() bool {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	return c.durable != nil && c.durableErr == nil
}

// Tools returns the tools found by the last detection.
func (c *Client) Tools() []protocol.Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.Tool(nil), c.tools...)
}

func (c *Client) setState(state, errText string) {
	c.setStateCode(state, errText, "")
}

func (c *Client) setStateCode(state, errText, code string) {
	var cfg config.Config
	if c.o.Store != nil {
		cfg = c.o.Store.Get()
	}
	c.mu.Lock()
	changed := c.status.State != state || c.status.Error != errText || c.status.ErrorCode != code || c.status.ClockSkew != nil
	if c.status.State != state {
		c.status.Since = nowMS()
	}
	c.status.State, c.status.Error, c.status.ErrorCode = state, errText, code
	c.status.ClockSkew = nil
	if state != StateConnected && cfg.EffectiveRemoteMode() == "bot" && botEventScopeRequired(cfg) {
		c.eventScope.Store(&eventUploadScope{identity: eventIdentity(cfg), required: true})
	}
	c.mu.Unlock()
	if changed {
		if errText != "" {
			c.log.Printf("hub: %s (%s)", state, errText)
		} else {
			c.log.Printf("hub: %s", state)
		}
		if c.o.OnStatus != nil {
			c.o.OnStatus(c.Status())
		}
	}
}

// A completed request belongs to one connection generation, even if a new
// connection uses exactly the same URL and device ID. Kick retires it under mu.
func (c *Client) publishConnectionFailure(cfg config.Config, epoch uint64, err error) (string, bool) {
	state, code, message := remoteFailure(err, cfg)
	clockSkew := hubingress.ClockSkew(err)
	c.mu.Lock()
	current := c.o.Store.Get() // Store notifications run after releasing its lock.
	if c.connectionEpoch != epoch || config.RemoteIdentity(cfg) != config.RemoteIdentity(current) || !current.HubRemoteEnabled() {
		c.mu.Unlock()
		return "", false
	}
	changed := c.status.State != state || c.status.Error != message || c.status.ErrorCode != code || !sameClockSkew(c.status.ClockSkew, clockSkew)
	if c.status.State != state {
		c.status.Since = nowMS()
	}
	c.status.State, c.status.Error, c.status.ErrorCode = state, message, code
	c.status.ClockSkew = clockSkew
	if current.EffectiveRemoteMode() == "bot" && botEventScopeRequired(current) {
		c.eventScope.Store(&eventUploadScope{identity: eventIdentity(current), required: true})
	}
	c.mu.Unlock()
	if changed {
		c.log.Printf("hub: %s (%s)", state, message)
		if c.o.OnStatus != nil {
			c.o.OnStatus(c.Status())
		}
	}
	return state, true
}

// Kick drops the current connection and reconnects immediately (after login/config changes).
func (c *Client) Kick() {
	c.cooperationEpoch.Add(1)
	c.featureCatalog.clear()
	// A normal reconnect also has an authorization gap before pair.status.
	// Retain progress for the same durable phone generation during that gap;
	// an explicit revoke clears PhoneHash and therefore cannot enter this gate.
	cfg := c.o.Store.Get()
	if cfg.PhoneBindingID != "" && cfg.PhoneHash != "" {
		c.handoverAwaitingAuth.Store(true)
	}
	nextState := StateConnecting
	if !cfg.LoggedIn() || !cfg.HubRemoteEnabled() {
		nextState = StateNotLoggedIn
	}
	c.mu.Lock()
	c.connectionEpoch++
	// Status reads the current saved Hub URL. Clear the old stream's connected
	// state in the same critical section as its authorization, so a just-saved
	// station cannot appear ready before its own stream has connected.
	stateChanged := c.status.State != nextState || c.status.Error != "" || c.status.ErrorCode != "" || c.status.ClockSkew != nil
	if c.status.State != nextState {
		c.status.Since = nowMS()
	}
	c.status.State, c.status.Error, c.status.ErrorCode = nextState, "", ""
	c.status.ClockSkew = nil
	c.status.Pairing = nil // unknown until the new station's authenticated snapshot
	c.uploadAuthorization = ""
	if cfg.EffectiveRemoteMode() == "bot" && botEventScopeRequired(cfg) {
		c.eventScope.Store(&eventUploadScope{identity: eventIdentity(cfg), required: true})
	}
	for pending := range c.nativeAdmissions {
		if !cfg.HubRemoteEnabled() || pending.identity != eventIdentity(cfg) {
			pending.cancel()
		}
	}
	if c.cancelConn != nil {
		c.cancelConn()
	}
	c.mu.Unlock()
	if stateChanged && c.o.OnStatus != nil {
		c.o.OnStatus(c.Status())
	}
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Reregister sends the device info again soon (projects changed, device renamed…).
func (c *Client) Reregister() {
	select {
	case c.regCh <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is done.
func (c *Client) Run(ctx context.Context) {
	if c.o.Store.Get().PhoneHash != "" {
		c.handoverAwaitingAuth.Store(true)
	}
	defer c.closeWorkspaceTerminals()
	defer c.closeWorkspaceProjectPreviews()
	defer c.closeWorkspacePreviews()
	go c.cleanupAttachmentStores(ctx)
	go c.flushLoop(ctx)
	go c.registerLoop(ctx)
	go c.phoneRevokeLoop(ctx)
	go c.standbyLoop(ctx)
	backoff := c.o.BackoffMin
	for ctx.Err() == nil {
		cfg := c.o.Store.Get()
		if !cfg.LoggedIn() || !cfg.HubRemoteEnabled() {
			c.setState(StateNotLoggedIn, "")
			c.waitWake(ctx, 0)
			continue
		}
		connCtx, cancel := context.WithCancel(ctx)
		c.mu.Lock()
		c.cancelConn = cancel
		epoch := c.connectionEpoch
		c.mu.Unlock()
		if st := c.Status().State; st != StateConnected {
			c.setState(StateConnecting, c.Status().Error)
		} else {
			c.setState(StateConnecting, "")
		}
		started := time.Now()
		err := c.register(connCtx, true)
		if err == nil {
			err = c.stream(connCtx)
		}
		interrupted := connCtx.Err() != nil
		cancel()
		if ctx.Err() != nil {
			return
		}
		if config.RemoteIdentity(cfg) != config.RemoteIdentity(c.o.Store.Get()) {
			backoff = c.o.BackoffMin
			continue // an old site's failure must not mark the new identity invalid.
		}
		if interrupted && !errors.Is(err, errHeartbeat) {
			// cancelled by Kick: reconnect right away
			backoff = c.o.BackoffMin
			continue
		}
		state, published := c.publishConnectionFailure(cfg, epoch, err)
		if !published {
			backoff = c.o.BackoffMin
			continue
		}
		if state == StateInvalidKey || state == StateDeviceAuthRequired || state == StateSourceUnauthorized || state == StatePairRequired || state == StateRemoteBlocked {
			c.waitWake(ctx, 5*time.Minute)
			backoff = c.o.BackoffMin
			continue
		}
		if time.Since(started) > 2*time.Minute {
			backoff = c.o.BackoffMin
		}
		c.waitWake(ctx, backoff)
		backoff *= 2
		if backoff > c.o.BackoffMax {
			backoff = c.o.BackoffMax
		}
	}
}

func (c *Client) waitWake(ctx context.Context, d time.Duration) {
	var timer <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-ctx.Done():
	case <-c.wake:
	case <-timer:
	}
}

func (c *Client) base() string { return c.o.Store.Get().EffectiveHubURL() + "/v1" }

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	cfg := c.o.Store.Get()
	return c.requestFor(ctx, cfg, method, path, rd, body != nil)
}

func (c *Client) requestFor(ctx context.Context, cfg config.Config, method, path string, rd io.Reader, hasBody bool) (*http.Request, error) {
	current := c.o.Store.Get()
	if !cfg.HubRemoteEnabled() || !current.HubRemoteEnabled() || cfg.EffectiveRemoteMode() != current.EffectiveRemoteMode() {
		return nil, errors.New("此远程方式未启用")
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.EffectiveHubURL()+"/v1"+path, rd)
	if err != nil {
		return nil, err
	}
	req = hubingress.Bind(req, cfg.EffectiveHubURL()+"/v1")
	id, publicKey, endpoint := cfg.RemoteIngress()
	req = hubingress.BindIdentity(req, id, publicKey, endpoint)
	if !cfg.RemoteDeviceOnly {
		req.Header.Set("Authorization", "Bearer "+cfg.AccountKey)
	}
	req.Header.Set("X-Salcara-Device-Secret", cfg.DeviceSecret)
	if cfg.RemoteDeviceOnly {
		req.Header.Set("X-Salcara-Device-Id", cfg.DeviceID)
		if method == http.MethodPost && path == "/device/register" {
			if groupID, invite, signature := cfg.BotEnrollment(); groupID != "" && invite != "" && signature != "" {
				req.Header.Set("X-Salcara-Bot-Group", groupID)
				req.Header.Set("X-Salcara-Bot-Invite", invite)
				req.Header.Set("X-Salcara-Bot-Signature", signature)
			} else if claim := cfg.DeviceEnrollmentClaim(); claim != "" {
				req.Header.Set("X-Salcara-Enrollment-Claim", claim)
			}
		}
	}
	req.Header.Set("User-Agent", "SalcaraBridge/"+c.o.Version)
	if path == "/bridge/events" || strings.HasPrefix(path, "/bridge/blobs") {
		scope := c.eventScopeFromContext(ctx, cfg)
		if scope != "" {
			req.Header.Set("X-Salcara-Event-Scope", scope)
		}
		if c.eventScopeRequired(cfg) && scope == "" {
			return nil, errors.New("Bot 事件代次尚未确认，未上传内容")
		}
	}
	if (path == "/bridge/events" || strings.HasPrefix(path, "/bridge/blobs")) && cfg.PhoneBindingID != "" {
		req.Header.Set("X-Salcara-Binding-Id", cfg.PhoneBindingID)
		req.Header.Set("X-Salcara-Phone-Hash", cfg.PhoneHash)
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// PairStart asks the Hub for a one-time code shown only in the local desktop
// app. The phone must enter it before it can see this computer or send commands.
func (c *Client) PairStart(ctx context.Context) (string, int64, error) {
	result, err := c.StartPair(ctx)
	return result.Code, result.ExpiresAt, err
}

type PairInfo struct {
	Code       string `json:"code"`
	Ticket     string `json:"ticket"`
	ExpiresAt  int64  `json:"expires_at"`
	ComputerID string `json:"-"`
	Revision   int64  `json:"revision"`
	AttemptID  string `json:"attemptId"`
}

func (c *Client) StartPair(ctx context.Context) (result PairInfo, retErr error) {
	c.phoneMu.Lock()
	defer c.phoneMu.Unlock()
	if c.Status().State != StateConnected {
		return PairInfo{}, errors.New("电脑尚未连接中转站")
	}
	initial := c.o.Store.Get()
	if initial.RemoteDeviceOnly || initial.PhoneBindingID != "" {
		if err := c.o.Store.Update(func(cfg *config.Config) error {
			if cfg.ComputerID == "" {
				cfg.ComputerID = config.NewUUID()
			}
			if cfg.PhoneBindingID == "" {
				cfg.PhoneBindingID = config.RandomToken(32)
			}
			cfg.PhonePairPending = config.RemoteIdentity(*cfg)
			cfg.PhonePairAttempt = config.RandomToken(32)
			cfg.PhonePairExpires = nowMS() + (5 * time.Minute).Milliseconds()
			return nil
		}); err != nil {
			return PairInfo{}, errors.New("无法保存本机配对授权")
		}
	}
	cfg := c.o.Store.Get()
	defer func() {
		if retErr != nil && cfg.PhonePairAttempt != "" {
			_ = c.o.Store.Update(func(next *config.Config) error {
				if next.PhonePairAttempt == cfg.PhonePairAttempt {
					next.PhonePairPending, next.PhonePairAttempt, next.PhonePairExpires = "", "", 0
				}
				return nil
			})
		}
	}()
	pairRequest := map[string]string{"deviceId": cfg.DeviceID, "bindingId": cfg.PhoneBindingID, "phoneHash": cfg.PhoneHash, "computerId": cfg.ComputerID, "attemptId": cfg.PhonePairAttempt}
	if cfg.RemoteDeviceOnly && cfg.PhoneBindingID != "" {
		// New Hubs use this single-generation seed for the initial QR token,
		// so later relay syncs reproduce it without retiring saved credentials.
		// Older Hubs ignore the optional field and retain random QR tokens.
		pairRequest["pairSeed"] = connectionPhoneSeed(cfg, config.RemoteConnection{HubURL: cfg.EffectiveHubURL(), DeviceID: cfg.DeviceID, DeviceSecret: cfg.DeviceSecret})
	}
	body, _ := json.Marshal(pairRequest)
	req, err := c.requestFor(ctx, cfg, http.MethodPost, "/bridge/pair/start", bytes.NewReader(body), true)
	if err != nil {
		return PairInfo{}, err
	}
	resp, err := c.o.HTTP.Do(req)
	if err != nil {
		return PairInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return PairInfo{}, checkResp(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result); err != nil {
		return PairInfo{}, err
	}
	if len(result.Code) != 8 {
		return PairInfo{}, errors.New("中转站返回的配对码无效")
	}
	if c.o.Store.Get().RemoteDeviceOnly && len(result.Ticket) != 64 {
		return PairInfo{}, errors.New("本站远程服务没有返回安全扫码凭证，请更新服务")
	}
	// New Hubs echo the attempt marker and are checked strictly. Older paired
	// Hubs have only the single-use ticket/expiry; keep that compatibility path
	// without treating a missing marker as proof of a newer state.
	if cfg.PhonePairAttempt != "" && result.AttemptID != "" && (result.AttemptID != cfg.PhonePairAttempt || result.ExpiresAt <= nowMS() || result.ExpiresAt > cfg.PhonePairExpires+30_000 || result.Revision < 0) {
		return PairInfo{}, errors.New("本站远程服务未确认配对请求，请更新服务后重新扫码")
	}
	// The active station can change while the HTTP request is in flight. Do
	// not publish an A-side QR/revision into the newly selected B connection;
	// the deferred cleanup is conditional on the old attempt marker.
	if config.RemoteIdentity(cfg) != config.RemoteIdentity(c.o.Store.Get()) {
		return PairInfo{}, errors.New("中转站已切换，请重新生成二维码")
	}
	c.pairRevisionIdentity, c.lastPairRevision = config.RemoteIdentity(cfg), result.Revision
	c.publishPair(PairStatus{DeviceID: cfg.DeviceID, Paired: cfg.PhoneHash != "" && c.canUpload(cfg), Revision: result.Revision, PendingExpiresAt: result.ExpiresAt}, c.currentUploadAuthorization())
	result.ComputerID = cfg.ComputerID
	return result, nil
}

func (c *Client) RevokePair(ctx context.Context) error {
	return c.revokePhone(ctx)
}

func (c *Client) post(ctx context.Context, path string, body any) error {
	return c.postFor(ctx, c.o.Store.Get(), path, body)
}

func (c *Client) postFor(ctx context.Context, cfg config.Config, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := c.requestFor(ctx, cfg, http.MethodPost, path, bytes.NewReader(b), true)
	if err != nil {
		return err
	}
	resp, err := c.o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return checkResp(resp)
}

// Device builds the registration body, detecting tools.
func (c *Client) Device(ctx context.Context) protocol.Device {
	cfg := c.o.Store.Get()
	d := protocol.Device{
		DeviceID: cfg.DeviceID,
		Name:     cfg.DeviceName,
		OS:       runtime.GOOS,
		Version:  c.o.Version,
		Tools:    []protocol.Tool{},
		Projects: cfg.Projects,
		// This is a process-level guarantee: execution commands are checked
		// against the Hub's non-secret source envelope before dispatch.  The Hub
		// uses it to reject restricted requests from older/unhardened Bridges.
		Capabilities: []string{"source.guard.v1", "source.grant.acquire.v1", "station.source-only.v1", "station.guard.v1", "station.guard.v2", "pair.connections.v1"},
	}
	if c.durableReady() {
		d.Capabilities = append(d.Capabilities, "durable.execution.v1", "durable.events.v1", "attachment.blobs.v1")
	}
	if runtime.GOOS == "windows" || runtime.GOOS == "linux" {
		d.Capabilities = append(d.Capabilities, "session.projectless.v1", "workspace.explorer.v1", "workspace.projects.v1", "workspace.files.v1", "workspace.project.files.v1", "workspace.files.write.v1", "workspace.files.manage.v1", "workspace.terminal.v1", "workspace.artifacts.v1", "workspace.artifacts.write.v1", "workspace.project.preview.v1")
		if _, err := exec.LookPath("git"); err == nil {
			d.Capabilities = append(d.Capabilities, "workspace.git.v1", "workspace.git.review.v1", "workspace.git.repositories.v1")
		}
		if previewBrowserExecutable() != "" {
			d.Capabilities = append(d.Capabilities, "workspace.preview.v1")
		}
	}
	if m := c.manager(); m != nil {
		c.mu.Lock()
		cached := !c.toolsAt.IsZero() && time.Since(c.toolsAt) < c.o.ToolCheckEvery
		if cached {
			d.Tools = append([]protocol.Tool{}, c.tools...)
		}
		c.mu.Unlock()
		if !cached {
			list := m.Agents()
			d.Tools = make([]protocol.Tool, len(list))
			var wg sync.WaitGroup
			for i, a := range list {
				wg.Add(1)
				go func(i int, a agents.Agent) {
					defer wg.Done()
					dctx, cancel := context.WithTimeout(ctx, 4*time.Second)
					defer cancel()
					d.Tools[i] = a.Detect(dctx)
				}(i, a)
			}
			wg.Wait()
			c.mu.Lock()
			c.toolsAt = time.Now()
			c.mu.Unlock()
		}
	}
	c.mu.Lock()
	c.tools = append([]protocol.Tool(nil), d.Tools...)
	c.mu.Unlock()
	return d
}

func fingerprint(d protocol.Device) string {
	b, _ := json.Marshal(d)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

func (c *Client) register(ctx context.Context, force bool) error {
	cfg := c.o.Store.Get()
	d := c.Device(ctx)
	if config.RemoteIdentity(cfg) != config.RemoteIdentity(c.o.Store.Get()) {
		return errors.New("中转站已切换，不上传旧站点设备信息")
	}
	if err := c.refreshStationCapabilities(ctx, cfg); err != nil {
		return err
	}
	latest := c.o.Store.Get()
	if config.RemoteIdentity(cfg) != config.RemoteIdentity(latest) {
		return errors.New("中转站已切换，请重试")
	}
	cfg = latest
	fp := fingerprint(d)
	c.mu.Lock()
	same := fp == c.lastFP && time.Since(c.lastReg) < c.o.ReregisterEvery
	c.mu.Unlock()
	if same && !force {
		return nil
	}
	path := "/bridge/register"
	if cfg.RemoteDeviceOnly {
		path = "/device/register"
	}
	claim := cfg.DeviceEnrollmentClaim()
	botGroup, botInvite, botSignature := cfg.BotEnrollment()
	if err := c.registerWithEventScope(ctx, cfg, path, d); err != nil {
		return err
	}
	if cfg.RemoteDeviceOnly && claim != "" {
		if err := c.o.Store.Update(func(current *config.Config) error {
			current.ClearDeviceEnrollmentClaim(cfg.EffectiveHubURL(), cfg.DeviceID, claim)
			return nil
		}); err != nil {
			return errors.New("设备已登记，但本机未能清除一次性登记码；请检查 Bridge 配置文件权限")
		}
		c.refreshStationCapabilities(ctx, c.o.Store.Get())
	}
	if cfg.RemoteDeviceOnly && botInvite != "" && botGroup != "" && botSignature != "" {
		if err := c.o.Store.Update(func(current *config.Config) error {
			current.ClearBotEnrollment(cfg.EffectiveHubURL(), cfg.DeviceID, botInvite)
			return nil
		}); err != nil {
			return errors.New("设备已登记，但本机未能清除 Bot 绑定包；请检查 Bridge 配置文件权限")
		}
	}
	c.mu.Lock()
	c.lastFP, c.lastReg = fp, time.Now()
	c.mu.Unlock()
	return nil
}

// registerLoop re-registers every ReregisterEvery and whenever tools/projects/name change.
func (c *Client) registerLoop(ctx context.Context) {
	t := time.NewTicker(c.o.ToolCheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.regCh:
		}
		if c.Status().State != StateConnected {
			continue
		}
		if err := c.register(ctx, false); err != nil {
			c.log.Printf("hub: re-register: %v", err)
			if errors.Is(err, errUnauthorized) || RemoteErrorCode(err) == "user_disabled" || RemoteErrorCode(err) == "ambiguous_credentials" || RemoteErrorCode(err) == "invalid_request" {
				c.Kick()
			}
		}
	}
}

var errHeartbeat = errors.New("连接超时，正在重连")

func (c *Client) acceptStreamConnection(ctx context.Context, cfg config.Config, epoch uint64) error {
	current := c.o.Store.Get()
	c.mu.Lock()
	// Kick clears connection state and increments the epoch under this same
	// lock. A delayed 200 response must never restore the retired stream.
	if err := ctx.Err(); err != nil {
		c.mu.Unlock()
		return err
	}
	if c.connectionEpoch != epoch || config.RemoteIdentity(cfg) != config.RemoteIdentity(current) {
		c.mu.Unlock()
		return errors.New("中转站连接已更换，不接受旧连接")
	}
	changed := c.status.State != StateConnected || c.status.Error != "" || c.status.ErrorCode != "" || c.status.ClockSkew != nil
	if c.status.State != StateConnected {
		c.status.Since = nowMS()
	}
	c.status.State, c.status.Error, c.status.ErrorCode = StateConnected, "", ""
	c.status.ClockSkew = nil
	c.mu.Unlock()
	if changed {
		c.log.Printf("hub: %s", StateConnected)
		if c.o.OnStatus != nil {
			c.o.OnStatus(c.Status())
		}
	}
	return nil
}

func (c *Client) stream(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cfg := c.o.Store.Get()
	c.mu.Lock()
	epoch := c.connectionEpoch
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c.phoneMu.Lock()
	c.pairRevisionIdentity = config.RemoteIdentity(cfg)
	c.lastPairRevision = -1 // Hub revisions are memory-local and may reset after restart.
	c.phoneMu.Unlock()
	req, err := c.requestFor(ctx, cfg, http.MethodGet, "/bridge/stream?deviceId="+cfg.DeviceID, nil, false)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return checkResp(resp)
	}
	if err := c.acceptStreamConnection(ctx, cfg, epoch); err != nil {
		return err
	}
	// Changes made while we were (re)connecting — e.g. a project added right after login, whose
	// Reregister() was skipped because we weren't connected yet — are sent now if the device differs
	// from what was registered.
	c.Reregister()

	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var timedOut atomic.Bool
	go func() {
		step := c.o.HeartbeatTimeout / 6
		if step < 10*time.Millisecond {
			step = 10 * time.Millisecond
		}
		t := time.NewTicker(step)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if time.Since(time.Unix(0, last.Load())) > c.o.HeartbeatTimeout {
					timedOut.Store(true)
					cancel()
					return
				}
			}
		}
	}()

	err = readBridgeSSE(resp.Body, func() { last.Store(time.Now().UnixNano()) }, func(name, data string) {
		c.handleSSEFor(ctx, name, data, cfg)
	})
	if timedOut.Load() {
		return errHeartbeat
	}
	if err == io.EOF {
		return errors.New("服务器断开了连接")
	}
	return err
}

func (c *Client) handleSSE(_ context.Context, name, data string) {
	c.handleSSEFor(context.Background(), name, data, c.o.Store.Get())
}

func (c *Client) handleSSEFor(ctx context.Context, name, data string, cfg config.Config) {
	if ctx.Err() != nil || config.RemoteIdentity(cfg) != config.RemoteIdentity(c.o.Store.Get()) {
		return
	}
	if name == "pair.status" {
		c.acceptPairState(cfg, data)
		return
	}
	if name == "hello" || name == "heartbeat" {
		current := c.o.Store.Get()
		// Creating the first local QR adds a binding ID while this authenticated
		// station stream is still open. Its pre-claim scope cannot authorize the
		// new identity, but reconnecting here makes the displayed QR temporarily
		// unusable. Keep the stream until the matching claim is accepted; that
		// transition reconnects before publishing an authorized paired state.
		if cfg.PhoneBindingID == "" && cfg.PhoneHash == "" && current.PhoneHash == "" &&
			validPhoneHash(current.PhoneBindingID) && current.PhonePairPending == config.RemoteIdentity(cfg) &&
			current.PhonePairAttempt != "" && current.PhonePairExpires > nowMS() {
			return
		}
		var greeting struct {
			EventScope         string `json:"eventScope"`
			EventScopeRequired bool   `json:"eventScopeRequired"`
		}
		if json.Unmarshal([]byte(data), &greeting) == nil {
			if err := c.acceptEventScope(cfg, greeting.EventScope, greeting.EventScopeRequired); err != nil {
				c.log.Printf("hub: event scope unavailable: %v", err)
				c.Kick()
			}
		}
		return
	}
	if name != "command" && name != "" {
		return
	}
	var env protocol.CommandEnvelope
	if err := json.Unmarshal([]byte(data), &env); err != nil || env.CommandID == "" {
		c.log.Printf("hub: bad command: %v", err)
		return
	}
	go c.executeFor(env, cfg)
}

func (c *Client) execute(env protocol.CommandEnvelope) {
	c.executeFor(env, c.o.Store.Get())
}

func (c *Client) executeFor(env protocol.CommandEnvelope, cfg config.Config) {
	current := c.o.Store.Get()
	if !current.HubRemoteEnabled() || cfg.EffectiveRemoteMode() != current.EffectiveRemoteMode() || config.RemoteIdentity(cfg) != config.RemoteIdentity(current) {
		return
	}
	typ, _ := env.Command["type"].(string)
	// Preserve Git's execution budget through the SSE entry point. The Hub
	// leaves a reply margin (95 s for Git, 45 s for ordinary commands).
	budget := 38 * time.Second
	if typ == "workspace.git.read" || typ == "workspace.git.mutate" {
		budget = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	stationSwitch := typ == "remote.station.switch"
	dispatched := false
	var res any
	var err error
	if env.DeviceID != cfg.DeviceID && (env.DeviceID != "" || cfg.RemoteDeviceOnly) || typ != "device.ping" && (!c.commandPhoneMatches(c.o.Store.Get(), env.BindingID, env.PhoneHash, env.EventScope) || !c.canUpload(c.o.Store.Get()) || !c.commandEventScopeValid(cfg, env.EventScope)) {
		err = errors.New("手机绑定已失效，请重新扫码")
	} else if err = c.refreshExecutionStation(ctx, cfg, env); err != nil {
		// A changed policy is fetched from this exact saved Hub, never a phone URL.
	} else if err = c.validateExecutionSource(env); err != nil {
		// The source contract is checked before Dispatch can select a worker,
		// native companion, or relay account. This is the final local boundary
		// that prevents an external process/config from being adopted remotely.
	} else {
		// Dispatch can wait for another task/API mutation. Freeze the identity
		// observed at admission and re-check it after that wait, otherwise an old
		// A command could run against the newly selected B station.
		guarded := withCommandGuard(ctx, commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID, bindingID: env.BindingID, phoneHash: env.PhoneHash, eventScope: env.EventScope})
		guarded = context.WithValue(guarded, eventScopeContextKey{}, env.EventScope)
		guarded = withExecutionSource(guarded, env)
		dispatched = true
		res, err = c.Dispatch(guarded, env.Command)
	}
	// Recheck after long reads, before any private history leaves the computer.
	identityChanged := config.RemoteIdentity(cfg) != config.RemoteIdentity(c.o.Store.Get())
	// A station switch commits the new identity before the old Hub receives its
	// reply.  The phone binding can be revoked in that small window, which may
	// turn the reply into an authorization error even though B is already the
	// active station.  Remember the commit separately so the old SSE is still
	// kicked and the Bridge cannot remain attached to A forever.
	stationCommitted := stationSwitch && identityChanged
	if (!stationSwitch && identityChanged) || typ != "device.ping" && !stationSwitch && (!c.commandPhoneMatches(c.o.Store.Get(), env.BindingID, env.PhoneHash, env.EventScope) || !c.canUpload(c.o.Store.Get()) || !c.commandEventScopeValid(cfg, env.EventScope)) {
		res, err = nil, errors.New("手机绑定已失效，请重新扫码")
		if dispatched {
			// Kick clears live upload authorization before a same-binding stream
			// reconnects. Neither that gap nor a real revoke can turn an already
			// admitted write into proof of non-execution. Keep its receipt while
			// withholding all result content from the old authorization scope.
			err = commandDeliveryUncertainError{}
		}
	}
	if stationSwitch && !c.commandPhoneMatches(c.o.Store.Get(), env.BindingID, env.PhoneHash, env.EventScope) {
		res, err = nil, errors.New("手机绑定已失效，请重新扫码")
		if dispatched {
			err = commandDeliveryUncertainError{}
		}
	}
	rep := protocol.Reply{DeviceID: cfg.DeviceID, CommandID: env.CommandID, OK: err == nil, Result: res}
	if err != nil {
		rep.Error = err.Error()
		rep.Code = protocol.ExecutionErrorCode(err)
		rep.Result = nil
		c.log.Printf("command %s failed: %v", typ, err)
	} else {
		c.log.Printf("command %s ok", typ)
	}
	for i := 0; i < 3; i++ {
		rctx, rcancel := context.WithTimeout(context.Background(), 15*time.Second)
		perr := c.postFor(rctx, cfg, "/bridge/reply", rep)
		rcancel()
		if perr == nil {
			if stationCommitted {
				// The old Hub must receive the acknowledgement before its stream is
				// dropped. ReconnectLoop will then read the new active station from
				// the atomically-saved config.
				c.Kick()
			}
			return
		}
		c.log.Printf("hub: reply %s: %v", env.CommandID, perr)
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}
	// switchStation has already committed the new identity before the reply is
	// attempted. If A disappeared after that commit, all reply retries can fail
	// even though B is the only correct destination. Do not leave the stream
	// attached to A until its heartbeat happens to end; reconnect immediately.
	if stationCommitted {
		c.Kick()
	}
}

// ---- events ----

// Push queues an event for upload. Overflow drops oldest unsent events, keeps
// the immutable in-flight prefix, and schedules bounded history-gap notices.
func (c *Client) Push(ev protocol.Event) {
	ev = c.ProjectSessionEvent(ev)
	originalConfig := c.o.Store.Get()
	originalScope := c.eventScopeFor(originalConfig)
	c.observeWorkspaceEvent(ev)
	if ev.Type == "features.changed" {
		var valid bool
		ev, valid = c.invalidateAgentFeatures(ev)
		if !valid {
			return
		}
	}
	mediaContext := context.WithValue(context.Background(), eventScopeContextKey{}, originalScope)
	decorateEventMediaWith(c.attachmentsForConfig(mediaContext, originalConfig), &ev)
	if ev.TS == 0 {
		ev.TS = nowMS()
	}
	c.qmu.Lock()
	cfg := c.o.Store.Get()
	if eventIdentity(cfg) != eventIdentity(c.queueConfig) {
		c.queue = nil
		c.queueBytes = 0
		c.inFlight = 0
		c.inFlightBatchID = ""
		c.inFlightPayload = nil
		c.inFlightRecovery = nil
		c.clearRecoveryLocked()
		c.queueConfig = cfg
		c.queueEventScope = c.eventScopeFor(cfg)
	}
	if eventIdentity(cfg) != eventIdentity(originalConfig) || c.eventScopeFor(cfg) != originalScope || c.eventScopeRequired(cfg) && originalScope == "" {
		c.recoveryGeneration++
		c.recoveryAll = true
		c.recoveryOverflow = c.recoveryGeneration
		_ = c.persistOutboxLocked()
		c.qmu.Unlock()
		return
	}
	if !c.canUpload(cfg) && !(c.handoverAwaitingAuth.Load() && cfg.PhoneHash != "") {
		c.qmu.Unlock()
		return
	}
	weight := eventWeight(ev)
	if weight > eventWeightLimit(ev) || weight > c.o.QueueBytes {
		c.noteDroppedLocked([]protocol.Event{ev})
	} else if !c.coalesceFeatureChangeLocked(ev) && !c.coalesceSnapshotLocked(ev) {
		c.queue = append(c.queue, ev)
		c.queueBytes += weight
	}
	c.trimQueueLocked()
	c.syncOutboxAttachmentPinsLocked()
	if err := c.journalEventLocked(ev); err != nil {
		c.noteDroppedLocked([]protocol.Event{ev})
		c.log.Printf("hub: event journal failed: %v", err)
	}
	n := len(c.queue) - c.inFlight
	c.qmu.Unlock()
	if n >= c.o.FlushBatch || ev.Type == "features.changed" {
		select {
		case c.flushNow <- struct{}{}:
		default:
		}
	}
}

// decorateEventMedia converts local provider paths into bounded refs before a
// live or historical event can cross the hub boundary. Only explicit image
// tool kinds (including the legacy read kind) are eligible; arbitrary tool
// text is never interpreted as a path, preventing accidental file exfiltration.
func decorateEventMedia(ev *protocol.Event) { decorateEventMediaWith(phoneAttachments, ev) }

func decorateEventMediaWith(attachments *attachmentStore, ev *protocol.Event) {
	if ev == nil || len(ev.Attachments) > 0 && len(ev.ImageData) == 0 {
		return
	}
	if len(ev.ImageData) > 0 {
		refs := make([]protocol.AttachmentRef, 0, len(ev.ImageData))
		for _, data := range ev.ImageData {
			if ref, ok := attachments.importBase64(ev.SessionKey, data); ok {
				refs = append(refs, ref)
				if len(refs) == 64 {
					break
				}
			}
		}
		ev.ImageData = nil
		if len(refs) > 0 {
			ev.Attachments = append(ev.Attachments, refs...)
		}
		return
	}
	if ev.Type != "tool" || (ev.Kind != "read" && ev.Kind != "image_view" && ev.Kind != "image_generation") || ev.Detail == "" {
		return
	}
	if strings.HasPrefix(ev.Detail, "image-base64:") {
		ref, ok := attachments.importBase64(ev.SessionKey, strings.TrimPrefix(ev.Detail, "image-base64:"))
		ev.Detail = ""
		if ok {
			ev.Attachments = []protocol.AttachmentRef{ref}
		}
		return
	}
	// Only image-view/generation paths are eligible. If the source has already
	// disappeared, still remove the absolute path before serializing the event;
	// a failed import must never turn a local filename into a phone-visible
	// detail string.
	if !filepath.IsAbs(ev.Detail) {
		return
	}
	if _, image := imageMimeForPath(ev.Detail); !image {
		return
	}
	ref, ok := attachments.importFile(ev.SessionKey, ev.Detail)
	ev.Detail = ""
	if !ok {
		return
	}
	ev.Attachments = []protocol.AttachmentRef{ref}
}

// Only supersede a cumulative unsent snapshot in the trailing snapshot segment.
// Approvals, tools, turn boundaries and final snapshots are never overwritten.
func (c *Client) coalesceSnapshotLocked(ev protocol.Event) bool {
	if (ev.Type != "message" && ev.Type != "reasoning") || ev.ID == "" || ev.SessionKey == "" {
		return false
	}
	for i := len(c.queue) - 1; i >= c.inFlight; i-- {
		previous := c.queue[i]
		if previous.Type != "message" && previous.Type != "reasoning" {
			break
		}
		if previous.Type == ev.Type && previous.SessionKey == ev.SessionKey && previous.ID == ev.ID &&
			previous.Tool == ev.Tool && previous.Role == ev.Role && previous.TurnID == ev.TurnID {
			if previous.Final {
				break
			}
			c.queueBytes += eventWeight(ev) - eventWeight(previous)
			c.queue[i] = ev
			return true
		}
	}
	return false
}

func (c *Client) flushLoop(ctx context.Context) {
	t := time.NewTicker(c.o.FlushInterval)
	defer t.Stop()
	var retryAt time.Time
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-c.flushNow:
		}
		if time.Now().Before(retryAt) {
			continue
		}
		st := c.Status().State
		if st == StateNotLoggedIn || st == StateInvalidKey || st == StateDeviceAuthRequired || st == StateSourceUnauthorized || st == StatePairRequired || st == StateRemoteBlocked {
			continue
		}
		for {
			c.qmu.Lock()
			current := c.o.Store.Get()
			if !current.HubRemoteEnabled() {
				c.qmu.Unlock()
				break
			}
			// A restart while another transport is active restores the same
			// durable Hub outbox with that inactive mode. Resuming its exact
			// identity updates admission flags without changing batch contents.
			if eventIdentity(c.queueConfig) == eventIdentity(current) {
				c.queueConfig = current
			}
			if c.stationSwitching.Load() {
				c.qmu.Unlock()
				break
			}
			if c.eventScopeRequired(c.queueConfig) && (c.eventScopeFor(c.queueConfig) == "" || c.queueEventScope != c.eventScopeFor(c.queueConfig)) {
				c.qmu.Unlock()
				break
			}
			n := len(c.queue)
			if n == 0 && len(c.recovery) == 0 {
				c.qmu.Unlock()
				break
			}
			if c.inFlightBatchID == "" {
				n = uploadCount(c.queue)
				upload, recoveryMarks := c.recoveryBatchLocked(c.queue[:n])
				batchID := config.NewUUID()
				envelope := map[string]any{"deviceId": c.queueConfig.DeviceID, "batchId": batchID, "events": upload}
				if c.queueEventScope != "" {
					envelope["eventScope"] = c.queueEventScope
				}
				payload, err := json.Marshal(envelope)
				if err != nil {
					c.qmu.Unlock()
					c.log.Printf("hub: cannot encode event batch: %v", err)
					break
				}
				c.inFlight, c.inFlightBatchID = n, batchID
				c.inFlightPayload, c.inFlightRecovery = payload, recoveryMarks
			}
			if err := c.persistOutboxLocked(); err != nil {
				c.qmu.Unlock()
				c.log.Printf("hub: cannot persist event batch: %v", err)
				break
			}
			n = c.inFlight
			batch := append([]protocol.Event(nil), c.queue[:n]...)
			batchID := c.inFlightBatchID
			payload, recoveryMarks := c.inFlightPayload, c.inFlightRecovery
			cfg := c.queueConfig
			frozenScope := c.queueEventScope
			c.qmu.Unlock()
			if eventIdentity(cfg) != eventIdentity(c.o.Store.Get()) || !c.canUpload(cfg) {
				// The active identity is already B, but its authenticated pair.status
				// has not arrived yet. Retain the queue for that bounded reconnect;
				// publishPair clears the gate on success or revocation.
				if c.handoverAwaitingAuth.Load() && cfg.PhoneHash != "" && eventIdentity(cfg) == eventIdentity(c.o.Store.Get()) {
					break
				}
				c.qmu.Lock()
				if eventIdentity(c.queueConfig) == eventIdentity(cfg) {
					c.queue = nil
					c.queueBytes = 0
					c.inFlight = 0
					c.inFlightBatchID = ""
					c.inFlightPayload = nil
					c.inFlightRecovery = nil
					c.clearRecoveryLocked()
				}
				c.qmu.Unlock()
				break
			}

			pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			pctx = context.WithValue(pctx, eventScopeContextKey{}, frozenScope)
			err := c.uploadBatchAttachments(pctx, cfg, batch)
			if err == nil {
				err = c.postFor(pctx, cfg, "/bridge/events", payload)
			}
			cancel()
			if err != nil {
				// Keep the prefix, exact serialized payload and recovery marks
				// immutable until acknowledgement. A lost reply may mean the Hub
				// already accepted this digest; new progress belongs to the next ID.
				fails++
				d := time.Duration(fails) * time.Second
				if d > 30*time.Second {
					d = 30 * time.Second
				}
				retryAt = time.Now().Add(d)
				c.log.Printf("hub: upload %d events: %v", len(batch), err)
				break
			}
			fails = 0
			c.lastSent.Store(nowMS())
			c.qmu.Lock()
			if eventIdentity(c.queueConfig) != eventIdentity(cfg) || c.inFlightBatchID != batchID {
				c.qmu.Unlock()
				break
			}
			c.inFlight = 0
			// the queue may have dropped from the front meanwhile; remove what we sent if still there
			k := 0
			for k < len(batch) && k < len(c.queue) && sameEvent(c.queue[k], batch[k]) {
				k++
			}
			c.queue = c.queue[k:]
			for _, event := range batch[:k] {
				c.queueBytes -= eventWeight(event)
			}
			c.acknowledgeRecoveryLocked(recoveryMarks)
			c.inFlightBatchID = ""
			c.inFlightPayload = nil
			c.inFlightRecovery = nil
			c.syncOutboxAttachmentPinsLocked()
			if err := c.persistOutboxLocked(); err != nil {
				c.log.Printf("hub: event acknowledgement persistence failed: %v", err)
			}
			waitForBatch := len(c.queue) < c.o.FlushBatch && len(c.recovery) == 0
			c.qmu.Unlock()
			if waitForBatch {
				break
			}
		}
	}
}

func sameEvent(a, b protocol.Event) bool {
	return a.TS == b.TS && a.Type == b.Type && a.SessionKey == b.SessionKey && a.ID == b.ID && a.Text == b.Text &&
		a.Status == b.Status && a.ApprovalID == b.ApprovalID && a.Final == b.Final && a.Role == b.Role && a.Tool == b.Tool && a.TurnID == b.TurnID
}

func nowMS() int64 { return time.Now().UnixMilli() }
