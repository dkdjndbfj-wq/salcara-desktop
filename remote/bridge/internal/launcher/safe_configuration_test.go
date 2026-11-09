package launcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/toolcfg"
)

func TestSafeClaudeDesktopApplyOnlyStopsWithoutStart(t *testing.T) {
	s, root, a, starts, stops := fixtureClaude3P(t, false)
	before, _ := os.ReadFile(filepath.Join(root, "local-agent-mode-sessions/chat.json"))
	nativeStops := 0
	ctx := WithConfigurationGuard(context.Background(), func() error { return nil })
	ctx = WithNativeConfigurationStopForTest(ctx, func(context.Context, Plan) error { nativeStops++; return nil })
	result, err := s.ApplyAuthorized(ctx, a, nil, true, func() error { return nil })
	if err != nil || *starts != 0 || *stops != 1 || nativeStops != 1 || result.PID != 0 {
		t.Fatal(result, err, *starts, *stops, nativeStops)
	}
	state, err := toolcfg.InspectClaude3P(root)
	if err != nil || state.Mode != "third-party" {
		t.Fatal("apply-only did not write provider", err)
	}
	after, _ := os.ReadFile(filepath.Join(root, "local-agent-mode-sessions/chat.json"))
	if string(before) != string(after) {
		t.Fatal("history changed")
	}
}

func TestSafeClaudeDesktopStopFailureKeepsNativeProfile(t *testing.T) {
	s, root, a, starts, _ := fixtureClaude3P(t, true)
	before := fixtureTree(t, root)
	ctx := WithConfigurationGuard(context.Background(), func() error { return nil })
	ctx = WithNativeConfigurationStopForTest(ctx, func(context.Context, Plan) error { return errors.New("synthetic unknown stop") })
	if _, err := s.ApplyAuthorized(ctx, a, nil, true, func() error { t.Fatal("authorized after failed stop"); return nil }); err == nil || *starts != 0 {
		t.Fatal("stop failure accepted")
	}
	after := fixtureTree(t, root)
	for path, value := range before {
		if after[path] != value {
			t.Fatal("stop failure changed native file", path)
		}
	}
}
