package agents

import (
	"path/filepath"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestManagerLabelsCLIEventsWithoutMutatingHistoryOrigin(t *testing.T) {
	var events []protocol.Event
	m := NewManagerWithOptions(func(e protocol.Event) { events = append(events, e) }, func() Settings { return Settings{} }, Options{StateDir: t.TempDir(), ClaudeHome: filepath.Join(t.TempDir(), ".claude")}).(*manager)
	defer m.Close()
	info := protocol.SessionInfo{SessionKey: "codex:existing", Client: "Codex App"}
	m.codex.sink(protocol.Event{Type: "session.updated", Session: &info})
	m.claude.sink(protocol.Event{Type: "session.updated", Session: &info})
	if len(events) != 2 {
		t.Fatal("events missing")
	}
	for _, e := range events {
		if e.Session.ControlSurface != "cli" || e.Session.Client != "Codex App" || e.Session == &info {
			t.Fatalf("event=%+v", e.Session)
		}
	}
	if info.ControlSurface != "" {
		t.Fatal("source session mutated")
	}
}
