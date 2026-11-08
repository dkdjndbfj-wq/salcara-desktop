package agents

import (
	"context"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/protocol"
)

type registryFixtureAgent struct{}

func (registryFixtureAgent) ID() string   { return "hermes" }
func (registryFixtureAgent) Name() string { return "Hermes" }
func (registryFixtureAgent) Detect(context.Context) protocol.Tool {
	return protocol.Tool{ID: "hermes", Name: "Hermes", Available: true}
}
func (registryFixtureAgent) Sessions(context.Context) ([]protocol.SessionInfo, error) {
	return nil, nil
}
func (registryFixtureAgent) Open(context.Context, string) (protocol.SessionInfo, []protocol.Event, error) {
	return protocol.SessionInfo{}, nil, nil
}
func (registryFixtureAgent) Start(context.Context, string, string, string, string) (string, error) {
	return "", nil
}
func (registryFixtureAgent) Send(context.Context, string, string) error { return nil }
func (registryFixtureAgent) Interrupt(context.Context, string) error    { return nil }
func (registryFixtureAgent) Respond(string, string, string) bool        { return false }
func (registryFixtureAgent) Models(context.Context) []string            { return nil }
func (registryFixtureAgent) Close()                                     {}
func (registryFixtureAgent) Descriptor() protocol.AgentDescriptor {
	return protocol.AgentDescriptor{ID: "hermes", Name: "Hermes", Tool: "hermes", APIFamily: "hermes", ControlSurface: "cli",
		Capabilities: protocol.AgentCapabilities{List: true, Read: true, Send: true, Questions: true, Attachments: true}}
}

func TestManagerLabelsCLIEventsWithoutMutatingHistoryOrigin(t *testing.T) {
	var events []protocol.Event
	m := NewManagerWithOptions(func(e protocol.Event) { events = append(events, e) }, func() Settings { return Settings{} }, Options{StateDir: t.TempDir(), ClaudeHome: filepath.Join(t.TempDir(), ".claude")}).(*manager)
	defer m.Close()
	info := protocol.SessionInfo{SessionKey: "codex:existing", Client: "Codex App"}
	m.codex.sink(protocol.Event{Type: "session.updated", Session: &info})
	claudeInfo := protocol.SessionInfo{SessionKey: "claude:existing", Client: "Claude Code"}
	m.claude.sink(protocol.Event{Type: "session.updated", Session: &claudeInfo})
	if len(events) != 2 {
		t.Fatal("events missing")
	}
	for _, e := range events {
		if e.Session.ControlSurface != "cli" || e.Session.Client == "" || e.Session == &info || e.Session == &claudeInfo {
			t.Fatalf("event=%+v", e.Session)
		}
	}
	if info.ControlSurface != "" {
		t.Fatal("source session mutated")
	}
}

func TestManagerRegistryKeepsExtensionAdapterSeparateFromBuiltIns(t *testing.T) {
	m := NewManagerWithOptions(nil, func() Settings { return Settings{} }, Options{
		StateDir: t.TempDir(), ClaudeHome: filepath.Join(t.TempDir(), ".claude"),
		ExtraAgents: []Agent{registryFixtureAgent{}},
	}).(*manager)
	defer m.Close()
	if m.Get("hermes") == nil || len(m.Agents()) != 3 {
		t.Fatalf("extension was not registered: %#v", m.Agents())
	}
	d := Describe(m.Get("hermes"))
	if d.ID != "hermes" || !d.Capabilities.Send || d.Tool == "codex" || d.Tool == "claude" {
		t.Fatalf("extension descriptor was mixed with a built-in: %+v", d)
	}
}
