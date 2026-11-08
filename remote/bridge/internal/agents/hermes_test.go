package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
	"strings"
	"testing"
)

type hermesFixtureGateway struct {
	calls    []string
	params   []map[string]any
	detached bool
	call     func(string, map[string]any) (any, error)
}

func (f *hermesFixtureGateway) Call(_ context.Context, method string, params any, out any) error {
	b, _ := json.Marshal(params)
	var p map[string]any
	_ = json.Unmarshal(b, &p)
	f.calls = append(f.calls, method)
	f.params = append(f.params, p)
	value, err := f.call(method, p)
	if err != nil {
		return err
	}
	if out != nil {
		b, _ = json.Marshal(value)
		return json.Unmarshal(b, out)
	}
	return nil
}
func (f *hermesFixtureGateway) alive() bool { return !f.detached }
func (f *hermesFixtureGateway) detach()     { f.detached = true }
func hermesFixture(t *testing.T) (*hermesAgent, *hermesFixtureGateway, *Settings, *hermesSnapshot) {
	t.Helper()
	settings := &Settings{Extensions: map[string]ExtensionSettings{hermesID: {Root: "https://models.invalid", Key: "fixture-private-key", Model: "fixture-model"}}, Approval: "ask"}
	a := newHermesAgent(func() Settings { return *settings }, t.TempDir())
	snapshot := &hermesSnapshot{NativeID: "native-session", SessionID: "native-session", Epoch: 7, Replay: "replay-1"}
	snapshot.Info.Cwd = t.TempDir()
	snapshot.Info.Model = "fixture-model"
	f := &hermesFixtureGateway{}
	f.call = func(method string, p map[string]any) (any, error) {
		switch method {
		case "salcara.initialize":
			return map[string]string{"protocol": "salcara-hermes-gateway-v1"}, nil
		case "session.create", "session.resume":
			return snapshot, nil
		case "prompt.submit":
			return map[string]any{"admission_id": "admission-1", "status": "started", "execution_generation": 1, "ref": map[string]string{"session_id": snapshot.NativeID}}, nil
		case "session.interrupt":
			return map[string]any{"execution_generation": snapshot.Generation, "ref": map[string]string{"session_id": snapshot.NativeID}}, nil
		case "approval.respond":
			return map[string]string{"status": "resolved", "prompt_id": p["prompt_id"].(string)}, nil
		}
		return nil, errors.New("unexpected method")
	}
	a.dial = func(context.Context, func(string, json.RawMessage)) (hermesGateway, error) { return f, nil }
	t.Cleanup(a.Close)
	return a, f, settings, snapshot
}
func hermesStart(t *testing.T, a *hermesAgent, snapshot *hermesSnapshot) string {
	t.Helper()
	id, err := a.Start(context.Background(), snapshot.Info.Cwd, "original task", "", "ask")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestHermesNativeCreateSubmitAndFrozenSource(t *testing.T) {
	a, f, settings, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	if strings.Join(f.calls, ",") != "salcara.initialize,session.create,session.resume,prompt.submit" {
		t.Fatal(f.calls)
	}
	create := f.params[1]
	if create["api_key"] != "fixture-private-key" || create["provider"] != "custom" || create["base_url"] != "https://models.invalid/v1" || create["cwd"] != snapshot.Info.Cwd || create["yolo"] != false {
		t.Fatal("incorrect frozen creation policy")
	}
	if f.params[2]["api_key"] != "fixture-private-key" {
		t.Fatal("resume must rebind the original ephemeral key")
	}
	data, err := os.ReadFile(filepath.Join(a.stateDir, id+".json"))
	if err != nil || strings.Contains(string(data), "fixture-private-key") {
		t.Fatal("credential persisted", err)
	}
	settings.Extensions[hermesID] = ExtensionSettings{Root: "https://other.invalid", Key: "other-key", Model: "fixture-model"}
	before := len(f.calls)
	if err = a.Send(context.Background(), id, "must not be sent"); err == nil || len(f.calls) != before {
		t.Fatal("changed API was silently rebound")
	}
}

func TestHermesExactGenerationInterruptAndDetach(t *testing.T) {
	a, f, _, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	snapshot.Generation = 3
	snapshot.Running = true
	op := "11111111-1111-4111-8111-111111111111"
	if _, err := a.InterruptTurn(context.Background(), id, "hermes-7-2", op); !errors.Is(err, ErrTurnControlStale) {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if call == "session.interrupt" {
			t.Fatal("stale turn interrupted")
		}
	}
	result, err := a.InterruptTurn(context.Background(), id, "hermes-7-3", op)
	if err != nil || result.Status != "requested" {
		t.Fatal(result, err)
	}
	p := f.params[len(f.params)-1]
	if p["execution_generation"] != float64(3) || p["session_id"] != "native-session" {
		t.Fatal(p)
	}
	count := len(f.calls)
	if err = a.Interrupt(context.Background(), id); !errors.Is(err, ErrExpectedTurnRequired) {
		t.Fatal(err)
	}
	a.Close()
	if !f.detached || len(f.calls) != count {
		t.Fatal("closing a viewer attempted a gateway stop")
	}
}

func TestHermesApprovalScopeGenerationAndChoices(t *testing.T) {
	a, f, settings, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	snapshot.Generation = 2
	snapshot.Running = true
	snapshot.Prompts = []hermesPrompt{{ID: "prompt-1", Kind: "approval", Generation: 2, Choices: []string{"once", "deny"}, Description: "Run command", Command: "echo safe"}}
	info, events, err := a.Open(context.Background(), id)
	if err != nil || info.Status != "waiting_approval" || len(events) != 1 {
		t.Fatal(info, events, err)
	}
	approval := hermesID + ":" + id + ":prompt-1"
	before := len(f.calls)
	if a.RespondForSession(context.Background(), "hermes:wrong", approval, ApprovalResponse{Decision: "allow"}) || a.Respond(approval, "allow_session", "") {
		t.Fatal("approval scope/choice bypass")
	}
	original := settings.Extensions[hermesID]
	settings.Extensions[hermesID] = ExtensionSettings{Key: "different"}
	if a.RespondForSession(context.Background(), info.SessionKey, approval, ApprovalResponse{Decision: "allow"}) {
		t.Fatal("changed source approved")
	}
	renewed := original
	renewed.Key = "renewed-route-lease"
	settings.Extensions[hermesID] = renewed
	if a.RespondForSession(context.Background(), info.SessionKey, approval, ApprovalResponse{Decision: "allow"}) {
		t.Fatal("new gateway lease revived an old approval")
	}
	settings.Extensions[hermesID] = original
	if len(f.calls) != before || !a.RespondForSession(context.Background(), info.SessionKey, approval, ApprovalResponse{Decision: "allow"}) {
		t.Fatal("valid approval refused")
	}
	p := f.params[len(f.params)-1]
	if p["choice"] != "once" || p["execution_generation"] != float64(2) || p["prompt_id"] != "prompt-1" {
		t.Fatal(p)
	}
}

func TestHermesHistoryRecoveryFencesEventsAndUnknownWork(t *testing.T) {
	a, f, _, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	s, _ := a.session(id)
	snapshot.Generation = 1
	snapshot.Sequence = 10
	snapshot.Pending = []struct {
		Status    string `json:"status"`
		Admission string `json:"admission_id,omitempty"`
	}{{Status: "unknown"}}
	if _, _, err := a.Open(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	before := len(f.calls)
	if err := a.Send(context.Background(), id, "duplicate"); err == nil {
		t.Fatal("unknown work admitted")
	}
	if len(f.calls) != before+1 || f.calls[len(f.calls)-1] != "session.resume" {
		t.Fatal(f.calls)
	}
	snapshot.Pending = nil
	snapshot.Sequence = 20
	original := f.call
	f.call = func(method string, p map[string]any) (any, error) {
		if method == "session.resume" {
			a.event(f, id, s, hermesNativeEvent{SessionID: s.NativeID, Type: "message.complete", AdmissionID: "a", Generation: 1, Replay: snapshot.Replay, Sequence: 21, Payload: json.RawMessage(`{"text":"finished after snapshot","outcome":"completed"}`)})
		}
		return original(method, p)
	}
	info, events, err := a.Open(context.Background(), id)
	if err != nil || info.Status != "idle" {
		t.Fatal(info, err)
	}
	found := false
	for _, e := range events {
		if e.Text == "finished after snapshot" && e.Final {
			found = true
		}
	}
	if !found {
		t.Fatal("resume discarded a later terminal event", events)
	}
	a.event(f, id, s, hermesNativeEvent{SessionID: "foreign", Type: "message.delta", Generation: 2, Replay: snapshot.Replay, Sequence: 22, Payload: json.RawMessage(`{"text":"foreign"}`)})
	a.event(f, id, s, hermesNativeEvent{SessionID: s.NativeID, Type: "message.delta", Generation: 0, Replay: snapshot.Replay, Sequence: 19, Payload: json.RawMessage(`{"text":"stale"}`)})
	if s.Info.Status != "idle" {
		t.Fatal("stale/foreign event changed session")
	}
}

func TestHermesGatewayBootstrapPreservesInstalledRuntime(t *testing.T) {
	python := filepath.Join(t.TempDir(), "python.exe")
	bootstrap := "import os,sys,runpy; sys.path.insert(0, 'installed-root'); import hermes_bootstrap; runpy.run_module('" + hermesSidecarModule + "', run_name='__main__', alter_sys=True)"
	raw, _ := json.Marshal([]string{python, "-I", "-c", bootstrap})
	cmd, err := hermesCommandFromRuntime(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path != python || cmd.Args[1] != "-I" || !strings.Contains(cmd.Args[3], "import hermes_bootstrap") || !strings.Contains(cmd.Args[3], "connect_gateway") {
		t.Fatal("installation bootstrap discarded")
	}
	for _, bad := range []string{`["python","-I","-c","anything"]`, `["C:/python.exe","-c","anything"]`, `null`} {
		if _, err := hermesCommandFromRuntime([]byte(bad)); err == nil {
			t.Fatal("unproven runtime accepted")
		}
	}
	if strings.Contains(cmd.Args[3], "fixture-private-key") {
		t.Fatal("credentials in process arguments")
	}
}

func TestHermesCapabilitiesStayConservative(t *testing.T) {
	a, _, _, _ := hermesFixture(t)
	caps := a.Descriptor().Capabilities
	if !caps.Send || !caps.PreciseInterrupt || !caps.Approval || !caps.StartModelOverride || caps.ModelOverride || caps.Questions || caps.Attachments || caps.Subagents || caps.Continue {
		t.Fatal(caps)
	}
	var _ protocol.Event
}

func TestHermesExplicitCreationModelWorksWithoutAccountDefault(t *testing.T) {
	a, f, settings, snapshot := hermesFixture(t)
	api := settings.Extensions[hermesID]
	api.Model = ""
	settings.Extensions[hermesID] = api
	snapshot.Info.Model = "chosen-at-create"
	id, err := a.StartWithOptions(context.Background(), snapshot.Info.Cwd, "task", "ask", TurnOptions{Model: "chosen-at-create"})
	if err != nil || f.params[1]["model"] != "chosen-at-create" {
		t.Fatal("creation model ignored", err)
	}
	before := len(f.calls)
	if a.SendWithOptions(context.Background(), id, "other", TurnOptions{Model: "another-model"}) == nil || len(f.calls) != before {
		t.Fatal("existing session model was changed")
	}
	if _, err = a.StartWithOptions(context.Background(), snapshot.Info.Cwd, "task", "ask", TurnOptions{Model: "chosen-at-create", Effort: "high"}); err == nil || len(f.calls) != before {
		t.Fatal("unsupported reasoning option admitted")
	}
}

func TestHermesMalformedTerminalCannotClaimSuccess(t *testing.T) {
	a, f, _, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	s, _ := a.session(id)
	a.event(f, id, s, hermesNativeEvent{SessionID: s.NativeID, Type: "message.complete", AdmissionID: "admission-1", Generation: 1, Replay: snapshot.Replay, Sequence: 1, Payload: json.RawMessage(`{"text":"unconfirmed"}`)})
	if !s.uncertain || s.Info.Status != "unknown" {
		t.Fatal("missing outcome was treated as success")
	}
	for _, event := range s.Events {
		if event.Type == "turn" && event.Status == "completed" {
			t.Fatal("unproven terminal event published")
		}
	}
}

func TestHermesRestartLeaseRotationRequiresNewSessionButPreservesReadAndStop(t *testing.T) {
	a, f, settings, snapshot := hermesFixture(t)
	account := config.LocalAccount{ID: "hermes-api", Kind: "api", Key: "upstream-fixture", BaseURL: "https://fixture.invalid", Model: snapshot.Info.Model}
	cfg := config.Config{GatewayRuntimeKey: "bridge-generation-1", DirectRelayEnabled: true, DirectPhoneID: "phone", DirectPhoneHash: "phone-hash", DeviceID: "pc", LocalAccounts: []config.LocalAccount{account}, DirectAPI: map[string]config.RemoteAPI{hermesID: {AccountID: account.ID, Model: snapshot.Info.Model}}, GatewayAuthEpochs: map[string]string{"direct-hermes": "route-epoch"}}
	*settings = DirectSettingsWithGateway(cfg, 47831)
	original := settings.Extensions[hermesID]
	if original.Key == "" || original.SourceFingerprint == "" {
		t.Fatal("fixture did not produce an authorized direct source")
	}
	id := hermesStart(t, a, snapshot)
	a.Close()
	cfg.GatewayRuntimeKey = "bridge-generation-2"
	*settings = DirectSettingsWithGateway(cfg, 47831)
	if settings.Extensions[hermesID].Key == original.Key || settings.Extensions[hermesID].SourceFingerprint != original.SourceFingerprint {
		t.Fatal("restart must rotate only the gateway lease")
	}
	restored := newHermesAgent(func() Settings { return *settings }, filepath.Dir(a.stateDir))
	defer restored.Close()
	if !restored.ActiveRemoteTurns() {
		t.Fatal("unverified restored native work allowed API switching")
	}
	f.detached = false
	restored.dial = func(context.Context, func(string, json.RawMessage)) (hermesGateway, error) { return f, nil }
	before := len(f.calls)
	err := restored.Send(context.Background(), id, "must not renew or submit")
	if err == nil || !strings.Contains(err.Error(), "启动凭据已失效") || len(f.calls) != before {
		t.Fatal("rotated launch key reached native resume/submit", err)
	}
	info, events, err := restored.Open(context.Background(), id)
	if err != nil || info.Status != "idle" || restored.ActiveRemoteTurns() {
		t.Fatal("authoritative idle read did not recover status", err)
	}
	if _, supplied := f.params[len(f.params)-1]["api_key"]; supplied {
		t.Fatal("read attempted to replace the frozen native launch key")
	}
	if len(events) == 0 || !strings.Contains(events[len(events)-1].Text, "请新建会话继续") {
		t.Fatal("history did not explain the restart boundary")
	}
	snapshot.Generation, snapshot.Running = 3, true
	result, err := restored.InterruptTurn(context.Background(), id, "hermes-7-3", "11111111-1111-4111-8111-111111111111")
	if err != nil || result.Status != "requested" {
		t.Fatal("rotated lease prevented exact native stop", result, err)
	}
	for _, p := range f.params[before:] {
		if _, supplied := p["api_key"]; supplied {
			t.Fatal("read/stop tried to rebind a new key")
		}
	}
	current := settings.Extensions[hermesID]
	current.SourceFingerprint = "changed-upstream-account"
	settings.Extensions[hermesID] = current
	if err = restored.Send(context.Background(), id, "different source"); err == nil || !strings.Contains(err.Error(), "API 已变化") {
		t.Fatal("upstream change confused with lease renewal", err)
	}
	current.SourceFingerprint = original.SourceFingerprint
	current.Root = "http://127.0.0.1:47832/gateway/direct-hermes"
	settings.Extensions[hermesID] = current
	if err = restored.Send(context.Background(), id, "different port"); err == nil || !strings.Contains(err.Error(), "端口已变化") {
		t.Fatal("frozen native endpoint change was accepted", err)
	}
}

func TestHermesActiveRemoteTurnsIncludesUncertainRecoveryAndApprovals(t *testing.T) {
	a, f, _, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	s, _ := a.session(id)
	for _, state := range []string{"busy", "uncertain", "recovering", "approval"} {
		a.mu.Lock()
		s.busy, s.uncertain, s.recovering = state == "busy", state == "uncertain", state == "recovering"
		s.Info.Status = "unknown"
		s.prompts = map[string]hermesPrompt{}
		if state == "approval" {
			s.prompts["pending"] = hermesPrompt{ID: "pending"}
		}
		a.mu.Unlock()
		if !a.ActiveRemoteTurns() {
			t.Fatal("API switching allowed during", state)
		}
	}
	if _, _, err := a.Open(context.Background(), id); err != nil || a.ActiveRemoteTurns() {
		t.Fatal("fresh idle snapshot did not clear the source-switch guard", err)
	}
	a.notify(f, "gateway.disconnected", nil)
	if !a.ActiveRemoteTurns() {
		t.Fatal("detached viewer's native work was assumed stopped")
	}
}
