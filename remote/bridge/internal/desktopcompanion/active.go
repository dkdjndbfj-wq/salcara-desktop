package desktopcompanion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"salcara/bridge/internal/protocol"
)

// NativeConnection is a short-lived, explicitly approved experimental desktop
// lease. It contains no local HTTP token, caller identity, pipe or file path.
type NativeConnection struct {
	Active            bool               `json:"active"`
	ExpiresAt         int64              `json:"expiresAt"`
	SessionKeys       []string           `json:"sessionKeys"`
	Capabilities      NativeCapabilities `json:"capabilities"`
	ApprovalTransport string             `json:"approvalTransport,omitempty"`
}

// Desktop transport is deliberately smaller than the app-server/CLI protocol.
// Missing metadata (older companion) means unsupported, never guessed support.
type NativeCapabilities struct {
	List          bool `json:"list"`
	Read          bool `json:"read"`
	Send          bool `json:"send"`
	Interrupt     bool `json:"interrupt"`
	Approval      bool `json:"approval"`
	Attachments   bool `json:"attachments"`
	ModelOverride bool `json:"modelOverride"`
}

// One explicit approval in Codex Desktop lasts until revoked, Codex restarts,
// or 30 days pass, for up to 200 threads.
const (
	MaxLeaseMillis   = 30 * 24 * 60 * 60 * 1000
	MaxLeaseSessions = 200
)

// ValidateNativeConnection keeps injected adapters and future implementations
// from accidentally leaking arbitrary scope strings in public metadata.
func ValidateNativeConnection(st NativeConnection, now int64) bool {
	if !st.Active || st.ExpiresAt <= now || st.ExpiresAt-now > MaxLeaseMillis || len(st.SessionKeys) == 0 || len(st.SessionKeys) > MaxLeaseSessions {
		return false
	}
	if st.Capabilities.Interrupt || st.Capabilities.Attachments || st.Capabilities.ModelOverride || !validApprovalTransport(st.Capabilities.Approval, st.ApprovalTransport) {
		return false
	}
	seen := map[string]bool{}
	for _, key := range st.SessionKeys {
		if _, ok := desktopSessionID(key); !ok || seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

var ErrActivationRequired = errors.New("desktop_activation_required: 请在 Codex Desktop 中明确授权 Salcara 桌面连接；不会改用 CLI")
var ErrDesktopUnavailable = errors.New("desktop_connection_unavailable: 桌面连接已断开或原生工具拒绝请求；不会改用 CLI")
var ErrDesktopScope = errors.New("desktop_session_not_authorized: 会话不在当前桌面授权范围内")
var ErrDesktopDeliveryUncertain = errors.New("command_delivery_uncertain: 桌面消息可能已被原会话接收，暂时无法确认；请恢复连接后查看原会话，不要作为新任务重复发送")
var ErrDesktopCapability = errors.New("desktop_capability_unavailable: 当前桌面接口不支持此操作；请在电脑中处理，不会改用 CLI")
var ErrDesktopRequestUnsent = errors.New("desktop_request_unsent: 桌面请求未派发，请检查连接后重试；不会改用 CLI")
var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
var gatewayToken = regexp.MustCompile(`^[0-9a-f]{64}$`)

func ValidNativeOperationID(id string) bool { return canonicalUUID.MatchString(id) }

const NativeApprovalTransport = "codex-hook-v1"

func validApprovalTransport(enabled bool, transport string) bool {
	return enabled && transport == NativeApprovalTransport || !enabled && transport == ""
}

type activeDescriptor struct {
	Version            int      `json:"version"`
	Port               int      `json:"port"`
	Token              string   `json:"token"`
	ExpiresAt          int64    `json:"expiresAt"`
	ControllerThreadID string   `json:"controllerThreadId"`
	SessionKeys        []string `json:"sessionKeys"`
}

type gatewayReply struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  string          `json:"error"`
}

// Only authenticated fixed pre-dispatch errors prove the desktop was not called.
// Host/native failures and lost responses remain uncertain.
type gatewayUnsentError struct{ code string }

func (gatewayUnsentError) Error() string { return ErrDesktopRequestUnsent.Error() }

type nativeResult struct {
	Success      bool `json:"success"`
	ContentItems []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"contentItems"`
}

// Active metadata is read only from this installer's exact private payload.
// Neither arbitrary URLs nor ports supplied by the phone can reach this client.
func (s *Service) activeDescriptor(ctx context.Context) (activeDescriptor, error) {
	var d activeDescriptor
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, err := s.planUninstall(ctx)
	if err != nil || !plan.installed {
		return d, ErrActivationRequired
	}
	var own ownership
	if json.Unmarshal(plan.state.data, &own) != nil || !hasPermissionHook(own.Version) {
		return d, ErrActivationRequired
	}
	p := plan.p
	path := filepath.Join(p.payload, "active-desktop.json")
	for _, privatePath := range []string{p.root, p.payload, p.state, path} {
		if rejectLinks(privatePath) != nil || checkPrivatePermissions(privatePath) != nil {
			return d, ErrActivationRequired
		}
	}
	b, err := readRegular(path, 16384)
	if err != nil || strictJSON(b, &d) != nil || !validDescriptor(d, time.Now().UnixMilli()) {
		return activeDescriptor{}, ErrActivationRequired
	}
	return d, nil
}

func validDescriptor(d activeDescriptor, now int64) bool {
	if d.Version != 1 || d.Port < 1 || d.Port > 65535 || !gatewayToken.MatchString(d.Token) || !canonicalUUID.MatchString(d.ControllerThreadID) || d.ExpiresAt <= now || d.ExpiresAt-now > MaxLeaseMillis || len(d.SessionKeys) == 0 || len(d.SessionKeys) > MaxLeaseSessions {
		return false
	}
	seen := map[string]bool{}
	for _, key := range d.SessionKeys {
		id, ok := desktopSessionID(key)
		if !ok || id == d.ControllerThreadID || seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

func desktopSessionID(key string) (string, bool) {
	id, ok := strings.CutPrefix(key, "codex:")
	return id, ok && canonicalUUID.MatchString(id)
}

func strictJSON(b []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}

func withinScope(d activeDescriptor, key string) bool {
	for _, candidate := range d.SessionKeys {
		if candidate == key {
			return true
		}
	}
	return false
}

// gatewayRPC never follows redirects, uses proxies, retries a send or surfaces
// untrusted error strings (which may contain credentials or local paths).
func gatewayRPC(ctx context.Context, d activeDescriptor, body map[string]any, timeout time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(d.Port)+"/rpc", bytes.NewReader(data))
	if err != nil {
		return nil, ErrDesktopUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+d.Token)
	req.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return nil, ErrDesktopUnavailable
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(b) > 4<<20 || time.Now().UnixMilli() >= d.ExpiresAt {
		return nil, ErrDesktopUnavailable
	}
	var reply gatewayReply
	if strictJSON(b, &reply) != nil {
		return nil, ErrDesktopUnavailable
	}
	if !reply.OK && (resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusTooManyRequests) {
		switch reply.Error {
		case "REQUEST_INVALID", "REQUEST_TOO_LARGE", "CONTROL_BUSY", "SESSION_NOT_ALLOWED", "CAPABILITY_UNAVAILABLE", "OPERATION_STORAGE_UNAVAILABLE", "APPROVAL_EXPIRED":
			return nil, gatewayUnsentError{code: reply.Error}
		}
	}
	if resp.StatusCode != http.StatusOK || !reply.OK || len(reply.Result) == 0 {
		return nil, ErrDesktopUnavailable
	}
	return reply.Result, nil
}

func verifyLive(ctx context.Context, d activeDescriptor) (NativeConnection, error) {
	raw, err := gatewayRPC(ctx, d, map[string]any{"type": "status"}, 10*time.Second)
	if err != nil {
		return NativeConnection{}, err
	}
	var status struct {
		DesktopControl     bool               `json:"desktopControl"`
		RemoteSend         bool               `json:"remoteSend"`
		ExpiresAt          int64              `json:"expiresAt"`
		ControllerThreadID string             `json:"controllerThreadId"`
		SessionKeys        []string           `json:"sessionKeys"`
		Capabilities       NativeCapabilities `json:"capabilities"`
		ApprovalTransport  string             `json:"approvalTransport"`
	}
	if strictJSON(raw, &status) != nil || !status.DesktopControl || !status.RemoteSend || status.ExpiresAt != d.ExpiresAt || status.ControllerThreadID != d.ControllerThreadID || len(status.SessionKeys) != len(d.SessionKeys) {
		return NativeConnection{}, ErrDesktopUnavailable
	}
	// Approval is a separately verified hook transport, not an inferred native
	// catalogue operation. Other unavailable operations remain closed.
	if status.Capabilities.Interrupt || status.Capabilities.Attachments || status.Capabilities.ModelOverride || !validApprovalTransport(status.Capabilities.Approval, status.ApprovalTransport) {
		return NativeConnection{}, ErrDesktopUnavailable
	}
	seen := map[string]bool{}
	for _, key := range status.SessionKeys {
		if !withinScope(d, key) || seen[key] {
			return NativeConnection{}, ErrDesktopUnavailable
		}
		seen[key] = true
	}
	return NativeConnection{Active: true, ExpiresAt: d.ExpiresAt, SessionKeys: append([]string{}, d.SessionKeys...), Capabilities: status.Capabilities, ApprovalTransport: status.ApprovalTransport}, nil
}

func (s *Service) NativeStatus(ctx context.Context) (NativeConnection, error) {
	d, err := s.activeDescriptor(ctx)
	if err != nil {
		return NativeConnection{}, err
	}
	return verifyLive(ctx, d)
}

func nativeJSON(raw json.RawMessage, target any) error {
	var result nativeResult
	if json.Unmarshal(raw, &result) != nil || !result.Success {
		return ErrDesktopUnavailable
	}
	for _, item := range result.ContentItems {
		if item.Type == "inputText" {
			data := []byte(item.Text)
			var wrapper map[string]json.RawMessage
			if json.Unmarshal(data, &wrapper) == nil && wrapper["data"] != nil {
				data = wrapper["data"]
			}
			if json.Unmarshal(data, target) == nil {
				return nil
			}
		}
	}
	return ErrDesktopUnavailable
}

type nativeThread struct {
	ID          string          `json:"id"`
	Kind        string          `json:"kind"`
	HostID      string          `json:"hostId"`
	Title       string          `json:"title"`
	Cwd         string          `json:"cwd"`
	UpdatedAt   int64           `json:"updatedAt"`
	Status      json.RawMessage `json:"status"`
	PinnedIndex int             `json:"pinnedIndex"`
}

func sessionFromNative(t nativeThread, expiry int64) protocol.SessionInfo {
	status := nativeStatus(t.Status)
	ts := t.UpdatedAt
	// Current native timestamps are epoch milliseconds. Tolerate the older
	// app-server seconds representation without exposing a fresh fake timestamp.
	if ts > 0 && ts < 100000000000 {
		ts *= 1000
	}
	return protocol.SessionInfo{SessionKey: "codex:" + t.ID, Tool: "codex", Client: "Codex App", ControlSurface: "desktop", ControlExpiresAt: expiry, Title: bounded(t.Title, 2000), Cwd: bounded(t.Cwd, 4096), UpdatedAt: ts, Status: status, Controllable: true}
}

func nativeStatus(raw json.RawMessage) string {
	var status string
	if json.Unmarshal(raw, &status) != nil {
		var obj struct {
			Type        string   `json:"type"`
			ActiveFlags []string `json:"activeFlags"`
		}
		_ = json.Unmarshal(raw, &obj)
		status = obj.Type
		for _, flag := range obj.ActiveFlags {
			if flag == "waitingOnApproval" {
				return "waiting_approval"
			}
		}
	}
	switch status {
	case "active", "inProgress", "running":
		return "running"
	case "systemError", "failed":
		return "failed"
	case "waiting_approval":
		return "waiting_approval"
	default:
		return "idle"
	}
}

func (s *Service) NativeList(ctx context.Context) ([]protocol.SessionInfo, error) {
	return s.nativeList(ctx, false)
}

// A lease authorizes access; it is not proof that an AI turn is still running.
// Only explicit idle states for every authorized target allow a station move.
func (s *Service) NativeStationIdle(ctx context.Context) error {
	_, err := s.nativeList(ctx, true)
	return err
}

func (s *Service) nativeList(ctx context.Context, requireIdle bool) ([]protocol.SessionInfo, error) {
	d, err := s.activeDescriptor(ctx)
	if err != nil {
		return nil, err
	}
	if st, e := verifyLive(ctx, d); e != nil {
		return nil, e
	} else if !st.Capabilities.List {
		return nil, ErrDesktopCapability
	}
	raw, err := gatewayRPC(ctx, d, map[string]any{"type": "list"}, 10*time.Second)
	if err != nil {
		return nil, err
	}
	var list struct {
		SchemaVersion int            `json:"schemaVersion"`
		PinnedThreads []nativeThread `json:"pinnedThreads"`
		Threads       []nativeThread `json:"threads"`
	}
	if err = nativeJSON(raw, &list); err != nil {
		return nil, err
	}
	if list.SchemaVersion != 4 || list.PinnedThreads == nil || list.Threads == nil {
		return nil, ErrDesktopUnavailable
	}
	sessions := []protocol.SessionInfo{}
	seen := map[string]bool{}
	for index, t := range append(list.PinnedThreads, list.Threads...) {
		key := "codex:" + t.ID
		if t.Kind != "codex" || t.HostID != "local" || !withinScope(d, key) || seen[key] {
			continue
		}
		seen[key] = true
		if requireIdle && !nativeThreadIdle(t.Status) {
			return nil, errors.New("Codex 桌面仍有任务或状态未确认，请稍后切换中转站")
		}
		info := sessionFromNative(t, d.ExpiresAt)
		info.SidebarIndex = len(sessions) + 1
		if index < len(list.PinnedThreads) {
			info.PinnedIndex = t.PinnedIndex
			if info.PinnedIndex < 1 || info.PinnedIndex > 10000 {
				info.PinnedIndex = index + 1
			}
		}
		sessions = append(sessions, info)
	}
	if requireIdle && len(seen) != len(d.SessionKeys) {
		return nil, errors.New("无法确认全部桌面任务状态，请刷新后重试")
	}
	// Preserve actual native pinned order, followed by sidebar recency order.
	return sessions, nil
}

func nativeThreadIdle(raw json.RawMessage) bool {
	var status string
	if json.Unmarshal(raw, &status) != nil {
		var object struct {
			Type        string   `json:"type"`
			ActiveFlags []string `json:"activeFlags"`
		}
		if json.Unmarshal(raw, &object) != nil || len(object.ActiveFlags) != 0 {
			return false
		}
		status = object.Type
	}
	return status == "idle" || status == "systemError" || status == "failed"
}

func (s *Service) NativeOpen(ctx context.Context, key string) (protocol.SessionInfo, []protocol.Event, error) {
	info, events, _, err := s.NativeOpenPage(ctx, key, "")
	return info, events, err
}

func (s *Service) NativeOpenPage(ctx context.Context, key, cursor string) (protocol.SessionInfo, []protocol.Event, string, error) {
	return s.NativeOpenPageLimited(ctx, key, cursor, 10)
}

// NativeOpenPageLimited keeps the first phone render small. The optional
// limit is bounded by the companion gateway; older callers retain the 10-turn
// default through NativeOpenPage above.
func (s *Service) NativeOpenPageLimited(ctx context.Context, key, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	return s.nativeOpenPage(ctx, key, cursor, limit, false)
}

func (s *Service) nativeOpenPage(ctx context.Context, key, cursor string, limit int, messages bool) (protocol.SessionInfo, []protocol.Event, string, error) {
	if limit < 1 || limit > 10 {
		return protocol.SessionInfo{}, nil, "", ErrDesktopRequestUnsent
	}
	if len(cursor) > 4096 || strings.ContainsAny(cursor, "\x00\r\n") {
		return protocol.SessionInfo{}, nil, "", ErrDesktopRequestUnsent
	}
	if _, ok := desktopSessionID(key); !ok {
		return protocol.SessionInfo{}, nil, "", ErrDesktopScope
	}
	d, err := s.activeDescriptor(ctx)
	if err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	if !withinScope(d, key) {
		return protocol.SessionInfo{}, nil, "", ErrDesktopScope
	}
	if st, e := verifyLive(ctx, d); e != nil {
		return protocol.SessionInfo{}, nil, "", e
	} else if !st.Capabilities.Read {
		return protocol.SessionInfo{}, nil, "", ErrDesktopCapability
	}
	command := map[string]any{"type": "read", "sessionKey": key}
	if cursor != "" {
		command["cursor"] = cursor
	}
	command["limit"] = limit
	raw, err := gatewayRPC(ctx, d, command, 45*time.Second)
	effectiveLimit := limit
	var unsent gatewayUnsentError
	if errors.As(err, &unsent) && unsent.code == "REQUEST_INVALID" {
		// The already installed 0.4.0 gateway did not accept a limit field.
		// Only its authenticated pre-dispatch rejection permits this read-only
		// compatibility retry. Never retry a send or an ambiguous host failure.
		delete(command, "limit")
		raw, err = gatewayRPC(ctx, d, command, 45*time.Second)
		effectiveLimit = 10
	}
	if err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	var read nativeRead
	if err = nativeJSON(raw, &read); err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	if read.SchemaVersion != 1 || read.Turns == nil || len(read.Turns) > effectiveLimit || len(read.Page.NextCursor) > 4096 || strings.ContainsAny(read.Page.NextCursor, "\x00\r\n") || read.Page.HasMore && read.Page.NextCursor == "" || cursor != "" && read.Page.NextCursor == cursor {
		return protocol.SessionInfo{}, nil, "", ErrDesktopUnavailable
	}
	if read.Thread.Kind != "codex" || read.Thread.HostID != "local" || "codex:"+read.Thread.ID != key {
		return protocol.SessionInfo{}, nil, "", ErrDesktopScope
	}
	info := sessionFromNative(read.Thread, d.ExpiresAt)
	events := nativeEvents(key, read, d.SessionKeys)
	// A page budget failure is explicit; never label silently discarded items
	// as a complete native history page.
	if !messages && len(events) > 400 || len(events) > 16000 {
		return protocol.SessionInfo{}, nil, "", ErrDesktopUnavailable
	}
	hooks, err := nativeApprovalEvents(key, read.ApprovalEvents, time.Now().UnixMilli())
	if err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	for _, event := range hooks {
		if event.Type == "approval.request" {
			info.Status = "waiting_approval"
		}
	}
	if cursor != "" {
		hooks = nil
	}
	next := ""
	if read.Page.HasMore {
		next = read.Page.NextCursor
	}
	return info, append(events, hooks...), next, nil
}

// NativeRespondApproval targets only an existing synchronous PermissionRequest
// hook. It cannot change permissions for future tools or respond to CLI prompts.
func (s *Service) NativeRespondApproval(ctx context.Context, key, approvalID, decision, message, operationID string) error {
	if _, ok := desktopSessionID(key); !ok {
		return ErrDesktopScope
	}
	if !canonicalUUID.MatchString(approvalID) || !canonicalUUID.MatchString(operationID) || (decision != "allow" && decision != "deny") || len(message) > 8000 {
		return ErrDesktopRequestUnsent
	}
	d, err := s.activeDescriptor(ctx)
	if err != nil {
		return err
	}
	if !withinScope(d, key) {
		return ErrDesktopScope
	}
	st, err := verifyLive(ctx, d)
	if err != nil {
		return err
	}
	if !st.Capabilities.Approval || st.ApprovalTransport != NativeApprovalTransport {
		return ErrDesktopCapability
	}
	raw, err := gatewayRPC(ctx, d, map[string]any{"type": "approval.respond", "sessionKey": key, "approvalId": approvalID, "decision": decision, "message": message, "operationId": operationID}, 15*time.Second)
	if err != nil {
		var unsent gatewayUnsentError
		if errors.As(err, &unsent) {
			return ErrDesktopRequestUnsent
		}
		return ErrDesktopDeliveryUncertain
	}
	var reply struct {
		Accepted   bool   `json:"accepted"`
		SessionKey string `json:"sessionKey"`
		ApprovalID string `json:"approvalId"`
	}
	if strictJSON(raw, &reply) != nil || !reply.Accepted || reply.SessionKey != key || reply.ApprovalID != approvalID {
		return ErrDesktopDeliveryUncertain
	}
	return nil
}

func (s *Service) NativeSend(ctx context.Context, key, text, operationID string) error {
	if _, ok := desktopSessionID(key); !ok {
		return ErrDesktopScope
	}
	if !canonicalUUID.MatchString(operationID) || strings.TrimSpace(text) == "" || len(text) > 128000 {
		return errors.New("desktop_invalid_send: 无效的桌面消息或操作标识")
	}
	d, err := s.activeDescriptor(ctx)
	if err != nil {
		return err
	}
	if !withinScope(d, key) {
		return ErrDesktopScope
	}
	if st, e := verifyLive(ctx, d); e != nil {
		return e
	} else if !st.Capabilities.Send {
		return ErrDesktopCapability
	}
	raw, err := gatewayRPC(ctx, d, map[string]any{"type": "send", "sessionKey": key, "text": text, "operationId": operationID}, 45*time.Second)
	if err != nil {
		var unsent gatewayUnsentError
		if errors.As(err, &unsent) {
			return ErrDesktopRequestUnsent
		}
		// Once dispatch starts, a lost response must never be presented as proof
		// that no task was created. The operation ID is retained by the gateway.
		return ErrDesktopDeliveryUncertain
	}
	var sent struct {
		ThreadID string `json:"threadId"`
	}
	if err = nativeJSON(raw, &sent); err != nil {
		return ErrDesktopDeliveryUncertain
	}
	if "codex:"+sent.ThreadID != key {
		return ErrDesktopDeliveryUncertain
	}
	return nil
}

// NativeDisconnect withdraws only the approved companion lease, never closes
// Codex, cancels a model task or kills a CLI. The caller must confirm locally.
func (s *Service) NativeDisconnect(ctx context.Context) error {
	d, err := s.activeDescriptor(ctx)
	if errors.Is(err, ErrActivationRequired) {
		return nil
	}
	if err != nil {
		return err
	}
	raw, err := gatewayRPC(ctx, d, map[string]any{"type": "disconnect"}, 10*time.Second)
	if err != nil {
		return err
	}
	var result struct {
		Disconnected bool `json:"disconnected"`
	}
	if strictJSON(raw, &result) != nil || !result.Disconnected {
		return ErrDesktopUnavailable
	}
	return nil
}

func bounded(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
