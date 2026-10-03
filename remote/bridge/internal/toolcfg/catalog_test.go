package toolcfg

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexCatalogNeverClonesGPTCapabilitiesIntoRelayIDs(t *testing.T) {
	dir := t.TempDir()
	cache := `{"models":[{"slug":"gpt-5","shell_type":"x","context_window":1},{"slug":"gpt-5-codex","shell_type":"shell_command","context_window":272000,"supported_reasoning_levels":[{"effort":"xhigh"}],"input_modalities":["text","image"],"supports_parallel_tool_calls":true,"upgrade":{"model":"y"},"visibility":"hide","base_instructions":"GPT-specific instructions"}]}`
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := CodexCatalogJSON(dir, []string{"grok-4", "claude-sonnet-4-5", "gpt-5-codex", "grok-4", ""}, "主力")
	if err != nil {
		t.Fatal(err)
	}
	var out struct{ Models []map[string]any }
	if err := json.Unmarshal(b, &out); err != nil || len(out.Models) != 3 {
		t.Fatalf("catalog %s %v", b, err)
	}
	for index, m := range out.Models {
		if m["slug"] != []string{"grok-4", "claude-sonnet-4-5", "gpt-5-codex"}[index] || m["visibility"] != "list" || m["supported_in_api"] != true {
			t.Fatalf("entry %v", m)
		}
		if m["context_window"] != nil || m["max_context_window"] != nil || m["default_reasoning_level"] != nil || len(m["supported_reasoning_levels"].([]any)) != 0 {
			t.Fatalf("invented context/reasoning: %v", m)
		}
		if modalities := m["input_modalities"].([]any); len(modalities) != 1 || modalities[0] != "text" {
			t.Fatalf("invented image modality: %v", modalities)
		}
		if m["upgrade"] != nil || m["supports_parallel_tool_calls"] != false || m["supports_reasoning_summaries"] != false || m["supports_search_tool"] != false || m["multi_agent_version"] != "disabled" {
			t.Fatalf("invented advanced tools: %v", m)
		}
		if m["base_instructions"] == "GPT-specific instructions" || m["base_instructions"] == "" || m["truncation_policy"] == nil || m["experimental_supported_tools"] == nil {
			t.Fatalf("invalid conservative client schema: %v", m)
		}
	}
	if _, err := CodexCatalogJSON(dir, nil, "x"); err == nil {
		t.Fatal("empty catalog must be refused")
	}
}

func TestCodexCatalogWithoutCacheUsesOnlyConservativeLegalSettings(t *testing.T) {
	for _, contents := range []string{"", "invalid-cache", `{"models":[{"slug":"custom","context_window":99999999,"input_modalities":["image"]}]}`} {
		dir := t.TempDir()
		if contents != "" {
			if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		b, err := CodexCatalogJSON(dir, []string{"custom", " padded ", "new\nline", "\x00", strings.Repeat("a", 201)}, "fixture")
		if err != nil {
			t.Fatal(err)
		}
		var out struct{ Models []map[string]any }
		if json.Unmarshal(b, &out) != nil || len(out.Models) != 1 || out.Models[0]["slug"] != "custom" || out.Models[0]["context_window"] != nil {
			t.Fatalf("unsafe entry: %s", b)
		}
	}
}

func TestCodexCatalogPointerOnlyRemovesOurOwn(t *testing.T) {
	dir := t.TempDir()
	path := CodexCatalogPath(dir)
	on := SetCodexCatalogTOML("model = \"gpt-5\"\n", dir, path)
	if !strings.Contains(on, "model_catalog_json") {
		t.Fatal(on)
	}
	if off := SetCodexCatalogTOML(on, dir, ""); strings.Contains(off, "model_catalog_json") {
		t.Fatal(off)
	}
	user := "model_catalog_json = \"/mine.json\"\n"
	if SetCodexCatalogTOML(user, dir, "") != user {
		t.Fatal("must not remove a catalog the user configured")
	}
}
