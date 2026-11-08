package launcher

import (
	"context"
	"os"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
)

func TestClaudeUnconfirmedLaunch(t *testing.T) {
	for _, action := range []string{"switch", "resume"} {
		t.Run(action, func(t *testing.T) {
			s, _, _ := switchFixture(t, "claude", "claude")
			var pin string
			s.Start = func(_ context.Context, p Plan) (int, error) { pin = p.ClaudeSettingsPin; return 0, nil }
			a := account("claude", "native", "synthetic-key")
			var result Result
			var err error
			if action == "switch" {
				result, err = s.Switch(context.Background(), a, "claude", t.TempDir(), nil)
			} else {
				result, err = s.Resume(context.Background(), a, "claude", "11111111-2222-4333-8444-555555555555", t.TempDir(), nil)
			}
			defer agents.RemoveClaudeRuntimeSettings(pin)
			if err != nil || result.PID != 0 || !strings.Contains(result.Message, "进程状态尚未确认") {
				t.Fatalf("uncertain launch reported success: %+v %v", result, err)
			}
			if pin == "" {
				t.Fatal("missing runtime pin")
			}
			if _, err := os.Stat(pin); err != nil {
				t.Fatal("uncertain launch removed a potentially live pin")
			}
		})
	}
}
