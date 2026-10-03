package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/protocol"
)

const readonlyHistoryTestKey = "claude-desktop:local_11111111-2222-4333-8444-555555555555"

type readonlyHistoryFixture struct {
	identity string
	sessions []protocol.SessionInfo
	events   []protocol.Event
	calls    int
	describe int
	open     int
	err      error
}

func (f *readonlyHistoryFixture) HistoryIdentity() string { return f.identity }
func (f *readonlyHistoryFixture) Describe(context.Context, string) (protocol.SessionInfo, error) {
	f.calls++
	f.describe++
	return f.sessions[0], f.err
}
func (f *readonlyHistoryFixture) SessionsPage(_ context.Context, _ string, _ int, scope string) ([]protocol.SessionInfo, string, error) {
	f.calls++
	out := append([]protocol.SessionInfo{}, f.sessions...)
	for index := range out {
		out[index].SessionScope = scope
	}
	return out, "", f.err
}
func (f *readonlyHistoryFixture) OpenPage(context.Context, string, string, int) (protocol.SessionInfo, []protocol.Event, string, error) {
	f.calls++
	f.open++
	return f.sessions[0], f.events, "", f.err
}

func newReadonlyHistoryClient() (*Client, *readonlyHistoryFixture) {
	f := &readonlyHistoryFixture{identity: strings.Repeat("a", 64), sessions: []protocol.SessionInfo{{SessionKey: readonlyHistoryTestKey, Tool: "claude", Client: "Claude Desktop", SessionScope: "desktop-chat", ControlSurface: "read-only", Title: "Fixture", Status: "idle"}}, events: []protocol.Event{{SessionKey: readonlyHistoryTestKey, Tool: "claude", Type: "message", ID: "m1", Role: "user", Text: "Fixture message"}}}
	c := New(Options{ClaudeDesktopHistory: func(context.Context) (ReadOnlyDesktopHistory, error) { return f, nil }, DiscoverTools: func(context.Context, map[string]string) []launcher.Tool { return nil }})
	return c, f
}

func TestClaudeDesktopReadOnlyRoutesPreserveDistinctNativeIdentity(t *testing.T) {
	c, f := newReadonlyHistoryClient()
	for _, scope := range []string{"desktop-chat", "desktop-cowork"} {
		cmd := map[string]any{"type": "sessions.list", "controlSurface": "read-only", "tool": "claude", "client": scope, "historyIdentity": f.identity}
		handled, result, err := c.dispatchClaudeDesktopHistory(context.Background(), cmd)
		if !handled || err != nil || result.(map[string]any)["historyIdentity"] != f.identity {
			t.Fatalf("list: %v %v %+v", handled, err, result)
		}
	}
	handled, result, err := c.dispatchClaudeDesktopHistory(context.Background(), map[string]any{"type": "session.open", "controlSurface": "read-only", "client": "desktop-chat", "sessionKey": readonlyHistoryTestKey, "historyIdentity": f.identity})
	if !handled || err != nil || result.(map[string]any)["session"].(protocol.SessionInfo).Controllable || result.(map[string]any)["historyIdentity"] != f.identity {
		t.Fatalf("open: %v %v %+v", handled, err, result)
	}
	st := c.claudeReadOnlyHistoryStatus(context.Background())
	if st == nil || !st.Available || !st.ReadOnly || st.Identity != f.identity || len(st.Scopes) != 2 {
		t.Fatalf("metadata: %+v", st)
	}
	raw, _ := json.Marshal(c.agentStatus(context.Background()))
	if !strings.Contains(string(raw), `"desktopHistory"`) || strings.Contains(string(raw), "root") || strings.Contains(string(raw), "uuid") {
		t.Fatalf("scope metadata leak: %s", raw)
	}
}

func TestClaudeDesktopReadOnlyDescribeResolvesColdCategoryWithoutHistory(t *testing.T) {
	for _, scope := range []string{"desktop-chat", "desktop-cowork"} {
		t.Run(scope, func(t *testing.T) {
			c, f := newReadonlyHistoryClient()
			f.sessions[0].SessionScope = scope
			// Describe must not fetch this actionable/untrusted transcript data.
			f.events[0].Type = "approval.request"
			f.events[0].ApprovalID = "must-not-read"
			cmd := map[string]any{"type": "session.describe", "controlSurface": "read-only", "sessionKey": readonlyHistoryTestKey, "historyIdentity": f.identity}
			result, err := c.Dispatch(context.Background(), cmd)
			if err != nil || f.describe != 1 || f.open != 0 || f.calls != 1 {
				t.Fatalf("describe read history/executor: %v %+v", err, f)
			}
			payload := result.(map[string]any)
			info := payload["session"].(protocol.SessionInfo)
			if len(payload) != 2 || payload["historyIdentity"] != f.identity || info.SessionKey != readonlyHistoryTestKey || info.SessionScope != scope || info.Controllable || info.ControlSurface != "read-only" {
				t.Fatalf("unsafe descriptor: %+v", payload)
			}
		})
	}
}

func TestClaudeDesktopReadOnlyDescribeRejectsStaleOrUnverifiedScope(t *testing.T) {
	for _, mode := range []string{"missing identity", "foreign identity", "CLI key", "malformed key", "wrong surface", "wrong tool", "wrong category"} {
		t.Run(mode, func(t *testing.T) {
			c, f := newReadonlyHistoryClient()
			cmd := map[string]any{"type": "session.describe", "controlSurface": "read-only", "sessionKey": readonlyHistoryTestKey, "historyIdentity": f.identity}
			switch mode {
			case "missing identity":
				delete(cmd, "historyIdentity")
			case "foreign identity":
				cmd["historyIdentity"] = strings.Repeat("b", 64)
			case "CLI key":
				cmd["sessionKey"] = "claude:11111111-2222-4333-8444-555555555555"
			case "malformed key":
				cmd["sessionKey"] = "claude-desktop:../other"
			case "wrong surface":
				cmd["controlSurface"] = "cli"
			case "wrong tool":
				cmd["tool"] = "codex"
			case "wrong category":
				cmd["client"] = "desktop-code"
			}
			result, err := c.Dispatch(context.Background(), cmd)
			if err == nil || result != nil || f.calls != 0 {
				t.Fatalf("unsafe describe reached provider: %v %v %+v", result, err, f)
			}
		})
	}
	for _, mode := range []string{"controllable", "lease", "hidden category", "different key", "different client", "different tool", "different surface", "requested category mismatch", "sensitive error"} {
		t.Run(mode, func(t *testing.T) {
			c, f := newReadonlyHistoryClient()
			cmd := map[string]any{"type": "session.describe", "controlSurface": "read-only", "sessionKey": readonlyHistoryTestKey, "historyIdentity": f.identity}
			switch mode {
			case "controllable":
				f.sessions[0].Controllable = true
			case "lease":
				f.sessions[0].ControlExpiresAt = 123
			case "hidden category":
				f.sessions[0].SessionScope = "hidden"
			case "different key":
				f.sessions[0].SessionKey = "claude:some-cli-session"
			case "different client":
				f.sessions[0].Client = "Claude Code"
			case "different tool":
				f.sessions[0].Tool = "codex"
			case "different surface":
				f.sessions[0].ControlSurface = "cli"
			case "requested category mismatch":
				cmd["client"] = "desktop-cowork"
			case "sensitive error":
				f.err = errors.New("C:/private/account-org/fixture-secret-key")
			}
			result, err := c.Dispatch(context.Background(), cmd)
			if err == nil || result != nil || f.describe != 1 || f.open != 0 || strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "C:/private") {
				t.Fatalf("unsafe descriptor/result: %v %v %+v", result, err, f)
			}
		})
	}
}

func TestClaudeDesktopReadOnlyRoutesNeverFallbackToCLIOrNativeTransport(t *testing.T) {
	c, f := newReadonlyHistoryClient()
	for _, typ := range []string{"session.send", "session.start", "session.interrupt", "approval.respond", "desktop.approval.respond", "desktop.navigate", "agents.api.set"} {
		for _, surface := range []string{"", "cli", "desktop", "read-only"} {
			handled, result, err := c.dispatchClaudeDesktopHistory(context.Background(), map[string]any{"type": typ, "controlSurface": surface, "sessionKey": readonlyHistoryTestKey, "historyIdentity": f.identity})
			if !handled || err == nil || result != nil || f.calls != 0 {
				t.Fatalf("native history became executor: %s %s %v %v", typ, surface, handled, err)
			}
		}
	}
	for _, cmd := range []map[string]any{
		{"type": "sessions.list", "controlSurface": "read-only", "tool": "claude", "client": "desktop-code"},
		{"type": "sessions.list", "tool": "claude", "client": "desktop-chat"},
		{"type": "session.open", "controlSurface": "read-only", "sessionKey": "claude:11111111-2222-4333-8444-555555555555"},
		{"type": "session.open", "controlSurface": "read-only", "sessionKey": "claude-desktop:local_../other"},
		{"type": "session.open", "controlSurface": "desktop", "sessionKey": readonlyHistoryTestKey},
	} {
		handled, result, err := c.dispatchClaudeDesktopHistory(context.Background(), cmd)
		if !handled || err == nil || result != nil || f.calls != 0 {
			t.Fatalf("unsafe scope routed: %v %v %v", cmd, handled, err)
		}
	}
	for _, typ := range []string{"sessions.list", "session.open"} {
		cmd := map[string]any{"type": typ, "controlSurface": "cli", "tool": "claude", "client": "desktop-code", "sessionKey": "claude:some-cli-session"}
		if handled, _, _ := c.dispatchClaudeDesktopHistory(context.Background(), cmd); handled {
			t.Fatal("existing Code route intercepted")
		}
	}
}

func TestClaudeDesktopReadOnlyRoutesRejectStaleIdentityAndActionableHistory(t *testing.T) {
	c, f := newReadonlyHistoryClient()
	cmd := map[string]any{"type": "session.open", "controlSurface": "read-only", "client": "desktop-chat", "sessionKey": readonlyHistoryTestKey, "historyIdentity": f.identity}
	for _, identity := range []string{"", strings.Repeat("b", 64), "../../other"} {
		cmd["historyIdentity"] = identity
		if handled, result, err := c.dispatchClaudeDesktopHistory(context.Background(), cmd); !handled || err == nil || result != nil || f.calls > 0 {
			t.Fatal("stale namespace read")
		}
	}
	cmd["historyIdentity"] = f.identity
	for _, mode := range []string{"controllable", "lease", "approval", "subtask link", "different session", "different scope", "different event tool", "different updated client", "sensitive error"} {
		t.Run(mode, func(t *testing.T) {
			c, f := newReadonlyHistoryClient()
			switch mode {
			case "controllable":
				f.sessions[0].Controllable = true
			case "lease":
				f.sessions[0].ControlExpiresAt = 123
			case "approval":
				f.events[0].Type = "approval.request"
				f.events[0].ApprovalID = "pending"
			case "subtask link":
				f.events[0].ChildSessionKeys = []string{"claude:some-cli-session"}
			case "different session":
				f.events[0].SessionKey = "claude:some-cli-session"
			case "different scope":
				f.sessions[0].SessionScope = "desktop-cowork"
			case "different event tool":
				f.events[0].Tool = "codex"
			case "different updated client":
				updated := f.sessions[0]
				updated.Client = "Claude Code"
				f.events[0].Type = "session.updated"
				f.events[0].Session = &updated
			case "sensitive error":
				f.err = errors.New("C:/private/account-org/fixture-secret-key")
			}
			handled, result, err := c.dispatchClaudeDesktopHistory(context.Background(), cmd)
			if !handled || err == nil || result != nil || strings.Contains(err.Error(), "fixture-secret") || strings.Contains(err.Error(), "C:/private") {
				t.Fatalf("unsafe history/error: %v %v %v", handled, err, result)
			}
		})
	}
}
