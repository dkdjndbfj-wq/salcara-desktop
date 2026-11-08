package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

type codexFeatureTransport struct {
	base            *resumeSafetyTransport
	agent           *codexAgent
	requests        []map[string]any
	cwd             string
	skills          []map[string]any
	modes           []codexModePreset
	missing         map[string]bool
	readCwd         string
	turnError       bool
	beforeTurnReply func()
	skillReads      int
	skillReadHook   func(int)
}

func (f *codexFeatureTransport) Close() error { return nil }
func (f *codexFeatureTransport) Write(raw []byte) (int, error) {
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		return 0, err
	}
	f.requests = append(f.requests, req)
	method := req["method"].(string)
	var result any
	var rpcErr *rpcError
	if f.missing[method] {
		rpcErr = &rpcError{Code: -32601, Message: "synthetic unsupported"}
	} else {
		switch method {
		case "collaborationMode/list":
			result = map[string]any{"data": f.modes}
		case "skills/list":
			f.skillReads++
			if f.skillReadHook != nil {
				f.skillReadHook(f.skillReads)
			}
			result = map[string]any{"data": []any{map[string]any{"cwd": f.cwd, "skills": f.skills}}}
		case "thread/read":
			thread := f.base.thread
			if f.readCwd != "" {
				thread.Cwd = f.readCwd
			}
			result = map[string]any{"thread": thread}
		case "thread/start":
			result = map[string]any{"thread": f.base.thread, "model": req["params"].(map[string]any)["model"]}
		case "turn/start":
			if f.beforeTurnReply != nil {
				f.beforeTurnReply()
			}
			if f.turnError {
				rpcErr = &rpcError{Code: -32602, Message: "synthetic rejected"}
			} else {
				result = map[string]any{"turn": cxTurn{ID: "feature-turn", Status: "inProgress"}}
			}
		default:
			return f.base.Write(raw)
		}
	}
	response := map[string]any{"id": req["id"], "result": result}
	if rpcErr != nil {
		delete(response, "result")
		response["error"] = rpcErr
	}
	encoded, _ := json.Marshal(response)
	f.base.conn.handleLine(encoded)
	return len(raw), nil
}
func newCodexFeatureFixture(t *testing.T) (*codexAgent, *codexFeatureTransport, *recorder) {
	t.Helper()
	cwd := t.TempDir()
	model := "fixture-model"
	thread := cxThread{ID: "feature-thread", Cwd: cwd, Model: &model, Status: cxThreadStatus{Type: "idle"}}
	a, base, rec := newResumeSafetyAgent(t, thread, false)
	a.applyThreadLocked(thread).loaded = true
	f := &codexFeatureTransport{base: base, agent: a, cwd: cwd, missing: map[string]bool{}, modes: []codexModePreset{{Mode: "default", Model: model}, {Mode: "plan", Model: model, Effort: "high"}}, skills: []map[string]any{
		{"name": "review", "description": "Synthetic review", "path": filepath.Join(cwd, "review", "SKILL.md"), "enabled": true, "interface": map[string]any{"displayName": "Review"}},
		{"name": "disabled", "path": filepath.Join(cwd, "disabled", "SKILL.md"), "enabled": false},
	}}
	a.conn.stdin = f
	return a, f, rec
}
func featureCalls(f *codexFeatureTransport, method string) []map[string]any {
	var out []map[string]any
	for _, r := range f.requests {
		if r["method"] == method {
			out = append(out, r["params"].(map[string]any))
		}
	}
	return out
}
func TestCodexFeaturesUsesNativeCatalogAndTrustedSessionCwd(t *testing.T) {
	a, f, _ := newCodexFeatureFixture(t)
	out, err := a.Features(context.Background(), t.TempDir(), "feature-thread")
	if err != nil || !out.SkillsSupported || len(out.Skills) != 1 || out.Skills[0].Name != "review" || out.Skills[0].DisplayName != "Review" || !reflect.DeepEqual(out.CollaborationModes, []string{"default", "plan"}) {
		t.Fatal(out, err)
	}
	for _, request := range featureCalls(f, "skills/list") {
		if request["forceReload"] != true || !reflect.DeepEqual(request["cwds"], []any{f.cwd}) {
			t.Fatal("untrusted supplied cwd reached skills lookup", request)
		}
	}
	if len(featureCalls(f, "thread/start"))+len(featureCalls(f, "thread/resume"))+len(featureCalls(f, "turn/start")) != 0 {
		t.Fatal("catalog read started a turn")
	}
}

func TestCodexFeaturesLargeNativeDirectoryIsNotTruncated(t *testing.T) {
	for _, count := range []int{1205, protocol.MaxFeatureSkills + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			a, transport, _ := newCodexFeatureFixture(t)
			transport.skills = make([]map[string]any, count)
			for i := range transport.skills {
				name := fmt.Sprintf("skill-%d", i)
				transport.skills[i] = map[string]any{"name": name, "description": name, "enabled": true, "path": filepath.Join(transport.cwd, name, "SKILL.md")}
			}
			value, err := a.Features(context.Background(), transport.cwd, "")
			if count <= protocol.MaxFeatureSkills {
				if err != nil || len(value.Skills) != count {
					t.Fatal("large native directory lost entries", len(value.Skills), err)
				}
			} else if err == nil {
				t.Fatal("oversized directory silently truncated")
			}
		})
	}
}
func TestCodexFeaturesTransportModeSkillsAndConfirmedAck(t *testing.T) {
	for _, operation := range []string{"send", "start", "continue"} {
		t.Run(operation, func(t *testing.T) {
			a, f, _ := newCodexFeatureFixture(t)
			opts := TurnOptions{CollaborationMode: "plan", Model: "chosen-model", Effort: "medium", Skills: []protocol.SkillReference{{Name: "review", Path: f.skills[0]["path"].(string)}}}
			f.beforeTurnReply = func() {
				if a.threads["feature-thread"].info.CollaborationMode != "" {
					t.Fatal("mode was optimistically confirmed before native acknowledgment")
				}
			}
			var err error
			switch operation {
			case "send":
				err = a.SendWithOptions(context.Background(), "feature-thread", "check this", opts)
			case "start":
				_, err = a.StartWithOptions(context.Background(), f.cwd, "check this", "ask", opts)
			case "continue":
				a.threads["feature-thread"].info.LastTerminalTurnID = "previous-turn"
				_, err = a.ContinueWithOptions(context.Background(), "feature-thread", "check this", "previous-turn", "11111111-2222-4333-8444-555555555555", opts)
			}
			if err != nil {
				t.Fatal(err)
			}
			calls := featureCalls(f, "turn/start")
			if len(calls) != 1 {
				t.Fatal(calls)
			}
			mode := calls[0]["collaborationMode"].(map[string]any)
			settings := mode["settings"].(map[string]any)
			if mode["mode"] != "plan" || settings["model"] != "chosen-model" || settings["reasoning_effort"] != "medium" {
				t.Fatal(mode)
			}
			if value, ok := settings["developer_instructions"]; !ok || value != nil {
				t.Fatal("native built-in instructions were replaced", settings)
			}
			input := calls[0]["input"].([]any)
			if len(input) != 2 || input[0].(map[string]any)["text"] != "check this" || input[1].(map[string]any)["type"] != "skill" || input[1].(map[string]any)["path"] != f.skills[0]["path"] {
				t.Fatal(input)
			}
			if a.threads["feature-thread"].info.CollaborationMode != "plan" {
				t.Fatal("accepted native mode was not confirmed")
			}
		})
	}
}
func TestCodexFeaturesRejectsUnknownDisabledAmbiguousAndForeignSkills(t *testing.T) {
	for _, kind := range []string{"unknown", "disabled", "path", "duplicate-name", "duplicate-selection", "foreign-cwd", "unsupported-mode", "unsupported-skill"} {
		t.Run(kind, func(t *testing.T) {
			a, f, _ := newCodexFeatureFixture(t)
			ref := protocol.SkillReference{Name: "review", Path: f.skills[0]["path"].(string)}
			opts := TurnOptions{Skills: []protocol.SkillReference{ref}}
			switch kind {
			case "unknown":
				opts.Skills[0].Name = "missing"
			case "disabled":
				opts.Skills[0] = protocol.SkillReference{Name: "disabled"}
			case "path":
				opts.Skills[0].Path = filepath.Join(t.TempDir(), "SKILL.md")
			case "duplicate-name":
				opts.Skills[0].Path = ""
				f.skills = append(f.skills, map[string]any{"name": "review", "path": filepath.Join(f.cwd, "other", "SKILL.md"), "enabled": true})
			case "duplicate-selection":
				opts.Skills = append(opts.Skills, ref)
			case "foreign-cwd":
				f.cwd = t.TempDir()
			case "unsupported-mode":
				opts.Skills = nil
				opts.CollaborationMode = "plan"
				f.missing["collaborationMode/list"] = true
			case "unsupported-skill":
				f.missing["skills/list"] = true
			}
			if err := a.SendWithOptions(context.Background(), "feature-thread", "input", opts); err == nil {
				t.Fatal("invalid feature reached native send")
			}
			if len(featureCalls(f, "turn/start"))+len(featureCalls(f, "thread/resume")) != 0 {
				t.Fatal("rejected feature had turn/resume side effects", f.requests)
			}
		})
	}
}
func TestCodexFeaturesRevalidatesCatalogBeforeTurnAndNoOptimisticMode(t *testing.T) {
	a, f, _ := newCodexFeatureFixture(t)
	f.turnError = true
	if err := a.SendWithOptions(context.Background(), "feature-thread", "input", TurnOptions{CollaborationMode: "plan"}); err == nil {
		t.Fatal("rejected turn succeeded")
	}
	if a.threads["feature-thread"].info.CollaborationMode != "" {
		t.Fatal("rejected turn confirmed mode")
	}
	f.turnError = false
	f.requests = nil
	if err := a.SendWithOptions(context.Background(), "feature-thread", "ordinary", TurnOptions{Skills: []protocol.SkillReference{}}); err != nil {
		t.Fatal(err)
	}
	if len(featureCalls(f, "skills/list"))+len(featureCalls(f, "collaborationMode/list")) != 0 {
		t.Fatal("empty selections changed host behavior")
	}
	if _, present := featureCalls(f, "turn/start")[0]["collaborationMode"]; present {
		t.Fatal("omitted mode became an override")
	}
}

func TestCodexFeaturesRemovedSelectionCannotReachFinalTurnAdmission(t *testing.T) {
	a, f, _ := newCodexFeatureFixture(t)
	f.skillReadHook = func(n int) {
		if n == 2 {
			f.skills[0]["enabled"] = false
		}
	}
	err := a.SendWithOptions(context.Background(), "feature-thread", "input", TurnOptions{Skills: []protocol.SkillReference{{Name: "review"}}})
	if err == nil || f.skillReads != 2 || len(featureCalls(f, "turn/start")) != 0 {
		t.Fatal("stale selection reached turn/start", err, f.skillReads)
	}
}
func TestCodexFeaturesUnavailableCatalogIsExplicitAndBounded(t *testing.T) {
	a, f, _ := newCodexFeatureFixture(t)
	f.missing["skills/list"] = true
	f.missing["collaborationMode/list"] = true
	out, err := a.Features(context.Background(), f.cwd, "")
	if err != nil || out.SkillsSupported || len(out.Skills)+len(out.CollaborationModes) != 0 {
		t.Fatal(out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Features(ctx, f.cwd, ""); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, opts := range []TurnOptions{{CollaborationMode: "made-up"}, {Skills: []protocol.SkillReference{{Name: "bad\nname"}}}, {Skills: make([]protocol.SkillReference, 9)}} {
		if ValidateTurnFeatures(opts) == nil {
			t.Fatal("malformed feature accepted", opts)
		}
	}
}
func TestCodexPlanProposalFinalIsAuthoritative(t *testing.T) {
	a, _, rec := newCodexFeatureFixture(t)
	a.threads["feature-thread"].turnID = "proposal-turn"
	a.onNotify("item/started", json.RawMessage(`{"threadId":"feature-thread","turnId":"proposal-turn","item":{"type":"plan","id":"plan-one","text":"seed"}}`))
	a.onNotify("item/plan/delta", json.RawMessage(`{"threadId":"feature-thread","turnId":"proposal-turn","itemId":"plan-one","delta":" draft"}`))
	if got := a.threads["feature-thread"].items["plan-one"].lastEvent; got.Output != "seed draft" || got.PlanKind != "proposal" {
		t.Fatal(got)
	}
	a.onNotify("item/completed", json.RawMessage(`{"threadId":"feature-thread","turnId":"proposal-turn","item":{"type":"plan","id":"plan-one","text":""}}`))
	a.onNotify("item/plan/delta", json.RawMessage(`{"threadId":"feature-thread","turnId":"proposal-turn","itemId":"plan-one","delta":"late"}`))
	got := a.threads["feature-thread"].items["plan-one"].lastEvent
	if got.Output != "" || !got.OutputFinal || got.Status != "done" || got.PlanKind != "proposal" {
		t.Fatal("final plan lost authority", got)
	}
	for _, event := range rec.all() {
		if strings.Contains(event.Output, "late") {
			t.Fatal("late delta overwrote final plan")
		}
	}
	progress := codexPlanEvent("codex:feature-thread", "progress", []cxPlanStep{{Step: "do work", Status: "inProgress"}})
	if progress.PlanKind != "" {
		t.Fatal("ordinary progress was mislabeled a proposal")
	}
}

func TestCodexFeaturesOmittedModeFollowsLoadedRuntimeButResumeClearsConfirmation(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "loaded", true: "resumed"}[resumed], func(t *testing.T) {
			a, f, _ := newCodexFeatureFixture(t)
			thread := a.threads["feature-thread"]
			thread.info.CollaborationMode = "plan"
			if resumed {
				old := resumeSafetyRollout(t, time.Hour)
				f.base.thread.Path = &old
				thread.loaded = false
			}
			if err := a.SendWithOptions(context.Background(), "feature-thread", "next input", TurnOptions{}); err != nil {
				t.Fatal(err)
			}
			calls := featureCalls(f, "turn/start")
			if len(calls) != 1 {
				t.Fatal(calls)
			}
			if _, exists := calls[0]["collaborationMode"]; exists {
				t.Fatal("omitted selection rewrote native mode", calls[0])
			}
			if resumed && thread.info.CollaborationMode != "" || !resumed && thread.info.CollaborationMode != "plan" {
				t.Fatal("mode confirmation crossed runtime/resume boundary", thread.info.CollaborationMode)
			}
		})
	}
}

func TestCodexFeaturesDelayedModeAckCannotOverwriteSuccessor(t *testing.T) {
	a, f, _ := newCodexFeatureFixture(t)
	f.beforeTurnReply = func() {
		thread := a.threads["feature-thread"]
		thread.turnVersion++
		thread.turnID = "successor-turn"
		thread.info.CollaborationMode = "default"
	}
	if err := a.SendWithOptions(context.Background(), "feature-thread", "input", TurnOptions{CollaborationMode: "plan"}); err != nil {
		t.Fatal(err)
	}
	if got := a.threads["feature-thread"].info.CollaborationMode; got != "default" {
		t.Fatal("late acknowledgment overwrote successor mode", got)
	}
}

func TestCodexFeaturesNullablePresetModelUsesConfirmedThreadModel(t *testing.T) {
	a, f, _ := newCodexFeatureFixture(t)
	f.modes[1].Model = "" // Native preset masks may omit their model.
	if err := a.SendWithOptions(context.Background(), "feature-thread", "input", TurnOptions{CollaborationMode: "plan"}); err != nil {
		t.Fatal(err)
	}
	calls := featureCalls(f, "turn/start")
	mode := calls[0]["collaborationMode"].(map[string]any)
	if got := mode["settings"].(map[string]any)["model"]; got != "fixture-model" {
		t.Fatal("metadata-only preflight lost the confirmed thread model", got)
	}
}
