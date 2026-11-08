package toolcfg

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// Uses an explicitly selected isolated official runtime; dump-config never
// starts the Web server or a model request. Nothing touches the user's home.
func TestDeepSeekAppliedWebConfigOfficialComposition(t *testing.T) {
	exe := os.Getenv("SALCARA_DSH_OFFICIAL_E2E_PATH")
	if exe == "" {
		t.Skip("requires an isolated official dsh installation")
	}
	home := t.TempDir()
	if err := ApplyAdditional("deepseek-harness", home, "http://127.0.0.1:1/gateway/deepseek-harness/v1", "synthetic-native-lease", "salcara-fixture", nil); err != nil {
		t.Fatal(err)
	}
	args := []string{"--profile", "web", "--dump-config"}
	entry := exe
	if ext := strings.ToLower(filepath.Ext(exe)); ext == ".cmd" || ext == ".bat" {
		entry = ""
		for _, candidate := range []string{filepath.Join(filepath.Dir(exe), "node_modules", "@deepseek-ai", "dsh", "lib", "bin.js"), filepath.Join(filepath.Dir(exe), "..", "@deepseek-ai", "dsh", "lib", "bin.js")} {
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				entry = candidate
				break
			}
		}
		if entry == "" {
			t.Fatal("cannot locate isolated npm entry")
		}
	}
	if ext := strings.ToLower(filepath.Ext(entry)); ext == ".js" || ext == ".mjs" {
		args = append([]string{entry}, args...)
		var err error
		exe, err = exec.LookPath("node")
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = t.TempDir()
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		upper := strings.ToUpper(name)
		if upper == "DSH_HOME" || strings.HasSuffix(upper, "_API_KEY") || strings.HasSuffix(upper, "_TOKEN") || strings.HasSuffix(upper, "_SECRET") {
			continue
		}
		cmd.Env = append(cmd.Env, pair)
	}
	cmd.Env = append(cmd.Env, "DSH_HOME="+home)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("official isolated config composition failed: %v", err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(output, &doc); err != nil {
		t.Fatal("official dump was not YAML", err)
	}
	var llm, defaults *yaml.Node
	var visit func(*yaml.Node)
	visit = func(node *yaml.Node) {
		if node.Kind == yaml.MappingNode {
			if id := yamlValue(node, "id"); id != nil {
				if id.Value == "llm-pi-ai" {
					llm = yamlValue(node, "config")
				}
				if id.Value == "agent-default-model" {
					defaults = yamlValue(node, "config")
				}
			}
		}
		for _, child := range node.Content {
			visit(child)
		}
	}
	visit(&doc)
	if llm == nil || defaults == nil {
		t.Fatal("official composition did not contain the configured plugins")
	}
	providers := yamlValue(llm, "providers")
	if providers == nil {
		t.Fatal("provider config missing")
	}
	provider := yamlValue(providers, "salcara")
	if provider == nil {
		t.Fatal("salcara provider missing")
	}
	for key, want := range map[string]string{"api": "openai-completions", "apiKeyEnv": "SALCARA_API_KEY", "baseURL": "http://127.0.0.1:1/gateway/deepseek-harness/v1"} {
		if value := yamlValue(provider, key); value == nil || value.Value != want {
			t.Fatalf("official provider field %s differs", key)
		}
	}
	for key, want := range map[string]string{"provider": "salcara", "model": "salcara-fixture"} {
		if value := yamlValue(defaults, key); value == nil || value.Value != want {
			t.Fatalf("official default field %s differs", key)
		}
	}
	if strings.Contains(string(output), "synthetic-native-lease") {
		t.Fatal("composed profile exposed credential value instead of reference")
	}
	credentials, err := readAgentYAML(filepath.Join(home, ".credentials.yaml"), yaml.MappingNode)
	if err != nil {
		t.Fatal(err)
	}
	refs := yamlValue(credentials, "refs")
	if refs == nil || yamlValue(refs, "SALCARA_API_KEY") == nil || yamlValue(refs, "SALCARA_API_KEY").Value != "synthetic-native-lease" {
		t.Fatal("native credential ref did not resolve to isolated lease document")
	}
}
