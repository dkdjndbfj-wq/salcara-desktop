// Package directrelay contains the TLS-pinned direct phone-to-Bridge transport.
// Its data path is phone ↔ computer and does not require a Salcara Hub.
//
// Architecture:
//
//	Phone ──WSS──► Bridge :47832/direct/ws
//	Bridge authorizes each command before dispatching it to local agents.
//
// Pairing:
//
//	The Bridge generates a one-time QR containing:
//	  { "mode": "direct", "urls": ["wss://computer.example/direct/ws"], "fingerprint": "<sha256-hex>", "token": "<32-hex>" }
//	The phone scans it, connects to the WebSocket, sends:
//	  { "type": "auth", "token": "<32-hex>", "phoneId": "<uuid>", "phoneHash": "<sha256-hex>" }
//	The Bridge validates the token, persists the phone ID and binding hash, and responds:
//	  { "type": "auth_ok", "deviceId": "<id>" }
//	Subsequent reconnects use phoneHash for auth (no token needed).
//
// Security:
//
//   - Token is one-time use and expires after 5 minutes.
//   - phoneHash is a sha-256 of the phone's persistent binding secret.
//   - TLS is mandatory. Pairing data is not generated unless the caller supplies
//     explicit WSS URLs; there is no plaintext fallback.
//   - This transport has no public NAT traversal: it does not implement STUN,
//     ICE, TURN, hole punching, or a server relay fallback. A VPN address is
//     reachable only when the VPN supplies a route; whether that VPN itself
//     relays traffic is determined by the VPN provider/topology, not Bridge.
package directrelay

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	pairTokenTTL                  = 5 * time.Minute
	maxAuthMessageBytes           = 8 << 10   // Auth contains only a device ID and two short credentials.
	maxMessageBytes               = 256 << 10 // 256 KiB per authenticated WS frame.
	maxUnauthenticatedConnections = 16
	maxQueuedCommands             = 32
	maxQueuedEvents               = 64
	pingInterval                  = 20 * time.Second
	pongWait                      = 30 * time.Second
)

var errPhoneAlreadyPaired = errors.New("a phone is already paired; revoke the existing pairing before creating a new QR code")
var errCommandAlreadyStarted = errors.New("direct command has already been admitted")
var errRelayClosed = errors.New("direct relay is closed")

var upgrader = websocket.Upgrader{
	// Keep Gorilla's same-origin default so browser pages cannot silently use
	// the native direct socket across origins.
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// ──────────────────────────────────────────────────────────────
// Relay
// ──────────────────────────────────────────────────────────────

// Relay is the P2P WebSocket server embedded in Bridge.
type Relay struct {
	o  Options
	mu sync.RWMutex
	// closed fences new pairing, authentication, and command admission during
	// application shutdown. The persisted phone binding remains untouched.
	closed    bool
	closeOnce sync.Once
	// Every admitted callback is registered while holding mu for reading.
	// Close takes mu for writing before Wait, which fences subsequent Add calls.
	activeCommands sync.WaitGroup
	// Bound unauthenticated handshakes so a LAN peer cannot create an
	// unbounded number of upgraded connections before authentication.
	authSlots chan struct{}

	// One-time pairing tokens: token hex → expiry.
	pendingTokens map[string]time.Time

	// Authenticated phone connection (only one paired phone at a time per device).
	phoneID              string // stable phone identity supplied by the phone's secure store
	phoneHash            string // sha-256 hex of the paired phone's binding secret
	instanceNonce        string // keeps temporary ownership local to this relay runtime
	bindingEpoch         uint64 // changes on re-pair/revoke, never on socket reconnect
	connectionGeneration uint64
	conn                 *phoneSession

	// Commands from the phone are pushed here.
	commands chan Command
}

// Options configures the Relay.
type Options struct {
	Logger            *log.Logger
	DeviceID          string
	DeviceName        string
	DeviceOS          string
	CertificateSHA256 string
	PairedPhoneID     string
	PairedPhoneHash   string
	// OnPaired persists the new binding before the relay accepts it. Returning
	// an error fails the handshake closed and leaves the token consumed.
	OnPaired func(phoneID, phoneHash string) error
	// OnRevoked removes the stored binding after this process immediately
	// disconnects and forgets it. A persistence error is returned to the caller.
	OnRevoked func() error
	// OnDisconnected retires connection-scoped event subscriptions after a
	// phone socket closes. It runs without the relay lock held.
	OnDisconnected func(EventTarget)
	// SecureURLs supplies the complete phone WebSocket URLs, including the
	// direct path. Only valid wss:// URLs are returned in pairing data.
	// There is deliberately no ws:// fallback: the auth exchange contains a
	// reusable reconnect credential.
	SecureURLs func() []string
}

// Command is an opaque handle for a payload received from a paired phone. The
// payload is available only inside RunCurrentCommand's guarded callback.
type Command struct {
	deviceID          string
	phoneID           string
	requestID         string
	sessionGeneration uint64
	bindingEpoch      uint64
	payload           map[string]any
	relay             *Relay
	session           *phoneSession
	state             *commandState
}

type commandState struct {
	started      atomic.Bool
	finished     atomic.Bool
	responseSent atomic.Bool
}

// EventTarget binds an asynchronous event to the phone connection that
// originated the work. Its fields are private so targets can only be captured
// from this relay's current session or an accepted command.
type EventTarget struct {
	relay   *Relay
	session *phoneSession
}

// EventTarget returns the connection capability carried by this command.
// Keep it with work that may later produce events; a target from a replaced
// or revoked session is rejected.
func (c Command) EventTarget() EventTarget {
	if c.state == nil || !c.state.started.Load() {
		return EventTarget{}
	}
	return EventTarget{relay: c.relay, session: c.session}
}

// BindingValidator captures temporary-task ownership only during guarded
// admission. The returned check survives an ordinary disconnect/reconnect but
// fails after revoke, re-pair or relay shutdown. It does not authorize execution
// by itself: callers must also retain the shared station/agent policy checks.
func (c Command) BindingValidator() (identity string, validate func() error) {
	invalid := func() error { return errors.New("direct phone binding is no longer authorized") }
	if c.relay == nil || c.session == nil || c.state == nil || !c.state.started.Load() || c.state.finished.Load() {
		return "", invalid
	}
	r := c.relay
	r.mu.RLock()
	if r.closed || r.conn != c.session || c.deviceID != r.o.DeviceID || c.phoneID == "" ||
		c.phoneID != r.phoneID || c.sessionGeneration != r.connectionGeneration || c.bindingEpoch != r.bindingEpoch {
		r.mu.RUnlock()
		return "", invalid
	}
	deviceID, phoneID, epoch := c.deviceID, c.phoneID, c.bindingEpoch
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", r.instanceNonce, deviceID, phoneID, epoch)))
	r.mu.RUnlock()
	return hex.EncodeToString(sum[:]), func() error {
		r.mu.RLock()
		defer r.mu.RUnlock()
		if r.closed || r.o.DeviceID != deviceID || r.phoneID != phoneID || r.phoneHash == "" || r.bindingEpoch != epoch {
			return invalid()
		}
		return nil
	}
}

// New creates a Relay.  Call Handler() to get the http.Handler, then serve it on
// a port reachable by the phone (e.g. :47832, separate from the console port).
func New(o Options) *Relay {
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	if o.PairedPhoneID != "" || o.PairedPhoneHash != "" {
		if !validPhoneBinding(o.PairedPhoneID, o.PairedPhoneHash) {
			o.Logger.Printf("direct: ignoring invalid persisted phone binding")
			o.PairedPhoneID, o.PairedPhoneHash = "", ""
		}
	}
	return &Relay{
		o:             o,
		pendingTokens: make(map[string]time.Time),
		phoneID:       o.PairedPhoneID,
		phoneHash:     o.PairedPhoneHash,
		instanceNonce: rand.Text(),
		bindingEpoch:  1,
		commands:      make(chan Command, maxQueuedCommands),
		authSlots:     make(chan struct{}, maxUnauthenticatedConnections),
	}
}

// Commands returns the channel on which received phone commands are published.
// A consumer must run each command through RunCurrentCommand and pass the
// supplied context into the remote-command authorization and dispatch boundary.
// IsCurrentCommand is informational only and must not gate dispatch. Do not
// treat pairing as permission to bypass station/account/agent policy.
func (r *Relay) Commands() <-chan Command {
	return r.commands
}

// Handler returns the http.Handler.  Mount it at /direct/ on the relay port.
func (r *Relay) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /direct/ws", r.handleWS)
	mux.HandleFunc("GET /direct/ping", r.handlePing)
	return mux
}

// CaptureEventTarget snapshots the current connection for asynchronous
// device-wide events. Capture it when the event-producing work starts, not
// when a delayed event is finally sent.
func (r *Relay) CaptureEventTarget() (EventTarget, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.closed || r.conn == nil {
		return EventTarget{}, false
	}
	return EventTarget{relay: r, session: r.conn}, true
}

// IsCurrentEventTarget reports whether target still refers to the active
// authenticated phone connection. Callers should still pin actual delivery
// through SendEvent, which rechecks while holding the relay lock.
func (r *Relay) IsCurrentEventTarget(target EventTarget) bool {
	if target.relay != r || target.session == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.closed && r.conn == target.session && r.connectionGeneration == target.session.generation
}

// ──────────────────────────────────────────────────────────────
// GET /direct/ping — phone can check reachability before upgrading
// ──────────────────────────────────────────────────────────────

func (r *Relay) handlePing(w http.ResponseWriter, req *http.Request) {
	// The direct listener is TLS-only. Keep the health endpoint subject to the
	// same invariant as the WebSocket route in case a caller mounts Handler()
	// on the wrong listener during integration or a test setup.
	if req.TLS == nil {
		http.Error(w, "TLS is required for direct connections", http.StatusUpgradeRequired)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"service":  "salcara-bridge-direct",
		"deviceId": r.o.DeviceID,
		"name":     r.o.DeviceName,
		"os":       r.o.DeviceOS,
	})
}

// lanURLs returns only explicitly configured secure WebSocket endpoints.
// The listener and phone must establish validated TLS before this package is
// exposed; advertising a plaintext URL would disclose the reusable phone hash.
func (r *Relay) lanURLs() []string {
	if r.o.SecureURLs == nil {
		return nil
	}
	var result []string
	seen := make(map[string]struct{})
	for _, raw := range r.o.SecureURLs() {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "wss" || u.Host == "" || u.User != nil ||
			u.Path != "/direct/ws" || u.RawQuery != "" || u.Fragment != "" {
			continue
		}
		if _, ok := seen[raw]; !ok {
			seen[raw] = struct{}{}
			result = append(result, raw)
		}
	}
	return result
}

func (r *Relay) issuePairingToken() (string, time.Time, error) {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", time.Time{}, errRelayClosed
	}
	if r.phoneHash != "" {
		return "", time.Time{}, errPhoneAlreadyPaired
	}
	// A QR is a local, short-lived pairing intent. Keep only the newest intent
	// so refreshing the desktop card invalidates every photo or screenshot of
	// a previous QR immediately.
	r.pendingTokens = make(map[string]time.Time, 1)
	token, err := randomHex(16)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("generate direct pairing token: %w", err)
	}
	for tries := 0; tries < 3; tries++ {
		if _, exists := r.pendingTokens[token]; !exists {
			break
		}
		token, err = randomHex(16)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("generate direct pairing token: %w", err)
		}
	}
	if _, exists := r.pendingTokens[token]; exists {
		return "", time.Time{}, errors.New("could not allocate a unique direct pairing token")
	}
	expires := now.Add(pairTokenTTL)
	r.pendingTokens[token] = expires
	return token, expires, nil
}

// ──────────────────────────────────────────────────────────────
// GET /direct/ws — WebSocket upgrade
// ──────────────────────────────────────────────────────────────

func (r *Relay) handleWS(w http.ResponseWriter, req *http.Request) {
	r.mu.RLock()
	closed := r.closed
	r.mu.RUnlock()
	if closed {
		http.Error(w, "direct relay is closed", http.StatusServiceUnavailable)
		return
	}
	if req.TLS == nil {
		http.Error(w, "TLS is required for direct connections", http.StatusUpgradeRequired)
		return
	}
	select {
	case r.authSlots <- struct{}{}:
	default:
		http.Error(w, "too many unauthenticated connections", http.StatusServiceUnavailable)
		return
	}
	authSlotHeld := true
	defer func() {
		if authSlotHeld {
			<-r.authSlots
		}
	}()
	conn, err := upgrader.Upgrade(w, req, nil)
	if err != nil {
		r.o.Logger.Printf("direct: WS upgrade failed: %v", err)
		return
	}
	r.o.Logger.Printf("direct: new WS connection from %s", req.RemoteAddr)

	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()

	// First message must be auth within 10s.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	conn.SetReadLimit(maxAuthMessageBytes)
	_, raw, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})

	var authMsg struct {
		Type      string `json:"type"`
		Token     string `json:"token"`
		PhoneID   string `json:"phoneId"`
		PhoneHash string `json:"phoneHash"`
	}
	if json.Unmarshal(raw, &authMsg) != nil || authMsg.Type != "auth" {
		_ = sendJSON(conn, map[string]any{"type": "error", "error": "first message must be auth"})
		_ = conn.Close()
		return
	}
	if !validPhoneBinding(authMsg.PhoneID, authMsg.PhoneHash) {
		_ = sendJSON(conn, map[string]any{"type": "error", "error": "手机标识无效"})
		_ = conn.Close()
		return
	}
	<-r.authSlots
	authSlotHeld = false

	// Authenticate: either a valid one-time token (first pairing) or a matching phoneHash (reconnect).
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = sendJSON(conn, map[string]any{"type": "error", "error": "电脑直连服务已停止"})
		_ = conn.Close()
		return
	}
	if authMsg.Token != "" {
		if r.phoneHash != "" {
			r.mu.Unlock()
			_ = sendJSON(conn, map[string]any{"type": "error", "error": "电脑已绑定其他手机，请先在电脑上撤销原绑定"})
			_ = conn.Close()
			return
		}
		if !validPairingToken(authMsg.Token) {
			r.mu.Unlock()
			_ = sendJSON(conn, map[string]any{"type": "error", "error": "配对码格式无效，请重新扫描"})
			_ = conn.Close()
			return
		}
		// First pairing — validate and consume the token.
		expires, ok := r.pendingTokens[authMsg.Token]
		if !ok || !time.Now().Before(expires) {
			r.mu.Unlock()
			_ = sendJSON(conn, map[string]any{"type": "error", "error": "配对码已过期，请重新扫描"})
			_ = conn.Close()
			return
		}
		// Consume the one outstanding intent before persistence. A copied QR
		// cannot claim the device after this phone succeeds.
		r.pendingTokens = make(map[string]time.Time)
		if r.o.OnPaired != nil {
			if err := r.o.OnPaired(authMsg.PhoneID, authMsg.PhoneHash); err != nil {
				r.mu.Unlock()
				r.o.Logger.Printf("direct: failed to persist phone pairing: %v", err)
				_ = sendJSON(conn, map[string]any{"type": "error", "error": "配对信息无法安全保存，请重试"})
				_ = conn.Close()
				return
			}
		}
		r.phoneID = authMsg.PhoneID
		r.phoneHash = authMsg.PhoneHash
		r.bindingEpoch++
		r.o.Logger.Printf("direct: phone pairing stored")
	} else {
		// Reconnect — verify both stable phone identity and its binding secret.
		if r.phoneHash == "" || r.phoneID != authMsg.PhoneID || !constantTimeEqual(r.phoneHash, authMsg.PhoneHash) {
			r.mu.Unlock()
			_ = sendJSON(conn, map[string]any{"type": "error", "error": "手机未配对或标识不匹配"})
			_ = conn.Close()
			return
		}
	}

	// Publish the new session in the same critical section as authentication.
	// Registering it after closing the previous connection permitted two
	// simultaneous first connections to leave the older session as the winner.
	oldSession := r.conn
	if oldSession != nil {
		oldSession.cancel()
	}
	r.connectionGeneration++
	if r.connectionGeneration == 0 {
		r.connectionGeneration++
	}
	session := &phoneSession{conn: conn, ctx: ctx, phoneID: authMsg.PhoneID, generation: r.connectionGeneration, send: make(chan []byte, maxQueuedEvents), cancel: cancel}
	r.conn = session
	r.mu.Unlock()

	if oldSession != nil {
		oldSession.close()
		oldSession.executionMu.Lock()
		oldSession.executionMu.Unlock()
	}

	// Confirm auth.
	_ = session.writeJSON(map[string]any{
		"type":     "auth_ok",
		"deviceId": r.o.DeviceID,
		"name":     r.o.DeviceName,
		"os":       r.o.DeviceOS,
	})
	r.o.Logger.Printf("direct: phone authenticated")

	// Run the read and write loops.
	go session.writeLoop()
	r.readLoop(ctx, session)
}

// readLoop reads commands from the phone and pushes them onto r.commands.
func (r *Relay) readLoop(ctx context.Context, s *phoneSession) {
	defer func() {
		r.mu.Lock()
		if r.conn == s {
			r.conn = nil
		}
		r.mu.Unlock()
		s.close()
		if r.o.OnDisconnected != nil {
			r.o.OnDisconnected(EventTarget{relay: r, session: s})
		}
		r.o.Logger.Printf("direct: phone disconnected")
	}()

	s.conn.SetReadLimit(maxMessageBytes)
	s.conn.SetPongHandler(func(string) error {
		return s.conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		_ = s.conn.SetReadDeadline(time.Now().Add(pongWait))
		_, raw, err := s.conn.ReadMessage()
		if err != nil {
			return
		}
		var frame map[string]any
		if json.Unmarshal(raw, &frame) != nil {
			continue
		}
		// Commands are the only client-originated frame accepted after auth.
		// Do not forward arbitrary event/response envelopes to the executor.
		if messageType, ok := frame["type"].(string); !ok || messageType != "command" {
			continue
		}
		requestID, _ := frame["requestId"].(string)
		if !validRequestID(requestID) {
			continue
		}
		payload, nested := frame["command"].(map[string]any)
		if !nested {
			// Keep the relay package's small lifecycle test/embedding surface
			// backwards compatible. The production Bridge consumer rejects
			// command types outside its explicit dispatch allow-list.
			payload = make(map[string]any, len(frame)-2)
			for key, value := range frame {
				if key != "type" && key != "requestId" {
					payload[key] = value
				}
			}
		}
		if len(payload) == 0 {
			continue
		}
		// Coordinate admission with Close/Revoke/reconnect. A non-blocking send
		// while holding the read lock lets shutdown invalidate the session and
		// drain every queued payload without a later racing enqueue.
		r.mu.RLock()
		current := !r.closed && r.conn == s && r.connectionGeneration == s.generation
		if !current {
			r.mu.RUnlock()
			return
		}
		select {
		case r.commands <- Command{deviceID: r.o.DeviceID, phoneID: s.phoneID, requestID: requestID, sessionGeneration: s.generation, bindingEpoch: r.bindingEpoch, payload: payload, relay: r, session: s, state: &commandState{}}:
			r.mu.RUnlock()
		default:
			r.mu.RUnlock()
			r.o.Logger.Printf("direct: command queue full; closing stale phone session")
			s.close()
			return
		}
	}
}

// SendEvent pushes an event JSON to the session captured by target. A stale
// sender cannot silently retarget a reconnecting phone. Request responses must
// use SendResponse with the originating Command.
func (r *Relay) SendEvent(target EventTarget, eventType string, data any) bool {
	if target.relay != r || target.session == nil {
		return false
	}
	envelope := map[string]any{"type": "event", "event": eventType, "data": data}
	raw, err := json.Marshal(envelope)
	if err != nil || len(raw) > maxMessageBytes {
		r.o.Logger.Printf("direct: event could not be encoded within the frame limit")
		r.mu.RLock()
		current := r.conn == target.session
		r.mu.RUnlock()
		if current {
			target.session.close()
		}
		return false
	}
	r.mu.RLock()
	s := r.conn
	if s == nil || s != target.session {
		r.mu.RUnlock()
		return false
	}
	select {
	case s.send <- raw:
		r.mu.RUnlock()
		return true
	default:
		r.mu.RUnlock()
		r.o.Logger.Printf("direct: event queue full; closing stale phone session")
		s.close()
		return false
	}
}

// SendResponse correlates a command response with the exact session that
// originated it. Responses from disconnected or replaced sessions are dropped
// rather than delivered to a newly connected phone.
func (r *Relay) SendResponse(command Command, result any, commandErr error) bool {
	if command.relay != r || command.session == nil || command.state == nil || !command.state.finished.Load() || !validRequestID(command.requestID) || command.deviceID != r.o.DeviceID || command.phoneID == "" || command.sessionGeneration == 0 {
		return false
	}
	envelope := map[string]any{"type": "response", "requestId": command.requestID}
	if commandErr != nil {
		envelope["error"] = commandErr.Error()
	} else {
		envelope["result"] = result
	}
	raw, err := json.Marshal(envelope)
	if err != nil || len(raw) > maxMessageBytes {
		r.o.Logger.Printf("direct: response could not be encoded within the frame limit")
		r.mu.RLock()
		s := r.conn
		ownsSession := s != nil && s == command.session && s.phoneID == command.phoneID && s.generation == command.sessionGeneration
		r.mu.RUnlock()
		if ownsSession {
			s.close()
		}
		return false
	}
	// Keep the generation check and non-blocking enqueue in one read-locked
	// section. Reconnect/revoke takes the write lock, so a response cannot be
	// reported as accepted after its owning session has already been replaced.
	r.mu.RLock()
	s := r.conn
	if s == nil || s != command.session || s.phoneID != command.phoneID || s.generation != command.sessionGeneration {
		r.mu.RUnlock()
		return false
	}
	if !command.state.responseSent.CompareAndSwap(false, true) {
		r.mu.RUnlock()
		return false
	}
	select {
	case s.send <- raw:
		r.mu.RUnlock()
		return true
	default:
		r.mu.RUnlock()
		r.o.Logger.Printf("direct: response queue full; closing stale phone session")
		s.close()
		return false
	}
}

// ──────────────────────────────────────────────────────────────
// phoneSession — one active phone WebSocket connection
// ──────────────────────────────────────────────────────────────

type phoneSession struct {
	conn        *websocket.Conn
	ctx         context.Context
	phoneID     string
	generation  uint64
	send        chan []byte
	cancel      context.CancelFunc
	executionMu sync.RWMutex
	once        sync.Once
	writeMu     sync.Mutex
	closed      bool
}

func (s *phoneSession) close() {
	s.once.Do(func() {
		s.cancel()
		s.writeMu.Lock()
		s.closed = true
		_ = s.conn.Close()
		s.writeMu.Unlock()
	})
}

func (s *phoneSession) writeMessage(messageType int, raw []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return net.ErrClosed
	}
	_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return s.conn.WriteMessage(messageType, raw)
}

func (s *phoneSession) writeJSON(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.writeMessage(websocket.TextMessage, raw)
}

func (s *phoneSession) writeJSONAndClose(v any) {
	s.once.Do(func() {
		s.cancel()
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		if !s.closed {
			if raw, err := json.Marshal(v); err == nil {
				_ = s.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				_ = s.conn.WriteMessage(websocket.TextMessage, raw)
			}
			s.closed = true
			_ = s.conn.Close()
		}
	})
}

func (s *phoneSession) writeLoop() {
	ticker := time.NewTicker(pingInterval)
	defer func() {
		ticker.Stop()
		s.close()
	}()
	for {
		select {
		case raw, ok := <-s.send:
			if !ok {
				return
			}
			if err := s.writeMessage(websocket.TextMessage, raw); err != nil {
				return
			}
		case <-ticker.C:
			if err := s.writeMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// ──────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func sendJSON(conn *websocket.Conn, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, raw)
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func validPhoneBinding(phoneID, phoneHash string) bool {
	if len(phoneID) == 0 || len(phoneID) > 128 || strings.TrimSpace(phoneID) != phoneID {
		return false
	}
	for _, r := range phoneID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	if len(phoneHash) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(phoneHash)
	return err == nil && len(decoded) == sha256.Size && phoneHash == strings.ToLower(phoneHash)
}

func validRequestID(value string) bool {
	if len(value) == 0 || len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r == 0x7f {
			return false
		}
	}
	return true
}

func validPairingToken(value string) bool {
	if len(value) != 32 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// HashPhoneSecret returns the sha-256 hex of a phone's raw binding secret.
// Call this on the Bridge side to compute what to store as phoneHash.
func HashPhoneSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// GenerateQR creates a one-time QR payload for phone pairing and returns it
// as a map suitable for JSON serialization.  The caller (console API) can
// encode it into a QR image and display it in the Bridge UI.
func (r *Relay) GenerateQR() (map[string]any, error) {
	urls := r.lanURLs()
	if len(urls) == 0 || !validFingerprint(r.o.CertificateSHA256) {
		return nil, errors.New("secure direct transport is not configured")
	}
	token, expires, err := r.issuePairingToken()
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"mode":        "direct",
		"urls":        urls,
		"token":       token,
		"deviceId":    r.o.DeviceID,
		"fingerprint": r.o.CertificateSHA256,
		"expiresAt":   expires.UnixMilli(),
	}, nil
}
func (r *Relay) IsPaired() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.phoneHash != ""
}

// IsConnected reports whether a phone has an open WebSocket connection.
func (r *Relay) IsConnected() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conn != nil
}

// IsCurrentCommand is a point-in-time status check only. It must not be used as
// the sole authorization step before dispatch because revocation can race the
// caller immediately after this method returns. Use RunCurrentCommand instead.
func (r *Relay) IsCurrentCommand(command Command) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return !r.closed && command.relay == r && command.session != nil && command.deviceID == r.o.DeviceID && command.phoneID != "" && r.conn != nil &&
		r.conn == command.session && command.phoneID == r.conn.phoneID && command.sessionGeneration == r.conn.generation
}

// RunCurrentCommand admits execution only while the originating connection is
// current. Revoke and reconnect invalidate the session first, cancel its
// context, then wait for admitted dispatch callbacks to return before finishing
// the lifecycle transition. The callback must pass ctx through to the shared
// remote-dispatch policy boundary, honor cancellation, complete synchronously,
// and not retain the mutable payload. Temporary tasks may outlive the callback
// only with a BindingValidator captured here and continuous execution-policy
// validation; ordinary connection-bound work must complete synchronously.
func (r *Relay) RunCurrentCommand(command Command, run func(context.Context, map[string]any) (any, error)) (any, error, bool) {
	if run == nil {
		return nil, errors.New("direct command executor is unavailable"), false
	}
	r.mu.RLock()
	s := r.conn
	if r.closed || command.relay != r || command.session == nil || command.state == nil || s == nil || s != command.session || command.deviceID != r.o.DeviceID || command.phoneID == "" ||
		command.phoneID != s.phoneID || command.sessionGeneration != s.generation || command.bindingEpoch != r.bindingEpoch {
		r.mu.RUnlock()
		return nil, nil, false
	}
	s.executionMu.RLock()
	if !command.state.started.CompareAndSwap(false, true) {
		s.executionMu.RUnlock()
		r.mu.RUnlock()
		return nil, errCommandAlreadyStarted, false
	}
	r.activeCommands.Add(1)
	r.mu.RUnlock()
	defer s.executionMu.RUnlock()
	defer r.activeCommands.Done()
	defer command.state.finished.Store(true)
	if err := s.ctx.Err(); err != nil {
		return nil, err, true
	}
	result, err := run(s.ctx, command.payload)
	return result, err, true
}

// Revoke removes the phone pairing and disconnects any active session.
func (r *Relay) Revoke() error {
	r.mu.Lock()
	r.phoneHash = ""
	r.phoneID = ""
	r.bindingEpoch++
	r.connectionGeneration++
	r.pendingTokens = make(map[string]time.Time)
	s := r.conn
	r.conn = nil
	if s != nil {
		s.cancel()
	}
	var persistErr error
	if r.o.OnRevoked != nil {
		persistErr = r.o.OnRevoked()
	}
	r.mu.Unlock()
	if s != nil {
		s.writeJSONAndClose(map[string]any{"type": "revoked"})
		s.executionMu.Lock()
		s.executionMu.Unlock()
	}
	// A phone may have disconnected before the revoke request reached the
	// desktop. In that case r.conn is already nil, so the session-local wait
	// above cannot observe an admitted callback that is still unwinding. The
	// relay-wide admission counter covers that orphaned generation as well.
	// Revoke has already taken the write lock and invalidated the connection,
	// therefore no new command can be admitted while this drain runs.
	r.activeCommands.Wait()
	r.o.Logger.Printf("direct: pairing revoked")
	if persistErr != nil {
		return fmt.Errorf("pairing revoked in this process, but persistence failed: %w", persistErr)
	}
	return nil
}

// Close ends the runtime relay without revoking the persisted phone binding.
// It prevents new handshakes, invalidates queued commands, cancels the current
// session, and waits for an admitted command callback to return. It is safe to
// call repeatedly and never invokes OnRevoked.
func (r *Relay) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.bindingEpoch++
		r.connectionGeneration++
		r.pendingTokens = make(map[string]time.Time)
		s := r.conn
		r.conn = nil
		if s != nil {
			s.cancel()
		}
		r.mu.Unlock()

		if s != nil {
			s.close()
		}
		// The session may already have disconnected and cleared r.conn while
		// its command callback is still unwinding. Tracking executions on Relay,
		// rather than only through the current session pointer, drains those too.
		r.activeCommands.Wait()

		// readLoop checks `closed` under r.mu before every non-blocking enqueue,
		// so after publishing shutdown no command can be added behind this drain.
		for {
			select {
			case <-r.commands:
			default:
				return
			}
		}
	})
	return nil
}
