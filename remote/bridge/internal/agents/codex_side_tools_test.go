package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

// Only an in-process RPC transport is used. These tests never launch Codex or
// contact a model/provider, including the tests for unconfirmed delivery.
type sideToolsTransport struct {
	mu            sync.Mutex
	conn          *rpcConn
	settings      Settings
	cwd           string
	requests      []map[string]any
	unsafe        string
	onTurn        func()
	turns         int
	missingSkills bool
}

func (f *sideToolsTransport) Close() error {
	f.conn.shutdown(errors.New("fixture closed"))
	return nil
}
func (f *sideToolsTransport) Write(raw []byte) (int, error) {
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return 0, err
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	method, _ := req["method"].(string)
	var result any
	switch method {
	case "config/read":
		cfg, _ := guardedProviderConfig(f.settings)
		if f.settings.codexProvider() == "openai" {
			cfg = map[string]any{"model_provider": "openai", "openai_base_url": strings.TrimRight(f.settings.codexRoot(), "/") + "/v1", "cli_auth_credentials_store": "ephemeral", "forced_login_method": "api"}
		}
		if f.unsafe == "integration" {
			cfg["mcp_servers"] = map[string]any{"unexpected": map[string]any{"command": "never-run"}}
		}
		result = map[string]any{"config": cfg}
	case "thread/start":
		eph := f.unsafe != "persistent"
		provider := f.settings.codexProvider()
		if f.unsafe == "provider" {
			provider = "ambient"
		}
		result = map[string]any{"thread": map[string]any{"id": "private-tools-fixture", "cwd": f.cwd, "ephemeral": eph, "modelProvider": provider, "status": map[string]any{"type": "idle"}}, "cwd": f.cwd, "modelProvider": provider, "runtimeWorkspaceRoots": []string{f.cwd}, "approvalPolicy": "on-request", "approvalsReviewer": "user", "sandbox": map[string]any{"type": "workspaceWrite", "writableRoots": []string{f.cwd}, "networkAccess": false, "excludeTmpdirEnvVar": true, "excludeSlashTmp": true}}
	case "thread/goal/get":
		result = map[string]any{"goal": nil}
		if f.unsafe == "goal" {
			result = map[string]any{"goal": map[string]any{"objective": "never inherit"}}
		}
	case "collaborationMode/list":
		result = map[string]any{"data": []any{map[string]any{"mode": "default", "model": "side-model"}, map[string]any{"mode": "plan", "model": "side-model", "reasoning_effort": "medium"}}}
	case "skills/list":
		skills := []any{}
		if !f.missingSkills {
			skills = append(skills, map[string]any{"name": "inspect", "description": "Inspect the current project", "path": filepath.Join(f.cwd, ".agents", "skills", "inspect", "SKILL.md"), "enabled": true})
		}
		result = map[string]any{"data": []any{map[string]any{"cwd": f.cwd, "skills": skills}}}
	case "turn/start":
		f.turns++
		result = map[string]any{"turn": cxTurn{ID: fmt.Sprintf("private-turn-%d", f.turns), Status: "inProgress"}}
	case "turn/interrupt":
		result = map[string]any{}
	default:
		f.mu.Unlock()
		return 0, fmt.Errorf("unexpected private RPC: %s", method)
	}
	hook := f.onTurn
	unsafe := f.unsafe
	f.mu.Unlock()
	if method == "turn/start" && hook != nil {
		hook()
	}
	if method == "turn/start" && unsafe == "delivery" {
		return 0, errors.New("fixture interrupted write")
	}
	encoded, _ := json.Marshal(map[string]any{"id": req["id"], "result": result})
	f.conn.handleLine(encoded)
	return len(raw), nil
}
func (f *sideToolsTransport) calls(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := []map[string]any{}
	for _, req := range f.requests {
		if req["method"] == method {
			result = append(result, req["params"].(map[string]any))
		}
	}
	return result
}

type sideToolsFixture struct {
	parent    *codexSideFixture
	transport *sideToolsTransport
	worker    *codexAgent
	session   *codexSideToolSession
	request   protocol.SideQuestionToolRequest
	owner     string
	starts    atomic.Int32
}

func newSideToolsFixture(t *testing.T) *sideToolsFixture {
	t.Helper()
	f := &sideToolsFixture{parent: newCodexSideFixture(t, false, config.Config{}, "http://127.0.0.1:47831"), owner: "fixture-owner"}
	req := f.parent.request(t, 1)
	f.request = protocol.SideQuestionToolRequest{Action: "open", ChainID: "75000000-0000-4000-8000-000000000001", ExpectedRevision: req.ExpectedRevision, OperationID: "75000000-0000-4000-8000-000000000002", Permission: "workspace-write"}
	f.parent.a.sideTools.workerFactory = func(ctx context.Context, settings Settings, home string, sink Sink) (*codexAgent, error) {
		f.starts.Add(1)
		if !strings.Contains(home, "salcara-side-tools-") || settings.CodexKey != f.parent.cfg.LocalGatewayCredential("codex") {
			t.Error("lost private home or exact gateway credential")
		}
		child := newCodexAgent(sink, func() Settings { return settings }, "", "")
		child.privateHome = home
		conn := &rpcConn{pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
		conn.recordSessionInitialization(codexSessionInitialize{UserAgent: sessionActionNativeUA})
		transport := &sideToolsTransport{conn: conn, settings: settings, cwd: f.parent.a.threads[sessionActionThread].info.Cwd}
		if f.transport != nil {
			transport.unsafe = f.transport.unsafe
			transport.onTurn = f.transport.onTurn
		}
		conn.stdin = transport
		child.conn, child.connSig = conn, codexSig(settings)
		f.worker, f.transport = child, transport
		return child, nil
	}
	t.Cleanup(func() {
		// There is no OS worker in this fixture. Its absence is explicitly
		// arranged here; production retains reservations until OS exit proof.
		if f.worker != nil {
			f.worker.mu.Lock()
			f.worker.conn = nil
			f.worker.mu.Unlock()
			f.worker.Close()
		}
		if f.session != nil {
			f.session.mu.Lock()
			f.session.workerConn = nil
			f.session.mu.Unlock()
			f.session.close("fixture cleanup")
		}
	})
	return f
}
func (f *sideToolsFixture) call(t *testing.T, r protocol.SideQuestionToolRequest, guard func() error) (protocol.SideQuestionToolState, error) {
	t.Helper()
	state, err := f.parent.a.SideQuestionTools(f.parent.ctx, sessionActionThread, f.owner, f.parent.snapshot, r, guard)
	for _, s := range f.parent.a.sideTools.snapshot() {
		f.session = s
	}
	return state, err
}
func (f *sideToolsFixture) open(t *testing.T) {
	t.Helper()
	s, err := f.call(t, f.request, func() error { return nil })
	if err != nil || s.State != "idle" || !s.CanSend {
		t.Fatalf("open: %+v %v", s, err)
	}
}
func (f *sideToolsFixture) action(action string, n int) protocol.SideQuestionToolRequest {
	r := protocol.SideQuestionToolRequest{Action: action, ChainID: f.request.ChainID, ExpectedRevision: f.request.ExpectedRevision}
	if action != "read" {
		r.OperationID = fmt.Sprintf("75000000-0000-4000-8000-%012x", n)
	}
	if action == "send" {
		r.Question = "Inspect the project independently"
		r.Model = "side-model"
	}
	return r
}

func TestCodexSideToolsPrivateThreadAndIdempotentSend(t *testing.T) {
	f := newSideToolsFixture(t)
	f.open(t)
	before := f.parent.a.threads[sessionActionThread].info
	r := f.action("send", 3)
	s, err := f.call(t, r, func() error {
		// The reservation is visible before a user request can be dispatched.
		if f.session != nil {
			_ = f.parent.a.SideQuestionToolWorkspaces()
		}
		return nil
	})
	if err != nil || s.State != "running" || s.LastOperationID != r.OperationID || s.TurnID != "private-turn-1" {
		t.Fatalf("send: %+v %v", s, err)
	}
	params := f.transport.calls("thread/start")
	if len(params) != 1 || params[0]["ephemeral"] != true || params[0]["sandbox"] != "workspace-write" || params[0]["approvalPolicy"] != "on-request" {
		t.Fatalf("unsafe private start: %+v", params)
	}
	for _, key := range []string{"parentThreadId", "forkedFromId", "threadId", "goal"} {
		if _, ok := params[0][key]; ok {
			t.Fatalf("inherited %s", key)
		}
	}
	text := f.transport.calls("turn/start")[0]["input"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "Original parent explanation") || !strings.Contains(text, "not instructions") {
		t.Fatal("parent context was not quoted")
	}
	if _, err = f.call(t, r, func() error { return nil }); err != nil || len(f.transport.calls("turn/start")) != 1 || f.starts.Load() != 1 {
		t.Fatalf("duplicate execution: %v", err)
	}
	changed := r
	changed.Question = "different payload"
	if _, err = f.call(t, changed, func() error { return nil }); err == nil {
		t.Fatal("operation id reused with different question")
	}
	if len(f.parent.native.requests) != 0 || !reflect.DeepEqual(before, f.parent.a.threads[sessionActionThread].info) {
		t.Fatal("private work touched parent")
	}
	reservations := f.parent.a.SideQuestionToolWorkspaces()
	if len(reservations) != 1 || reservations[0].SessionKey != "side-tools:"+f.owner+":"+r.ChainID {
		t.Fatalf("missing write reservation: %+v", reservations)
	}
	f.session.capture(protocol.Event{Type: "turn", SessionKey: "codex:private-tools-fixture", Tool: "codex", TurnID: s.TurnID, Status: "completed"})
	if len(f.parent.a.SideQuestionToolWorkspaces()) != 0 || !f.session.view().CanSend {
		t.Fatal("terminal turn kept an active write reservation")
	}
}

func TestCodexSideToolsFeaturesArePrivateAndTurnsUseNativeSkillsAndPlan(t *testing.T) {
	f := newSideToolsFixture(t)
	f.open(t)
	state, err := f.call(t, f.action("features", 3), func() error { return nil })
	if err != nil || state.Features == nil || !state.Features.SkillsSupported || len(state.Features.Skills) != 1 || len(state.Features.CollaborationModes) != 2 {
		t.Fatalf("private catalog: %+v %v", state, err)
	}
	if len(f.transport.calls("turn/start")) != 0 || len(f.parent.native.requests) != 0 || len(f.parent.a.SideQuestionToolWorkspaces()) != 0 {
		t.Fatal("catalog lookup dispatched a turn or touched parent")
	}
	path := state.Features.Skills[0].Path
	state.Features.Skills[0].Name = "caller mutation"
	if f.session.view().Features.Skills[0].Name != "inspect" {
		t.Fatal("catalog view aliases private state")
	}
	r := f.action("send", 4)
	r.CollaborationMode, r.Effort = "plan", "high"
	r.Skills = []protocol.SkillReference{{Name: "inspect", Path: path}}
	if _, err := f.call(t, r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	calls := f.transport.calls("turn/start")
	if len(calls) != 1 || f.starts.Load() != 1 || calls[0]["threadId"] != "private-tools-fixture" {
		t.Fatalf("wrong worker/turn: %+v", calls)
	}
	mode := calls[0]["collaborationMode"].(map[string]any)
	settings := mode["settings"].(map[string]any)
	input := calls[0]["input"].([]any)
	if mode["mode"] != "plan" || settings["model"] != "side-model" || settings["reasoning_effort"] != "high" || len(input) != 2 || input[1].(map[string]any)["type"] != "skill" || input[1].(map[string]any)["path"] != path {
		t.Fatalf("features were not native turn input: %+v", calls[0])
	}
	if len(f.transport.calls("skills/list")) != 2 {
		t.Fatal("send did not refresh the private skills catalog")
	}
}

func TestCodexSideToolsRemovedSkillNeverSendsOrConsumesDraft(t *testing.T) {
	f := newSideToolsFixture(t)
	f.open(t)
	state, err := f.call(t, f.action("features", 3), func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	r := f.action("send", 4)
	r.Skills = []protocol.SkillReference{{Name: "inspect", Path: state.Features.Skills[0].Path}}
	f.transport.mu.Lock()
	f.transport.missingSkills = true
	f.transport.mu.Unlock()
	state, err = f.call(t, r, func() error { return nil })
	if err == nil || len(f.transport.calls("turn/start")) != 0 || !state.CanSend || state.State != "idle" || state.Features != nil || len(f.parent.a.SideQuestionToolWorkspaces()) != 0 {
		t.Fatalf("stale skill reached turn/start or froze draft: %+v %v", state, err)
	}
	r.Skills = nil
	if _, err := f.call(t, r, func() error { return nil }); err != nil || len(f.transport.calls("turn/start")) != 1 {
		t.Fatalf("unattempted operation was not retryable: %v", err)
	}
}

func TestCodexSideToolsFeatureRequestShape(t *testing.T) {
	f := newSideToolsFixture(t)
	valid := f.action("features", 3)
	if !ValidSideQuestionToolRequest(valid) {
		t.Fatal("features lookup rejected")
	}
	for _, mutate := range []func(*protocol.SideQuestionToolRequest){
		func(r *protocol.SideQuestionToolRequest) { r.OperationID = "" },
		func(r *protocol.SideQuestionToolRequest) { r.CollaborationMode = "plan" },
		func(r *protocol.SideQuestionToolRequest) { r.Skills = []protocol.SkillReference{{Name: "inspect"}} },
		func(r *protocol.SideQuestionToolRequest) { r.Question = "should not be sent" },
	} {
		r := valid
		mutate(&r)
		if ValidSideQuestionToolRequest(r) {
			t.Fatalf("invalid features lookup admitted: %+v", r)
		}
	}
	for _, mode := range []string{"plan", "default", "invented"} {
		r := f.action("send", 4)
		r.CollaborationMode = mode
		if ValidSideQuestionToolRequest(r) != (mode != "invented") {
			t.Fatalf("invalid mode validation: %s", mode)
		}
	}
}

func TestCodexSideToolsUnsafeWorkerNeverSendsQuestion(t *testing.T) {
	for _, unsafe := range []string{"persistent", "provider", "integration", "goal"} {
		t.Run(unsafe, func(t *testing.T) {
			f := newSideToolsFixture(t)
			f.transport = &sideToolsTransport{unsafe: unsafe}
			f.open(t)
			s, err := f.call(t, f.action("send", 3), func() error { return nil })
			if err == nil || s.State != "unconfirmed" || len(f.transport.calls("turn/start")) != 0 {
				t.Fatalf("unsafe environment executed: %+v %v", s, err)
			}
		})
	}
}

func TestCodexSideToolsAdmissionReservationAndReadPreservesGuard(t *testing.T) {
	f := newSideToolsFixture(t)
	f.open(t)
	var reject atomic.Bool
	guard := func() error {
		if reject.Load() {
			return errors.New("workspace conflict")
		}
		return nil
	}
	if _, err := f.call(t, f.action("send", 3), guard); err != nil {
		t.Fatal(err)
	}
	if _, err := f.call(t, f.action("read", 0), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	reject.Store(true)
	if f.session.checkExecution() == nil {
		t.Fatal("read replaced the occupied worker's send guard")
	}
	stop := f.action("stop", 4)
	stop.ExpectedTurnID = "private-turn-1"
	if _, err := f.call(t, stop, func() error { return nil }); err != nil {
		t.Fatalf("workspace conflict blocked private stop: %v", err)
	}
	interrupt := f.transport.calls("turn/interrupt")
	if len(interrupt) != 1 || interrupt[0]["threadId"] != "private-tools-fixture" || interrupt[0]["turnId"] != "private-turn-1" {
		t.Fatalf("wrong interrupt target: %+v", interrupt)
	}
	if len(f.parent.native.requests) != 0 {
		t.Fatal("private interrupt reached parent")
	}
}

func TestCodexSideToolsFailedAdmissionReleasesReservation(t *testing.T) {
	f := newSideToolsFixture(t)
	f.open(t)
	var observed atomic.Bool
	guard := func() error {
		for _, r := range f.parent.a.SideQuestionToolWorkspaces() {
			if r.SessionKey == "side-tools:"+f.owner+":"+f.request.ChainID {
				observed.Store(true)
				return errors.New("competing task")
			}
		}
		return nil
	}
	if _, err := f.call(t, f.action("send", 3), guard); err == nil {
		t.Fatal("failed admission was ignored")
	}
	if !observed.Load() || f.starts.Load() != 0 || len(f.parent.a.SideQuestionToolWorkspaces()) != 0 || !f.session.view().CanSend {
		t.Fatal("reservation did not fence startup or was not released")
	}
}

func TestCodexSideToolsUnknownDeliveryNeverResends(t *testing.T) {
	f := newSideToolsFixture(t)
	f.transport = &sideToolsTransport{unsafe: "delivery"}
	f.open(t)
	r := f.action("send", 3)
	s, err := f.call(t, r, func() error { return nil })
	if err != nil || s.State != "unconfirmed" || s.CanSend {
		t.Fatalf("ambiguous delivery not frozen: %+v %v", s, err)
	}
	if _, err = f.call(t, r, func() error { return nil }); err != nil || len(f.transport.calls("turn/start")) != 1 {
		t.Fatal("ambiguous turn/start was dispatched twice")
	}
	if _, err = f.call(t, f.action("send", 4), func() error { return nil }); err == nil {
		t.Fatal("new send bypassed unconfirmed turn")
	}
}

func TestCodexSideToolsCaptureIsBoundedAndSourceScoped(t *testing.T) {
	f := newSideToolsFixture(t)
	f.open(t)
	if _, err := f.call(t, f.action("send", 3), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 150; n++ {
		f.session.capture(protocol.Event{Type: "tool", ID: fmt.Sprint(n), SessionKey: "codex:private-tools-fixture", TurnID: "private-turn-1", Tool: "foreign", Output: strings.Repeat("x", 9000), ImageData: []string{"private-image"}, Attachments: []protocol.AttachmentRef{{}}})
	}
	f.session.capture(protocol.Event{Type: "message", Role: "assistant", SessionKey: "codex:private-tools-fixture", TurnID: "private-turn-1", Text: strings.Repeat("界", 40000)})
	f.session.capture(protocol.Event{Type: "message", Role: "assistant", SessionKey: "codex:somebody-else", TurnID: "private-turn-1", Text: "foreign source"})
	s := f.session.view()
	raw, _ := json.Marshal(s)
	if len(raw) > maxSideToolStateBytes || len(s.Events) > 128 || len(s.Answer) > 64<<10 || !s.Truncated || s.Answer == "foreign source" {
		t.Fatal("unbounded or foreign output")
	}
	for _, ev := range s.Events {
		if ev.SessionKey != "codex:"+sessionActionThread || ev.Tool != "codex" || len(ev.ImageData) > 0 || len(ev.Attachments) > 0 {
			t.Fatalf("private handles leaked: %+v", ev)
		}
	}
	s.Events[0].Output = "caller mutation"
	if f.session.view().Events[0].Output == "caller mutation" {
		t.Fatal("state view aliases private transcript")
	}
	if len(f.parent.native.requests) != 0 {
		t.Fatal("private output touched parent")
	}
}

func TestCodexSideToolsOpenDoesNotInheritRequestCancellation(t *testing.T) {
	f := newSideToolsFixture(t)
	ctx, cancel := context.WithCancel(f.parent.ctx)
	f.parent.ctx = ctx
	f.open(t)
	cancel()
	if err := f.session.check(); err != nil {
		t.Fatalf("request cancellation revoked independent lease: %v", err)
	}
	f.session.mu.Lock()
	f.session.state.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	f.session.mu.Unlock()
	if f.session.check() == nil {
		t.Fatal("expired private lease remained active")
	}
	f.session.close("expired fixture")
	if f.session.view().State != "closed" {
		t.Fatal("lease retirement did not close chain")
	}
	f.parent.ctx = context.WithoutCancel(ctx)
	if state, err := f.call(t, f.action("read", 0), func() error { return nil }); err != nil || state.State != "closed" {
		t.Fatalf("reconnected owner could not confirm retirement: %+v %v", state, err)
	}
}

func TestCodexSideToolsCleanupOnlyRemovesOwnedTemporaryHome(t *testing.T) {
	outside := t.TempDir()
	removeSideToolHome(outside)
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("unrelated directory removed")
	}
	home, err := os.MkdirTemp("", "salcara-side-tools-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { removeSideToolHome(home) })
	removeSideToolHome(home)
	if _, err = os.Stat(home); !os.IsNotExist(err) {
		t.Fatal("owned temporary home was not removed")
	}
}

func TestCodexSideToolsApprovalsAreIndependentAndBounded(t *testing.T) {
	f := newSideToolsFixture(t)
	f.open(t)
	if _, err := f.call(t, f.action("send", 3), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	parentPending, _ := f.parent.a.aps.addValidatedRequestForSource("codex:"+sessionActionThread, "codex", "parent-ref", protocol.Event{Kind: "command", TurnID: "parent-turn"}, nil, f.parent.a.conn.done)
	privatePending, ev := f.worker.aps.addValidatedRequestForSource("codex:private-tools-fixture", "codex", "private-ref", protocol.Event{Kind: "command", TurnID: "private-turn-1", Detail: "Inspect a project file", ApprovalOptions: &protocol.ApprovalOptions{Version: 1, Decisions: []string{"allow", "deny"}}}, nil, f.worker.conn.done)
	f.session.capture(ev)
	state := f.session.view()
	if len(state.Approvals) != 1 || state.Approvals[0].SessionKey != "codex:"+sessionActionThread || state.State != "waiting_approval" {
		t.Fatalf("wrong private approval state: %+v", state)
	}
	r := f.action("approve", 4)
	r.ExpectedTurnID, r.ApprovalID, r.Decision = "private-turn-1", parentPending.id, "allow"
	if _, err := f.call(t, r, func() error { return nil }); err == nil {
		t.Fatal("temporary session consumed parent's approval")
	}
	r.ApprovalID = privatePending.id
	if _, err := f.call(t, r, func() error { return nil }); err != nil {
		t.Fatalf("private approval failed: %v", err)
	}
	if answer := <-privatePending.ch; answer.decision != "allow" {
		t.Fatal("private decision changed")
	}
	select {
	case <-parentPending.ch:
		t.Fatal("private action answered parent approval")
	default:
	}
	large, ev := f.worker.aps.addValidatedRequestForSource("codex:private-tools-fixture", "codex", "large-ref", protocol.Event{Kind: "command", TurnID: "private-turn-1", Detail: strings.Repeat("x", 9000), ApprovalOptions: &protocol.ApprovalOptions{Version: 1, Decisions: []string{"allow", "deny"}}}, nil, f.worker.conn.done)
	f.session.capture(ev)
	visible := f.session.view().Approvals[0]
	if approvalDecisionAllowed(visible.ApprovalOptions, "allow") || visible.Detail == strings.Repeat("x", 9000) {
		t.Fatal("truncated evidence remained executable")
	}
	r = f.action("approve", 5)
	r.ExpectedTurnID, r.ApprovalID, r.Decision = "private-turn-1", large.id, "allow"
	if _, err := f.call(t, r, func() error { return nil }); err == nil {
		t.Fatal("allowed request whose evidence was hidden")
	}
	r.Decision = "deny"
	if _, err := f.call(t, r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if answer := <-large.ch; answer.decision != "deny" {
		t.Fatal("denial was not delivered to private worker")
	}
	question, ev := f.worker.aps.addValidatedRequestForSource("codex:private-tools-fixture", "codex", "question-ref", protocol.Event{Kind: "question", TurnID: "private-turn-1", ApprovalOptions: &protocol.ApprovalOptions{Version: 1, Decisions: []string{"deny"}}}, nil, f.worker.conn.done)
	f.session.capture(ev)
	visible = f.session.view().Approvals[0]
	if visible.Kind != "tool" || approvalDecisionAllowed(visible.ApprovalOptions, "allow") || !approvalDecisionAllowed(visible.ApprovalOptions, "deny") {
		t.Fatal("unsupported question did not expose its independent denial action")
	}
	r = f.action("approve", 6)
	r.ExpectedTurnID, r.ApprovalID, r.Decision = "private-turn-1", question.id, "deny"
	if _, err := f.call(t, r, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
