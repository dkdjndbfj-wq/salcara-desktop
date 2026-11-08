package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func newClaudeResultFixture(t *testing.T) (*claudeAgent, *claudeProc, *recorder) {
	t.Helper()
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings {
		return Settings{Approval: "ask", ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-api-key"}
	}, "not-launched", t.TempDir())
	t.Cleanup(a.Close)
	p := claudeSyntheticPermissionWorker(t, a, "result-fixture")
	p.mapper = newClaudeMapper("claude:"+p.id, p.cwd)
	p.workerSettingsSig = claudeSettingsSig(a.settings(), a.exeOverride)
	p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) { return len(data), nil }}
	return a, p, rec
}

func sendClaudeResultFixture(t *testing.T, a *claudeAgent, p *claudeProc) string {
	t.Helper()
	if err := a.Send(context.Background(), p.id, "synthetic input"); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.turnIDs[len(p.turnIDs)-1]
}

func TestClaudeExplicitPermissionModeNeverInheritsHostDefault(t *testing.T) {
	for _, tc := range []struct{ policy, mode string }{
		{"ask", "default"}, {"", "default"}, {"auto_all", "bypassPermissions"}, {"auto_edits", "acceptEdits"},
	} {
		for _, resume := range []bool{false, true} {
			args := claudeArgs("synthetic", "synthetic-mcp.json", "synthetic-model", tc.policy, resume)
			count := 0
			for i, arg := range args {
				if arg == "--permission-mode" {
					count++
					if i+1 >= len(args) || args[i+1] != tc.mode {
						t.Fatalf("policy %q: mode mismatch", tc.policy)
					}
				}
				if tc.policy != "auto_all" && strings.Contains(arg, "bypass") {
					t.Fatal("non-bypass approval inherited native bypass")
				}
			}
			if count != 1 {
				t.Fatalf("policy %q has %d explicit modes", tc.policy, count)
			}
		}
	}
}

func TestClaudeNonHumanOrForeignResultsDoNotConsumeAdmittedTurns(t *testing.T) {
	for name, extra := range map[string]string{
		"background":               `"origin":{"kind":"task-notification"}`,
		"future source":            `"origin":{"kind":"future-source"}`,
		"missing kind":             `"origin":{}`,
		"invalid origin":           `"origin":"human"`,
		"wrong session":            `"session_id":"other-session"`,
		"wrong transcript session": `"sessionId":"other-session"`,
		"child agent":              `"agent_id":"child-fixture"`,
		"child parent":             `"parent_tool_use_id":"tool-child"`,
		"sidechain":                `"isSidechain":true`,
	} {
		t.Run(name, func(t *testing.T) {
			a, p, rec := newClaudeResultFixture(t)
			first := sendClaudeResultFixture(t, a, p)
			second := sendClaudeResultFixture(t, a, p)
			before := len(rec.all())
			a.handleLine(p, []byte(`{"type":"result","subtype":"success","uuid":"foreign-result",`+extra+`}`))
			p.mu.Lock()
			unchanged := p.turns == 2 && len(p.turnIDs) == 2 && p.turnIDs[0] == first && p.turnIDs[1] == second
			p.mu.Unlock()
			if !unchanged {
				t.Fatal("unrelated result consumed phone work")
			}
			for _, e := range rec.all()[before:] {
				if e.Type == "turn" && e.ParentID == "" {
					t.Fatal("unrelated result emitted root terminal")
				}
			}
			a.mu.Lock()
			info := a.live[p.id]
			a.mu.Unlock()
			if info.ActiveTurnID != first || info.Status != "running" || info.LastTerminalTurnID != "" {
				t.Fatal("unrelated result changed session control target")
			}
		})
	}
}

func TestClaudeResultUUIDReplayCannotCompleteNextTurn(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	first := sendClaudeResultFixture(t, a, p)
	second := sendClaudeResultFixture(t, a, p)
	frame := []byte(`{"type":"result","session_id":"result-fixture","subtype":"success","uuid":"result-one","origin":{"kind":"human"}}`)
	a.handleLine(p, frame)
	a.handleLine(p, frame)
	p.mu.Lock()
	remaining, current := p.turns, p.turnIDs[0]
	p.mu.Unlock()
	if remaining != 1 || current != second {
		t.Fatal("duplicate consumed next prompt")
	}
	var ended []string
	for _, e := range rec.all() {
		if e.Type == "turn" && e.Status != "started" {
			ended = append(ended, e.TurnID)
		}
	}
	if len(ended) != 1 || ended[0] != first {
		t.Fatal("duplicate terminal event was published")
	}
	a.handleLine(p, []byte(`{"type":"result","subtype":"success","uuid":"result-two","origin":null}`))
	a.mu.Lock()
	info := a.live[p.id]
	a.mu.Unlock()
	if info.Status != "idle" || info.ActiveTurnID != "" || info.LastTerminalTurnID != second {
		t.Fatal("real second result did not complete")
	}
}

func TestClaudeUnsolicitedAndRetiredWorkerResultsCannotInventCompletion(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	frame := []byte(`{"type":"result","subtype":"success","uuid":"unsolicited"}`)
	a.handleLine(p, frame)
	if len(rec.all()) != 0 {
		t.Fatal("unsolicited result invented terminal")
	}
	turn := sendClaudeResultFixture(t, a, p)
	a.handleLine(p, frame)
	if p.activeTurnID() != turn {
		t.Fatal("replayed unsolicited result consumed later work")
	}
	next := claudeSyntheticPermissionWorker(t, a, p.id)
	next.mapper = newClaudeMapper("claude:"+p.id, next.cwd)
	before := len(rec.all())
	a.handleLine(p, []byte(`{"type":"result","subtype":"success","uuid":"late-old-worker"}`))
	if len(rec.all()) != before || p.activeTurnID() != turn {
		t.Fatal("replaced worker result changed live state")
	}
	a.mu.Lock()
	a.procs[p.id] = p
	a.mu.Unlock()
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	a.handleLine(p, []byte(`{"type":"result","subtype":"success","uuid":"late-stopped-worker"}`))
	if len(rec.all()) != before || p.activeTurnID() != turn {
		t.Fatal("stopped worker result won over process stop")
	}
}

func TestClaudeResultTerminalReasonAndLegacyCompatibility(t *testing.T) {
	for _, tc := range []struct{ name, body, status, errorPart string }{
		{"legacy success", `"subtype":"success"`, "completed", ""},
		{"explicit success", `"subtype":"success","terminal_reason":"completed"`, "completed", ""},
		{"error despite success", `"subtype":"success","is_error":true,"result":"synthetic API failure"`, "failed", "synthetic API failure"},
		{"API status", `"subtype":"success","terminal_reason":"api_error","api_error_status":429`, "failed", "429"},
		{"aborted streaming", `"subtype":"success","terminal_reason":"aborted_streaming"`, "interrupted", ""},
		{"aborted tool with detail", `"subtype":"success","terminal_reason":"aborted_tools","errors":["synthetic cancellation"]`, "interrupted", "synthetic cancellation"},
		{"limit", `"subtype":"error_max_turns","errors":["synthetic turn limit"]`, "failed", "synthetic turn limit"},
		{"future reason", `"subtype":"success","terminal_reason":"future_outcome"`, "failed", "future_outcome"},
		{"missing status", `"result":"not sufficient evidence"`, "failed", "not sufficient evidence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, p, rec := newClaudeResultFixture(t)
			id := sendClaudeResultFixture(t, a, p)
			a.handleLine(p, []byte(`{"type":"result","duration_ms":123,`+tc.body+`}`))
			var got protocol.Event
			for _, e := range rec.all() {
				if e.Type == "turn" && e.Status != "started" {
					got = e
				}
			}
			if got.TurnID != id || got.Status != tc.status || got.DurationMS != 123 || !strings.Contains(got.Error, tc.errorPart) {
				t.Fatalf("terminal mismatch: %+v", got)
			}
		})
	}
}

func TestClaudeIdenticalAnonymousLegacyResultsRemainSeparateTurns(t *testing.T) {
	a, p, _ := newClaudeResultFixture(t)
	sendClaudeResultFixture(t, a, p)
	sendClaudeResultFixture(t, a, p)
	frame := []byte(`{"type":"result","subtype":"success","result":"same text"}`)
	a.handleLine(p, frame)
	a.handleLine(p, frame)
	if a.ActiveRemoteTurns() {
		t.Fatal("content dedup swallowed legitimate legacy completion")
	}
}

func TestClaudeResultReplayWindowIsBoundedAndExact(t *testing.T) {
	p := &claudeProc{}
	for i := 0; i < maxClaudeResultUUIDs*2; i++ {
		if !p.rememberResultLocked(fmt.Sprintf("synthetic-%d", i)) {
			t.Fatal("fresh result rejected")
		}
	}
	if len(p.resultUUIDs) != maxClaudeResultUUIDs || len(p.resultOrder) != maxClaudeResultUUIDs {
		t.Fatal("unbounded result replay storage")
	}
	if p.rememberResultLocked(fmt.Sprintf("synthetic-%d", maxClaudeResultUUIDs*2-1)) || p.rememberResultLocked(strings.Repeat("x", 257)) {
		t.Fatal("duplicate or oversized identity accepted")
	}
}

func TestClaudeTerminalSinkMayReenterWithoutOverwritingNextAdmission(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	first := sendClaudeResultFixture(t, a, p)
	var second string
	a.sink = func(e protocol.Event) {
		rec.sink(e)
		if e.Type == "session.updated" && e.Session != nil && e.Session.Status == "idle" && second == "" {
			second = sendClaudeResultFixture(t, a, p)
		}
	}
	frame, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "uuid": "reentrant-result"})
	a.handleLine(p, frame)
	a.mu.Lock()
	info := a.live[p.id]
	a.mu.Unlock()
	if second == "" || second == first || info.ActiveTurnID != second || info.Status != "running" {
		t.Fatal("terminal snapshot overwrote new admission")
	}
}
