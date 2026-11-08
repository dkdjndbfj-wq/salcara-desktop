package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"salcara/bridge/internal/toolcfg"
	"strings"
	"testing"
)

func TestAdditionalAgentApplyLaunchRestoreAndRollback(t *testing.T) {
	for _, kind := range []string{"deepseek-harness", "hermes"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("DSH_HOME", home)
			t.Setenv("HERMES_HOME", home)
			s := New(t.TempDir())
			tool := stubTool(t, kind)
			tool.Kind = kind
			s.FindTools = func(context.Context, map[string]string) []Tool { return []Tool{tool} }
			a := account(kind, "test", "synthetic-lease")
			started, authorized := false, false
			s.Stop = func(context.Context, Plan) error { t.Fatal("stopped unrelated process"); return nil }
			s.Start = func(_ context.Context, p Plan) (int, error) {
				if !authorized {
					t.Fatal("started before route lease was committed")
				}
				if p.ProfileDir != home || len(p.Args) != 1 {
					t.Fatal("invalid native launch plan")
				}
				want := "web"
				if kind == "hermes" {
					want = "chat"
				}
				if p.Args[0] != want {
					t.Fatal("wrong native entry")
				}
				started = true
				return 42, nil
			}
			authorize := func() error { authorized = true; return nil }
			result, err := s.ApplyAuthorized(context.Background(), a, kind, t.TempDir(), nil, authorize)
			if err != nil || result.PID != 0 || started || !authorized {
				t.Fatalf("apply: %v", err)
			}
			for _, path := range toolcfg.AdditionalPaths(kind, home) {
				if _, err := os.Stat(path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(filepath.Join(home, "settings.json")); !os.IsNotExist(err) {
				t.Fatal("wrote Claude settings")
			}
			result, err = s.SwitchAuthorized(context.Background(), a, kind, t.TempDir(), nil, authorize)
			if err != nil || result.PID != 42 || !started {
				t.Fatalf("launch: %v", err)
			}
			if err := s.RestoreDefault(kind); err != nil {
				t.Fatal(err)
			}
			for _, path := range toolcfg.AdditionalPaths(kind, home) {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("restore did not restore file absence")
				}
			}
			_, err = s.ApplyAuthorized(context.Background(), a, kind, t.TempDir(), nil, func() error { return errors.New("fixture refusal") })
			if err == nil {
				t.Fatal("ignored authorization failure")
			}
			for _, path := range toolcfg.AdditionalPaths(kind, home) {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("failed apply left live configuration")
				}
			}
			if strings.Contains(result.Message, a.Key) {
				t.Fatal("lease exposed")
			}
		})
	}
}
