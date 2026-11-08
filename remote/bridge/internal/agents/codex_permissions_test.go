package agents

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

type permissionTransport struct {
	base       *resumeSafetyTransport
	requests   []map[string]any
	corrupt    func(map[string]any)
	resumeHook func()
}

func (p *permissionTransport) Close() error { return nil }
func (p *permissionTransport) Write(data []byte) (int, error) {
	var req map[string]any
	if err := json.Unmarshal(data, &req); err != nil {
		return 0, err
	}
	p.requests = append(p.requests, req)
	if req["method"] != "thread/resume" && req["method"] != "thread/start" {
		return p.base.Write(data)
	}
	params := req["params"].(map[string]any)
	thread := p.base.thread
	if req["method"] == "thread/start" {
		thread.ID = "new-permission-thread"
	}
	sandbox := "workspaceWrite"
	if params["sandbox"] == "danger-full-access" {
		sandbox = "dangerFullAccess"
	}
	result := map[string]any{"thread": thread, "approvalPolicy": params["approvalPolicy"], "approvalsReviewer": params["approvalsReviewer"], "sandbox": map[string]any{"type": sandbox}}
	if p.corrupt != nil {
		p.corrupt(result)
	}
	if p.resumeHook != nil {
		p.resumeHook()
	}
	raw, err := json.Marshal(map[string]any{"id": req["id"], "result": result})
	if err != nil {
		return 0, err
	}
	p.base.conn.handleLine(raw)
	return len(data), nil
}

func permissionFixture(t *testing.T) (*codexAgent, *permissionTransport) {
	t.Helper()
	thread := cxThread{ID: "permission-thread", Status: cxThreadStatus{Type: "idle"}}
	a, base, _ := newResumeSafetyAgent(t, thread, false)
	a.applyThreadLocked(thread).loaded = true
	p := &permissionTransport{base: base}
	a.conn.stdin = p
	return a, p
}

func TestCodexPermissionNativeModesAreDistinct(t *testing.T) {
	for _, mode := range []string{"ask", "auto_review", "full_access"} {
		t.Run(mode, func(t *testing.T) {
			a, p := permissionFixture(t)
			a.threads["other-thread"] = &codexThread{policy: "auto_edits"}
			err := a.SendWithOptions(context.Background(), "permission-thread", "synthetic", TurnOptions{PermissionMode: mode, Model: "fixture-model"})
			if err != nil {
				t.Fatal(err)
			}
			var resume map[string]any
			var calls []string
			for _, req := range p.requests {
				calls = append(calls, req["method"].(string))
				if req["method"] == "thread/resume" {
					resume = req["params"].(map[string]any)
				}
			}
			if !reflect.DeepEqual(calls, []string{"config/read", "thread/resume", "turn/start"}) {
				t.Fatalf("unexpected RPC: %v", calls)
			}
			if resume["threadId"] != "permission-thread" || resume["model"] != "fixture-model" {
				t.Fatalf("wrong target/model: %v", resume)
			}
			want := map[string]any{}
			codexPermissionParams(want, mode)
			for key, value := range want {
				if resume[key] != value {
					t.Fatalf("%s=%v want %v", key, resume[key], value)
				}
			}
			if a.KnownSessionPermission("permission-thread") != mode || a.threads["permission-thread"].policy != "ask" || a.threads["other-thread"].policy != "auto_edits" {
				t.Fatal("incorrect permission projection or cross-thread change")
			}
		})
	}
}

func TestCodexPermissionMissingMismatchedOrBusyAcknowledgementCannotSend(t *testing.T) {
	for _, mutation := range []string{"missing-reviewer", "wrong-reviewer", "wrong-sandbox", "wrong-id", "active", "future-status", "late-turn"} {
		t.Run(mutation, func(t *testing.T) {
			a, p := permissionFixture(t)
			p.corrupt = func(r map[string]any) {
				switch mutation {
				case "missing-reviewer":
					delete(r, "approvalsReviewer")
				case "wrong-reviewer":
					r["approvalsReviewer"] = "user"
				case "wrong-sandbox":
					r["sandbox"] = map[string]any{"type": "dangerFullAccess"}
				case "wrong-id":
					r["thread"] = cxThread{ID: "other"}
				case "active", "future-status":
					r["thread"] = cxThread{ID: "permission-thread", Status: cxThreadStatus{Type: mutation}}
				}
			}
			if mutation == "late-turn" {
				p.resumeHook = func() { a.mu.Lock(); a.threads["permission-thread"].turnVersion++; a.mu.Unlock() }
			}
			err := a.SendWithOptions(context.Background(), "permission-thread", "synthetic", TurnOptions{PermissionMode: "auto_review"})
			if !errors.Is(err, errCodexPermissionUnconfirmed) {
				t.Fatalf("unexpected error %v", err)
			}
			if err = a.Send(context.Background(), "permission-thread", "must not bypass"); !errors.Is(err, errCodexPermissionUnconfirmed) {
				t.Fatalf("unconfirmed permission bypass: %v", err)
			}
			for _, req := range p.requests {
				if req["method"] == "turn/start" {
					t.Fatal("input sent despite unconfirmed mode")
				}
			}
		})
	}
}

func TestCodexPermissionCannotChangeRunningOrPendingChild(t *testing.T) {
	for _, state := range []string{"running", "waiting_approval", "child", "unknown"} {
		t.Run(state, func(t *testing.T) {
			a, p := permissionFixture(t)
			target := a.threads["permission-thread"]
			switch state {
			case "child":
				target.childStates = map[string]string{"child": "running"}
			case "unknown":
				target.statusUnconfirmed = true
			default:
				target.info.Status = state
			}
			if err := a.SendWithOptions(context.Background(), "permission-thread", "synthetic", TurnOptions{PermissionMode: "full_access"}); err == nil {
				t.Fatal("busy permission change allowed")
			}
			for _, req := range p.requests {
				if req["method"] == "thread/resume" || req["method"] == "turn/start" {
					t.Fatal("busy session mutated")
				}
			}
		})
	}
}

func TestCodexPermissionStartConfirmsBeforeFirstInput(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsupported", true: "confirmed"}[confirmed], func(t *testing.T) {
			a, p := permissionFixture(t)
			if !confirmed {
				p.corrupt = func(r map[string]any) { delete(r, "approvalsReviewer") }
			}
			id, err := a.StartWithOptions(context.Background(), t.TempDir(), "synthetic", "auto_all", TurnOptions{PermissionMode: "auto_review"})
			if id == "" {
				t.Fatal("created thread identity must survive refusal")
			}
			if confirmed && err != nil || !confirmed && !errors.Is(err, errCodexPermissionUnconfirmed) {
				t.Fatalf("unexpected err %v", err)
			}
			turns := 0
			for _, req := range p.requests {
				if req["method"] == "turn/start" {
					turns++
				}
			}
			if confirmed && turns != 1 || !confirmed && turns != 0 {
				t.Fatalf("turn count %d", turns)
			}
		})
	}
}

func TestCodexPermissionConsentRecheckedAfterRuntimeReply(t *testing.T) {
	a, p := permissionFixture(t)
	allowed := true
	p.resumeHook = func() { allowed = false }
	err := a.SendWithOptions(context.Background(), "permission-thread", "must not send", TurnOptions{
		PermissionMode: "full_access", ExpectedTerminalTurnID: "terminal",
		ValidatePermission: func() error {
			if !allowed {
				return errors.New("consent revoked")
			}
			return nil
		},
	})
	if err == nil || err.Error() != "consent revoked" {
		t.Fatalf("late revocation lost: %v", err)
	}
	for _, req := range p.requests {
		if req["method"] == "turn/start" {
			t.Fatal("input sent after consent revoked")
		}
	}
	if a.threads["permission-thread"].continuedTerminal != "" {
		t.Fatal("revoked operation consumed terminal continuation")
	}
}

func TestCodexPermissionUnconfirmedCanRecoverExplicitly(t *testing.T) {
	a, p := permissionFixture(t)
	p.corrupt = func(r map[string]any) { delete(r, "approvalsReviewer") }
	if err := a.SendWithOptions(context.Background(), "permission-thread", "unsent", TurnOptions{PermissionMode: "auto_review"}); !errors.Is(err, errCodexPermissionUnconfirmed) {
		t.Fatal(err)
	}
	p.corrupt = nil
	if err := a.SendWithOptions(context.Background(), "permission-thread", "recovered", TurnOptions{PermissionMode: "ask"}); err != nil {
		t.Fatal(err)
	}
	if a.threads["permission-thread"].permissionUnconfirmed || a.KnownSessionPermission("permission-thread") != "ask" {
		t.Fatal("explicit safe permission did not recover")
	}
}

func TestCodexPermissionRuntimeEpochMustReconfirm(t *testing.T) {
	a, p := permissionFixture(t)
	target := a.threads["permission-thread"]
	target.loaded = false
	target.info.PermissionMode = "auto_review"
	if err := a.Send(context.Background(), "permission-thread", "resume"); err != nil {
		t.Fatal(err)
	}
	resumes := 0
	for _, req := range p.requests {
		if req["method"] == "thread/resume" {
			resumes++
		}
	}
	if resumes != 2 || a.KnownSessionPermission("permission-thread") != "auto_review" {
		t.Fatalf("runtime mode not reconfirmed: %d %s", resumes, a.KnownSessionPermission("permission-thread"))
	}
	// The verified live epoch avoids another resume on the next explicit send.
	p.requests = nil
	if err := a.SendWithOptions(context.Background(), "permission-thread", "next", TurnOptions{PermissionMode: "auto_review"}); err != nil {
		t.Fatal(err)
	}
	for _, req := range p.requests {
		if req["method"] == "thread/resume" {
			t.Fatal("unchanged live mode performed redundant resume")
		}
	}
}

func TestCodexPermissionUnknownOriginalKeepsHostSettingsAcrossReload(t *testing.T) {
	a, p := permissionFixture(t)
	for attempt := 0; attempt < 2; attempt++ {
		a.threads["permission-thread"].loaded = false
		if err := a.Send(context.Background(), "permission-thread", "follow original"); err != nil {
			t.Fatal(err)
		}
	}
	resumes := 0
	for _, req := range p.requests {
		if req["method"] != "thread/resume" {
			continue
		}
		resumes++
		params := req["params"].(map[string]any)
		for _, key := range []string{"approvalPolicy", "sandbox", "approvalsReviewer"} {
			if _, exists := params[key]; exists {
				t.Fatalf("unknown original overwritten on reload: %s", key)
			}
		}
	}
	if resumes != 2 || a.KnownSessionPermission("permission-thread") != "" {
		t.Fatal("follow-host mode incorrectly confirmed")
	}
}

func TestCodexPermissionGranularOriginalResumesWithoutRewritingPolicy(t *testing.T) {
	a, p := permissionFixture(t)
	a.threads["permission-thread"].loaded = false
	p.corrupt = func(r map[string]any) {
		r["approvalPolicy"] = map[string]any{"granular": map[string]bool{
			"rules": true, "mcp_elicitations": false, "sandbox_approval": true,
			"request_permissions": true, "skill_approval": true,
		}}
		r["approvalsReviewer"] = "user"
	}
	if err := a.Send(context.Background(), "permission-thread", "preserve granular host policy"); err != nil {
		t.Fatal(err)
	}
	turns := 0
	for _, req := range p.requests {
		params, _ := req["params"].(map[string]any)
		if req["method"] == "thread/resume" || req["method"] == "turn/start" {
			for _, key := range []string{"approvalPolicy", "sandbox", "approvalsReviewer"} {
				if _, overwritten := params[key]; overwritten {
					t.Fatalf("inherited granular policy overwritten: %s", key)
				}
			}
		}
		if req["method"] == "turn/start" {
			turns++
		}
	}
	if turns != 1 || a.KnownSessionPermission("permission-thread") != "" {
		t.Fatal("granular resume failed or was labeled as a confirmed UI permission")
	}
	// A granular response is never proof that an explicit UI policy was applied.
	p.requests = nil
	if err := a.SendWithOptions(context.Background(), "permission-thread", "unconfirmed", TurnOptions{PermissionMode: "ask"}); !errors.Is(err, errCodexPermissionUnconfirmed) {
		t.Fatalf("granular acknowledgement accepted for ask: %v", err)
	}
	for _, req := range p.requests {
		if req["method"] == "turn/start" {
			t.Fatal("input sent despite mismatched granular acknowledgement")
		}
	}
}

func TestCodexPermissionPolicyUnionKeepsSiblingFieldsAndRejectsMalformedPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, policy string
		valid        bool
	}{
		{"string", `"on-request"`, true},
		{"absent-policy", `null`, true},
		{"granular", `{"granular":{"rules":true,"mcp_elicitations":false,"sandbox_approval":true}}`, true},
		{"missing-field", `{"granular":{"rules":true}}`, false},
		{"null-field", `{"granular":{"rules":null,"mcp_elicitations":true,"sandbox_approval":true}}`, false},
		{"invalid-flag", `{"granular":{"rules":"true","mcp_elicitations":true,"sandbox_approval":true}}`, false},
		{"wrong-policy", `{"never":true}`, false},
		{"ambiguous-policy", `{"granular":{"rules":true,"mcp_elicitations":true,"sandbox_approval":true},"never":true}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reply struct {
				Thread cxThread `json:"thread"`
				Model  string   `json:"model"`
				codexPermissionReply
			}
			err := json.Unmarshal([]byte(`{"thread":{"id":"native-original","status":{"type":"idle"}},"model":"fixture-model","approvalsReviewer":"user","sandbox":{"type":"workspaceWrite"},"approvalPolicy":`+tc.policy+`}`), &reply)
			if (err == nil) != tc.valid {
				t.Fatalf("policy decode mismatch: %v", err)
			}
			if tc.valid && (reply.Thread.ID != "native-original" || reply.Model != "fixture-model" || reply.ApprovalsReviewer != "user" || reply.Sandbox.Type != "workspaceWrite") {
				t.Fatal("policy decoder consumed sibling reply fields")
			}
		})
	}
}

func TestCodexPermissionExplicitDowngradeNeverResumesWithLegacyAutoAll(t *testing.T) {
	a, p := permissionFixture(t)
	target := a.threads["permission-thread"]
	target.loaded, target.legacyPolicyConfigured, target.policy = false, true, "auto_all"
	resumes := 0
	p.resumeHook = func() {
		resumes++
		if resumes != 1 {
			return
		}
		params := p.requests[len(p.requests)-1]["params"].(map[string]any)
		if _, hasPolicy := params["approvalPolicy"]; hasPolicy || params["sandbox"] != nil {
			t.Fatal("explicit downgrade temporarily restored legacy host permissions")
		}
		a.mu.Lock()
		policy := target.policy
		a.mu.Unlock()
		if policy != "ask" {
			t.Fatal("resume window retained local blanket approval")
		}
	}
	if err := a.SendWithOptions(context.Background(), "permission-thread", "safe downgrade", TurnOptions{PermissionMode: "ask"}); err != nil {
		t.Fatal(err)
	}
	if resumes != 2 || a.KnownSessionPermission("permission-thread") != "ask" {
		t.Fatal("explicit downgrade was not confirmed")
	}
}

type blockedPermissionRead struct {
	base             *permissionTransport
	entered, release chan struct{}
	blocked          bool
}

func (w *blockedPermissionRead) Close() error { return nil }
func (w *blockedPermissionRead) Write(b []byte) (int, error) {
	var r struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, err
	}
	if r.Method == "thread/read" && !w.blocked {
		w.blocked = true
		close(w.entered)
		<-w.release
	}
	return w.base.Write(b)
}

func TestCodexPermissionFirstResumeAndTurnShareAdmissionLock(t *testing.T) {
	a, p := permissionFixture(t)
	target := a.threads["permission-thread"]
	target.loaded = false
	w := &blockedPermissionRead{base: p, entered: make(chan struct{}), release: make(chan struct{})}
	a.conn.stdin = w
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, second := make(chan error, 1), make(chan error, 1)
	go func() {
		first <- a.SendWithOptions(ctx, "permission-thread", "first", TurnOptions{PermissionMode: "full_access"})
	}()
	select {
	case <-w.entered:
	case <-ctx.Done():
		t.Fatal("initial read not reached")
	}
	locked := !target.controlMu.TryLock()
	if !locked {
		target.controlMu.Unlock()
	}
	go func() {
		second <- a.SendWithOptions(ctx, "permission-thread", "second", TurnOptions{PermissionMode: "ask"})
	}()
	close(w.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if !locked {
		t.Fatal("initial read/resume was outside per-thread admission lock")
	}
	var operations []string
	for _, req := range p.requests {
		if req["method"] == "thread/resume" {
			params := req["params"].(map[string]any)
			if params["sandbox"] == "danger-full-access" {
				operations = append(operations, "full_access")
			} else if params["approvalPolicy"] == "on-request" {
				operations = append(operations, "ask")
			}
		}
		if req["method"] == "turn/start" {
			operations = append(operations, "input")
		}
	}
	if !reflect.DeepEqual(operations, []string{"full_access", "input", "ask", "input"}) {
		t.Fatalf("permissions interleaved across input: %v", operations)
	}
}
