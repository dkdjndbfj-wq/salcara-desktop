package hubserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ──────────────────────────────────────────────────────────────
// GET /salcara-hub/v1/ping — discovery handshake
// ──────────────────────────────────────────────────────────────

func (h *Hub) handlePing(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"service":         "salcara-hub",
		"protocol":        "salcara-remote",
		"protocolVersion": protocolVersion,
		"version":         "self-hosted-1.0",
		"capabilities":    []string{"pair.qr.v1", "device.identity.v1", "session.remote.v1", "pair.revoke.v1", "station.guard.v1"},
		"authModes":       []string{"device-pairing"},
	}
	if h.o.StationMode != "" && h.o.StationMode != "disabled" {
		station := map[string]any{
			"version":               1,
			"allowOfficialAccounts": h.o.StationMode == "open",
		}
		if len(h.o.APIBases) > 0 {
			station["apiBases"] = h.o.APIBases
		}
		if h.o.StationMode == "open" && len(h.o.APIBases) == 0 {
			// Self-hosted in open mode with no APIBases: allow any account.
			// The official cloud Hub never sets this field.
			station["allowAllAccounts"] = true
		}
		resp["remoteAccess"] = map[string]any{
			"mode":      h.o.StationMode,
			"revision":  h.o.StationRevision,
			"station":   station,
			"checkedAt": time.Now().UnixMilli(),
		}
	}
	writeJSON(w, 200, resp)
}

// ──────────────────────────────────────────────────────────────
// POST /salcara-hub/v1/bridge/register
// Bridge announces itself on first connect and every reconnect.
// ──────────────────────────────────────────────────────────────

type registerRequest struct {
	DeviceID     string           `json:"deviceId"`
	DeviceSecret string           `json:"deviceSecret"`
	Name         string           `json:"name"`
	OS           string           `json:"os"`
	Version      string           `json:"version"`
	Tools        []map[string]any `json:"tools"`
	Projects     []map[string]any `json:"projects"`
	Capabilities []string         `json:"capabilities"`
}

func (h *Hub) handleBridgeRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}
	if req.DeviceID == "" || req.DeviceSecret == "" {
		writeErr(w, 400, "缺少设备标识或密钥")
		return
	}

	h.mu.Lock()
	d, exists := h.devices[req.DeviceID]
	if !exists {
		// New device — accept any secret on first registration.
		d = &device{
			id:            req.DeviceID,
			secret:        req.DeviceSecret,
			bridgeCh:      make(chan sseMsg, 256),
			pendingTokens: make(map[string]time.Time),
		}
		h.devices[req.DeviceID] = d
	} else {
		// Reconnect — verify the secret matches.
		if !constantTimeEqual(d.secret, req.DeviceSecret) {
			h.mu.Unlock()
			writeErr(w, 403, "设备密钥不匹配")
			return
		}
	}

	d.mu.Lock()
	d.name = req.Name
	d.os = req.OS
	d.version = req.Version
	d.tools = req.Tools
	d.projects = req.Projects
	d.capabilities = req.Capabilities
	d.lastSeen = time.Now()
	d.mu.Unlock()
	h.mu.Unlock()

	h.o.Logger.Printf("bridge registered: %s (%s %s)", req.DeviceID, req.Name, req.OS)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ──────────────────────────────────────────────────────────────
// GET /salcara-hub/v1/bridge/stream — SSE command stream
// The Bridge holds this open; the Hub pushes commands from phones.
// ──────────────────────────────────────────────────────────────

func (h *Hub) handleBridgeStream(w http.ResponseWriter, r *http.Request) {
	deviceID := r.Header.Get("X-Device-Id")
	secret := r.Header.Get("X-Device-Secret")
	if deviceID == "" || secret == "" {
		writeErr(w, 401, "需要设备标识和密钥")
		return
	}

	h.mu.RLock()
	d, ok := h.devices[deviceID]
	h.mu.RUnlock()
	if !ok || !constantTimeEqual(d.secret, secret) {
		writeErr(w, 403, "设备未注册或密钥不匹配")
		return
	}

	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "不支持流式连接")
		return
	}

	// Mark the device as online while this stream is open.
	d.mu.Lock()
	d.lastSeen = time.Now()
	d.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	// Send a hello so the Bridge knows the stream is live.
	writeSSE(w, "hello", []byte(`{}`))
	fl.Flush()

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-d.bridgeCh:
			writeSSE(w, msg.event, msg.data)
			fl.Flush()
		case <-ticker.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// ──────────────────────────────────────────────────────────────
// POST /salcara-hub/v1/bridge/event — Bridge pushes agent events
// ──────────────────────────────────────────────────────────────

type eventRequest struct {
	DeviceID string           `json:"deviceId"`
	Secret   string           `json:"secret"`
	Events   []map[string]any `json:"events"`
}

func (h *Hub) handleBridgeEvent(w http.ResponseWriter, r *http.Request) {
	var req eventRequest
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}

	h.mu.RLock()
	d, ok := h.devices[req.DeviceID]
	h.mu.RUnlock()
	if !ok || !constantTimeEqual(d.secret, req.Secret) {
		writeErr(w, 403, "设备未注册或密钥不匹配")
		return
	}

	// Forward each event to every connected phone for this device.
	h.mu.RLock()
	phoneConns := make([]*phoneConn, 0, len(h.phones))
	for _, pc := range h.phones {
		if pc.deviceID == req.DeviceID {
			phoneConns = append(phoneConns, pc)
		}
	}
	h.mu.RUnlock()

	for _, ev := range req.Events {
		data, _ := json.Marshal(ev)
		for _, pc := range phoneConns {
			select {
			case pc.ch <- sseMsg{event: "event", data: data}:
			default: // phone is slow; it will resync on reconnect
			}
		}
	}

	writeJSON(w, 200, map[string]any{"ok": true})
}

// ──────────────────────────────────────────────────────────────
// POST /salcara-hub/v1/pair — QR pairing
// The Bridge generates a QR containing a one-time token.  The phone
// scans it and POSTs the token here.  The Hub records the binding.
// ──────────────────────────────────────────────────────────────

type pairRequest struct {
	DeviceID string `json:"deviceId"`
	Secret   string `json:"secret"`
	// Token is the one-time code from the QR.  Required on first pairing.
	Token string `json:"token"`
	// PhoneHash is sha-256 hex of the phone's persistent binding id.
	PhoneHash string `json:"phoneHash"`
	// BindingID is the phone-side identifier for this binding.
	BindingID string `json:"bindingId"`
}

type pairResponse struct {
	Paired           bool   `json:"paired"`
	DeviceID         string `json:"deviceId"`
	Revision         int64  `json:"revision"`
	PendingExpiresAt int64  `json:"pendingExpiresAt"`
}

func (h *Hub) handlePair(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}

	h.mu.RLock()
	d, ok := h.devices[req.DeviceID]
	h.mu.RUnlock()
	if !ok || !constantTimeEqual(d.secret, req.Secret) {
		writeErr(w, 403, "设备未注册或密钥不匹配")
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Validate the one-time token from the QR.
	if req.Token != "" {
		expires, exists := d.pendingTokens[req.Token]
		if !exists || time.Now().After(expires) {
			writeErr(w, 410, "配对码已过期，请重新扫描")
			return
		}
		delete(d.pendingTokens, req.Token) // one-time use
	}

	if req.PhoneHash == "" || req.BindingID == "" {
		writeErr(w, 400, "缺少手机标识")
		return
	}

	d.phoneHash = req.PhoneHash
	d.bindingID = req.BindingID
	d.pairRevision++
	d.pairExpires = time.Time{} // paired

	h.o.Logger.Printf("paired: device=%s phone=%s binding=%s", req.DeviceID, req.PhoneHash[:8]+"…", req.BindingID)

	writeJSON(w, 200, pairResponse{
		Paired:           true,
		DeviceID:         req.DeviceID,
		Revision:         d.pairRevision,
		PendingExpiresAt: 0,
	})
}

// ──────────────────────────────────────────────────────────────
// POST /salcara-hub/v1/pair/revoke
// ──────────────────────────────────────────────────────────────

type revokeRequest struct {
	DeviceID string `json:"deviceId"`
	Secret   string `json:"secret"`
}

func (h *Hub) handlePairRevoke(w http.ResponseWriter, r *http.Request) {
	var req revokeRequest
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}

	h.mu.RLock()
	d, ok := h.devices[req.DeviceID]
	h.mu.RUnlock()
	if !ok || !constantTimeEqual(d.secret, req.Secret) {
		writeErr(w, 403, "设备未注册或密钥不匹配")
		return
	}

	d.mu.Lock()
	d.phoneHash = ""
	d.bindingID = ""
	d.pairRevision++
	d.pairExpires = time.Time{}
	d.mu.Unlock()

	// Push a pair-revoked snapshot to the Bridge stream.
	revokedData, _ := json.Marshal(pairResponse{
		Paired:   false,
		DeviceID: req.DeviceID,
		Revision: d.pairRevision,
	})
	select {
	case d.bridgeCh <- sseMsg{event: "pair", data: revokedData}:
	default:
	}

	writeJSON(w, 200, map[string]any{"ok": true})
}

// ──────────────────────────────────────────────────────────────
// GET /salcara-hub/v1/phone/stream — SSE event stream for phones
// ──────────────────────────────────────────────────────────────

func (h *Hub) handlePhoneStream(w http.ResponseWriter, r *http.Request) {
	deviceID := r.URL.Query().Get("deviceId")
	phoneHash := r.URL.Query().Get("phoneHash")
	bindingID := r.URL.Query().Get("bindingId")
	if deviceID == "" || phoneHash == "" || bindingID == "" {
		writeErr(w, 400, "缺少手机标识参数")
		return
	}

	h.mu.RLock()
	d, ok := h.devices[deviceID]
	h.mu.RUnlock()
	if !ok {
		writeErr(w, 404, "设备不在线")
		return
	}

	// Verify the phone is paired with this device.
	d.mu.Lock()
	paired := d.phoneHash == phoneHash && d.bindingID == bindingID
	d.mu.Unlock()
	if !paired {
		writeErr(w, 403, "手机未与这台设备配对")
		return
	}

	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "不支持流式连接")
		return
	}

	connID := randomHex(16)
	pc := &phoneConn{deviceID: deviceID, ch: make(chan sseMsg, eventQueueCap)}

	h.mu.Lock()
	h.phones[connID] = pc
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.phones, connID)
		h.mu.Unlock()
	}()

	// Send an initial device status snapshot.
	d.mu.Lock()
	snapshot := map[string]any{
		"deviceId":     d.id,
		"name":         d.name,
		"os":           d.os,
		"version":      d.version,
		"tools":        d.tools,
		"projects":     d.projects,
		"capabilities": d.capabilities,
		"online":       true,
		"paired":       true,
	}
	d.mu.Unlock()
	snapData, _ := json.Marshal(snapshot)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	writeSSE(w, "status", snapData)
	fl.Flush()

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-pc.ch:
			writeSSE(w, msg.event, msg.data)
			fl.Flush()
		case <-ticker.C:
			_, _ = io.WriteString(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// ──────────────────────────────────────────────────────────────
// POST /salcara-hub/v1/phone/command — phone submits a command
// ──────────────────────────────────────────────────────────────

type commandRequest struct {
	DeviceID  string         `json:"deviceId"`
	PhoneHash string         `json:"phoneHash"`
	BindingID string         `json:"bindingId"`
	Command   map[string]any `json:"command"`
}

func (h *Hub) handlePhoneCommand(w http.ResponseWriter, r *http.Request) {
	var req commandRequest
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}

	h.mu.RLock()
	d, ok := h.devices[req.DeviceID]
	h.mu.RUnlock()
	if !ok {
		writeErr(w, 404, "设备不在线")
		return
	}

	// Verify pairing.
	d.mu.Lock()
	paired := d.phoneHash == req.PhoneHash && d.bindingID == req.BindingID
	d.mu.Unlock()
	if !paired {
		writeErr(w, 403, "手机未与这台设备配对")
		return
	}

	// Optional policy filter.
	if h.CommandFilter != nil {
		if err := h.CommandFilter(req.DeviceID, req.Command); err != nil {
			writeErr(w, 403, fmt.Sprintf("命令被拒绝：%s", err.Error()))
			return
		}
	}

	// Stamp the command with a source envelope so the Bridge's source guard
	// accepts it.  In self-hosted "open" mode, the envelope is minimal.
	if req.Command != nil {
		if _, hasSource := req.Command["source"]; !hasSource {
			req.Command["source"] = map[string]any{
				"mode":           h.o.StationMode,
				"policyRevision": h.o.StationRevision,
				"station": map[string]any{
					"version":               1,
					"allowOfficialAccounts": h.o.StationMode == "open",
					"apiBases":              h.o.APIBases,
					"allowAllAccounts":      h.o.StationMode == "open" && len(h.o.APIBases) == 0,
				},
				"agent":            "",
				"controlSurface":   "cli",
				"sourceType":       "",
				"credentialHandle": "",
				"issuer":           "self-hosted",
				"configRevision":   "",
				"origin":           "",
				"apiBase":          "",
				"model":            "",
			}
		}
	}

	data, _ := json.Marshal(req.Command)

	// Push to the Bridge's command channel.
	select {
	case d.bridgeCh <- sseMsg{event: "command", data: data}:
		writeJSON(w, 200, map[string]any{"ok": true, "queued": true})
	default:
		writeErr(w, 503, "设备命令队列已满，请稍后重试")
	}
}

// ──────────────────────────────────────────────────────────────
// QR token generation — Bridge calls this to create a one-time code.
// ──────────────────────────────────────────────────────────────

type qrTokenRequest struct {
	DeviceID string `json:"deviceId"`
	Secret   string `json:"secret"`
}

type qrTokenResponse struct {
	Token     string `json:"token"`
	ExpiresAt int64  `json:"expiresAt"`
}

// HandleQrToken is an extra endpoint for Bridge-side QR generation.
// It lives at POST /salcara-hub/v1/qr.
func (h *Hub) HandleQrToken(w http.ResponseWriter, r *http.Request) {
	var req qrTokenRequest
	if err := readBody(r, &req); err != nil {
		writeErr(w, 400, "请求格式有误")
		return
	}

	h.mu.RLock()
	d, ok := h.devices[req.DeviceID]
	h.mu.RUnlock()
	if !ok || !constantTimeEqual(d.secret, req.Secret) {
		writeErr(w, 403, "设备未注册或密钥不匹配")
		return
	}

	token := randomHex(16)
	expires := time.Now().Add(pairTokenTTL)

	d.mu.Lock()
	if d.pendingTokens == nil {
		d.pendingTokens = make(map[string]time.Time)
	}
	// Clean expired tokens.
	for t, exp := range d.pendingTokens {
		if time.Now().After(exp) {
			delete(d.pendingTokens, t)
		}
	}
	d.pendingTokens[token] = expires
	d.pairExpires = expires
	d.mu.Unlock()

	writeJSON(w, 200, qrTokenResponse{
		Token:     token,
		ExpiresAt: expires.UnixMilli(),
	})
}
