package agents

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestDeepSeekOfficialEventProjection(t *testing.T) {
	for _, tc := range []struct {
		kind, data, role, text, status string
		count                          int
	}{
		{"user/message", `{"id":"u1","role":"user","content":[{"type":"text","text":"hello"}],"source":{"kind":"user"}}`, "user", "hello", "", 1},
		{"user/message", `{"id":"ctx","role":"user","content":[{"type":"text","text":"private runtime context"}],"source":{"kind":"runtime-context"}}`, "", "", "", 0},
		{"assistant/message", `{"turn":2,"message":{"id":"a1","role":"assistant","content":[{"type":"text","text":"done"},{"type":"image","data":"hidden"}]},"usage":{"inputTokens":10,"outputTokens":2}}`, "assistant", "done", "", 1},
		{"turn/end", `{"turn":2,"reason":{"kind":"error","error":{"message":"failed"}}}`, "", "", "failed", 1},
		{"unknown/future", `{}`, "", "", "", 0},
	} {
		events := mapDeepSeekEvent(dshEvent{Type: tc.kind, Seq: 3, Time: 77, Data: json.RawMessage(tc.data)})
		if len(events) != tc.count {
			t.Fatalf("%s: %+v", tc.kind, events)
		}
		if tc.count == 1 && (events[0].Role != tc.role || events[0].Text != tc.text || events[0].Status != tc.status) {
			t.Fatalf("%s: %+v", tc.kind, events[0])
		}
	}
}

func TestBundledAgentRegistryCapabilities(t *testing.T) {
	m := NewManagerWithOptions(nil, func() Settings { return Settings{} }, Options{StateDir: t.TempDir(), ClaudeHome: t.TempDir(), BundledAgents: true})
	defer m.Close()
	if len(m.Agents()) != 4 || m.Get(deepSeekID) == nil || m.Get("hermes") == nil {
		t.Fatal("bundled registry missing an adapter")
	}
	d := Describe(m.Get(deepSeekID))
	if !d.Capabilities.Send || !d.Capabilities.Approval || !d.Capabilities.ModelOverride || !d.Capabilities.PreciseInterrupt || d.Capabilities.ImageInput || d.Capabilities.Subagents {
		t.Fatal("unimplemented dsh capability advertised")
	}
	h := Describe(m.Get("hermes"))
	if !h.Capabilities.Send || !h.Capabilities.PreciseInterrupt || !h.Capabilities.Approval || h.Capabilities.ModelOverride || h.Capabilities.ImageInput || h.Capabilities.Questions {
		t.Fatal("Hermes must advertise only its implemented native Gateway controls")
	}
}

func TestExtensionSettingsPinTheirOwnFamilyAndSnapshot(t *testing.T) {
	a := config.LocalAccount{ID: "dsh-api", Kind: "api", Key: "fixture-dsh", BaseURL: "https://fixture-dsh.invalid", Model: "fixture-model"}
	c := config.Config{AccountKey: "must-not-fallback", LocalAccounts: []config.LocalAccount{a}}
	if s := SettingsFromConfig(c); s.Extensions[deepSeekID].Key != "" || s.Extensions["hermes"].Key != "" {
		t.Fatal("extension inherited relay credentials")
	}
	c.RecordAppliedAPI(deepSeekID, a, "")
	s := LocalSettingsWithGateway(c, 47831)
	if s.Extensions[deepSeekID].Key != a.Key || s.Extensions["hermes"].Key != "" {
		t.Fatal("extension source family mixed")
	}
	ctx := WithLocalConfigSnapshot(context.Background(), c)
	c.LocalAccounts[0].Key = "edited"
	pinned := settingsForContext(ctx, func() Settings { return LocalSettingsWithGateway(c, 47831) })
	if pinned.Extensions[deepSeekID].Key != a.Key {
		t.Fatal("frozen extension source changed")
	}
}

func TestDeepSeekSDKLifecycleAndScopedApprovals(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "dsh-fixture")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", exe, "./testdata/deepseek_sdk_helper")
	prepareCmd(build)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatal("fixture build", err, string(output))
	}
	state := t.TempDir()
	settings := Settings{DeepSeekPath: exe, Approval: "ask", Extensions: map[string]ExtensionSettings{deepSeekID: {Root: "http://127.0.0.1:1", Key: "synthetic-fixture-key", Model: "fixture-model"}}}
	a := newDeepSeekAgent(func() Settings { return settings }, state)
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, err := a.Start(ctx, t.TempDir(), "approval", "", "")
	if err != nil {
		t.Fatal(err)
	}
	waitForSession := func(sessionID string, predicate func(protocol.SessionInfo, []protocol.Event) bool) (protocol.SessionInfo, []protocol.Event) {
		t.Helper()
		deadline := time.Now().Add(6 * time.Second)
		for time.Now().Before(deadline) {
			info, events, err := a.Open(ctx, sessionID)
			if err != nil {
				t.Fatal(err)
			}
			if predicate(info, events) {
				return info, events
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("fixture event timeout")
		return protocol.SessionInfo{}, nil
	}
	wait := func(predicate func(protocol.SessionInfo, []protocol.Event) bool) (protocol.SessionInfo, []protocol.Event) {
		return waitForSession(id, predicate)
	}
	_, events := wait(func(info protocol.SessionInfo, events []protocol.Event) bool {
		for _, e := range events {
			if e.Type == "approval.request" {
				return info.Status == "waiting_approval"
			}
		}
		return false
	})
	var approval string
	for _, e := range events {
		if e.Type == "approval.request" {
			approval = e.ApprovalID
		}
	}
	if a.RespondForSession(ctx, deepSeekID+":foreign", approval, ApprovalResponse{Decision: "allow"}) {
		t.Fatal("foreign session answered approval")
	}
	if a.Respond(approval, "allow_session", "") {
		t.Fatal("unsupported durable grant accepted")
	}
	selected := settings.Extensions[deepSeekID]
	settings.Extensions[deepSeekID] = ExtensionSettings{Root: selected.Root, Key: "different-source", Model: selected.Model}
	if a.RespondForSession(ctx, deepSeekID+":"+id, approval, ApprovalResponse{Decision: "allow"}) {
		t.Fatal("approval accepted after API source changed")
	}
	settings.Extensions[deepSeekID] = selected
	if !a.RespondForSession(ctx, deepSeekID+":"+id, approval, ApprovalResponse{Decision: "allow"}) {
		t.Fatal("valid scoped approval rejected")
	}
	_, events = wait(func(info protocol.SessionInfo, _ []protocol.Event) bool { return info.Status == "idle" })
	user, assistant := false, false
	for _, e := range events {
		if strings.Contains(e.Text, "MUST NOT LEAK") {
			t.Fatal("foreign notification leaked")
		}
		user = user || e.Role == "user" && e.Text == "approval"
		assistant = assistant || e.Role == "assistant" && e.Text == "fixture complete"
	}
	if !user || !assistant {
		t.Fatal("native messages missing")
	}
	if a.Respond(approval, "allow", "") {
		t.Fatal("approval replay accepted")
	}
	if a.ActiveRemoteTurns() {
		t.Fatal("completed SDK session still blocks API changes")
	}
	a.mu.Lock()
	previous := a.sessions[id].conn
	a.mu.Unlock()
	settings.Extensions[deepSeekID] = ExtensionSettings{Root: "http://127.0.0.1:2", Key: "synthetic-replacement-key", Model: "fixture-model"}
	if err := a.SendWithOptions(ctx, id, "source changed", TurnOptions{Model: "replacement-model"}); err != nil {
		t.Fatal("source replacement failed", err)
	}
	wait(func(info protocol.SessionInfo, _ []protocol.Event) bool { return info.Status == "idle" })
	a.mu.Lock()
	replacement := a.sessions[id].conn
	a.mu.Unlock()
	if replacement == previous || !previous.rootProcessExitConfirmed() {
		t.Fatal("API change reused a stale live runtime")
	}
	a.mu.Lock()
	session := a.sessions[id]
	a.mu.Unlock()
	a.notify(id, session, previous, "session.status", json.RawMessage(`{"sessionId":"`+id+`","status":"running"}`))
	if a.ActiveRemoteTurns() {
		t.Fatal("retired runtime revived session activity")
	}
	if a.KnownSessionModel(id) != "replacement-model" {
		t.Fatal("SDK model selection did not change")
	}
	if err := a.Send(ctx, id, "hold"); err != nil {
		t.Fatal(err)
	}
	active, _ := wait(func(info protocol.SessionInfo, _ []protocol.Event) bool {
		return info.Status == "running" && info.ActiveTurnID != ""
	})
	secondID, err := a.Start(ctx, t.TempDir(), "hold", "replacement-model", "")
	if err != nil {
		t.Fatal("second session with the same API did not start", err)
	}
	secondActive, _ := waitForSession(secondID, func(info protocol.SessionInfo, _ []protocol.Event) bool {
		return info.Status == "running" && info.ActiveTurnID != ""
	})
	a.mu.Lock()
	secondRuntime := a.sessions[secondID].conn
	a.mu.Unlock()
	if !a.ActiveRemoteTurns() {
		t.Fatal("running SDK work did not fence API changes")
	}
	if err := a.Send(ctx, id, "must not enqueue"); err == nil {
		t.Fatal("busy send accepted without steer capability")
	}
	if err := a.Interrupt(ctx, id); err != ErrExpectedTurnRequired {
		t.Fatal("ID-less interrupt was accepted")
	}
	if _, err := a.InterruptTurn(ctx, id, "wrong-turn", "01234567-1234-1234-1234-123456789abc"); err != ErrTurnControlStale {
		t.Fatal("stale interrupt was accepted", err)
	}
	a.mu.Lock()
	current := a.sessions[id].conn
	a.mu.Unlock()
	a.notify(id, session, current, "subagent.started", json.RawMessage(`{"parentSessionId":"`+id+`","childSessionId":"synthetic-child"}`))
	if _, err := a.InterruptTurn(ctx, id, active.ActiveTurnID, "01234567-1234-1234-1234-123456789abc"); err != ErrTurnControlStale {
		t.Fatal("process stop ignored live child work")
	}
	a.notify(id, session, current, "subagent.finished", json.RawMessage(`{"parentSessionId":"`+id+`","childSessionId":"synthetic-child"}`))
	if result, err := a.InterruptTurn(ctx, id, active.ActiveTurnID, "01234567-1234-1234-1234-123456789abc"); err != nil || result.Status != "stopped" {
		t.Fatal("exact turn stop failed", result, err)
	}
	secondAfter, _, err := a.Open(ctx, secondID)
	if err != nil || secondAfter.Status != "running" || secondAfter.ActiveTurnID != secondActive.ActiveTurnID || !secondRuntime.alive() || !a.ActiveRemoteTurns() {
		t.Fatal("stopping one session disturbed another session using the same API", err)
	}
	if result, err := a.InterruptTurn(ctx, secondID, secondActive.ActiveTurnID, "01234567-1234-1234-1234-123456789abd"); err != nil || result.Status != "stopped" {
		t.Fatal("second session could not stop independently", result, err)
	}
	if err := a.Send(ctx, id, "resume"); err != nil {
		t.Fatal("native resume failed", err)
	}
	wait(func(info protocol.SessionInfo, _ []protocol.Event) bool { return info.Status == "idle" })
	if err := a.stopSession(id); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(state, deepSeekID, id, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	patch, err := os.ReadFile(filepath.Join(state, deepSeekID, id, "gateway.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored), "synthetic-fixture-key") || strings.Contains(string(patch), "synthetic-fixture-key") {
		t.Fatal("credential persisted in history/config patch")
	}
	b := newDeepSeekAgent(func() Settings { return settings }, state)
	defer b.Close()
	if _, events, err := b.Open(ctx, id); err != nil || len(events) == 0 {
		t.Fatal("owned history not restored", err)
	}
	if err := b.Send(ctx, id, "after restart"); err != nil {
		t.Fatal("persisted native resume failed", err)
	}
}
