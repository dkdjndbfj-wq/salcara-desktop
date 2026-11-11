package console

import (
	"runtime"
	"strings"
	"testing"

	"salcara/bridge/internal/launcher"
)

func TestInstallersAreFixedOfficialCommands(t *testing.T) {
	want := map[string]string{"claude": "claude.ai/install", "codex": "chatgpt.com/codex/install"}
	for tool, host := range want {
		cmd, err := installCommand(tool)
		if runtime.GOOS != "windows" && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			continue
		}
		if err != nil || !strings.Contains(strings.Join(cmd.Args, " "), host) {
			t.Fatalf("%s: %v %v", tool, cmd, err)
		}
	}
	if _, err := installCommand("rm -rf /"); err == nil {
		t.Fatal("unknown tool accepted")
	}
}

func TestBundledCodexIsRecognised(t *testing.T) {
	if !bundledCodex(`C:\Users\a\AppData\Local\Programs\Codex\resources\codex.exe`) || bundledCodex(`C:\Users\a\AppData\Local\Programs\OpenAI\Codex\bin\codex.exe`) {
		t.Fatal("bundled detection")
	}
}

func TestSetupKeepsAgentRequirementsLocalToTheChosenTool(t *testing.T) {
	for _, available := range []string{"", "codex", "claude", "deepseek-harness", "hermes"} {
		t.Run(available, func(t *testing.T) {
			items := setupInventoryItems("windows", []launcher.Tool{{ID: available, Available: available != ""}}, "", false, setupItem{ID: "claude-sdk"})
			found := available == ""
			for _, item := range items {
				if item.Required {
					t.Fatalf("%s incorrectly blocks global setup and pairing", item.ID)
				}
				if item.ID == available {
					found = item.Installed
				} else if item.Installed {
					t.Fatalf("missing tool %s was marked available", item.ID)
				}
			}
			if !found {
				t.Fatal("the selected installed Agent was lost")
			}
		})
	}
}
