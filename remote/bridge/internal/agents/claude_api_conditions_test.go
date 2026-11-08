package agents

import (
	"strings"
	"testing"
)

func TestClaudeAPIRetryKeepsTurnAndUsesSafeTypedNotice(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	turn := sendClaudeResultFixture(t, a, p)
	before := len(rec.all())
	for _, raw := range []string{
		`{"type":"system","subtype":"api_retry","error":"overloaded","attempt":1,"max_retries":3,"error_status":529}`,
		`{"type":"system","subtype":"api_retry","error":"rate_limit","attempt":2,"max_retries":3,"error_status":429}`,
	} {
		a.handleLine(p, []byte(raw))
	}
	events := rec.all()[before:]
	if len(events) != 2 || events[0].ErrorCode != "model_capacity" || events[1].ErrorCode != "upstream_rate_limit" {
		t.Fatal("native API retries dropped or mistyped")
	}
	for _, e := range events {
		if e.Type != "notice" || !e.Retrying || e.Level != "warn" || e.TurnID != turn || e.SessionKey != "claude:"+p.id || e.ID != events[0].ID {
			t.Fatal("retry changed turn or flooded distinct rows")
		}
	}
	if p.turns != 1 || a.live[p.id].ActiveTurnID != turn {
		t.Fatal("retry completed owned work")
	}
	l := &claudeLine{Type: "system", Subtype: "api_retry", UUID: "synthetic-retry", RetryError: []byte(`"synthetic-private-header"`), RetryAttempt: 999999, RetryStatus: 401}
	ev := newClaudeMapper("claude:history", "").entry(l)
	if len(ev) != 1 || ev[0].ErrorCode != "unknown_execution_error" || strings.Contains(ev[0].Text, "synthetic-private") || strings.Contains(ev[0].Text, "999999") {
		t.Fatal("unknown retry leaked raw data or guessed key failure")
	}
}

func TestClaudeOtherFramesKeepForwardCompatibleErrorObjects(t *testing.T) {
	a, p, rec := newClaudeResultFixture(t)
	turn := sendClaudeResultFixture(t, a, p)
	a.handleLine(p, []byte(`{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":503,"error":{"future":"shape"},"uuid":"terminal-object"}`))
	for _, e := range rec.all() {
		if e.Type == "turn" && e.Status == "failed" && e.TurnID == turn && e.ErrorCode == "upstream_internal_error" {
			return
		}
	}
	t.Fatal("adding retry parsing dropped an otherwise valid terminal frame")
}

func TestClaudeAPITerminalStatusDoesNotGuessFromErrorText(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{401, "upstream_unauthorized"}, {403, "upstream_forbidden"}, {429, "upstream_rate_limit"}, {529, "model_capacity"}, {500, "upstream_internal_error"},
	} {
		e := claudeTerminalEvent(&claudeLine{Subtype: "error_during_execution", IsError: true, APIErrorStatus: tc.status, Result: "unrelated quota/key prose"}, "claude:synthetic")
		if e.Status != "failed" || e.ErrorCode != tc.code || e.Retrying {
			t.Fatal("terminal did not preserve explicit HTTP condition")
		}
	}
	completed := claudeTerminalEvent(&claudeLine{Subtype: "success"}, "claude:synthetic")
	if completed.ErrorCode != "" || completed.Status != "completed" {
		t.Fatal("normal completion changed")
	}
}
