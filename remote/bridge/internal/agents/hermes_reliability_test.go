package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"salcara/bridge/internal/protocol"
	"strings"
	"testing"
	"time"
)

const hermesTestOperation = "11111111-1111-4111-8111-111111111111"

func TestHermesPermissionRevocationAtNativeBoundaries(t *testing.T) {
	for _, stage := range []string{"create", "submit"} {
		t.Run(stage, func(t *testing.T) {
			a, f, _, snapshot := hermesFixture(t)
			revoked := stage == "create"
			original := f.call
			f.call = func(method string, p map[string]any) (any, error) {
				if stage == "submit" && method == "session.resume" {
					revoked = true
				}
				return original(method, p)
			}
			guard := func() error {
				if revoked {
					return errors.New("permission_mode_denied: fixture revoked")
				}
				return nil
			}
			id, err := a.StartWithOptions(context.Background(), snapshot.Info.Cwd, "task", "auto_all", TurnOptions{ValidatePermission: guard})
			if err == nil || !strings.Contains(err.Error(), "fixture revoked") {
				t.Fatal(id, err)
			}
			if protocol.ExecutionErrorCode(err) == "command_delivery_uncertain" {
				t.Fatal("unsent refusal became uncertain")
			}
			for _, method := range f.calls {
				if method == "prompt.submit" || stage == "create" && method == "session.create" {
					t.Fatal("revoked operation dispatched", f.calls)
				}
			}
			if stage == "submit" && id == "" {
				t.Fatal("created native identity lost")
			}
		})
	}
}

func TestExtensionInheritedFullAccessAndHermesPolicyPersistence(t *testing.T) {
	a, _, settings, snapshot := hermesFixture(t)
	id, err := a.Start(context.Background(), snapshot.Info.Cwd, "task", "", "auto_all")
	if err != nil || !a.SessionRequiresFullAccessConsent(id) {
		t.Fatal(err)
	}
	restored := newHermesAgent(func() Settings { return *settings }, filepath.Dir(a.stateDir))
	defer restored.Close()
	if !restored.SessionRequiresFullAccessConsent(id) {
		t.Fatal("persisted yolo missing")
	}
	restored.sessions[id].Policy = ""
	if !restored.SessionRequiresFullAccessConsent(id) {
		t.Fatal("legacy policy silently granted")
	}
	ds := &deepSeekAgent{sessions: map[string]*deepSeekSession{"full": {Policy: "auto_all"}, "ask": {Policy: "ask"}, "legacy": {}}}
	if !ds.SessionRequiresFullAccessConsent("full") || !ds.SessionRequiresFullAccessConsent("legacy") || ds.SessionRequiresFullAccessConsent("ask") {
		t.Fatal("deepseek policy observation")
	}
}

func TestHermesRuntimeRecoveryRestoresControl(t *testing.T) {
	a, f, _, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	a.conn = nil
	a.dial = func(context.Context, func(string, json.RawMessage)) (hermesGateway, error) {
		return nil, errors.New("fixture unavailable")
	}
	info, _, err := a.Open(context.Background(), id)
	if err != nil || info.Controllable {
		t.Fatal(info, err)
	}
	a.conn = f
	info, _, err = a.Open(context.Background(), id)
	if err != nil || info.Status != "idle" || !info.Controllable {
		t.Fatal(info, err)
	}
}

func hermesPendingFixture(t *testing.T) (*hermesAgent, *hermesFixtureGateway, *Settings, *hermesSnapshot, string, string) {
	t.Helper()
	a, f, settings, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	snapshot.Generation, snapshot.Running = 2, true
	snapshot.Prompts = []hermesPrompt{{ID: "prompt-1", Kind: "approval", Generation: 2, Choices: []string{"once", "deny"}}}
	if _, _, err := a.Open(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return a, f, settings, snapshot, id, "hermes:" + id + ":prompt-1"
}

func TestHermesApprovalUncertaintyPersistsAndNeverRedispatches(t *testing.T) {
	a, f, settings, snapshot, id, approval := hermesPendingFixture(t)
	original := f.call
	accepted := 0
	f.call = func(method string, p map[string]any) (any, error) {
		if method == "approval.respond" {
			accepted++
			return nil, &rpcDeliveryUncertainError{message: "lost acknowledgement"}
		}
		return original(method, p)
	}
	response := ApprovalResponse{Decision: "allow", OperationID: hermesTestOperation}
	ok, err := a.RespondForSessionResult(context.Background(), "hermes:"+id, approval, response)
	if ok || protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" || accepted != 1 {
		t.Fatal(ok, err, accepted)
	}
	for _, next := range []ApprovalResponse{response, {Decision: "deny", OperationID: "22222222-2222-4222-8222-222222222222"}} {
		if ok, err = a.RespondForSessionResult(context.Background(), "hermes:"+id, approval, next); ok || protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" {
			t.Fatal(ok, err)
		}
	}
	if accepted != 1 {
		t.Fatal("duplicate native approval", accepted)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if ok, err = a.RespondForSessionResult(cancelled, "hermes:"+id, approval, response); ok || protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" || accepted != 1 {
		t.Fatal("cancelled retry erased the prior attempted operation", ok, err, accepted)
	}
	_, events, err := a.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == "approval.request" {
			t.Fatal("uncertain approval revived")
		}
	}
	a.Close()
	f.detached = false
	restored := newHermesAgent(func() Settings { return *settings }, filepath.Dir(a.stateDir))
	defer restored.Close()
	restored.dial = func(context.Context, func(string, json.RawMessage)) (hermesGateway, error) { return f, nil }
	snapshot.Prompts = nil
	if _, events, err = restored.Open(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	settled := false
	for _, e := range events {
		if e.Type == "approval.resolved" {
			settled = e.Decision == "unknown" && e.By == "unknown" && e.OperationID == hermesTestOperation
		}
	}
	if !settled {
		t.Fatal("native absence fabricated or lost outcome", events)
	}
	if ok, err = restored.RespondForSessionResult(context.Background(), "hermes:"+id, approval, response); ok || protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" || accepted != 1 {
		t.Fatal(ok, err, accepted)
	}
}

func TestHermesApprovalSettledBeforeAckAndLateEventPreserveConfirmedDecision(t *testing.T) {
	a, f, _, snapshot, id, approval := hermesPendingFixture(t)
	s := a.sessions[id]
	original := f.call
	f.call = func(method string, p map[string]any) (any, error) {
		if method == "approval.respond" {
			a.event(f, id, s, hermesNativeEvent{SessionID: s.NativeID, Type: "approval.settled", Generation: 2, Replay: snapshot.Replay, Sequence: 1, Payload: json.RawMessage(`{"prompt_id":"prompt-1"}`)})
		}
		return original(method, p)
	}
	response := ApprovalResponse{Decision: "allow", OperationID: hermesTestOperation}
	if ok, err := a.RespondForSessionResult(context.Background(), s.Info.SessionKey, approval, response); !ok || err != nil {
		t.Fatal(ok, err)
	}
	before := len(f.calls)
	if ok, err := a.RespondForSessionResult(context.Background(), s.Info.SessionKey, approval, response); !ok || err != nil || len(f.calls) != before {
		t.Fatal("confirmed retry redispatched", ok, err)
	}
	a.event(f, id, s, hermesNativeEvent{SessionID: s.NativeID, Type: "approval.settled", Generation: 2, Replay: snapshot.Replay, Sequence: 2, Payload: json.RawMessage(`{"prompt_id":"prompt-1"}`)})
	last := protocol.Event{}
	for _, e := range s.Events {
		if e.Type == "approval.resolved" {
			last = e
		}
	}
	if last.Decision != "allow" || last.By != "phone" {
		t.Fatal("late native settlement downgraded proven result", last)
	}
}

func TestHermesApprovalPreDispatchStorageAndDefinitiveRejection(t *testing.T) {
	a, f, _, _, id, approval := hermesPendingFixture(t)
	original := f.call
	f.call = func(method string, p map[string]any) (any, error) {
		if method == "approval.respond" {
			return nil, &rpcError{Code: 4001, Message: "permission_denied"}
		}
		return original(method, p)
	}
	response := ApprovalResponse{Decision: "allow", OperationID: hermesTestOperation}
	if ok, err := a.RespondForSessionResult(context.Background(), "hermes:"+id, approval, response); ok || protocol.ExecutionErrorCode(err) != "approval_rejected" {
		t.Fatal(ok, err)
	}
	if len(a.sessions[id].Approvals) != 0 {
		t.Fatal("definitive refusal retained uncertain write")
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	a.stateDir = blocker
	before := len(f.calls)
	if ok, err := a.RespondForSessionResult(context.Background(), "hermes:"+id, approval, response); ok || protocol.ExecutionErrorCode(err) != "approval_not_sent" || len(f.calls) != before {
		t.Fatal("storage failure dispatched", ok, err)
	}
}

func TestHermesApprovalRPCClassificationDoesNotReleaseAmbiguousWrites(t *testing.T) {
	for _, tc := range []struct {
		name, message, want string
		code                int64
	}{
		{"native-preflight", "stale_generation", "approval_rejected", 4001},
		{"native-validation", "invalid_params", "approval_rejected", 4001},
		{"local-capacity", "gateway_busy", "approval_not_sent", -32002},
		{"internal", "internal_error", "command_delivery_uncertain", -32603},
		{"storage", "storage_unavailable", "command_delivery_uncertain", 5001},
		{"unknown-reason", "future_reason", "command_delivery_uncertain", 4001},
		{"wrong-code", "permission_denied", "command_delivery_uncertain", -32603},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, f, _, _, id, approval := hermesPendingFixture(t)
			original, attempts := f.call, 0
			f.call = func(method string, p map[string]any) (any, error) {
				if method == "approval.respond" {
					attempts++
					return nil, &rpcError{Code: tc.code, Message: tc.message}
				}
				return original(method, p)
			}
			response := ApprovalResponse{Decision: "allow", OperationID: hermesTestOperation}
			if ok, err := a.RespondForSessionResult(context.Background(), "hermes:"+id, approval, response); ok || protocol.ExecutionErrorCode(err) != tc.want || attempts != 1 {
				t.Fatal(ok, err, attempts)
			}
			if tc.want == "command_delivery_uncertain" {
				response.OperationID = "22222222-2222-4222-8222-222222222222"
				if ok, err := a.RespondForSessionResult(context.Background(), "hermes:"+id, approval, response); ok || protocol.ExecutionErrorCode(err) != tc.want || attempts != 1 {
					t.Fatal("ambiguous native error released retry lock", ok, err, attempts)
				}
			} else if len(a.sessions[id].Approvals) != 0 {
				t.Fatal("proven refusal retained pending attempt")
			}
		})
	}
}

func TestHermesClarifyAnswerOwnershipExpiryAndNativeShape(t *testing.T) {
	a, f, _, snapshot := hermesFixture(t)
	original := f.call
	f.call = func(method string, p map[string]any) (any, error) {
		if method == "salcara.initialize" {
			return map[string]any{"protocol": "salcara-hermes-gateway-v1", "questions": true}, nil
		}
		if method == "clarify.respond" {
			return map[string]string{"status": "resolved", "prompt_id": p["prompt_id"].(string)}, nil
		}
		return original(method, p)
	}
	id := hermesStart(t, a, snapshot)
	if _, batch := f.params[len(f.params)-1]["clarify_batch"]; batch {
		t.Fatal("Bridge must use native per-question cards, not unimplemented server-request batches")
	}
	snapshot.Generation, snapshot.Running = 3, true
	snapshot.Prompts = []hermesPrompt{{ID: "question-1", Kind: "clarify", Generation: 3, Question: "Pick targets", Choices: []string{"a,b", "c"}, MultiSelect: true, TimeoutSeconds: 60}}
	_, events, err := a.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Descriptor().Capabilities.Questions || len(events) != 1 || events[0].Kind != "question" || events[0].ExpiresAt <= time.Now().UnixMilli() {
		t.Fatal(events)
	}
	expiry := events[0].ExpiresAt
	if _, events, err = a.Open(context.Background(), id); err != nil || events[0].ExpiresAt != expiry {
		t.Fatal("reconnect extended deadline")
	}
	response := ApprovalResponse{Decision: "allow", OperationID: hermesTestOperation, Answers: map[string][]string{"question-1": {"a,b", "c"}}}
	approval := "hermes:" + id + ":question-1"
	before := len(f.calls)
	if ok, _ := a.RespondForSessionResult(context.Background(), "hermes:foreign", approval, response); ok || len(f.calls) != before {
		t.Fatal("foreign answer sent")
	}
	a.sessions[id].PromptDeadlines["question-1"] = time.Now().Add(-time.Second).UnixMilli()
	if ok, err := a.RespondForSessionResult(context.Background(), "hermes:"+id, approval, response); ok || protocol.ExecutionErrorCode(err) != "approval_not_sent" || len(f.calls) != before {
		t.Fatal("expired answer sent")
	}
	a.sessions[id].PromptDeadlines["question-1"] = expiry
	if ok, err := a.RespondForSessionResult(context.Background(), "hermes:"+id, approval, response); !ok || err != nil {
		t.Fatal(ok, err)
	}
	params := f.params[len(f.params)-1]
	if f.calls[len(f.calls)-1] != "clarify.respond" || params["answer"] != `["a,b","c"]` || params["execution_generation"] != float64(3) {
		t.Fatal(params)
	}
}

func TestHermesControlReadOmitsHistoryWithoutErasingCachedTranscript(t *testing.T) {
	a, f, _, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	s := a.sessions[id]
	s.Events = []protocol.Event{{Tool: hermesID, Type: "message", ID: "cached", Role: "assistant", Text: "retained transcript", Final: true}}
	snapshot.Generation, snapshot.Running = 3, true
	original := f.call
	f.call = func(method string, p map[string]any) (any, error) {
		if method == "session.resume" && p["omit_messages"] != true {
			t.Fatal("stop requires only native ownership/status, not the full transcript")
		}
		return original(method, p)
	}
	result, err := a.InterruptTurn(context.Background(), id, "hermes-7-3", hermesTestOperation)
	if err != nil || result.Status != "requested" {
		t.Fatal(result, err)
	}
	found := false
	for _, event := range s.Events {
		found = found || event.ID == "cached" && event.Text == "retained transcript"
	}
	if !found {
		t.Fatal("control read erased cached history", s.Events)
	}
}

func TestHermesNativeEventsEmitMobileContract(t *testing.T) {
	a, f, _, snapshot := hermesFixture(t)
	id := hermesStart(t, a, snapshot)
	s := a.sessions[id]
	for index, row := range []struct{ kind, payload string }{
		{"tool.complete", `{"tool_call_id":"tool-1","tool_name":"fixture","result":"done"}`},
		{"approval.settled", `{"prompt_id":"native-prompt"}`},
		{"message.complete", `{"outcome":"future-native-outcome"}`},
	} {
		a.event(f, id, s, hermesNativeEvent{SessionID: s.NativeID, Type: row.kind, Generation: s.Generation, Replay: snapshot.Replay, Sequence: int64(index + 1), Payload: json.RawMessage(row.payload)})
	}
	a.emit(id, protocol.Event{Type: "notice", Text: "legacy empty severity"})
	tool, settled, warn, info := false, false, false, false
	for _, event := range s.Events {
		switch event.Type {
		case "tool":
			tool = event.Status == "done" && event.OutputFinal
		case "approval.resolved":
			settled = event.Decision == "unknown" && event.By == "unknown"
		case "notice":
			warn = warn || event.Level == "warn"
			info = info || event.Level == "info"
		}
	}
	if !tool || !settled || !warn || !info {
		t.Fatal("native event violated mobile contract", s.Events)
	}
}
