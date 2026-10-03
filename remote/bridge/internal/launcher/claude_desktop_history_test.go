package launcher

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"salcara/bridge/internal/toolcfg"
)

func TestClaudeDesktopReadOnlyHistoryFactoryDoesNotLaunchOrChangeMode(t *testing.T) {
	s, root, _, starts, stops := fixtureClaude3P(t, true)
	account := "01234567-89ab-4cde-8fab-0123456789ab"
	fixtureFile(t, root, "ant-did", base64.StdEncoding.EncodeToString([]byte(account)))
	historyRoot := filepath.Join(root, "local-agent-mode-sessions", account, "00000000-0000-4000-8000-000000000001")
	if err := os.MkdirAll(historyRoot, 0700); err != nil {
		t.Fatal(err)
	}
	before := fixtureTree(t, root)
	h, err := s.ReadOnlyHistory(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.HistoryIdentity()) != 64 {
		t.Fatal("missing opaque namespace")
	}
	list, _, err := h.SessionsPage(context.Background(), "", 10, "desktop-chat")
	if err != nil || len(list) != 0 {
		t.Fatalf("empty real scope: %v %v", list, err)
	}
	if *starts != 0 || *stops != 0 || !reflect.DeepEqual(before, fixtureTree(t, root)) {
		t.Fatal("read-only factory mutated/restarted desktop")
	}
	for _, mode := range []string{"unsupported version", "managed policy", "standard mode"} {
		t.Run(mode, func(t *testing.T) {
			s.Platform = func(context.Context, Tool) (Claude3PPlatform, error) {
				p := Claude3PPlatform{Root: root, Version: toolcfg.ClaudeDesktop3PVersion}
				if mode == "unsupported version" {
					p.Version = "unknown"
				}
				if mode == "managed policy" {
					p.Managed = true
				}
				return p, nil
			}
			if mode == "standard mode" {
				fixtureFile(t, root, "claude_desktop_config.json", `{"deploymentMode":"1p"}`)
			}
			if provider, err := s.ReadOnlyHistory(context.Background(), nil); err == nil || provider != nil {
				t.Fatal("unverified native runtime enabled")
			}
		})
	}
}
