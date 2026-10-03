package console

import (
	"runtime"
	"strings"
	"testing"
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
