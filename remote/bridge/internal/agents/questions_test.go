package agents

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

// Requests/replies are entirely in-process; no executable, user config or
// provider is contacted by these interaction fixtures.
type questionReplyCapture struct{ replies chan rpcMessage }

type legacyQuestionAdapter struct{ Agent }

func TestPhoneQuestionLegacyAdapterCannotDiscardSubmittedAnswers(t *testing.T) {
	a, _, _, _ := newCodexQuestionFixture(t, "ask")
	p, _ := a.aps.addRequest("codex:fixture-thread", "codex", "fixture", protocol.Event{Kind: "command", Title: "Fixture command"})
	legacy := legacyQuestionAdapter{a}
	if RespondWithAnswers(legacy, p.id, ApprovalResponse{Decision: "allow", Answers: map[string][]string{}}) {
		t.Fatal("old adapter discarded submitted question payload")
	}
	if !RespondWithAnswers(legacy, p.id, ApprovalResponse{Decision: "allow_session"}) {
		t.Fatal("old command approval compatibility lost")
	}
	if ans := <-p.ch; ans.decision != "allow_session" {
		t.Fatal("command approval decision changed")
	}
}

func (c *questionReplyCapture) Close() error { return nil }
func (c *questionReplyCapture) Write(data []byte) (int, error) {
	var reply rpcMessage
	if err := json.Unmarshal(data, &reply); err != nil {
		return 0, err
	}
	c.replies <- reply
	return len(data), nil
}

func newCodexQuestionFixture(t *testing.T, policy string) (*codexAgent, *rpcConn, *recorder, <-chan rpcMessage) {
	t.Helper()
	rec := &recorder{}
	a := newCodexAgent(rec.sink, func() Settings { return Settings{Approval: policy} }, "", "")
	thread := a.threadLocked("fixture-thread")
	thread.turnID, thread.policy = "fixture-turn", policy
	capture := &questionReplyCapture{replies: make(chan rpcMessage, 8)}
	c := &rpcConn{stdin: capture, pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
	t.Cleanup(a.Close)
	return a, c, rec, capture.replies
}

func startQuestionRequest(a *codexAgent, c *rpcConn, method, body string) <-chan struct{} {
	done := make(chan struct{})
	go func() { defer close(done); a.onRequest(c, json.RawMessage("7"), method, json.RawMessage(body)) }()
	return done
}

func questionFixtureWait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("question request did not finish")
	}
}

func questionFixtureReply(t *testing.T, replies <-chan rpcMessage) map[string]any {
	t.Helper()
	select {
	case reply := <-replies:
		if reply.Error != nil {
			t.Fatalf("unexpected RPC error: %+v", reply.Error)
		}
		var result map[string]any
		if err := json.Unmarshal(reply.Result, &result); err != nil {
			t.Fatal(err)
		}
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("missing question reply")
		return nil
	}
}

const codexQuestionFixture = `{"threadId":"fixture-thread","turnId":"fixture-turn","itemId":"fixture-item","questions":[{"id":"database","header":"Database","question":"Choose a database","isOther":true,"options":[{"label":"SQLite","description":"Local"},{"label":"Postgres","description":"Server"}]}]}`

func TestPhoneQuestionCodexReturnsActualAnswersNotAutomaticPolicy(t *testing.T) {
	a, c, rec, replies := newCodexQuestionFixture(t, "auto_all")
	done := startQuestionRequest(a, c, "item/tool/requestUserInput", codexQuestionFixture)
	ev := rec.wait(t, "question", func(e protocol.Event) bool { return e.Type == "approval.request" })
	if ev.Kind != "question" || ev.QuestionMode != "codex" || len(ev.Questions) != 1 || ev.Questions[0].ID != "database" || ev.ExpiresAt <= nowMs() {
		t.Fatalf("wrong question event: %+v", ev)
	}
	select {
	case <-replies:
		t.Fatal("auto_all answered a question")
	default:
	}
	if a.Respond(ev.ApprovalID, "allow", "") {
		t.Fatal("legacy empty approval answered a question")
	}
	if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow_session", Answers: map[string][]string{"database": {"SQLite"}}}) {
		t.Fatal("question granted session permission")
	}
	if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: map[string][]string{"unknown": {"SQLite"}}}) {
		t.Fatal("unknown question was accepted")
	}
	if len(a.aps.snapshot(ev.SessionKey)) != 1 {
		t.Fatal("invalid input consumed pending request")
	}
	if !RespondWithAnswers(a, ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: map[string][]string{"database": {"My own database"}}}) {
		t.Fatal("custom answer not accepted")
	}
	questionFixtureWait(t, done)
	result := questionFixtureReply(t, replies)
	want := map[string]any{"answers": map[string]any{"database": map[string]any{"answers": []any{"My own database"}}}}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("wrong Codex response: %+v", result)
	}
	if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: map[string][]string{"database": {"SQLite"}}}) || len(a.aps.snapshot(ev.SessionKey)) != 0 {
		t.Fatal("answered question replayed")
	}
}

func TestPhoneQuestionCodexShortDeadlineAndWithdrawal(t *testing.T) {
	t.Run("short deadline", func(t *testing.T) {
		a, c, rec, replies := newCodexQuestionFixture(t, "ask")
		body := strings.Replace(codexQuestionFixture, `"threadId":`, `"autoResolutionMs":120,"threadId":`, 1)
		done := startQuestionRequest(a, c, "item/tool/requestUserInput", body)
		ev := rec.wait(t, "short question", func(e protocol.Event) bool { return e.Type == "approval.request" })
		if ev.ExpiresAt > nowMs()+200 {
			t.Fatal("native short timeout was extended")
		}
		questionFixtureWait(t, done)
		if answers := questionFixtureReply(t, replies)["answers"].(map[string]any); len(answers) != 0 {
			t.Fatal("timeout invented an answer")
		}
		if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: map[string][]string{"database": {"SQLite"}}}) {
			t.Fatal("late answer accepted")
		}
	})
	t.Run("native resolved", func(t *testing.T) {
		a, c, rec, replies := newCodexQuestionFixture(t, "ask")
		done := startQuestionRequest(a, c, "item/tool/requestUserInput", codexQuestionFixture)
		ev := rec.wait(t, "withdrawn question", func(e protocol.Event) bool { return e.Type == "approval.request" })
		a.aps.resolveByRef("7")
		questionFixtureWait(t, done)
		select {
		case <-replies:
			t.Fatal("withdrawn request received a second RPC reply")
		default:
		}
		if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: map[string][]string{"database": {"SQLite"}}}) {
			t.Fatal("withdrawn question accepted")
		}
	})
	t.Run("interrupted", func(t *testing.T) {
		a, c, rec, replies := newCodexQuestionFixture(t, "ask")
		done := startQuestionRequest(a, c, "item/tool/requestUserInput", codexQuestionFixture)
		ev := rec.wait(t, "interrupted question", func(e protocol.Event) bool { return e.Type == "approval.request" })
		a.aps.cancelSession(ev.SessionKey)
		questionFixtureWait(t, done)
		questionFixtureReply(t, replies)
		if len(a.aps.snapshot(ev.SessionKey)) != 0 || a.Respond(ev.ApprovalID, "allow", "") {
			t.Fatal("interrupted question resurrected")
		}
	})
	t.Run("connection closed", func(t *testing.T) {
		a, c, rec, replies := newCodexQuestionFixture(t, "ask")
		done := startQuestionRequest(a, c, "item/tool/requestUserInput", codexQuestionFixture)
		ev := rec.wait(t, "connection question", func(e protocol.Event) bool { return e.Type == "approval.request" })
		close(c.done)
		questionFixtureWait(t, done)
		select {
		case <-replies:
			t.Fatal("closed connection received a reply")
		default:
		}
		if len(a.aps.snapshot(ev.SessionKey)) != 0 || a.Respond(ev.ApprovalID, "allow", "") {
			t.Fatal("closed connection resurrected question")
		}
	})
}

func TestPhoneQuestionClaudeMapsTextKeysAndPreservesInput(t *testing.T) {
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings { return Settings{Approval: "auto_all"} }, "fixture-not-launched", t.TempDir())
	t.Cleanup(a.Close)
	// Even a stale session-wide allow rule must not bypass user input.
	a.allowed["fixture-thread"] = map[string]bool{"AskUserQuestion": true}
	input := json.RawMessage(`{"questions":[{"header":"Format","question":"How should I format?","options":[{"label":"Summary"},{"label":"Detailed"}],"multiSelect":false},{"header":"Sections","question":"Which sections?","options":[{"label":"Intro"},{"label":"Results"}],"multiSelect":true}],"metadata":{"keep":"original"},"answers":{"stale":"wrong"}}`)
	r := httptest.NewRequest("POST", PermissionPath, nil)
	decisions := make(chan PermissionDecision, 1)
	go func() {
		decisions <- a.decide(r, PermissionRequest{Session: "fixture-thread", ToolName: "AskUserQuestion", Input: input})
	}()
	ev := rec.wait(t, "Claude question", func(e protocol.Event) bool { return e.Type == "approval.request" })
	if ev.Kind != "question" || ev.QuestionMode != "claude" || !ev.Questions[1].MultiSelect || !ev.Questions[0].AllowCustom {
		t.Fatalf("wrong Claude event %+v", ev)
	}
	select {
	case <-decisions:
		t.Fatal("auto_all or old allow rule auto answered")
	default:
	}
	if !a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: map[string][]string{"q_0": {"My own format"}, "q_1": {"Intro", "Results"}}}) {
		t.Fatal("Claude answers rejected")
	}
	select {
	case decision := <-decisions:
		if decision.Behavior != "allow" {
			t.Fatalf("wrong decision %+v", decision)
		}
		var updated map[string]any
		if json.Unmarshal(decision.UpdatedInput, &updated) != nil {
			t.Fatal("bad updatedInput")
		}
		want := map[string]any{"How should I format?": "My own format", "Which sections?": "Intro, Results"}
		if !reflect.DeepEqual(updated["answers"], want) || !reflect.DeepEqual(updated["metadata"], map[string]any{"keep": "original"}) || len(updated["questions"].([]any)) != 2 {
			t.Fatalf("answers lost or wrong keys %+v", updated)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Claude response not delivered")
	}
}

func TestPhoneQuestionClaudeHelperDisconnectCancelsAndRejectsReplay(t *testing.T) {
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings { return Settings{Approval: "ask"} }, "fixture-not-launched", t.TempDir())
	t.Cleanup(a.Close)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("POST", PermissionPath, nil).WithContext(ctx)
	results := make(chan PermissionDecision, 1)
	go func() {
		results <- a.decide(r, PermissionRequest{Session: "fixture-thread", ToolName: "AskUserQuestion", Input: json.RawMessage(`{"questions":[{"header":"Name","question":"Name?","options":[]}]}`)})
	}()
	ev := rec.wait(t, "cancelled Claude question", func(e protocol.Event) bool { return e.Type == "approval.request" })
	cancel()
	select {
	case result := <-results:
		if result.Behavior != "deny" {
			t.Fatal("disconnect allowed tool")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper cancellation not observed")
	}
	if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: map[string][]string{"q_0": {"Name"}}}) || len(a.aps.snapshot(ev.SessionKey)) != 0 {
		t.Fatal("disconnected question resumed")
	}
}

func TestPhoneQuestionMCPPrimitiveFormReturnsTypedContent(t *testing.T) {
	a, c, rec, replies := newCodexQuestionFixture(t, "auto_all")
	body := `{"threadId":"fixture-thread","serverName":"fixture-server","mode":"form","message":"Fill this form","requestedSchema":{"type":"object","properties":{"name":{"type":"string","minLength":2,"maxLength":12,"pattern":"^[A-Za-z]+$"},"enabled":{"type":"boolean"},"count":{"type":"integer","minimum":1,"maximum":4},"color":{"type":"string","oneOf":[{"const":"red","title":"Red"},{"const":"blue","title":"Blue"}]},"optional":{"type":"number","minimum":null,"maximum":null}},"required":["name","enabled","count","color"]}}`
	done := startQuestionRequest(a, c, "mcpServer/elicitation/request", body)
	ev := rec.wait(t, "MCP question", func(e protocol.Event) bool { return e.Type == "approval.request" })
	if ev.Kind != "question" || ev.QuestionMode != "mcp-form" || ev.Title != "MCP · fixture-server" {
		t.Fatalf("server identity missing: %+v", ev)
	}
	answers := map[string][]string{"name": {"Ada"}, "enabled": {"false"}, "count": {"3"}, "color": {"blue"}}
	bad := cloneAnswers(answers)
	bad["count"] = []string{"2.5"}
	if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: bad}) {
		t.Fatal("fractional integer accepted")
	}
	bad = cloneAnswers(answers)
	bad["color"] = []string{"Green"}
	if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: bad}) {
		t.Fatal("unknown enum accepted")
	}
	bad = cloneAnswers(answers)
	bad["name"] = []string{"A"}
	if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: bad}) {
		t.Fatal("string constraint ignored")
	}
	bad = cloneAnswers(answers)
	bad["count"] = []string{"9007199254740993"}
	if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: bad}) {
		t.Fatal("integer silently lost precision")
	}
	if !a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: answers}) {
		t.Fatal("valid form rejected")
	}
	questionFixtureWait(t, done)
	result := questionFixtureReply(t, replies)
	want := map[string]any{"action": "accept", "content": map[string]any{"name": "Ada", "enabled": false, "count": float64(3), "color": "blue"}, "_meta": nil}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("not typed content: %+v", result)
	}
}

func TestPhoneQuestionMCPUnsafeRequestsFailClosed(t *testing.T) {
	for name, body := range map[string]string{
		"URL":                `{"mode":"url","url":"https://fixture.invalid/auth","message":"Sign in"}`,
		"nested":             `{"mode":"form","requestedSchema":{"type":"object","properties":{"data":{"type":"object","properties":{"value":{"type":"string"}}}}}}`,
		"secret field":       `{"mode":"form","requestedSchema":{"type":"object","properties":{"api_key":{"type":"string"}}}}`,
		"secret message":     `{"mode":"form","message":"Enter your password","requestedSchema":{"type":"object","properties":{"value":{"type":"string"}}}}`,
		"unknown validation": `{"mode":"form","requestedSchema":{"type":"object","properties":{"count":{"type":"number","multipleOf":2}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			a, c, rec, replies := newCodexQuestionFixture(t, "auto_all")
			body = strings.Replace(body, `{`, `{"threadId":"fixture-thread","serverName":"fixture-server",`, 1)
			done := startQuestionRequest(a, c, "mcpServer/elicitation/request", body)
			questionFixtureWait(t, done)
			result := questionFixtureReply(t, replies)
			if result["action"] != "decline" || result["content"] != nil {
				t.Fatalf("unsafe request accepted %+v", result)
			}
			if len(a.aps.snapshot("codex:fixture-thread")) != 0 || rec.count(func(e protocol.Event) bool { return e.Type == "approval.request" }) != 0 {
				t.Fatal("unsafe data sent to phone")
			}
			if rec.count(func(e protocol.Event) bool { return e.Type == "notice" && strings.Contains(e.Text, "电脑") }) != 1 {
				t.Fatal("missing computer guidance")
			}
		})
	}
}

func TestPhoneQuestionMCPStringPresenceAndWhitespace(t *testing.T) {
	f, err := parseElicitationForm(json.RawMessage(`{"type":"object","properties":{"requiredText":{"type":"string"},"optionalText":{"type":"string"},"constrained":{"type":"string","minLength":1},"enumText":{"type":"string","enum":["","named"]}},"required":["requiredText"]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, question := range f.questions {
		if !question.PreserveWhitespace || !question.AllowEmpty {
			t.Fatalf("MCP string semantics not relayed: %+v", question)
		}
	}
	if _, err := f.content(map[string][]string{}); err == nil {
		t.Fatal("absent required string accepted")
	}
	want := map[string]any{"requiredText": "", "optionalText": "  padded\n  ", "enumText": ""}
	got, err := f.content(map[string][]string{"requiredText": {""}, "optionalText": {"  padded\n  "}, "enumText": {""}})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("string values lost: got %+v, err %v", got, err)
	}
	got, err = f.content(map[string][]string{"requiredText": {""}})
	if err != nil || !reflect.DeepEqual(got, map[string]any{"requiredText": ""}) {
		t.Fatalf("optional absence was not preserved: %+v, %v", got, err)
	}
	if _, err := f.content(map[string][]string{"requiredText": {""}, "constrained": {""}}); err == nil {
		t.Fatal("minLength discarded for empty string")
	}
	ordinary, err := parseToolQuestions(json.RawMessage(`{"questions":[{"id":"answer","question":"Answer?"}]}`), "codex")
	if err != nil || validateQuestionAnswers(ordinary, map[string][]string{"answer": {"   "}}) == nil {
		t.Fatal("ordinary required tool question accepted a blank answer")
	}
}

func TestPhoneQuestionSnapshotOwnsDeepCopiesAndAnswerBuffers(t *testing.T) {
	qs, err := parseToolQuestions(json.RawMessage(codexQuestionFixture), "codex")
	if err != nil {
		t.Fatal(err)
	}
	a := newApprovals()
	p, visible := a.addValidatedRequest("codex:fixture", "codex", "7", questionEvent("codex", qs, "Question", "", ""), func(answers map[string][]string) error { return validateQuestionAnswers(qs, answers) })
	visible.Questions[0].Options[0].Label = "Changed visible"
	snapshot := a.snapshot("codex:fixture")
	snapshot[0].Questions[0].Options[0].Label = "Changed snapshot"
	if a.snapshot("codex:fixture")[0].Questions[0].Options[0].Label != "SQLite" {
		t.Fatal("question options mutated registry")
	}
	answers := map[string][]string{"database": {"Postgres"}}
	if !a.answer(p.id, approvalAnswer{decision: "allow", answers: answers}) {
		t.Fatal("answer rejected")
	}
	answers["database"][0] = "Changed answer"
	if ans := <-p.ch; ans.answers["database"][0] != "Postgres" {
		t.Fatal("caller mutated delivered answer")
	}
}

func TestPhoneQuestionParserRejectsAmbiguousIDsSecretsAndOversize(t *testing.T) {
	for _, raw := range []string{
		`{"questions":[{"id":"x","question":"One"},{"id":"x","question":"Two"}]}`,
		`{"questions":[{"id":"x","question":"Secret","isSecret":true}]}`,
		`{"questions":[{"id":"x","question":"Choose","options":[{"label":"same"},{"label":"same"}]}]}`,
		`{"questions":[{"id":"x","question":"` + strings.Repeat("x", maxQuestionText+1) + `"}]}`,
	} {
		if _, err := parseToolQuestions(json.RawMessage(raw), "codex"); err == nil {
			t.Fatal("unsafe/ambiguous questions accepted")
		}
	}
	if _, err := parseToolQuestions(json.RawMessage(`{"questions":[{"question":"Same"},{"question":"Same"}]}`), "claude"); err == nil {
		t.Fatal("duplicate Claude text keys accepted")
	}
}
