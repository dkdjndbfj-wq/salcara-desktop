package hubclient

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func surfaceClient(t *testing.T) (*Client, *fakeAgent) {
	t.Helper()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	a := &fakeAgent{id: "codex", sessions: []protocol.SessionInfo{{SessionKey: "codex:1", Client: "Codex App", Controllable: true}}}
	c := New(Options{Store: store})
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	return c, a
}

func TestDesktopControlRequestsNeverFallBackToCLI(t *testing.T) {
	for _, typ := range []string{"sessions.list", "session.open", "session.start", "session.send", "session.interrupt", "approval.respond", "models.list", "desktop.send", "desktop.session.send"} {
		t.Run(typ, func(t *testing.T) {
			c, a := surfaceClient(t)
			cmd := map[string]any{"type": typ, "tool": "codex", "sessionKey": "codex:1", "text": "must not send", "prompt": "must not start", "controlSurface": "desktop"}
			if result, err := c.Dispatch(context.Background(), cmd); err == nil || result != nil || !(strings.Contains(err.Error(), "不会自动改用 CLI") || strings.Contains(err.Error(), "不会改用 CLI")) {
				t.Fatalf("result=%v err=%v", result, err)
			}
			if len(a.opened)+len(a.started)+len(a.sent)+len(a.stopped) != 0 {
				t.Fatal("CLI was called")
			}
		})
	}
	for _, value := range []any{"", "unknown", "read-only", nil, 1, true} {
		c := &Client{}
		if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "controlSurface": value}); err == nil || !strings.Contains(err.Error(), "不受支持") {
			t.Fatalf("surface=%v err=%v", value, err)
		}
	}
}

func TestHistoryOriginAndActualExecutorAreIndependent(t *testing.T) {
	c, a := surfaceClient(t)
	res, err := c.Dispatch(context.Background(), map[string]any{"type": "sessions.list"})
	if err != nil {
		t.Fatal(err)
	}
	ss := res.(map[string]any)["sessions"].([]protocol.SessionInfo)
	if len(ss) != 1 || ss[0].Client != "Codex App" || ss[0].ControlSurface != "cli" {
		t.Fatalf("sessions=%+v", ss)
	}
	if a.sessions[0].ControlSurface != "" {
		t.Fatal("mutated agent-owned history")
	}
	res, err = c.Dispatch(context.Background(), map[string]any{"type": "session.open", "sessionKey": "codex:1", "controlSurface": "cli"})
	if err != nil {
		t.Fatal(err)
	}
	if info := res.(map[string]any)["session"].(protocol.SessionInfo); info.ControlSurface != "cli" {
		t.Fatalf("info=%+v", info)
	}
	for _, surface := range []map[string]any{{"type": "session.send", "sessionKey": "codex:1", "text": "legacy"}, {"type": "session.send", "sessionKey": "codex:1", "text": "explicit", "controlSurface": "cli"}} {
		if _, err := c.Dispatch(context.Background(), surface); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.sent) != 2 {
		t.Fatalf("CLI sends=%v", a.sent)
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "sessions.list", "tool": "codex-desktop"}); err == nil {
		t.Fatal("unknown desktop alias silently accepted")
	}
}

func TestSessionOpenRejectsMismatchedIdentityAndDropsForeignEvents(t *testing.T) {
	c, a := surfaceClient(t)
	a.openKey = "codex:other"
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.open", "sessionKey": "codex:1"}); err == nil || !strings.Contains(err.Error(), "身份不一致") {
		t.Fatalf("expected identity refusal, got %v", err)
	}
	a.openKey = ""
	original := protocol.SessionInfo{SessionKey: "codex:1", ControlSurface: "desktop"}
	a.openEvents = []protocol.Event{
		{Type: "message", SessionKey: "codex:1", Text: "own"},
		{Type: "message", SessionKey: "codex:other", Text: "foreign"},
		{Type: "session.updated", SessionKey: "codex:1", Session: &protocol.SessionInfo{SessionKey: "codex:other"}},
		{Type: "session.updated", SessionKey: "codex:1", Session: &original},
	}
	res, err := c.Dispatch(context.Background(), map[string]any{"type": "session.open", "sessionKey": "codex:1"})
	if err != nil {
		t.Fatal(err)
	}
	events := res.(map[string]any)["events"].([]protocol.Event)
	if len(events) != 2 || events[0].Text != "own" || events[1].Session.ControlSurface != "cli" {
		t.Fatalf("events=%+v", events)
	}
	if original.ControlSurface != "desktop" {
		t.Fatal("source event mutated")
	}
}

func TestDesktopNavigateOnlyRequestsExistingChatWithoutSending(t *testing.T) {
	key := "codex:0199aaa1-1234-5678-9abc-0123456789ab"
	c, a := surfaceClient(t)
	var keys []string
	c.o.NavigateDesktop = func(_ context.Context, k string) error { keys = append(keys, k); return nil }
	res, err := c.Dispatch(context.Background(), map[string]any{"type": "desktop.navigate", "sessionKey": key, "controlSurface": "desktop"})
	if err != nil {
		t.Fatal(err)
	}
	r := res.(map[string]any)
	if r["requested"] != true || r["action"] != "navigate" || len(keys) != 1 || keys[0] != key || len(a.opened) != 1 || len(a.sent)+len(a.started)+len(a.stopped) != 0 {
		t.Fatalf("result=%v keys=%v agent=%+v", r, keys, a)
	}
	for _, bad := range []string{"codex:new", key + "?prompt=send", "claude:" + key[6:], key + "\n"} {
		if _, err := c.Dispatch(context.Background(), map[string]any{"type": "desktop.navigate", "sessionKey": bad}); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if len(keys) != 1 || len(a.opened) != 1 {
		t.Fatal("unsafe navigation reached native callback or history")
	}
}

func TestDesktopNavigateFailsClosedOnUnavailableOrMismatchedHistory(t *testing.T) {
	key := "codex:0199aaa1-1234-5678-9abc-0123456789ab"
	for _, mode := range []string{"unavailable", "missing", "mismatch", "native-error"} {
		t.Run(mode, func(t *testing.T) {
			c, a := surfaceClient(t)
			calls := 0
			c.o.NavigateDesktop = func(context.Context, string) error { calls++; return errors.New("native launch failed") }
			switch mode {
			case "unavailable":
				c.o.NavigateDesktop = nil
			case "missing":
				a.openErr = errors.New("no such thread")
			case "mismatch":
				a.openKey = "codex:another-thread"
			}
			if _, err := c.Dispatch(context.Background(), map[string]any{"type": "desktop.navigate", "sessionKey": key}); err == nil {
				t.Fatal("expected refusal")
			}
			if mode != "native-error" && calls != 0 {
				t.Fatal("native callback invoked before safe verification")
			}
			if len(a.started)+len(a.sent)+len(a.stopped) != 0 {
				t.Fatal("navigation fell back to CLI turn")
			}
		})
	}
}
