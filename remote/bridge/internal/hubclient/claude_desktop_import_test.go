package hubclient

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/desktoplink"
)

type importClaimAgent struct {
	*fakeAgent
	claimed atomic.Bool
}

func (a *importClaimAgent) ClaimDesktopImport(string) bool {
	return a.claimed.CompareAndSwap(false, true)
}

func TestClaudeDesktopNavigationConsumesOneImportAttempt(t *testing.T) {
	for _, nativeError := range []bool{false, true} {
		c, _ := surfaceClient(t)
		a := &importClaimAgent{fakeAgent: &fakeAgent{id: "claude"}}
		c.SetManager(&fakeManager{list: []agents.Agent{a}})
		calls := 0
		c.o.NavigateDesktop = func(ctx context.Context, key string) error {
			calls++
			if !ImportRequested(ctx) || !desktoplink.ClaudeKey(key) {
				t.Fatal("wrong import identity")
			}
			if nativeError {
				return errors.New("synthetic uncertain native result")
			}
			return nil
		}
		cmd := map[string]any{"type": "desktop.navigate", "sessionKey": "claude:0199aaa1-1234-5678-9abc-0123456789ab", "controlSurface": "desktop"}
		result, err := c.Dispatch(context.Background(), cmd)
		if (err != nil) != nativeError {
			t.Fatalf("unexpected handler outcome: %v", err)
		}
		if !nativeError && result.(map[string]any)["requested"] != true {
			t.Fatal("handler response claimed more than requested")
		}
		if _, err := c.Dispatch(context.Background(), cmd); !errors.Is(err, desktoplink.ErrClaudeSessionNavigationUnavailable) {
			t.Fatal("second navigation did not refuse duplicate import")
		}
		if calls != 1 || len(a.sent)+len(a.started)+len(a.stopped) != 0 {
			t.Fatal("navigation repeated or fell back to CLI")
		}
	}
}

func TestClaudeDesktopCanceledNavigationDoesNotConsumeImport(t *testing.T) {
	c, _ := surfaceClient(t)
	a := &importClaimAgent{fakeAgent: &fakeAgent{id: "claude"}}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	c.o.NavigateDesktop = func(context.Context, string) error { t.Fatal("canceled navigation launched handler"); return nil }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.Dispatch(ctx, map[string]any{"type": "desktop.navigate", "sessionKey": "claude:0199aaa1-1234-5678-9abc-0123456789ab", "controlSurface": "desktop"})
	if err == nil || a.claimed.Load() {
		t.Fatal("canceled navigation consumed import")
	}
}
