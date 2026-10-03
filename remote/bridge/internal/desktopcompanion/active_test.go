package desktopcompanion

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const targetID = "01a0ae56-e9f6-7933-9ad4-5e08cd7874e5"
const controllerID = "02a0ae56-e9f6-7933-9ad4-5e08cd7874e5"
const otherID = "03a0ae56-e9f6-7933-9ad4-5e08cd7874e5"
const operationID = "04a0ae56-e9f6-4933-9ad4-5e08cd7874e5"

func nativeEnvelope(value any) map[string]any {
	b, _ := json.Marshal(value)
	return map[string]any{"success": true, "contentItems": []any{map[string]any{"type": "inputText", "text": string(b)}}}
}

type activeFixture struct {
	service           *Service
	o                 Options
	descriptor        activeDescriptor
	path              string
	mu                sync.Mutex
	calls             []map[string]any
	server            *httptest.Server
	result            func(map[string]any) any
	capabilities      *NativeCapabilities
	approvalTransport string
	gatewayFailure    string
}

func gatewayFixture(t *testing.T) *activeFixture {
	t.Helper()
	s, o := fixture(t, "# retained fixture\nmodel = \"original\"\n")
	if _, err := s.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := &activeFixture{service: s, o: o}
	f.capabilities = &NativeCapabilities{List: true, Read: true, Send: true}
	f.descriptor = activeDescriptor{Version: 1, Token: strings.Repeat("a", 64), ExpiresAt: time.Now().Add(2 * time.Minute).UnixMilli(), ControllerThreadID: controllerID, SessionKeys: []string{"codex:" + targetID}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rpc" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer "+f.descriptor.Token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var cmd map[string]any
		if json.NewDecoder(r.Body).Decode(&cmd) != nil {
			w.WriteHeader(400)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, cmd)
		handler := f.result
		f.mu.Unlock()
		var result any
		if cmd["type"] == "status" {
			result = map[string]any{"desktopControl": true, "remoteSend": true, "expiresAt": f.descriptor.ExpiresAt, "controllerThreadId": f.descriptor.ControllerThreadID, "sessionKeys": f.descriptor.SessionKeys}
			if f.capabilities != nil {
				result.(map[string]any)["capabilities"] = f.capabilities
			}
			if f.approvalTransport != "" {
				result.(map[string]any)["approvalTransport"] = f.approvalTransport
			}
		} else if f.gatewayFailure != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": f.gatewayFailure})
			return
		} else if handler != nil {
			result = handler(cmd)
		} else {
			result = nativeEnvelope(map[string]any{"threadId": targetID})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	}))
	t.Cleanup(f.server.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.server.URL, "http://"))
	f.descriptor.Port, _ = strconv.Atoi(port)
	f.path = filepath.Join(o.DataDir, "desktop-companion", "v"+Version, "active-desktop.json")
	f.write(t)
	return f
}
func (f *activeFixture) write(t *testing.T) {
	t.Helper()
	b, _ := json.Marshal(f.descriptor)
	if err := atomicWrite(f.path, b); err != nil {
		t.Fatal(err)
	}
}
func (f *activeFixture) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func TestNativeLeaseStatusScopedListReadSend(t *testing.T) {
	f := gatewayFixture(t)
	f.result = func(cmd map[string]any) any {
		switch cmd["type"] {
		case "list":
			return nativeEnvelope(map[string]any{"schemaVersion": 4, "pinnedThreads": []any{map[string]any{"id": targetID, "kind": "codex", "hostId": "local", "title": "原桌面任务", "status": "active", "updatedAt": 1727590000000, "cwd": "fixture-project"}}, "threads": []any{map[string]any{"id": otherID, "kind": "codex", "hostId": "local"}, map[string]any{"id": targetID, "kind": "chatgpt", "hostId": "local"}, map[string]any{"id": targetID, "kind": "codex", "hostId": "durable"}}})
		case "read":
			return nativeEnvelope(map[string]any{"schemaVersion": 1, "thread": map[string]any{"id": targetID, "kind": "codex", "hostId": "local", "title": "原桌面任务", "status": map[string]any{"type": "active"}, "updatedAt": 1727590000000}, "page": map[string]any{"hasMore": true, "nextCursor": "signed-fixture-anchor"}, "turns": []any{map[string]any{"id": "turn-new", "status": "completed", "startedAt": 1727590000000, "completedAt": 1727590001500, "durationMs": 1500, "items": []any{map[string]any{"id": "a", "type": "agentMessage", "text": "原生回复"}, map[string]any{"id": "r", "type": "reasoning", "summary": []string{"原生可见摘要"}}, map[string]any{"id": "c", "type": "commandExecution", "command": "go test", "output": map[string]any{"text": "PASS"}, "exitCode": 0}, map[string]any{"id": "f", "type": "fileChange", "changes": []any{map[string]any{"path": "app.go", "diff": map[string]any{"text": "+fixture"}}}}, map[string]any{"id": "sub", "type": "subAgentActivity", "kind": "spawn", "agentPath": "/root/test"}}}, map[string]any{"id": "turn-old", "status": "completed", "items": []any{map[string]any{"id": "u", "type": "userMessage", "content": []any{map[string]any{"type": "text", "text": "原来的任务"}}}}}}})
		case "send":
			return nativeEnvelope(map[string]any{"threadId": targetID})
		}
		return nil
	}
	ctx := context.Background()
	st, err := f.service.NativeStatus(ctx)
	if err != nil || !st.Active || st.ExpiresAt != f.descriptor.ExpiresAt {
		t.Fatal(st, err)
	}
	if !st.Capabilities.Send || st.Capabilities.Approval || st.Capabilities.Interrupt {
		t.Fatal("unsupported native capabilities advertised", st)
	}
	ss, err := f.service.NativeList(ctx)
	if err != nil || len(ss) != 1 || ss[0].SessionKey != "codex:"+targetID || ss[0].ControlSurface != "desktop" || ss[0].ControlExpiresAt != st.ExpiresAt || ss[0].Status != "running" {
		t.Fatal(ss, err)
	}
	info, ev, err := f.service.NativeOpen(ctx, "codex:"+targetID)
	if err != nil || info.ControlSurface != "desktop" || len(ev) != 8 {
		t.Fatal(info, len(ev), err)
	}
	if ev[0].Text != "原来的任务" || ev[2].Text != "原生回复" || ev[3].Text != "原生可见摘要" || ev[4].Output != "PASS" || ev[5].Diff != "+fixture" || ev[7].Detail != "处理 1.5 秒" {
		t.Fatal("native history conversion mismatch", ev)
	}
	if err = f.service.NativeSend(ctx, "codex:"+targetID, "继续", operationID); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	if last["operationId"] != operationID || last["text"] != "继续" || len(last) != 4 {
		t.Fatal("send included unexpected fields", last)
	}
}

func TestNativeDescriptorColdExpiryScopeFailClosedBeforeNetwork(t *testing.T) {
	s, _ := fixture(t, "")
	if _, err := s.NativeStatus(context.Background()); !errors.Is(err, ErrActivationRequired) {
		t.Fatal(err)
	}
	f := gatewayFixture(t)
	if err := f.service.NativeSend(context.Background(), "codex:"+otherID, "text", operationID); !errors.Is(err, ErrDesktopScope) {
		t.Fatal(err)
	}
	if err := f.service.NativeSend(context.Background(), "codex:"+targetID, "text", "bad-id"); err == nil {
		t.Fatal("missing idempotency id accepted")
	}
	f.descriptor.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	f.write(t)
	if _, err := f.service.NativeStatus(context.Background()); !errors.Is(err, ErrActivationRequired) {
		t.Fatal(err)
	}
	if f.count() != 0 {
		t.Fatal("invalid descriptor contacted gateway")
	}
}

func TestNativeDescriptorValidationRejectsControllerAndURLExtras(t *testing.T) {
	f := gatewayFixture(t)
	valid := f.descriptor
	now := time.Now().UnixMilli()
	for _, bad := range []activeDescriptor{
		{Version: 1, Port: valid.Port, Token: valid.Token, ExpiresAt: now + MaxLeaseMillis + 1, ControllerThreadID: controllerID, SessionKeys: valid.SessionKeys},
		{Version: 1, Port: valid.Port, Token: valid.Token, ExpiresAt: valid.ExpiresAt, ControllerThreadID: controllerID, SessionKeys: []string{"codex:" + controllerID}},
		{Version: 1, Port: valid.Port, Token: valid.Token, ExpiresAt: valid.ExpiresAt, ControllerThreadID: controllerID, SessionKeys: []string{"https://fixture.invalid"}},
		{Version: 1, Port: valid.Port, Token: valid.Token, ExpiresAt: valid.ExpiresAt, ControllerThreadID: controllerID, SessionKeys: []string{valid.SessionKeys[0], valid.SessionKeys[0]}},
	} {
		if validDescriptor(bad, now) {
			t.Fatal("invalid descriptor allowed")
		}
	}
	b, _ := json.Marshal(f.descriptor)
	b = append(b[:len(b)-1], []byte(`,"url":"https://fixture.invalid"}`)...)
	if err := atomicWrite(f.path, b); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.NativeStatus(context.Background()); !errors.Is(err, ErrActivationRequired) {
		t.Fatal(err)
	}
	if f.count() != 0 {
		t.Fatal("unknown URL descriptor contacted a server")
	}
}

func TestNativeUninstallImmediatelyInvalidatesRetainedDescriptor(t *testing.T) {
	f := gatewayFixture(t)
	if _, err := f.service.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.path); err != nil {
		t.Fatal("uninstall removed retained payload")
	}
	if _, err := f.service.NativeStatus(context.Background()); !errors.Is(err, ErrActivationRequired) {
		t.Fatal(err)
	}
	if _, _, err := f.service.NativeOpen(context.Background(), "codex:"+targetID); !errors.Is(err, ErrActivationRequired) {
		t.Fatal(err)
	}
	if err := f.service.NativeSend(context.Background(), "codex:"+targetID, "not sent", operationID); !errors.Is(err, ErrActivationRequired) {
		t.Fatal(err)
	}
	if f.count() != 0 {
		t.Fatal("uninstalled descriptor contacted gateway")
	}
}

func TestNativeGatewayErrorsDoNotLeakOrFallback(t *testing.T) {
	f := gatewayFixture(t)
	f.result = func(map[string]any) any {
		return map[string]any{"success": false, "contentItems": []any{map[string]any{"type": "inputText", "text": "sk-fixture-secret https://fixture.invalid C:/private/fixture"}}}
	}
	_, _, err := f.service.NativeOpen(context.Background(), "codex:"+targetID)
	if !errors.Is(err, ErrDesktopUnavailable) || strings.Contains(err.Error(), "fixture") {
		t.Fatal("gateway error escaped fixed code")
	}
	f.result = func(map[string]any) any {
		return nativeEnvelope(map[string]any{"schemaVersion": 1, "thread": map[string]any{"id": otherID, "kind": "codex", "hostId": "local"}, "turns": []any{}})
	}
	if _, _, err = f.service.NativeOpen(context.Background(), "codex:"+targetID); !errors.Is(err, ErrDesktopScope) {
		t.Fatal(err)
	}
}

func TestNativeSendFailureAfterDispatchIsUncertainNotUnsent(t *testing.T) {
	f := gatewayFixture(t)
	f.result = func(cmd map[string]any) any {
		return map[string]any{"success": false, "contentItems": []any{map[string]any{"type": "inputText", "text": "fixture-private-host-error"}}}
	}
	err := f.service.NativeSend(context.Background(), "codex:"+targetID, "original task", operationID)
	if !errors.Is(err, ErrDesktopDeliveryUncertain) || !strings.HasPrefix(err.Error(), "command_delivery_uncertain:") || strings.Contains(err.Error(), "fixture") {
		t.Fatal("ambiguous dispatch must preserve the uncertainty code without raw host output", err)
	}
	if f.count() != 2 {
		t.Fatal("send must have one status check and one dispatch, never an automatic resend")
	}
}

func TestNativeCapabilitiesMissingDefaultFalseAndUnsupportedFlagsRefused(t *testing.T) {
	f := gatewayFixture(t)
	f.capabilities = nil
	st, err := f.service.NativeStatus(context.Background())
	if err != nil || st.Capabilities != (NativeCapabilities{}) {
		t.Fatal("old metadata guessed capabilities", st, err)
	}
	if err = f.service.NativeSend(context.Background(), "codex:"+targetID, "not sent", operationID); !errors.Is(err, ErrDesktopCapability) {
		t.Fatal(err)
	}
	if _, _, err = f.service.NativeOpen(context.Background(), "codex:"+targetID); !errors.Is(err, ErrDesktopCapability) {
		t.Fatal(err)
	}
	if _, err = f.service.NativeList(context.Background()); !errors.Is(err, ErrDesktopCapability) {
		t.Fatal(err)
	}
	for _, caps := range []NativeCapabilities{{Send: true, Approval: true}, {Send: true, Interrupt: true}, {Send: true, Attachments: true}, {Send: true, ModelOverride: true}} {
		f.capabilities = &caps
		if _, err = f.service.NativeStatus(context.Background()); !errors.Is(err, ErrDesktopUnavailable) {
			t.Fatal("unimplemented native capability accepted", caps, err)
		}
	}
}

func TestNativeHookApprovalProofAndScopeCannotBroadenOtherCapabilities(t *testing.T) {
	f := gatewayFixture(t)
	f.capabilities.Approval = true
	if _, err := f.service.NativeStatus(context.Background()); !errors.Is(err, ErrDesktopUnavailable) {
		t.Fatal("unproven approval advertised", err)
	}
	f.approvalTransport = NativeApprovalTransport
	st, err := f.service.NativeStatus(context.Background())
	if err != nil || !ValidateNativeConnection(st, time.Now().UnixMilli()) || !st.Capabilities.Approval || st.ApprovalTransport != NativeApprovalTransport {
		t.Fatal(st, err)
	}
	f.result = func(cmd map[string]any) any {
		return map[string]any{"accepted": true, "sessionKey": cmd["sessionKey"], "approvalId": cmd["approvalId"]}
	}
	key := "codex:" + targetID
	if err := f.service.NativeRespondApproval(context.Background(), key, otherID, "allow", "", operationID); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	if last["type"] != "approval.respond" || last["operationId"] != operationID || last["approvalId"] != otherID || len(last) != 6 {
		t.Fatal(last)
	}
	before := f.count()
	for _, args := range [][5]string{{"codex:" + otherID, otherID, "allow", "", operationID}, {key, otherID, "allow_session", "", operationID}, {key, otherID, "allow", "", "not-a-uuid"}} {
		if f.service.NativeRespondApproval(context.Background(), args[0], args[1], args[2], args[3], args[4]) == nil {
			t.Fatal("invalid scope or operation accepted")
		}
	}
	if f.count() != before {
		t.Fatal("invalid approval contacted gateway")
	}
	f.capabilities.Interrupt = true
	if _, err := f.service.NativeStatus(context.Background()); !errors.Is(err, ErrDesktopUnavailable) {
		t.Fatal("hook expanded stop capability", err)
	}
}

func TestNativeApprovalSnapshotOnlyReturnsActualScopedHookRequests(t *testing.T) {
	f := gatewayFixture(t)
	key := "codex:" + targetID
	pending := map[string]any{"type": "approval.request", "sessionKey": key, "tool": "codex", "approvalId": otherID, "turnId": operationID, "kind": "command", "title": "Bash", "detail": `{"command":"git status"}`, "ts": time.Now().UnixMilli(), "expiresAt": time.Now().Add(time.Minute).UnixMilli(), "approvalTransport": NativeApprovalTransport}
	f.result = func(map[string]any) any {
		return nativeEnvelope(map[string]any{"schemaVersion": 1, "thread": map[string]any{"id": targetID, "kind": "codex", "hostId": "local"}, "turns": []any{}, "approvalEvents": []any{pending}})
	}
	info, events, err := f.service.NativeOpen(context.Background(), key)
	if err != nil || info.Status != "waiting_approval" || len(events) != 1 || events[0].ApprovalTransport != NativeApprovalTransport || events[0].Detail != pending["detail"] {
		t.Fatal(info, events, err)
	}
	pending["sessionKey"] = "codex:" + otherID
	if _, _, err := f.service.NativeOpen(context.Background(), key); !errors.Is(err, ErrDesktopUnavailable) {
		t.Fatal("cross-session hook returned", err)
	}
	pending["sessionKey"] = key
	pending["approvalTransport"] = "unverified"
	if _, _, err := f.service.NativeOpen(context.Background(), key); !errors.Is(err, ErrDesktopUnavailable) {
		t.Fatal("unknown hook accepted", err)
	}
}

func TestNativeApprovalLostAckIsUncertainAndKnownExpiryIsNotSent(t *testing.T) {
	for _, code := range []string{"APPROVAL_EXPIRED", "NATIVE_CALL_FAILED"} {
		f := gatewayFixture(t)
		f.capabilities.Approval = true
		f.approvalTransport = NativeApprovalTransport
		f.gatewayFailure = code
		err := f.service.NativeRespondApproval(context.Background(), "codex:"+targetID, otherID, "allow", "", operationID)
		want := ErrDesktopDeliveryUncertain
		if code == "APPROVAL_EXPIRED" {
			want = ErrDesktopRequestUnsent
		}
		if !errors.Is(err, want) || f.count() != 2 {
			t.Fatal(code, err, f.count())
		}
	}
}

func TestNativeGatewayDefinitePreDispatchErrorsVersusUncertainTombstone(t *testing.T) {
	for _, code := range []string{"CONTROL_BUSY", "OPERATION_STORAGE_UNAVAILABLE", "REQUEST_INVALID", "OPERATION_UNCERTAIN", "NATIVE_CALL_FAILED", "CONTROL_REVOKED", "unknown-private-host-output"} {
		t.Run(code, func(t *testing.T) {
			f := gatewayFixture(t)
			f.gatewayFailure = code
			err := f.service.NativeSend(context.Background(), "codex:"+targetID, "task", operationID)
			want := ErrDesktopDeliveryUncertain
			if code == "CONTROL_BUSY" || code == "OPERATION_STORAGE_UNAVAILABLE" || code == "REQUEST_INVALID" {
				want = ErrDesktopRequestUnsent
			}
			if !errors.Is(err, want) || strings.Contains(err.Error(), "private") || f.count() != 2 {
				t.Fatal("incorrect dispatch certainty or automatic retry", err, f.count())
			}
		})
	}
}

func TestNativeChildLinksRequireActualIDsWithinApprovedScope(t *testing.T) {
	key := "codex:" + targetID
	item := map[string]json.RawMessage{}
	for name, value := range map[string]any{"type": "collabAgentToolCall", "id": "spawn", "agentPath": "/root/fake", "receiverThreadIds": []string{otherID, targetID, "not-a-uuid"}, "agentsStates": map[string]any{controllerID: map[string]any{"status": "completed"}, otherID: map[string]any{"status": "running"}}} {
		item[name], _ = json.Marshal(value)
	}
	r := nativeRead{Turns: []nativeTurn{{ID: "t", Items: []map[string]json.RawMessage{item}}}}
	events := nativeEvents(key, r, []string{key, "codex:" + otherID})
	if len(events) != 2 || len(events[0].ChildSessionKeys) != 1 || events[0].ChildSessionKeys[0] != "codex:"+otherID {
		t.Fatal("invented or unauthorized child links", events)
	}
	withoutScope := nativeEvents(key, r)
	if len(withoutScope[0].ChildSessionKeys) != 0 {
		t.Fatal("expanded authorization from model-supplied child IDs")
	}
}

func TestLegacyOwnedProbeUninstallPreservesOtherSettings(t *testing.T) {
	s, o := fixture(t, "# original\nmodel = \"unchanged\"\n")
	if _, err := s.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	p, err := s.uninstallPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, err := readRegular(p.state, maxFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	var own ownership
	_ = json.Unmarshal(b, &own)
	own.Version = "0.1.0"
	p.payload = filepath.Join(p.root, "v0.1.0")
	p.node = o.NodePath
	own.EntryHash = entryHash(expectedEntryVersion(p, own.Version))
	config := []byte("# original\nmodel = \"unchanged\"\n\n# Salcara desktop companion installer: " + own.ID + "\n" + entryTOMLVersion(p, own.Version) + "\n[ui]\nfixture = true\n")
	if err = atomicWrite(p.config, config); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(own)
	if err = atomicWrite(p.state, b); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Install(context.Background()); err == nil {
		t.Fatal("legacy installation overwritten")
	}
	st, err := s.PreviewUninstall(context.Background())
	if err != nil || !st.UninstallAvailable {
		t.Fatal(st, err)
	}
	if _, err = s.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := string(configBytes(t, o))
	if strings.Contains(after, ServerName) || !strings.Contains(after, "model = \"unchanged\"") || !strings.Contains(after, "fixture = true") {
		t.Fatal("legacy uninstall changed other settings")
	}
}
