package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestCodexManagedProjectProviderCannotOverrideSelection(t *testing.T) {
	for _, provider := range []string{"openai", "salcara"} {
		for _, op := range []string{"start", "resume"} {
			t.Run(provider+"/"+op, func(t *testing.T) {
				f := newFixture(t, "auto_all")
				s := Settings{CodexRoot: "http://127.0.0.1:12345/gateway/codex", CodexProvider: provider, CodexKey: "fake-loopback", Approval: "auto_all"}
				f.mu.Lock()
				f.s = s
				f.mu.Unlock()
				var cfg map[string]any
				if provider == "openai" {
					cfg = builtinGuardConfig(s)
					cfg["cli_auth_credentials_store"] = "file"
				} else {
					var entry map[string]any
					cfg, entry = guardedProviderConfig(s)
					entry["http_headers"] = map[string]any{"Authorization": "synthetic-project-retired"}
				}
				b, _ := json.Marshal(cfg)
				t.Setenv("SALCARA_FAKE_CODEX_PROJECT_CONFIG", string(b))
				a := f.m.Get("codex").(*codexAgent)
				var err error
				if op == "start" {
					_, err = a.Start(context.Background(), f.workDir, "must not run", "gpt-new", "auto_all")
				} else {
					err = a.SendWithOptions(context.Background(), "fixture-original-uuid", "must not run", TurnOptions{Model: "gpt-new"})
				}
				if err == nil || err.Error() != errCodexRemoteProvider {
					t.Fatal("project configuration bypassed selected provider")
				}
				for _, row := range readLog(t, f.logDir, "codex-calls.jsonl") {
					if row["method"] == "thread/start" || row["method"] == "thread/resume" || row["method"] == "turn/start" {
						t.Fatal("unsafe project reached thread mutation")
					}
				}
			})
		}
	}
}

func TestCodexLoadedTurnRechecksProjectProvider(t *testing.T) {
	f := newFixture(t, "auto_all")
	s := Settings{CodexRoot: "http://127.0.0.1:12345/gateway/codex", CodexProvider: "salcara", CodexKey: "fake-loopback", Approval: "auto_all"}
	f.mu.Lock()
	f.s = s
	f.mu.Unlock()
	name := filepath.Join(t.TempDir(), "synthetic-project.json")
	t.Setenv("SALCARA_FAKE_CODEX_PROJECT_CONFIG_FILE", name)
	a := f.m.Get("codex").(*codexAgent)
	id, err := a.Start(context.Background(), f.workDir, "first", "gpt-new", "auto_all")
	if err != nil {
		t.Fatal(err)
	}
	f.rec.wait(t, "initial completed", func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" })
	cfg, entry := guardedProviderConfig(s)
	entry["base_url"] = "https://fixture.invalid/retired/v1"
	b, _ := json.Marshal(cfg)
	if err := os.WriteFile(name, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := a.SendWithOptions(context.Background(), id, "must not run", TurnOptions{}); err == nil || err.Error() != errCodexRemoteProvider {
		t.Fatal("loaded turn bypassed changed effective project configuration")
	}
	turns := 0
	for _, row := range readLog(t, f.logDir, "codex-calls.jsonl") {
		if row["method"] == "turn/start" {
			turns++
		}
	}
	if turns != 1 {
		t.Fatal("changed project ran another turn")
	}
}
