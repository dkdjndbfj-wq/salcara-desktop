package hubclient

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/protocol"
)

func claudeDesktopStatus(t *testing.T, c *Client) agentStatus {
	t.Helper()
	for _, status := range c.agentStatus(context.Background())["agents"].([]agentStatus) {
		if status.ID == "claude-desktop" {
			return status
		}
	}
	t.Fatal("Claude Desktop status missing")
	return agentStatus{}
}

func TestClaudeDesktopCodePublishesCompleteWorkerCapabilities(t *testing.T) {
	// Read only the real adapter's contract; no installed CLI or provider is used.
	root := t.TempDir()
	m := agents.NewManagerWithOptions(nil, func() agents.Settings { return agents.Settings{} }, agents.Options{
		StateDir: root, ClaudeHome: root, CodexPath: filepath.Join(root, "no-codex"), ClaudePath: filepath.Join(root, "no-claude"),
	})
	t.Cleanup(m.Close)
	c := statusFixture(t, config.Config{})
	c.SetManager(m)
	c.o.DiscoverTools = func(context.Context, map[string]string) []launcher.Tool {
		return []launcher.Tool{{ID: "claude-desktop", Available: true}, {ID: "claude", Available: true}}
	}
	descriptor, err := cliAgentDescriptor(m.Get("claude"))
	if err != nil {
		t.Fatal(err)
	}
	want := publishedRemoteCapabilities(m.Get("claude"), descriptor.Capabilities)
	if !want.Features || !want.ContextReferences || !want.ContextAnnotations || !want.ContextDiffAnnotations || !want.SideQuestion || !want.Context || !want.Compact || !want.SessionActions {
		t.Fatalf("real worker no longer declares the tested advanced controls: %+v", want)
	}
	got := claudeDesktopStatus(t, c)
	if !got.RemoteSendSupported || got.SessionScope != "code" || got.Execution == nil || *got.Execution != (agentExecution{Tool: "claude", ControlSurface: "cli", Entrypoint: "claude-desktop"}) {
		t.Fatalf("Code executor was not retained: %+v", got)
	}
	// Compare the whole contract so newly added capabilities cannot be silently
	// lost by another field whitelist at the Desktop entry point.
	if !reflect.DeepEqual(got.Capabilities, want) {
		t.Fatalf("Desktop Code dropped worker capabilities:\n got %+v\nwant %+v", got.Capabilities, want)
	}
}

func TestClaudeDesktopWithoutWritableWorkerPublishesOnlyHistoryCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name             string
		workerInstalled  bool
		workerRegistered bool
		workerSend       bool
		history          bool
	}{
		{name: "uninstalled-worker-with-history", workerRegistered: true, workerSend: true, history: true},
		{name: "missing-worker-with-history", workerInstalled: true, history: true},
		{name: "send-disabled-with-history", workerInstalled: true, workerRegistered: true, history: true},
		{name: "missing-worker-without-history", workerInstalled: true},
		{name: "send-disabled-without-history", workerInstalled: true, workerRegistered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, worker, _ := aggregationFixture(t, "claude")
			worker.descriptor.Capabilities.Send = tc.workerSend
			worker.descriptor.Capabilities.Subagents = true
			if !tc.workerRegistered {
				c.SetManager(&fakeManager{})
			}
			c.o.DiscoverTools = func(context.Context, map[string]string) []launcher.Tool {
				return []launcher.Tool{{ID: "claude-desktop", Available: true}, {ID: "claude", Available: tc.workerInstalled}}
			}
			if tc.history {
				history := &readonlyHistoryFixture{identity: strings.Repeat("a", 64)}
				c.o.ClaudeDesktopHistory = func(context.Context) (ReadOnlyDesktopHistory, error) { return history, nil }
			}
			got := claudeDesktopStatus(t, c)
			readable := tc.history || tc.workerInstalled && tc.workerRegistered
			want := protocol.AgentCapabilities{List: readable, Read: readable, ImageOutput: readable}
			if got.RemoteSendSupported || !reflect.DeepEqual(got.Capabilities, want) {
				t.Fatalf("non-writable worker advertised execution: %+v", got.Capabilities)
			}
			if tc.history && (got.ReadOnlyHistory == nil || !got.ReadOnlyHistory.Available || !got.ReadOnlyHistory.ReadOnly) {
				t.Fatal("native Chat/Cowork history lost its read-only boundary")
			}
		})
	}
}
