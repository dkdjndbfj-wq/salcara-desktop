package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

func claudeSyntheticPermissionWorker(t *testing.T, a *claudeAgent, id string) *claudeProc {
	t.Helper()
	lease, err := newClaudePermissionLease()
	if err != nil {
		t.Fatal("cannot create synthetic helper capability", err)
	}
	p := &claudeProc{id: id, cwd: t.TempDir(), done: make(chan struct{}), stopCh: make(chan struct{}), permissionLease: lease}
	a.mu.Lock()
	a.procs[id] = p
	a.workers[p] = struct{}{}
	a.mu.Unlock()
	return p
}

func claudeScopedPermissionRequest(a *claudeAgent, p *claudeProc, tool, input string, ctx context.Context) *httptest.ResponseRecorder {
	body, _ := json.Marshal(PermissionRequest{Session: p.id, ToolName: tool, Input: json.RawMessage(input)})
	r := httptest.NewRequest(http.MethodPost, PermissionPath, strings.NewReader(string(body))).WithContext(ctx)
	r.Header.Set("X-Salcara-Token", p.permissionLease.token)
	w := httptest.NewRecorder()
	a.permissionHandler(w, r)
	return w
}

func TestClaudePermissionLeaseRetiresApprovalBeforeExitCleanup(t *testing.T) {
	for _, tool := range []string{"Bash", "AskUserQuestion"} {
		t.Run(tool, func(t *testing.T) {
			rec := &recorder{}
			a := newClaudeAgent(rec.sink, func() Settings { return Settings{Approval: "ask"} }, "not-launched", t.TempDir())
			t.Cleanup(a.Close)
			old := claudeSyntheticPermissionWorker(t, a, "fixture-session")
			input := `{}`
			if tool == "AskUserQuestion" {
				input = `{"questions":[{"header":"Name","question":"Name?","options":[]}]}`
			}
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() { result <- claudeScopedPermissionRequest(a, old, tool, input, context.Background()) }()
			ev := rec.wait(t, "worker-scoped permission", func(e protocol.Event) bool { return e.Type == "approval.request" })
			old.permissionLease.revoke()
			// Deliberately replace without waiting for onExit/cancelSession: the
			// immutable source must exclude the request immediately on revoke.
			next := claudeSyntheticPermissionWorker(t, a, old.id)
			if a.aps.countFor(ev.SessionKey) != 0 || len(a.aps.snapshot(ev.SessionKey)) != 0 {
				t.Fatal("closed helper request remained busy or visible")
			}
			if a.Respond(ev.ApprovalID, "allow_session", "") || a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: map[string][]string{"q_0": {"Name"}}}) {
				t.Fatal("replacement answered previous worker's request")
			}
			select {
			case got := <-result:
				var dec PermissionDecision
				if got.Code != http.StatusOK || json.Unmarshal(got.Body.Bytes(), &dec) != nil || dec.Behavior != "deny" {
					t.Fatal("old helper did not receive fail-closed cancellation")
				}
			case <-time.After(time.Second):
				t.Fatal("closed helper request did not retire promptly")
			}
			a.mu.Lock()
			allowed := a.allowed[next.id][tool]
			a.mu.Unlock()
			if allowed {
				t.Fatal("old reply changed replacement's allow_session policy")
			}
			current := make(chan *httptest.ResponseRecorder, 1)
			go func() { current <- claudeScopedPermissionRequest(a, next, "Bash", `{}`, context.Background()) }()
			live := rec.wait(t, "replacement permission", func(e protocol.Event) bool { return e.Type == "approval.request" && e.ApprovalID != ev.ApprovalID })
			if a.Respond(live.ApprovalID, "allow_session", "") {
				t.Fatal("legacy helper accepted a broad session grant")
			}
			if !a.Respond(live.ApprovalID, "allow", "") {
				t.Fatal("current worker permission was not answerable")
			}
			select {
			case got := <-current:
				var dec PermissionDecision
				if json.Unmarshal(got.Body.Bytes(), &dec) != nil || dec.Behavior != "allow" {
					t.Fatal("current permission was not accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("current helper answer not delivered")
			}
		})
	}
}

func TestClaudePermissionLeaseRejectsLateRegistrationAndClosedAgent(t *testing.T) {
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return Settings{Approval: "auto_all"} }, "not-launched", t.TempDir())
	p := claudeSyntheticPermissionWorker(t, a, "fixture-session")
	p.permissionLease.revoke()
	if got := claudeScopedPermissionRequest(a, p, "Bash", `{}`, context.Background()); got.Code != http.StatusForbidden {
		t.Fatal("closed capability accepted a late auto-approved tool")
	}
	a.Close()
	if got := claudeScopedPermissionRequest(a, p, "Bash", `{}`, context.Background()); got.Code != http.StatusForbidden {
		t.Fatal("closed agent accepted a helper request")
	}
}

func TestClaudePermissionLeaseRejectsRetirementReservationBeforeCleanup(t *testing.T) {
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings { return Settings{Approval: "ask"} }, "not-launched", t.TempDir())
	t.Cleanup(a.Close)
	p := claudeSyntheticPermissionWorker(t, a, "fixture-retiring")
	// Model the reservation boundary before close(stopCh) or lease cleanup.
	a.mu.Lock()
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	a.mu.Unlock()
	if p.transportRetired() || !approvalSourceOpen(p.permissionLease.done) {
		t.Fatal("fixture already crossed the cleanup boundary")
	}
	if a.permissionWorkerActive(p) || a.permissionWorkerForToken(p.id, p.permissionLease.token) != nil {
		t.Fatal("retiring worker retained permission admission")
	}
	if got := claudeScopedPermissionRequest(a, p, "Bash", `{}`, context.Background()); got.Code != http.StatusForbidden {
		t.Fatal("retirement reservation accepted a late permission request")
	}
	if len(rec.all()) != 0 || a.aps.countFor("claude:"+p.id) != 0 {
		t.Fatal("retiring worker published a pending approval")
	}
}

func TestClaudeExitedWorkerBlocksSameSessionUntilCleanupDone(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	a := newClaudeAgent(func(e protocol.Event) {
		if e.Type == "turn" && e.Status == "failed" {
			close(entered)
			<-release
		}
	}, func() Settings { return Settings{} }, "not-launched", t.TempDir())
	p := claudeSyntheticPermissionWorker(t, a, "fixture-session")
	p.turns = 1
	finished := make(chan struct{})
	go func() { a.onExit(p, nil); close(finished) }()
	t.Cleanup(func() { close(release); <-finished; a.Close() })
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("synthetic cleanup did not reach its terminal event")
	}
	if next, err := a.spawn(context.Background(), p.id, p.cwd, "", "ask", true); next != nil || err == nil || !strings.Contains(err.Error(), "正在启动或运行") {
		t.Fatal("confirmed exit released same-session startup before approval cleanup settled")
	}
}

func TestClaudeLosingWorkerCleanupPreservesReplacementApproval(t *testing.T) {
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings { return Settings{} }, "not-launched", t.TempDir())
	t.Cleanup(a.Close)
	old := claudeSyntheticPermissionWorker(t, a, "fixture-session")
	next := claudeSyntheticPermissionWorker(t, a, old.id)
	pending, _ := a.aps.addValidatedRequestForSource("claude:"+next.id, "claude", "", protocol.Event{Kind: "command"}, nil, next.permissionLease.done)
	a.onExit(old, nil)
	a.mu.Lock()
	current := a.procs[next.id]
	a.mu.Unlock()
	if current != next || !a.permissionWorkerActive(next) || !a.Respond(pending.id, "allow", "") {
		t.Fatal("losing startup cleanup consumed replacement worker or approval")
	}
	if len(rec.all()) != 0 || approvalSourceOpen(old.permissionLease.done) {
		t.Fatal("losing worker published terminal state or retained its helper capability")
	}
}
