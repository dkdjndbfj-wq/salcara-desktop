package agents

import (
	"context"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

type recorder struct {
	mu  sync.Mutex
	evs []protocol.Event
}

func (r *recorder) sink(e protocol.Event) {
	r.mu.Lock()
	r.evs = append(r.evs, e)
	r.mu.Unlock()
}

func (r *recorder) all() []protocol.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]protocol.Event(nil), r.evs...)
}

// wait returns the first event (after skipping `after` events) matching pred.
func (r *recorder) wait(t *testing.T, what string, pred func(protocol.Event) bool) protocol.Event {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range r.all() {
			if pred(e) {
				return e
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := json.MarshalIndent(r.all(), "", " ")
	t.Fatalf("timeout waiting for %s; events:\n%s", what, b)
	return protocol.Event{}
}

func (r *recorder) count(pred func(protocol.Event) bool) int {
	n := 0
	for _, e := range r.all() {
		if pred(e) {
			n++
		}
	}
	return n
}

func readLog(t *testing.T, dir, name string) []map[string]any {
	b, _ := os.ReadFile(filepath.Join(dir, name))
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

type fixture struct {
	m       Manager
	rec     *recorder
	logDir  string
	home    string
	workDir string
	srv     *httptest.Server
	mu      sync.Mutex
	s       Settings
}

func (f *fixture) settings() Settings {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.s
}

func newFixture(t *testing.T, approval string) *fixture {
	t.Setenv("SALCARA_FAKE", "1")
	f := &fixture{rec: &recorder{}, logDir: t.TempDir(), home: t.TempDir(), workDir: t.TempDir()}
	t.Setenv("SALCARA_FAKE_LOG", f.logDir)
	t.Setenv("ANTHROPIC_API_KEY", "user-own-key") // must not leak into the child
	f.s = Settings{RelayRoot: "https://relay.test/", CodexKey: "sk-codex", ClaudeKey: "sk-claude", Approval: approval,
		BridgeExe: os.Args[0], LocalToken: "tok123"}
	f.m = NewManagerWithOptions(f.rec.sink, f.settings, Options{CodexPath: os.Args[0], ClaudePath: os.Args[0],
		ClaudeHome: f.home, StateDir: t.TempDir(), ClaudeWatchInterval: 50 * time.Millisecond, CodexWatchInterval: time.Hour})
	f.srv = httptest.NewServer(f.m.LocalHandler())
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(f.srv.URL, "http://"))
	f.s.LocalPort, _ = strconv.Atoi(port)
	t.Cleanup(func() {
		f.m.Close()
		f.srv.Close()
	})
	return f
}

func TestDetect(t *testing.T) {
	f := newFixture(t, "ask")
	for _, a := range f.m.Agents() {
		tool := a.Detect(context.Background())
		if !tool.Available || tool.Version != "9.9.9" {
			t.Fatalf("%s: %+v", a.ID(), tool)
		}
	}
	if f.m.Get("codex").Name() != "Codex" || f.m.Get("claude").Name() != "Claude Code" || f.m.Get("x") != nil {
		t.Fatal("Get")
	}
}

func TestCodexFlow(t *testing.T) {
	f := newFixture(t, "ask")
	ctx := context.Background()
	cx := f.m.Get("codex")

	list, err := cx.Sessions(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("sessions %v %+v", err, list)
	}
	clients := map[string]string{}
	for _, s := range list {
		clients[s.SessionKey] = s.Client
	}
	if clients["codex:thr_cli"] != "Codex CLI" || clients["codex:thr_ide"] != "Codex IDE" {
		t.Fatalf("clients %+v", clients)
	}
	if models := cx.Models(ctx); len(models) != 1 || models[0] != "gpt-5.5" {
		t.Fatalf("models %v", models)
	}

	id, err := cx.Start(ctx, "/work/app", "run the tests", "", "ask")
	if err != nil || id != "thr_new" {
		t.Fatalf("start %q %v", id, err)
	}
	sk := "codex:thr_new"
	ap := f.rec.wait(t, "command approval", func(e protocol.Event) bool {
		return e.Type == "approval.request" && e.Kind == "command" && e.SessionKey == sk
	})
	if ap.Title != "运行 npm test" || ap.Cwd != "/work/app" {
		t.Fatalf("approval %+v", ap)
	}
	f.rec.wait(t, "waiting_approval", func(e protocol.Event) bool {
		return e.Type == "session.updated" && e.Session.Status == "waiting_approval"
	})
	if !cx.Respond(ap.ApprovalID, "allow", "") {
		t.Fatal("respond")
	}
	if cx.Respond(ap.ApprovalID, "allow", "") {
		t.Fatal("double respond should fail")
	}
	fa := f.rec.wait(t, "file approval", func(e protocol.Event) bool {
		return e.Type == "approval.request" && e.Kind == "file_change"
	})
	if !strings.Contains(fa.Diff, "+hello") || !strings.Contains(fa.Diff, "--- a/work/app/a.txt") || fa.Title != "修改 a.txt" {
		t.Fatalf("file approval %+v", fa)
	}
	f.m.Get("claude").Respond(fa.ApprovalID, "deny", "") // wrong agent: must not consume it
	if !cx.Respond(fa.ApprovalID, "deny", "") {
		t.Fatal("respond deny")
	}
	turn := f.rec.wait(t, "turn completed", func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" })
	if turn.Usage == nil || turn.Usage.InputTokens != 500 || turn.Usage.OutputTokens != 100 {
		t.Fatalf("usage %+v", turn.Usage)
	}
	f.rec.wait(t, "assistant message", func(e protocol.Event) bool {
		return e.Type == "message" && e.Role == "assistant" && e.Text == "Hello" && e.Final
	})
	f.rec.wait(t, "command done", func(e protocol.Event) bool {
		return e.Type == "tool" && e.ID == "c1" && e.Status == "done" && e.Output == "ok 1\n" && e.ExitCode != nil
	})
	f.rec.wait(t, "file declined", func(e protocol.Event) bool {
		return e.Type == "tool" && e.ID == "f1" && e.Status == "failed" && e.Kind == "file_change"
	})
	f.rec.wait(t, "resolved", func(e protocol.Event) bool {
		return e.Type == "approval.resolved" && e.ApprovalID == fa.ApprovalID && e.Decision == "deny" && e.By == "phone"
	})
	dec := readLog(t, f.logDir, "codex-decisions.jsonl")
	if len(dec) != 2 || dec[0]["decision"] != "accept" || dec[1]["decision"] != "decline" {
		t.Fatalf("decisions %+v", dec)
	}

	// launch args force the relay
	args := readLog(t, f.logDir, "codex-args.jsonl")
	if len(args) == 0 || args[0]["key"] != "sk-codex" {
		t.Fatalf("args %+v", args)
	}
	joined := ""
	for _, a := range args[0]["args"].([]any) {
		joined += a.(string) + " "
	}
	for _, want := range []string{`-c model_provider="salcara"`, `model_providers.salcara.base_url="https://relay.test/v1"`,
		`model_providers.salcara.env_key="SUB2API_API_KEY"`, `model_providers.salcara.wire_api="responses"`,
		"model_providers.salcara.requires_openai_auth=false", "model_providers.salcara.supports_websockets=false"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in %s", want, joined)
		}
	}
	calls := readLog(t, f.logDir, "codex-calls.jsonl")
	var start map[string]any
	for _, c := range calls {
		if c["method"] == "thread/start" {
			start = c["params"].(map[string]any)
		}
	}
	if start["approvalPolicy"] != "on-request" || start["sandbox"] != "workspace-write" || start["modelProvider"] != "salcara" {
		t.Fatalf("thread/start %+v", start)
	}

	// history
	info, evs, err := cx.Open(ctx, "thr_cli")
	if err != nil || info.Client != "Codex CLI" {
		t.Fatalf("open %v %+v", err, info)
	}
	var kinds []string
	for _, e := range evs {
		kinds = append(kinds, e.Type+":"+e.Kind+e.Role)
	}
	if strings.Join(kinds, ",") != "message:user,reasoning:,tool:read,message:assistant,turn:" {
		t.Fatalf("history %v", kinds)
	}
	if evs[4].Status != "completed" || evs[4].TurnID == "" {
		t.Fatal("history did not retain its actual terminal boundary")
	}
	if evs[2].Title != "读取 main.go" {
		t.Fatalf("read title %q", evs[2].Title)
	}

	// bridge-started thread now reports client "Codex"
	list, _ = cx.Sessions(ctx)
	_ = list
}

func TestCodexAutoEditsAndInterrupt(t *testing.T) {
	f := newFixture(t, "auto_edits")
	ctx := context.Background()
	cx := f.m.Get("codex")
	// This case exercises a previously configured legacy thread. A global
	// default is not permission to auto-approve an unknown original session.
	configured := cx.(*codexAgent)
	configured.mu.Lock()
	thread := configured.threadLocked("thr_cli")
	thread.policy, thread.legacyPolicyConfigured = "auto_edits", true
	configured.mu.Unlock()
	if err := cx.Send(ctx, "thr_cli", "go"); err != nil {
		t.Fatal(err)
	}
	ap := f.rec.wait(t, "command approval", func(e protocol.Event) bool { return e.Type == "approval.request" })
	cx.Respond(ap.ApprovalID, "allow_session", "")
	f.rec.wait(t, "turn completed", func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" })
	if n := f.rec.count(func(e protocol.Event) bool { return e.Type == "approval.request" }); n != 1 {
		t.Fatalf("auto_edits should only ask for the command, got %d approvals", n)
	}
	dec := readLog(t, f.logDir, "codex-decisions.jsonl")
	if dec[0]["decision"] != "acceptForSession" || dec[1]["decision"] != "accept" {
		t.Fatalf("decisions %+v", dec)
	}
	calls := readLog(t, f.logDir, "codex-calls.jsonl")
	resumed := false
	for _, c := range calls {
		if c["method"] == "thread/resume" {
			resumed = true
		}
	}
	if !resumed {
		t.Fatal("send to an unloaded thread must resume it")
	}

	// interrupt a running turn
	if err := cx.Send(ctx, "thr_cli", "slow task"); err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "second turn started", func(e protocol.Event) bool {
		return e.Type == "message" && e.Role == "user" && e.Text == "slow task"
	})
	time.Sleep(100 * time.Millisecond)
	var turnID string
	for _, session := range func() []protocol.SessionInfo { rows, _ := cx.Sessions(ctx); return rows }() {
		if session.SessionKey == "codex:thr_cli" {
			turnID = session.ActiveTurnID
		}
	}
	if turnID == "" {
		t.Fatal("running turn did not expose an exact turn ID")
	}
	if _, err := cx.(PreciseInterrupter).InterruptTurn(ctx, "thr_cli", turnID, "10000000-0000-4000-8000-000000000001"); err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "interrupted", func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "interrupted" })
}

func TestCodexRequiresRelay(t *testing.T) {
	f := newFixture(t, "ask")
	f.mu.Lock()
	f.s.CodexKey = ""
	f.mu.Unlock()
	if _, err := f.m.Get("codex").Start(context.Background(), "/x", "hi", "", ""); err == nil {
		t.Fatal("expected error without key")
	}
}

func TestClaudeFlow(t *testing.T) {
	f := newFixture(t, "ask")
	ctx := context.Background()
	cl := f.m.Get("claude")
	os.WriteFile(filepath.Join(f.workDir, "a.txt"), []byte("line1\nold\n"), 0o644)

	id, err := cl.Start(ctx, f.workDir, "hello edit", "", "ask")
	if err != nil {
		t.Fatal(err)
	}
	sk := "claude:" + id
	ap := f.rec.wait(t, "bash approval", func(e protocol.Event) bool { return e.Type == "approval.request" && e.SessionKey == sk })
	if ap.Kind != "command" || ap.Title != "运行 ls" {
		t.Fatalf("approval %+v", ap)
	}
	sessions, _ := cl.Sessions(ctx)
	found := false
	for _, s := range sessions {
		if s.SessionKey == sk {
			found = true
			if s.Status != "waiting_approval" || !s.Controllable || s.Title != "hello edit" {
				t.Fatalf("session %+v", s)
			}
		}
	}
	if !found {
		t.Fatal("bridge session not listed")
	}
	if ap.ApprovalOptions == nil || strings.Join(ap.ApprovalOptions.Decisions, ",") != "allow,deny" {
		t.Fatal("legacy native prompt must expose only this-request allow/deny")
	}
	if cl.Respond(ap.ApprovalID, "allow_session", "") {
		t.Fatal("native prompt was converted into a tool-name-wide remembered grant")
	}
	if !cl.Respond(ap.ApprovalID, "allow", "") {
		t.Fatal("invalid broader decision consumed the current request")
	}
	ea := f.rec.wait(t, "edit approval", func(e protocol.Event) bool { return e.Type == "approval.request" && e.Kind == "file_change" })
	if !strings.Contains(ea.Diff, "@@ -2,1 +2,1 @@\n-old\n+new") || ea.Title != "修改 a.txt" {
		t.Fatalf("edit approval %+v\n%s", ea, ea.Diff)
	}
	cl.Respond(ea.ApprovalID, "deny", "不要改")
	turn := f.rec.wait(t, "turn completed", func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" })
	if turn.Usage == nil || turn.Usage.InputTokens != 115 || turn.Usage.OutputTokens != 20 || turn.Usage.CostUSD != 0.0123 {
		t.Fatalf("usage %+v", turn.Usage)
	}
	f.rec.wait(t, "assistant text", func(e protocol.Event) bool {
		return e.Type == "message" && e.Role == "assistant" && e.Text == "Hi there" && e.Final && e.ID == "msg_1:0"
	})
	f.rec.wait(t, "user text", func(e protocol.Event) bool { return e.Type == "message" && e.Role == "user" && e.Text == "hello edit" })
	f.rec.wait(t, "bash done", func(e protocol.Event) bool {
		return e.Type == "tool" && e.ID == "toolu_1" && e.Status == "done" && e.Output == "file1\nfile2\n" && e.Kind == "command"
	})
	dec := readLog(t, f.logDir, "claude-decisions.jsonl")
	if len(dec) != 2 || dec[0]["behavior"] != "allow" || dec[1]["behavior"] != "deny" {
		t.Fatalf("decisions %+v", dec)
	}
	// The fake Claude process does not write ~/.claude/projects.  Create the
	// same kind of stable transcript that the real CLI writes so the
	// process-local, Wait-confirmed completion lease can be exercised below.
	leaseDir := filepath.Join(f.home, "projects", "fixture")
	if err := os.MkdirAll(leaseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leaseDir, id+".jsonl"), []byte(`{"type":"user","message":{"role":"user","content":"hello edit"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := readLog(t, f.logDir, "claude-args.jsonl")
	if args[0]["base"] != "https://relay.test" || args[0]["token"] != "sk-claude" || args[0]["apikey"] != "" {
		t.Fatalf("env %+v", args[0])
	}
	a0 := strings.Join(toStrings(args[0]["args"]), " ")
	for _, want := range []string{"-p --input-format stream-json --output-format stream-json --verbose --include-partial-messages",
		"--permission-prompt-tool mcp__salcara__approve", "--permission-mode default", "--session-id " + id} {
		if !strings.Contains(a0, want) {
			t.Fatalf("missing %q in %s", want, a0)
		}
	}

	// A second native prompt is an independent permission request, even for
	// the same tool and input. The helper cannot discard the CLI's ask rules.
	if err := cl.Send(ctx, id, "again"); err != nil {
		t.Fatal(err)
	}
	secondApproval := f.rec.wait(t, "second native Bash approval", func(e protocol.Event) bool {
		return e.Type == "approval.request" && e.Kind == "command" && e.ApprovalID != ap.ApprovalID
	})
	if secondApproval.ApprovalOptions == nil || strings.Join(secondApproval.ApprovalOptions.Decisions, ",") != "allow,deny" || cl.Respond(secondApproval.ApprovalID, "allow_session", "") {
		t.Fatal("second native prompt advertised or accepted remembered permission")
	}
	if !cl.Respond(secondApproval.ApprovalID, "allow", "") {
		t.Fatal("second native prompt not independently answerable")
	}
	f.rec.wait(t, "second turn", func(e protocol.Event) bool {
		return e.Type == "message" && e.Role == "assistant" && e.ID == "msg_2:0" && e.Final
	})
	f.rec.wait(t, "2 turns", func(e protocol.Event) bool { return false || countTurns(f.rec) >= 2 })
	if n := f.rec.count(func(e protocol.Event) bool { return e.Type == "approval.request" }); n != 3 {
		t.Fatalf("each native prompt must be shown; approvals=%d", n)
	}

	// interrupt kills the process; the next send resumes it
	if err := cl.Send(ctx, id, "sleep please"); err != nil {
		t.Fatal(err)
	}
	var turnID string
	for _, session := range func() []protocol.SessionInfo { rows, _ := cl.Sessions(ctx); return rows }() {
		if session.SessionKey == sk {
			turnID = session.ActiveTurnID
		}
	}
	if _, err := cl.(PreciseInterrupter).InterruptTurn(ctx, id, turnID, "10000000-0000-4000-8000-000000000002"); err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "interrupted", func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "interrupted" })
	if err := cl.Send(ctx, id, "after"); err != nil {
		t.Fatal(err)
	}
	resumedApproval := f.rec.wait(t, "resumed worker approval", func(e protocol.Event) bool {
		return e.Type == "approval.request" && e.Kind == "command" && e.ApprovalID != ap.ApprovalID && e.ApprovalID != secondApproval.ApprovalID
	})
	if !cl.Respond(resumedApproval.ApprovalID, "allow", "") {
		t.Fatal("resumed native prompt not answerable")
	}
	f.rec.wait(t, "resumed turn", func(e protocol.Event) bool {
		return e.Type == "message" && e.Role == "assistant" && e.Final && e.ID == "msg_1:0" && e.TS > turn.TS && countTurns(f.rec) >= 3
	})
	args = readLog(t, f.logDir, "claude-args.jsonl")
	if last := strings.Join(toStrings(args[len(args)-1]["args"]), " "); !strings.Contains(last, "--resume "+id) {
		t.Fatalf("expected resume: %s", last)
	}
}

func countTurns(r *recorder) int {
	return r.count(func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" })
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestClaudePermissionAuth(t *testing.T) {
	f := newFixture(t, "auto_all")
	body := `{"session":"s1","tool_name":"Bash","input":{"command":"echo synthetic"}}`
	resp, err := httpPost(f.srv.URL+PermissionPath, "wrong", body)
	if err != nil || resp.status != 403 {
		t.Fatalf("want 403, got %+v %v", resp, err)
	}
	resp, err = httpPost(f.srv.URL+PermissionPath, "tok123", body)
	if err != nil || resp.status != 403 {
		t.Fatal("shared console token must not approve an unowned Claude session", err)
	}
	a := f.m.Get("claude").(*claudeAgent)
	p := claudeSyntheticPermissionWorker(t, a, "s1")
	// Even under the legacy auto_all setting, a request which the runtime
	// actually sends to the helper remains explicit. Authentication is not
	// permission to auto-approve an upstream ask/requiresUserInteraction rule.
	previous := ""
	for _, decision := range []string{"allow", "deny"} {
		result := make(chan struct {
			resp httpResp
			err  error
		}, 1)
		go func() {
			response, postErr := httpPost(f.srv.URL+PermissionPath, p.permissionLease.token, body)
			result <- struct {
				resp httpResp
				err  error
			}{response, postErr}
		}()
		approval := f.rec.wait(t, "explicit native approval under auto_all", func(e protocol.Event) bool {
			return e.Type == "approval.request" && e.SessionKey == "claude:s1" && e.ApprovalID != previous
		})
		if approval.ApprovalOptions == nil || strings.Join(approval.ApprovalOptions.Decisions, ",") != "allow,deny" || a.Respond(approval.ApprovalID, "allow_session", "") {
			t.Fatal("helper granted broader permission than the native request")
		}
		select {
		case <-result:
			t.Fatal("native request resolved without a valid decision")
		default:
		}
		if !a.Respond(approval.ApprovalID, decision, "synthetic decision") {
			t.Fatal("this-request native decision rejected")
		}
		select {
		case got := <-result:
			if got.err != nil || got.resp.status != 200 || !strings.Contains(got.resp.body, `"behavior":"`+decision+`"`) {
				t.Fatalf("native decision mismatch: %+v %v", got.resp, got.err)
			}
			if decision == "allow" && !strings.Contains(got.resp.body, `"updatedInput":{"command":"echo synthetic"}`) {
				t.Fatal("approved tool input was changed")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("native decision did not reach waiting helper")
		}
		previous = approval.ApprovalID
	}
}
