package hubclient

import (
	"context"
	"encoding/json"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

func TestClaudeDesktopExecutionDescriptorIsSeparateFromNativeHistory(t *testing.T) {
	for _, tc := range []struct {
		name          string
		code, history bool
	}{{"code and history", true, true}, {"code only", true, false}, {"history only", false, true}, {"neither", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			c := statusFixture(t, config.Config{})
			c.o.DiscoverTools = func(context.Context, map[string]string) []launcher.Tool {
				return []launcher.Tool{{ID: "claude-desktop", Available: true}, {ID: "claude", Available: tc.code}}
			}
			if tc.history {
				fixture, _ := newReadonlyHistoryClient()
				c.o.ClaudeDesktopHistory = fixture.o.ClaudeDesktopHistory
			}
			for _, status := range dispatchStatus(t, c) {
				if status.ID != "claude-desktop" {
					if status.Execution != nil {
						t.Fatalf("Code presentation descriptor leaked to %s", status.ID)
					}
					continue
				}
				if status.Name != "Claude Desktop" || status.ControlSurface != "desktop" || status.SessionScope != "code" {
					t.Fatalf("display identity changed: %+v", status)
				}
				if status.Execution == nil || *status.Execution != (agentExecution{Tool: "claude", ControlSurface: "cli", Entrypoint: "claude-desktop"}) {
					t.Fatalf("Code executor ambiguous: %+v", status.Execution)
				}
				if status.RemoteSendSupported != tc.code || status.Capabilities.Send != tc.code || status.Capabilities.Approval != tc.code || status.Capabilities.Continue != tc.code {
					t.Fatalf("native history granted Code execution: %+v", status.Capabilities)
				}
				if (status.ReadOnlyHistory != nil) != tc.history || status.Capabilities.Read != (tc.code || tc.history) {
					t.Fatal("native history capability was lost or invented")
				}
				encoded, err := json.Marshal(status.Execution)
				if err != nil || string(encoded) != `{"tool":"claude","controlSurface":"cli","entrypoint":"claude-desktop"}` {
					t.Fatalf("unexpected public executor fields: %s %v", encoded, err)
				}
			}
		})
	}
}
