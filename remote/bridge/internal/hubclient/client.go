// Package hubclient keeps this computer registered and online at the Salcara hub (PROTOCOL.md §2): it
// registers the device, holds the /bridge/stream SSE connection open, executes commands from the phone and
// uploads agent events in batches.
package hubclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/protocol"
)

// Connection states shown in the console.
const (
	StateNotLoggedIn = "not_logged_in" // 未登录
	StateConnecting  = "connecting"    // 连接中
	StateConnected   = "connected"     // 已连接
	StateInvalidKey  = "invalid_key"   // Key 无效
)

// Status is a snapshot of the hub connection.
type Status struct {
	State     string `json:"state"`
	Error     string `json:"error,omitempty"`
	Since     int64  `json:"since"` // ms
	HubURL    string `json:"hubUrl"`
	DeviceID  string `json:"deviceId"`
	Queued    int    `json:"queued"`
	LastEvent int64  `json:"lastEventSent,omitempty"`
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

	mgrMu             sync.RWMutex
	mgr               agents.Manager
	taskConfigMu      sync.Mutex // serialize phone task admission with API changes
	modelCatalogMu    sync.Mutex
	modelCatalog      map[string]verifiedModelCatalog
	modelCatalogEpoch uint64

	mu         sync.Mutex
	status     Status
	cancelConn context.CancelFunc
	lastFP     string
	lastReg    time.Time
	tools      []protocol.Tool

	wake  chan struct{}
	regCh chan struct{}

	qmu                sync.Mutex
	queue              []protocol.Event
	queueBytes         int
	inFlight           int // immutable prefix currently being uploaded; never coalesce it
	queueConfig        config.Config
	recoveryGeneration uint64
	recovery           map[string]recoveryMark
	recoveryAll        bool
	recoveryOverflow   uint64
	recoverySeen       map[string]uint64
	recoverySeenOrder  []string
	flushNow           chan struct{}
	lastSent           atomic.Int64

	streamHTTP *http.Client
}

var errUnauthorized = errors.New("Key 无效或不是中转站用户")

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
	c := &Client{
		o:          o,
		log:        o.Logger,
		wake:       make(chan struct{}, 1),
		regCh:      make(chan struct{}, 1),
		flushNow:   make(chan struct{}, 1),
		streamHTTP: &http.Client{Transport: tr, CheckRedirect: o.HTTP.CheckRedirect},
	}
	c.status = Status{State: StateNotLoggedIn, Since: nowMS()}
	return c
}

// SetManager attaches the agents manager (commands fail with an error until it is set).
func (c *Client) SetManager(m agents.Manager) {
	c.mgrMu.Lock()
	c.mgr = m
	c.mgrMu.Unlock()
}

func (c *Client) manager() agents.Manager {
	c.mgrMu.RLock()
	defer c.mgrMu.RUnlock()
	return c.mgr
}

// Status returns the current connection status.
func (c *Client) Status() Status {
	c.mu.Lock()
	s := c.status
	c.mu.Unlock()
	cfg := c.o.Store.Get()
	s.HubURL = cfg.EffectiveHubURL()
	s.DeviceID = cfg.DeviceID
	c.qmu.Lock()
	s.Queued = len(c.queue)
	c.qmu.Unlock()
	s.LastEvent = c.lastSent.Load()
	return s
}

// Tools returns the tools found by the last detection.
func (c *Client) Tools() []protocol.Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]protocol.Tool(nil), c.tools...)
}

func (c *Client) setState(state, errText string) {
	c.mu.Lock()
	changed := c.status.State != state || c.status.Error != errText
	if c.status.State != state {
		c.status.Since = nowMS()
	}
	c.status.State, c.status.Error = state, errText
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

// Kick drops the current connection and reconnects immediately (after login/config changes).
func (c *Client) Kick() {
	c.mu.Lock()
	if c.cancelConn != nil {
		c.cancelConn()
	}
	c.mu.Unlock()
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
	go c.flushLoop(ctx)
	go c.registerLoop(ctx)
	backoff := c.o.BackoffMin
	for ctx.Err() == nil {
		cfg := c.o.Store.Get()
		if !cfg.LoggedIn() {
			c.setState(StateNotLoggedIn, "")
			c.waitWake(ctx, 0)
			continue
		}
		connCtx, cancel := context.WithCancel(ctx)
		c.mu.Lock()
		c.cancelConn = cancel
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
		if errors.Is(err, errUnauthorized) {
			c.setState(StateInvalidKey, err.Error())
			c.waitWake(ctx, 5*time.Minute)
			backoff = c.o.BackoffMin
			continue
		}
		if interrupted && !errors.Is(err, errHeartbeat) {
			// cancelled by Kick: reconnect right away
			backoff = c.o.BackoffMin
			continue
		}
		msg := "连接中断"
		if err != nil {
			msg = err.Error()
		}
		c.setState(StateConnecting, msg)
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
	req, err := http.NewRequestWithContext(ctx, method, cfg.EffectiveHubURL()+"/v1"+path, rd)
	if err != nil {
		return nil, err
	}
	if !cfg.RemoteDeviceOnly {
		req.Header.Set("Authorization", "Bearer "+cfg.AccountKey)
	}
	req.Header.Set("X-Salcara-Device-Secret", cfg.DeviceSecret)
	if cfg.RemoteDeviceOnly {
		req.Header.Set("X-Salcara-Device-Id", cfg.DeviceID)
	}
	req.Header.Set("User-Agent", "SalcaraBridge/"+c.o.Version)
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
	Code      string `json:"code"`
	Ticket    string `json:"ticket"`
	ExpiresAt int64  `json:"expires_at"`
}

func (c *Client) StartPair(ctx context.Context) (PairInfo, error) {
	if c.Status().State != StateConnected {
		return PairInfo{}, errors.New("电脑尚未连接中转站")
	}
	req, err := c.newRequest(ctx, http.MethodPost, "/bridge/pair/start", map[string]string{"deviceId": c.o.Store.Get().DeviceID})
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
	var result PairInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&result); err != nil {
		return PairInfo{}, err
	}
	if len(result.Code) != 8 {
		return PairInfo{}, errors.New("中转站返回的配对码无效")
	}
	if c.o.Store.Get().RemoteDeviceOnly && len(result.Ticket) != 64 {
		return PairInfo{}, errors.New("本站插件没有返回安全扫码凭证，请更新插件")
	}
	return result, nil
}

func (c *Client) RevokePair(ctx context.Context) error {
	return c.post(ctx, "/bridge/pair/revoke", map[string]string{"deviceId": c.o.Store.Get().DeviceID})
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

func checkResp(resp *http.Response) error {
	if resp.StatusCode/100 == 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return errUnauthorized
	}
	// Do not reflect arbitrary station response bodies into the local logs/UI:
	// a misconfigured station can echo a device secret or a private command.
	if resp.StatusCode == http.StatusNotFound {
		return errors.New("这个中转站还没有部署远程编程服务 (404)")
	}
	return fmt.Errorf("中转站返回 %d", resp.StatusCode)
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
	}
	if m := c.manager(); m != nil {
		for _, a := range m.Agents() {
			dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			d.Tools = append(d.Tools, a.Detect(dctx))
			cancel()
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
	if err := c.postFor(ctx, cfg, path, d); err != nil {
		return err
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
			if errors.Is(err, errUnauthorized) {
				c.Kick()
			}
		}
	}
}

var errHeartbeat = errors.New("连接超时，正在重连")

func (c *Client) stream(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cfg := c.o.Store.Get()
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
	c.setState(StateConnected, "")
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

	rd := bufio.NewReaderSize(resp.Body, 64*1024)
	var evName string
	var data strings.Builder
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			if timedOut.Load() {
				return errHeartbeat
			}
			if err == io.EOF {
				return errors.New("服务器断开了连接")
			}
			return err
		}
		last.Store(time.Now().UnixNano())
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if data.Len() > 0 {
				c.handleSSEFor(ctx, evName, data.String(), cfg)
			}
			evName = ""
			data.Reset()
		case strings.HasPrefix(line, ":"):
			// keep-alive comment
		case strings.HasPrefix(line, "event:"):
			evName = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line[5:], " "))
		}
	}
}

func (c *Client) handleSSE(_ context.Context, name, data string) {
	c.handleSSEFor(context.Background(), name, data, c.o.Store.Get())
}

func (c *Client) handleSSEFor(_ context.Context, name, data string, cfg config.Config) {
	if config.RemoteIdentity(cfg) != config.RemoteIdentity(c.o.Store.Get()) {
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
	if config.RemoteIdentity(cfg) != config.RemoteIdentity(c.o.Store.Get()) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 44*time.Second)
	defer cancel()
	typ, _ := env.Command["type"].(string)
	res, err := c.Dispatch(ctx, env.Command)
	rep := protocol.Reply{DeviceID: cfg.DeviceID, CommandID: env.CommandID, OK: err == nil, Result: res}
	if err != nil {
		rep.Error = err.Error()
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
			return
		}
		c.log.Printf("hub: reply %s: %v", env.CommandID, perr)
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}
}

// ---- events ----

// Push queues an event for upload. Overflow drops oldest unsent events, keeps
// the immutable in-flight prefix, and schedules bounded history-gap notices.
func (c *Client) Push(ev protocol.Event) {
	if ev.TS == 0 {
		ev.TS = nowMS()
	}
	c.qmu.Lock()
	cfg := c.o.Store.Get()
	if config.RemoteIdentity(cfg) != config.RemoteIdentity(c.queueConfig) {
		c.queue = nil
		c.queueBytes = 0
		c.inFlight = 0
		c.clearRecoveryLocked()
		c.queueConfig = cfg
	}
	weight := eventWeight(ev)
	if weight > maxEventWeight || weight > c.o.QueueBytes {
		c.noteDroppedLocked([]protocol.Event{ev})
	} else if !c.coalesceSnapshotLocked(ev) {
		c.queue = append(c.queue, ev)
		c.queueBytes += weight
	}
	c.trimQueueLocked()
	n := len(c.queue)
	c.qmu.Unlock()
	if n >= c.o.FlushBatch {
		select {
		case c.flushNow <- struct{}{}:
		default:
		}
	}
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
		if st == StateNotLoggedIn || st == StateInvalidKey {
			continue
		}
		for {
			c.qmu.Lock()
			n := len(c.queue)
			if n == 0 && len(c.recovery) == 0 {
				c.qmu.Unlock()
				break
			}
			n = uploadCount(c.queue)
			batch := append([]protocol.Event(nil), c.queue[:n]...)
			upload, recoveryMarks := c.recoveryBatchLocked(batch)
			c.inFlight = n
			cfg := c.queueConfig
			c.qmu.Unlock()
			if config.RemoteIdentity(cfg) != config.RemoteIdentity(c.o.Store.Get()) {
				c.qmu.Lock()
				if config.RemoteIdentity(c.queueConfig) == config.RemoteIdentity(cfg) {
					c.queue = nil
					c.queueBytes = 0
					c.inFlight = 0
					c.clearRecoveryLocked()
				}
				c.qmu.Unlock()
				break
			}

			pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := c.postFor(pctx, cfg, "/bridge/events", map[string]any{"deviceId": cfg.DeviceID, "events": upload})
			cancel()
			if err != nil {
				c.qmu.Lock()
				if config.RemoteIdentity(c.queueConfig) == config.RemoteIdentity(cfg) {
					c.inFlight = 0
				}
				c.qmu.Unlock()
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
			if config.RemoteIdentity(c.queueConfig) != config.RemoteIdentity(cfg) {
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
			c.qmu.Unlock()
		}
	}
}

func sameEvent(a, b protocol.Event) bool {
	return a.TS == b.TS && a.Type == b.Type && a.SessionKey == b.SessionKey && a.ID == b.ID && a.Text == b.Text &&
		a.Status == b.Status && a.ApprovalID == b.ApprovalID && a.Final == b.Final && a.Role == b.Role && a.Tool == b.Tool && a.TurnID == b.TurnID
}

func nowMS() int64 { return time.Now().UnixMilli() }
