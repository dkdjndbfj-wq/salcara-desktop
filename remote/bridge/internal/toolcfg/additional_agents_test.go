package toolcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAdditionalConfigsPreserveOtherSettings(t *testing.T) {
	t.Run("hermes", func(t *testing.T) {
		home := t.TempDir()
		path := AdditionalPaths("hermes", home)[0]
		original := "# keep user settings\nmodel:\n  default: old\n  reasoning: high\nterminal:\n  backend: local\n"
		if err := os.WriteFile(path, []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
		if err := ApplyAdditional("hermes", home, "http://127.0.0.1/gateway/hermes/v1", "synthetic-lease", "model-one", nil); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		for _, text := range []string{"# keep user settings", "reasoning: high", "backend: local", "provider: custom", "default: model-one", "api_key: synthetic-lease"} {
			if !strings.Contains(string(b), text) {
				t.Fatalf("lost %q: %s", text, b)
			}
		}
	})
	t.Run("deepseek-harness", func(t *testing.T) {
		home := t.TempDir()
		paths := AdditionalPaths("deepseek-harness", home)
		if err := os.MkdirAll(filepath.Dir(paths[0]), 0700); err != nil {
			t.Fatal(err)
		}
		original := "# user patch\n- id: llm-pi-ai\n  config:\n    providers:\n      existing:\n        apiKeyEnv: OTHER_KEY\n- id: unrelated\n  config:\n    setting: keep\n- id: agent-default-model\n  config:\n    model: old\n    reasoningEffort: xhigh\n"
		if err := os.WriteFile(paths[0], []byte(original), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(paths[1], []byte("version: 1\nrefs:\n  OTHER_KEY: keep-key\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := ApplyAdditional("deepseek-harness", home, "http://127.0.0.1/gateway/deepseek-harness/v1", "synthetic-lease", "deepseek-chat", []string{"second"}); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(paths[0])
		if strings.Contains(string(b), "reasoningEffort") {
			t.Fatal("old model effort leaked into the new route")
		}
		credentials, _ := os.ReadFile(paths[1])
		for _, text := range []string{"# user patch", "apiKeyEnv: OTHER_KEY", "setting: keep", "api: openai-completions", "provider: salcara", "model: deepseek-chat", "id: second"} {
			if !strings.Contains(string(b), text) {
				t.Fatalf("lost %q: %s", text, b)
			}
		}
		if !strings.Contains(string(credentials), "OTHER_KEY: keep-key") || !strings.Contains(string(credentials), "SALCARA_API_KEY: synthetic-lease") || strings.Contains(string(b), "synthetic-lease") {
			t.Fatal("credential isolation failed")
		}
	})
}

func TestAdditionalConfigInvalidYAMLDoesNotOverwrite(t *testing.T) {
	for _, kind := range []string{"hermes", "deepseek-harness"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			path := AdditionalPaths(kind, home)[0]
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			original := "[ broken YAML"
			if err := os.WriteFile(path, []byte(original), 0600); err != nil {
				t.Fatal(err)
			}
			if err := ApplyAdditional(kind, home, "http://127.0.0.1/v1", "synthetic-lease", "model", nil); err == nil {
				t.Fatal("accepted malformed native configuration")
			}
			b, _ := os.ReadFile(path)
			if string(b) != original {
				t.Fatal("overwrote malformed original")
			}
		})
	}
}

func TestAdditionalHermesLegacyModelScalarMigratesWithoutLosingOtherSettings(t *testing.T) {
	home := t.TempDir()
	path := AdditionalPaths("hermes", home)[0]
	if err := os.WriteFile(path, []byte("model: provider/old\nterminal:\n  backend: local\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyAdditional("hermes", home, "http://127.0.0.1/v1", "lease", "new-model", nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "backend: local") || !strings.Contains(string(data), "default: new-model") || strings.Contains(string(data), "provider/old") {
		t.Fatalf("legacy Hermes model migration failed: %s, %v", data, err)
	}
}

func TestHarnessHomePatchCannotSilentlyOverrideAppliedAPI(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "cordis.patch.yml"), []byte("- id: llm-pi-ai\n  config:\n    providers: {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ApplyAdditional("deepseek-harness", home, "http://127.0.0.1/v1", "lease", "model", nil); err == nil {
		t.Fatal("reported success with a higher priority override")
	}
	for _, path := range AdditionalPaths("deepseek-harness", home) {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("conflict modified native configuration")
		}
	}
}

func TestAdditionalHomeExpandsTilde(t *testing.T) {
	home, _ := os.UserHomeDir()
	t.Setenv("DSH_HOME", "~/.dsh-fixture")
	if got := AdditionalHome("deepseek-harness"); got != filepath.Join(home, ".dsh-fixture") {
		t.Fatalf("unexpected home: %s", got)
	}
}
