package launcher

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeDesktop3PLaterBuildRunsInCompatibilityMode(t *testing.T) {
	s, root, a, starts, _ := fixtureClaude3P(t, true)
	s.Platform = func(context.Context, Tool) (Claude3PPlatform, error) {
		return Claude3PPlatform{Root: root, Version: "2.16500.2"}, nil
	}
	status := s.Status(context.Background(), nil)
	if !status.Supported || status.Verified || !strings.Contains(status.Message, "兼容模式") {
		t.Fatalf("later build should be supported in compatibility mode: %+v", status)
	}
	if _, err := s.Switch(context.Background(), a, nil, false); err != nil || *starts == 0 {
		t.Fatalf("compatible build could not switch: %v", err)
	}
	if _, err := s.Restore(context.Background(), nil); err != nil {
		t.Fatalf("compatible build must stay restorable: %v", err)
	}
	for _, old := range []string{"2.16000.0", "3.0.0"} {
		s.Platform = func(context.Context, Tool) (Claude3PPlatform, error) {
			return Claude3PPlatform{Root: root, Version: old}, nil
		}
		if st := s.Status(context.Background(), nil); st.Supported {
			t.Fatalf("%s must be rejected", old)
		}
	}
}

func TestClaude3PLocationsForMacAndWindows(t *testing.T) {
	root, asar, err := claude3PLocations("darwin", "/Applications/Claude.app/Contents/MacOS/Claude")
	if err != nil || !strings.HasSuffix(root, filepath.Join("Library", "Application Support", "Claude-3p")) || asar != filepath.Join("/Applications/Claude.app/Contents", "Resources", "app.asar") {
		t.Fatalf("mac locations: %q %q %v", root, asar, err)
	}
	if _, _, err := claude3PLocations("darwin", "/usr/local/bin/claude"); err == nil {
		t.Fatal("a non-bundle path must not be treated as Claude.app")
	}
	if _, _, err := claude3PLocations("linux", "/opt/claude/claude"); err == nil {
		t.Fatal("unsupported platforms fail closed")
	}
}
