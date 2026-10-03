package agents

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

const claudeMCPFormFixture = `{"session_id":"fixture-thread","hook_event_name":"Elicitation","mcp_server_name":"fixture-server","message":"Choose settings","mode":"form","transcript_path":"/private/not-forwarded.jsonl","cwd":"/untrusted/not-forwarded","requested_schema":{"type":"object","properties":{"name":{"type":"string","minLength":2},"count":{"type":"integer","minimum":1,"maximum":4},"enabled":{"type":"boolean"},"color":{"type":"string","enum":[" red ","blue"]},"optional":{"type":"number"}},"required":["name","count","enabled","color"]}}`

func newClaudeMCPQuestionFixture(t *testing.T) (*claudeAgent, *recorder, *claudeElicitationLease) {
	t.Helper()
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings { return Settings{Approval: "auto_all", LocalToken: "shared-fixture-token"} }, "fixture-not-launched", t.TempDir())
	lease := &claudeElicitationLease{token: "worker-specific-fixture-token", cwd: "fixture-directory", done: make(chan struct{})}
	a.elicitationHooks["fixture-thread"] = lease
	t.Cleanup(func() { a.revokeElicitationHook("fixture-thread", lease); a.Close() })
	return a, rec, lease
}

func startClaudeMCPQuestion(a *claudeAgent, body, token string, ctx context.Context) (*httptest.ResponseRecorder, <-chan struct{}) {
	r := httptest.NewRequest(http.MethodPost, ClaudeElicitationPath, strings.NewReader(body))
	r.Header.Set(claudeElicitationHeader, token)
	if ctx != nil {
		r = r.WithContext(ctx)
	}
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); a.elicitationHandler(w, r) }()
	return w, done
}

func claudeMCPQuestionOutput(t *testing.T, w *httptest.ResponseRecorder) claudeElicitationOutput {
	t.Helper()
	var output claudeElicitationOutput
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &output) != nil || output.Specific.Event != "Elicitation" {
		t.Fatalf("invalid native hook output: %d %s", w.Code, w.Body.String())
	}
	return output
}

func TestPhoneQuestionClaudeMCPFormReturnsTypedContent(t *testing.T) {
	a, rec, lease := newClaudeMCPQuestionFixture(t)
	w, done := startClaudeMCPQuestion(a, claudeMCPFormFixture, lease.token, nil)
	ev := rec.wait(t, "Claude MCP form", func(e protocol.Event) bool { return e.Type == "approval.request" })
	if ev.Tool != "claude" || ev.Kind != "question" || ev.QuestionMode != "mcp-form" || ev.Title != "MCP · fixture-server" || ev.Cwd != lease.cwd || ev.ExpiresAt <= nowMs() {
		t.Fatalf("wrong bridge form: %+v", ev)
	}
	select {
	case <-done:
		t.Fatal("auto_all fabricated a form answer")
	default:
	}
	answers := map[string][]string{"name": {"Ada"}, "count": {"3"}, "enabled": {"false"}, "color": {" red "}}
	bad := cloneAnswers(answers)
	bad["count"] = []string{"2.5"}
	if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: bad}) || a.Respond(ev.ApprovalID, "allow_session", "") || len(a.aps.snapshot(ev.SessionKey)) != 1 {
		t.Fatal("invalid answer consumed or allowed pending form")
	}
	if !a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: answers}) {
		t.Fatal("valid form rejected")
	}
	questionFixtureWait(t, done)
	output := claudeMCPQuestionOutput(t, w)
	want := map[string]any{"name": "Ada", "count": float64(3), "enabled": false, "color": " red "}
	if output.Specific.Action != "accept" || !reflect.DeepEqual(output.Specific.Content, want) || a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: answers}) {
		t.Fatalf("answers altered, untyped or replayable: %+v", output)
	}
	for _, e := range rec.all() {
		encoded, _ := json.Marshal(e)
		if strings.Contains(string(encoded), "/private/") || strings.Contains(string(encoded), "/untrusted/") || strings.Contains(string(encoded), lease.token) {
			t.Fatal("native context/capability leaked to phone")
		}
	}
}

func TestPhoneQuestionClaudeMCPOptionalFormDoesNotInventDefaults(t *testing.T) {
	a, rec, lease := newClaudeMCPQuestionFixture(t)
	body := `{"session_id":"fixture-thread","hook_event_name":"Elicitation","mcp_server_name":"fixture-server","mode":"form","requested_schema":{"type":"object","properties":{"optional":{"type":"string","default":"not-a-user-answer"}}}}`
	w, done := startClaudeMCPQuestion(a, body, lease.token, nil)
	ev := rec.wait(t, "optional form", func(e protocol.Event) bool { return e.Type == "approval.request" })
	if !a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: map[string][]string{}}) {
		t.Fatal("optional form required fabricated answers")
	}
	questionFixtureWait(t, done)
	output := claudeMCPQuestionOutput(t, w)
	if output.Specific.Action != "accept" || !reflect.DeepEqual(output.Specific.Content, map[string]any{}) || strings.Contains(w.Body.String(), "not-a-user-answer") {
		t.Fatal("empty accepted content missing or invented default included")
	}
}

func TestPhoneQuestionClaudeMCPAuthorizationAndMalformedRequests(t *testing.T) {
	for name, body := range map[string]string{
		"bad JSON": `{`, "null": `null`, "extra JSON": claudeMCPFormFixture + `{}`,
		"oversize": `{"session_id":"fixture-thread","unused":"` + strings.Repeat("x", 129<<10) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			a, rec, lease := newClaudeMCPQuestionFixture(t)
			w, done := startClaudeMCPQuestion(a, body, lease.token, nil)
			questionFixtureWait(t, done)
			if w.Code != http.StatusBadRequest || len(rec.all()) != 0 || len(a.aps.snapshot("claude:fixture-thread")) != 0 {
				t.Fatal("malformed native callback relayed")
			}
		})
	}
	for name, token := range map[string]string{"missing": "", "global instead of worker": "shared-fixture-token", "wrong": "wrong"} {
		t.Run(name, func(t *testing.T) {
			a, rec, _ := newClaudeMCPQuestionFixture(t)
			w, done := startClaudeMCPQuestion(a, claudeMCPFormFixture, token, nil)
			questionFixtureWait(t, done)
			if w.Code != http.StatusForbidden || len(rec.all()) != 0 {
				t.Fatal("untrusted callback read/created questions")
			}
		})
	}
	t.Run("wrong session", func(t *testing.T) {
		a, rec, lease := newClaudeMCPQuestionFixture(t)
		w, done := startClaudeMCPQuestion(a, strings.Replace(claudeMCPFormFixture, "fixture-thread", "another-thread", 1), lease.token, nil)
		questionFixtureWait(t, done)
		if w.Code != http.StatusForbidden || len(rec.all()) != 0 {
			t.Fatal("capability crossed sessions")
		}
	})
}

func TestPhoneQuestionClaudeMCPUnsafeFormsDeclineWithDesktopGuidance(t *testing.T) {
	for name, fragment := range map[string]string{
		"URL":                `"mode":"url","url":"https://fixture.invalid/auth","message":"Sign in"`,
		"nested":             `"mode":"form","requested_schema":{"type":"object","properties":{"data":{"type":"object"}}}`,
		"secret field":       `"mode":"form","requested_schema":{"type":"object","properties":{"api_key":{"type":"string"}}}`,
		"secret message":     `"mode":"form","message":"Enter password","requested_schema":{"type":"object","properties":{"value":{"type":"string"}}}`,
		"unknown validation": `"mode":"form","requested_schema":{"type":"object","properties":{"count":{"type":"number","multipleOf":2}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			a, rec, lease := newClaudeMCPQuestionFixture(t)
			body := `{"session_id":"fixture-thread","hook_event_name":"Elicitation","mcp_server_name":"fixture-server",` + fragment + `}`
			w, done := startClaudeMCPQuestion(a, body, lease.token, nil)
			questionFixtureWait(t, done)
			if output := claudeMCPQuestionOutput(t, w); output.Specific.Action != "decline" || output.Specific.Content != nil {
				t.Fatal("unsafe form accepted")
			}
			if rec.count(func(e protocol.Event) bool { return e.Type == "approval.request" }) != 0 || rec.count(func(e protocol.Event) bool { return e.Type == "notice" && strings.Contains(e.Text, "电脑") }) != 1 {
				t.Fatal("unsafe form relayed or no desktop guidance")
			}
		})
	}
}

func TestPhoneQuestionClaudeMCPWithdrawalExpiryAndWorkerRotation(t *testing.T) {
	for _, how := range []string{"HTTP cancel", "worker stop", "interrupt", "expire", "phone decline"} {
		t.Run(how, func(t *testing.T) {
			a, rec, lease := newClaudeMCPQuestionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w, done := startClaudeMCPQuestion(a, claudeMCPFormFixture, lease.token, ctx)
			ev := rec.wait(t, "Claude MCP pending", func(e protocol.Event) bool { return e.Type == "approval.request" })
			switch how {
			case "HTTP cancel":
				cancel()
			case "worker stop":
				a.revokeElicitationHook("fixture-thread", lease)
			case "interrupt":
				a.aps.cancelSession(ev.SessionKey)
			case "expire":
				a.aps.mu.Lock()
				a.aps.waiting[ev.ApprovalID].expiresAt = time.Now().Add(-time.Second)
				a.aps.mu.Unlock()
				if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: map[string][]string{}}) {
					t.Fatal("expired form accepted")
				}
			case "phone decline":
				if !a.Respond(ev.ApprovalID, "deny", "") {
					t.Fatal("could not decline form")
				}
			}
			if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: map[string][]string{"name": {"Ada"}, "count": {"3"}, "enabled": {"false"}, "color": {"blue"}}}) {
				t.Fatal("withdrawn native request accepted a racing valid answer")
			}
			questionFixtureWait(t, done)
			if out := claudeMCPQuestionOutput(t, w); out.Specific.Action == "accept" || out.Specific.Content != nil {
				t.Fatal("withdrawn form allowed")
			}
			if a.RespondWithAnswers(ev.ApprovalID, ApprovalResponse{Decision: "allow", Answers: map[string][]string{}}) || len(a.aps.snapshot(ev.SessionKey)) != 0 {
				t.Fatal("withdrawal revived old question")
			}
		})
	}
	t.Run("old worker capability", func(t *testing.T) {
		a, rec, old := newClaudeMCPQuestionFixture(t)
		a.revokeElicitationHook("fixture-thread", old)
		fresh := &claudeElicitationLease{token: "fresh-worker-token", done: make(chan struct{})}
		a.elicitationHooks["fixture-thread"] = fresh
		defer a.revokeElicitationHook("fixture-thread", fresh)
		a.revokeElicitationHook("fixture-thread", old) // delayed old onExit must not remove new worker
		w, done := startClaudeMCPQuestion(a, claudeMCPFormFixture, old.token, nil)
		questionFixtureWait(t, done)
		if w.Code != http.StatusForbidden || len(rec.all()) != 0 || !a.activeElicitationLease("fixture-thread", fresh) {
			t.Fatal("old worker can answer or revoke replacement worker")
		}
	})
}

func TestPhoneQuestionClaudeMCPVersionGateAndEphemeralSettings(t *testing.T) {
	for version, supported := range map[string]bool{"": false, "unknown": false, "2.1.63": false, "2.1.75": false, "2.1.76": true, "2.1.117": true, "2.2.0": true, "3.0.0": true, "2.1.76-beta": false, "2.-1.100": false} {
		if claudeElicitationVersionSupported(version) != supported {
			t.Errorf("version %s gate wrong", version)
		}
	}
	a, _, existing := newClaudeMCPQuestionFixture(t)
	a.revokeElicitationHook("fixture-thread", existing)
	exe := os.Args[0] // only metadata, no executable/provider is launched
	st, err := os.Stat(exe)
	if err != nil {
		t.Fatal(err)
	}
	versionMu.Lock()
	previous, hadPrevious := versionCache[exe]
	versionCache[exe] = versionCacheEntry{mod: st.ModTime(), version: "2.1.75", ok: true}
	versionMu.Unlock()
	t.Cleanup(func() {
		versionMu.Lock()
		defer versionMu.Unlock()
		if hadPrevious {
			versionCache[exe] = previous
		} else {
			delete(versionCache, exe)
		}
	})
	settings := Settings{LocalPort: 12345, ClaudeKey: "do-not-copy", LocalToken: "do-not-copy-either"}
	if lease, overlay := a.prepareElicitationHook(exe, settings, "fixture-thread", "fixture-directory"); lease != nil || overlay != "" {
		t.Fatal("old host injected unsupported hook/settings")
	}
	versionMu.Lock()
	versionCache[exe] = versionCacheEntry{mod: st.ModTime(), version: "2.1.76", ok: true}
	versionMu.Unlock()
	lease, overlay := a.prepareElicitationHook(exe, settings, "fixture-thread", "fixture-directory")
	if lease == nil || lease.token == "" || strings.Contains(overlay, lease.token) || strings.Contains(overlay, "do-not-copy") {
		t.Fatal("no supported hook or secret stored in settings/args")
	}
	defer a.revokeElicitationHook("fixture-thread", lease)
	var parsed map[string]any
	if json.Unmarshal([]byte(overlay), &parsed) != nil || len(parsed) != 1 || len(parsed["hooks"].(map[string]any)) != 1 {
		t.Fatal("overlay changed settings beyond Elicitation hook")
	}
	handler := parsed["hooks"].(map[string]any)["Elicitation"].([]any)[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	if handler["type"] != "http" || handler["url"] != "http://127.0.0.1:12345"+ClaudeElicitationPath || handler["timeout"].(float64) <= approvalTimeout.Seconds() || !reflect.DeepEqual(handler["allowedEnvVars"], []any{claudeElicitationEnv}) || handler["headers"].(map[string]any)[claudeElicitationHeader] != "$"+claudeElicitationEnv {
		t.Fatalf("incorrect HTTP hook contract: %+v", handler)
	}
	fresh, _ := a.prepareElicitationHook(exe, settings, "other-thread", "fixture-directory")
	defer a.revokeElicitationHook("other-thread", fresh)
	if fresh == nil || fresh.token == lease.token {
		t.Fatal("workers share a reusable capability")
	}
}

func TestPhoneQuestionClaudeMCPManagerRegistersHookRoute(t *testing.T) {
	m := NewManagerWithOptions(func(protocol.Event) {}, func() Settings { return Settings{} }, Options{ClaudeHome: t.TempDir(), StateDir: t.TempDir()})
	defer m.Close()
	w := httptest.NewRecorder()
	m.LocalHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, ClaudeElicitationPath, nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatal("hook route is not registered")
	}
}

func TestPhoneQuestionClaudeMCPMockWorkerLaunchAndStop(t *testing.T) {
	for _, supported := range []bool{false, true} {
		name := "old host fallback"
		if supported {
			name = "supported ephemeral hook"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, "auto_all") // test binary only, never the real Claude installation
			version := "2.1.75"
			if supported {
				version = "2.1.76"
			}
			exe := os.Args[0]
			st, err := os.Stat(exe)
			if err != nil {
				t.Fatal(err)
			}
			versionMu.Lock()
			previous, hadPrevious := versionCache[exe]
			versionCache[exe] = versionCacheEntry{mod: st.ModTime(), version: version, ok: true}
			versionMu.Unlock()
			t.Cleanup(func() {
				versionMu.Lock()
				defer versionMu.Unlock()
				if hadPrevious {
					versionCache[exe] = previous
				} else {
					delete(versionCache, exe)
				}
			})
			settingsFile := filepath.Join(f.home, "settings.json")
			original := `{"hooks":{"Stop":[]},"theme":"light"}`
			if err := os.WriteFile(settingsFile, []byte(original), 0o600); err != nil {
				t.Fatal(err)
			}
			a := f.m.Get("claude").(*claudeAgent)
			id, err := a.Start(context.Background(), f.workDir, "fixture hello", "", "auto_all")
			if err != nil {
				t.Fatalf("ordinary launch broken: %v", err)
			}
			f.rec.wait(t, "mock turn", func(e protocol.Event) bool {
				return e.SessionKey == "claude:"+id && e.Type == "turn" && e.Status == "completed"
			})
			a.mu.Lock()
			p := a.procs[id]
			a.mu.Unlock()
			if p == nil {
				t.Fatal("mock worker unexpectedly gone")
			}
			withSettings := false
			for _, arg := range p.cmd.Args {
				if arg == "--settings" {
					withSettings = true
				}
			}
			if withSettings != supported || (p.elicitationHook != nil) != supported {
				t.Fatal("version gate changed ordinary process launch")
			}
			if supported {
				joinedArgs := strings.Join(p.cmd.Args, " ")
				if strings.Contains(joinedArgs, p.elicitationHook.token) {
					t.Fatal("capability in process arguments")
				}
				found := false
				for _, env := range p.cmd.Env {
					if env == claudeElicitationEnv+"="+p.elicitationHook.token {
						found = true
					}
				}
				if !found {
					t.Fatal("HTTP hook cannot authenticate via worker env")
				}
				w, done := startClaudeMCPQuestion(a, strings.Replace(claudeMCPFormFixture, "fixture-thread", id, 1), p.elicitationHook.token, nil)
				ev := f.rec.wait(t, "mock worker MCP request", func(e protocol.Event) bool {
					return e.SessionKey == "claude:"+id && e.Type == "approval.request" && e.Kind == "question"
				})
				if err := a.Interrupt(context.Background(), id); err != nil {
					t.Fatal(err)
				}
				questionFixtureWait(t, done)
				if output := claudeMCPQuestionOutput(t, w); output.Specific.Action != "cancel" || a.Respond(ev.ApprovalID, "allow", "") {
					t.Fatal("stopped worker elicitation remains usable")
				}
			}
			unchanged, err := os.ReadFile(settingsFile)
			if err != nil || string(unchanged) != original {
				t.Fatal("user settings file changed")
			}
		})
	}
}
