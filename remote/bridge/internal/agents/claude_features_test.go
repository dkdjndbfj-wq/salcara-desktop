package agents

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

// This process fixture speaks only the SDK JSONL controls and deterministic
// turn results. It never contacts a model, authentication service or MCP server.
func init() {
	role := os.Getenv("SALCARA_CLAUDE_FEATURE_FIXTURE")
	if role == "" {
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("2.1.330 (Claude Code)")
		os.Exit(0)
	}
	log, err := os.OpenFile(filepath.Join(os.Getenv("SALCARA_CLAUDE_FEATURE_LOG"), "frames.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(2)
	}
	defer log.Close()
	enc := json.NewEncoder(os.Stdout)
	args, _ := json.Marshal(map[string]any{"type": "fixture_args", "args": os.Args[1:]})
	_, _ = log.Write(append(args, '\n'))
	mode := "default"
	for i, arg := range os.Args {
		if arg == "--permission-mode" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		_, _ = log.Write(append(append([]byte(nil), scanner.Bytes()...), '\n'))
		_ = log.Sync()
		var frame struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			SessionID string `json:"session_id"`
			Request   struct {
				Subtype string `json:"subtype"`
				Mode    string `json:"mode"`
			} `json:"request"`
		}
		_ = json.Unmarshal(scanner.Bytes(), &frame)
		if frame.Type == "control_request" {
			response := map[string]any{"subtype": "success", "request_id": frame.RequestID, "response": map[string]any{}}
			switch frame.Request.Subtype {
			case "initialize":
				response["response"] = map[string]any{"commands": []any{map[string]any{"name": "builtin-not-a-skill", "description": "must not leak"}}}
				if role == "init-rejected" {
					response["subtype"], response["error"] = "error", "fixture initialization failed"
				}
			case "reload_skills":
				if role == "unsupported" {
					response["subtype"] = "error"
					response["error"] = "Unsupported control request subtype: reload_skills"
				} else if role == "skill-rejected" {
					response["subtype"], response["error"] = "error", "fixture policy refused request"
				} else if role == "malformed" {
					response["response"] = map[string]any{"commands": []any{}}
				} else if role == "timeout" {
					continue
				} else {
					response["response"] = map[string]any{"skills": []any{map[string]any{"name": "plugin:review", "description": "Fixture skill", "path": "must-never-leak"}}}
				}
			case "set_permission_mode":
				mode = frame.Request.Mode
				response["response"] = nil // SDK's void controls permit a null acknowledgement.
			}
			_ = enc.Encode(map[string]any{"type": "control_response", "response": response})
		} else if frame.Type == "user" {
			_ = enc.Encode(map[string]any{"type": "system", "subtype": "init", "session_id": frame.SessionID, "permissionMode": mode})
			_ = enc.Encode(map[string]any{"type": "result", "subtype": "success", "session_id": frame.SessionID, "uuid": fmt.Sprintf("fixture-%d", time.Now().UnixNano())})
		}
	}
	os.Exit(0)
}

func newClaudeFeatureFixture(t *testing.T, role string) (*claudeAgent, *recorder, string, string) {
	t.Helper()
	t.Setenv("SALCARA_CLAUDE_FEATURE_FIXTURE", role)
	logDir, cwd := t.TempDir(), t.TempDir()
	t.Setenv("SALCARA_CLAUDE_FEATURE_LOG", logDir)
	rec := &recorder{}
	s := Settings{BridgeExe: os.Args[0], ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-no-network", Approval: "ask"}
	a := newClaudeAgent(rec.sink, func() Settings { return s }, os.Args[0], t.TempDir())
	t.Cleanup(a.Close)
	return a, rec, logDir, cwd
}

func TestClaudeFeaturesProbeHasNoPromptOrPhantomSession(t *testing.T) {
	for _, role := range []string{"supported", "unsupported", "malformed", "init-rejected", "skill-rejected"} {
		t.Run(role, func(t *testing.T) {
			a, rec, logDir, cwd := newClaudeFeatureFixture(t, role)
			features, err := a.Features(context.Background(), cwd, "")
			wantError := role == "malformed" || role == "init-rejected" || role == "skill-rejected"
			if (err != nil) != wantError || features.SkillsSupported != (role == "supported") {
				t.Fatalf("catalog result %+v, %v", features, err)
			}
			if role == "supported" && (len(features.Skills) != 1 || features.Skills[0].Name != "plugin:review" || features.Skills[0].Path != "") {
				t.Fatalf("wrong native skill catalog: %+v", features)
			}
			frames := readLog(t, logDir, "frames.jsonl")
			wantFrames := 3
			if role == "init-rejected" {
				wantFrames = 2
				if len(features.CollaborationModes) != 0 {
					t.Fatal("failed initialization advertised usable modes")
				}
			}
			if len(frames) != wantFrames || frames[1]["type"] != "control_request" || wantFrames == 3 && frames[2]["type"] != "control_request" {
				t.Fatalf("probe sent non-control prompt: %+v", frames)
			}
			if !strings.Contains(strings.Join(toStrings(frames[0]["args"]), " "), "--no-session-persistence") {
				t.Fatal("probe did not disable session persistence")
			}
			a.mu.Lock()
			ghost := len(a.live) != 0 || len(a.procs) != 0 || len(a.workers) != 0 || len(a.recentOwned) != 0
			a.mu.Unlock()
			if ghost || len(rec.all()) != 0 {
				t.Fatalf("feature probe leaked state: live=%d procs=%d workers=%d recent=%d events=%+v", len(a.live), len(a.procs), len(a.workers), len(a.recentOwned), rec.all())
			}
		})
	}
}

func TestClaudeFeaturesPlanSkillAndPolicyRemainIndependent(t *testing.T) {
	a, rec, logDir, cwd := newClaudeFeatureFixture(t, "supported")
	id, err := a.StartWithOptions(context.Background(), cwd, "inspect the code", "auto_edits", TurnOptions{CollaborationMode: "plan", Skills: []protocol.SkillReference{{Name: "plugin:review"}}})
	if err != nil {
		t.Fatal(err)
	}
	first := rec.wait(t, "plan completion", func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" })
	a.mu.Lock()
	info := a.live[id]
	p := a.procs[id]
	a.mu.Unlock()
	if info.CollaborationMode != "plan" || info.ApprovalMode != "auto_edits" || p.policy != "auto_edits" {
		t.Fatalf("plan overwrote policy or was not confirmed: %+v", info)
	}
	if err := a.SendWithOptions(context.Background(), id, "implement the plan", TurnOptions{CollaborationMode: "default"}); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "default completion", func(e protocol.Event) bool {
		return e.Type == "turn" && e.Status == "completed" && e.TurnID != first.TurnID
	})
	frames := readLog(t, logDir, "frames.jsonl")
	var modes, texts []string
	for _, frame := range frames {
		if req, ok := frame["request"].(map[string]any); ok && req["subtype"] == "set_permission_mode" {
			modes = append(modes, req["mode"].(string))
		}
		if frame["type"] == "user" {
			body := frame["message"].(map[string]any)["content"].([]any)
			texts = append(texts, body[0].(map[string]any)["text"].(string))
		}
	}
	if strings.Join(modes, ",") != "plan,acceptEdits" || len(texts) != 2 || texts[0] != "/plugin:review inspect the code" || texts[1] != "implement the plan" {
		t.Fatalf("wrong native wire: modes=%v texts=%v", modes, texts)
	}
}

func TestClaudeFeaturesRejectInvalidOrStaleSkillBeforeUserFrame(t *testing.T) {
	invalid := []TurnOptions{
		{CollaborationMode: "execution"},
		{Skills: []protocol.SkillReference{{Name: "plugin:review", Path: "C:/untrusted/SKILL.md"}}},
		{Skills: []protocol.SkillReference{{Name: "plugin:review"}, {Name: "second"}}},
		{Skills: []protocol.SkillReference{{Name: " /review"}}},
		{Skills: []protocol.SkillReference{{Name: "a\nb"}}},
		{Skills: []protocol.SkillReference{{Name: "wild*"}}},
		{Skills: []protocol.SkillReference{{Name: strings.Repeat("n", 161)}}},
		{Skills: []protocol.SkillReference{{Name: "a$b"}}},
		{Skills: []protocol.SkillReference{{Name: "a<b"}}},
	}
	for _, opts := range invalid {
		if validateClaudeFeatures(opts) == nil {
			t.Fatalf("accepted invalid option %+v", opts)
		}
	}
	a, _, logDir, cwd := newClaudeFeatureFixture(t, "supported")
	if _, err := a.StartWithOptions(context.Background(), cwd, "must not send", "ask", TurnOptions{Skills: []protocol.SkillReference{{Name: "missing"}}}); err == nil {
		t.Fatal("missing skill was silently sent as ordinary prompt")
	}
	for _, frame := range readLog(t, logDir, "frames.jsonl") {
		if frame["type"] == "user" {
			t.Fatal("unavailable skill reached user frame")
		}
	}
}

func TestClaudeFeaturesDeniedExitPreservesPlanAndInlineProposal(t *testing.T) {
	a, rec, _, cwd := newClaudeFeatureFixture(t, "supported")
	id, err := a.StartWithOptions(context.Background(), cwd, "plan only", "ask", TurnOptions{CollaborationMode: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "completed planning", func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" })
	a.mu.Lock()
	p := a.procs[id]
	a.mu.Unlock()
	r := httptest.NewRequest(http.MethodPost, PermissionPath, nil)
	result := make(chan PermissionDecision, 1)
	go func() {
		result <- a.decideForWorker(r, PermissionRequest{Session: id, ToolName: "ExitPlanMode", ToolUseID: "proposal-id", Input: json.RawMessage(`{"plan":"Review this exact plan","filePath":"C:/must-not-read"}`)}, p)
	}()
	request := rec.wait(t, "exit approval", func(e protocol.Event) bool { return e.Type == "approval.request" })
	if !a.Respond(request.ApprovalID, "deny", "keep planning") {
		t.Fatal("exit approval not found")
	}
	if decision := <-result; decision.Behavior != "deny" {
		t.Fatal("unapproved exit was allowed")
	}
	proposal := rec.wait(t, "inline proposal", func(e protocol.Event) bool { return e.PlanKind == "proposal" })
	if proposal.Output != "Review this exact plan" || proposal.ID != "proposal-id" {
		t.Fatal("proposal not preserved")
	}
	a.observeClaudeMode(p, "default", false)
	p.mu.Lock()
	locked := p.requirePlan && p.collaborationMode == ""
	p.mu.Unlock()
	if !locked {
		t.Fatal("unexpected native exit silently cleared plan restriction")
	}
	if err := a.SendWithOptions(context.Background(), id, "revise the plan", TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	mode := a.live[id].CollaborationMode
	a.mu.Unlock()
	if mode != "plan" {
		t.Fatal("empty mode did not preserve denied plan")
	}
}

func TestClaudeFeaturesProposalMappingPreservesBodyAndDistinguishesChecklist(t *testing.T) {
	m := newClaudeMapper("claude:fixture", t.TempDir())
	for _, name := range []string{"ExitPlanMode", "TodoWrite"} {
		raw := json.RawMessage(fmt.Sprintf(`{"id":"%s","content":[{"type":"tool_use","id":"%s","name":"%s","input":{"plan":"proposal body","todos":[{"content":"check item","status":"pending"}]}}]}`, name, name, name))
		events := m.assistant(raw, "", 1)
		if len(events) != 1 {
			t.Fatal("missing tool projection")
		}
		want := "checklist"
		if name == "ExitPlanMode" {
			want = "proposal"
		}
		if events[0].PlanKind != want {
			t.Fatalf("wrong plan semantic: %+v", events[0])
		}
		result := m.toolResult(claudeBlock{ToolUseID: name, Content: json.RawMessage(`"acknowledged"`)}, 2)
		if result.Output != events[0].Output {
			t.Fatal("tool acknowledgement replaced plan content")
		}
	}
}

func TestClaudeFeaturesControlTimeoutRetiresExactWorkerAndIgnoresWrongID(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	p.stdin = &claudeDeliveryFixtureWriter{write: func(frame []byte) (int, error) {
		// A response for a different request cannot acknowledge a mode change.
		a.handleLine(p, []byte(`{"type":"control_response","response":{"subtype":"success","request_id":"wrong-id","response":{}}}`))
		return len(frame), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Millisecond)
	defer cancel()
	_, err := a.claudeControl(ctx, p, map[string]any{"subtype": "set_permission_mode", "mode": "default"})
	if !errors.Is(err, context.DeadlineExceeded) || !p.transportRetired() || len(rec.all()) != 0 {
		t.Fatal("unacknowledged mode change remained reusable or published confirmed state")
	}
	p.controlMu.Lock()
	pending := len(p.controlPending)
	p.controlMu.Unlock()
	if pending != 0 {
		t.Fatal("timed-out control leaked a request waiter")
	}
}

func TestClaudeFeaturesPlanRestrictionSurvivesConfirmedWorkerExit(t *testing.T) {
	a, rec, logDir, cwd := newClaudeFeatureFixture(t, "supported")
	id, err := a.StartWithOptions(context.Background(), cwd, "prepare plan", "auto_edits", TurnOptions{CollaborationMode: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	first := rec.wait(t, "initial plan completed", func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" })
	a.mu.Lock()
	p := a.procs[id]
	a.mu.Unlock()
	a.observeClaudeMode(p, "default", false) // An unapproved transition is unknown, not executable.
	dir := filepath.Join(a.home, "projects", "fixture")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(`{"type":"user","message":{"role":"user","content":"synthetic"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a.stop(p)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture worker did not exit")
	}
	if !a.hist.ownedCompletion(id) {
		t.Fatal("synthetic completed transcript has no confirmed receipt")
	}
	if err := a.SendWithOptions(context.Background(), id, "revise plan after restart", TurnOptions{}); err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "resumed plan completed", func(e protocol.Event) bool {
		return e.Type == "turn" && e.Status == "completed" && e.TurnID != first.TurnID
	})
	var starts []string
	for _, frame := range readLog(t, logDir, "frames.jsonl") {
		if frame["type"] == "fixture_args" {
			starts = append(starts, strings.Join(toStrings(frame["args"]), " "))
		}
	}
	if len(starts) != 2 || !strings.Contains(starts[1], "--resume "+id) || !strings.Contains(starts[1], "--permission-mode plan") {
		t.Fatalf("resumption lost plan restriction: %v", starts)
	}
}

func TestClaudeFeaturesInternalReloadTimeoutIsAnError(t *testing.T) {
	a, p, _ := newClaudeResultFixture(t)
	_, supported, err := a.reloadClaudeSkills(context.Background(), p)
	if !errors.Is(err, context.DeadlineExceeded) || supported || !p.transportRetired() {
		t.Fatalf("internal timeout was hidden as unsupported: %v %v", supported, err)
	}
}

func TestClaudeFeaturesSkillCatalogBoundsAreExplicitErrors(t *testing.T) {
	if maxClaudeControlResponseBytes > MaxAgentLineBytes {
		t.Fatal("control budget exceeds shared transport bound")
	}
	for _, count := range []int{maxClaudeSkills, maxClaudeSkills + 1, -1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			a, p, _ := newClaudeResultFixture(t)
			p.stdin = &claudeDeliveryFixtureWriter{write: func(frame []byte) (int, error) {
				var request map[string]any
				_ = json.Unmarshal(frame, &request)
				catalog := []map[string]any{}
				if count == -1 {
					catalog = append(catalog, map[string]any{"name": "large", "description": strings.Repeat("x", maxClaudeControlResponseBytes)})
				} else {
					for i := 0; i < count; i++ {
						catalog = append(catalog, map[string]any{"name": fmt.Sprintf("skill-%d", i)})
					}
				}
				raw, _ := json.Marshal(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": request["request_id"], "response": map[string]any{"skills": catalog}}})
				a.handleLine(p, raw)
				return len(frame), nil
			}}
			skills, supported, err := a.reloadClaudeSkills(context.Background(), p)
			if count == maxClaudeSkills {
				if err != nil || !supported || len(skills) != count {
					t.Fatal("in-budget catalog was rejected")
				}
			} else if !errors.Is(err, errClaudeFeatureTooLarge) || supported || len(skills) != 0 {
				t.Fatal("oversized catalog was silently truncated or hidden as unsupported")
			}
		})
	}
}

func TestClaudeFeaturesAnonymousProposalSettlesAfterEveryDecision(t *testing.T) {
	for _, answer := range []string{"allow", "deny", "cancel"} {
		t.Run(answer, func(t *testing.T) {
			a, p, rec := newClaudeResultFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := httptest.NewRequest(http.MethodPost, PermissionPath, nil).WithContext(ctx)
			result := make(chan PermissionDecision, 1)
			go func() {
				result <- a.decideForWorker(r, PermissionRequest{Session: p.id, ToolName: "ExitPlanMode", Input: json.RawMessage(`{"plan":"anonymous proposal"}`)}, p)
			}()
			request := rec.wait(t, "anonymous plan approval", func(e protocol.Event) bool { return e.Type == "approval.request" })
			if answer == "cancel" {
				cancel()
			} else if !a.Respond(request.ApprovalID, answer, "") {
				t.Fatal("approval could not be answered")
			}
			select {
			case <-result:
			case <-time.After(2 * time.Second):
				t.Fatal("anonymous proposal callback did not finish")
			}
			var proposals []protocol.Event
			for _, event := range rec.all() {
				if event.PlanKind == "proposal" {
					proposals = append(proposals, event)
				}
			}
			want := "failed"
			if answer == "allow" {
				want = "done"
			}
			if len(proposals) != 2 || proposals[0].ID != proposals[1].ID || proposals[1].Status != want || proposals[1].Output != "anonymous proposal" {
				t.Fatalf("anonymous plan retained running or lost body: %+v", proposals)
			}
		})
	}
}
