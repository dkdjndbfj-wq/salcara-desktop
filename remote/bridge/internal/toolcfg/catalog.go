package toolcfg

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// CodexCatalogFile is the model catalog the Bridge publishes into Codex's own
// model picker (config.toml: model_catalog_json). Only this file is owned by us.
const CodexCatalogFile = "salcara-models.json"

// CodexCatalogPath is where the catalog lives inside a Codex home directory.
func CodexCatalogPath(dir string) string { return filepath.Join(dir, CodexCatalogFile) }

// This interface receives relay IDs, not verified per-model capabilities. Even
// an exact GPT-looking slug can route to a different model/group. Never clone
// its native cache entry. These are conservative local client settings, not a
// claim that the upstream supports particular reasoning/image/context limits.
func conservativeRelayEntry() map[string]any {
	return map[string]any{
		"default_reasoning_level":           nil,
		"supported_reasoning_levels":        []map[string]string{},
		"shell_type":                        "shell_command",
		"supports_reasoning_summaries":      false,
		"default_reasoning_summary":         "none",
		"support_verbosity":                 false,
		"supports_parallel_tool_calls":      false,
		"supports_image_detail_original":    false,
		"context_window":                    nil,
		"max_context_window":                nil,
		"input_modalities":                  []string{"text"},
		"experimental_supported_tools":      []string{},
		"supports_search_tool":              false,
		"supports_experimental_context":     false,
		"supports_reasoning_effort_updates": false,
		"multi_agent_version":               "disabled",
		// A neutral client prompt is required by Codex's catalog decoder. Do not
		// copy GPT-family prompts or their unverified tool/capability metadata.
		"base_instructions": "You are a coding assistant. Follow the user's task, use only the supplied tools, and never invent tool results.",
		// A bounded local tool-output budget, not a model context capacity.
		"truncation_policy": map[string]any{"mode": "tokens", "limit": 10000},
	}
}

// CodexCatalogJSON builds model_catalog_json content listing exactly models.
func CodexCatalogJSON(_ string, models []string, source string) ([]byte, error) {
	seen := map[string]bool{}
	list := []map[string]any{}
	for _, m := range models {
		if m == "" || m != strings.TrimSpace(m) || len(m) > 200 || strings.ContainsAny(m, "\r\n\x00") || seen[m] {
			continue
		}
		seen[m] = true
		e := conservativeRelayEntry()
		e["slug"] = m
		e["display_name"] = m
		e["description"] = "来自 " + source + "（Salcara Bridge）"
		e["visibility"] = "list"
		// Required API-key picker registration. This flag is not an entitlement
		// or function-calling test of the relay; /models only supplied its ID.
		e["supported_in_api"] = true
		e["priority"] = len(list) + 1
		list = append(list, e)
	}
	if len(list) == 0 {
		return nil, errors.New("这个 API 还没有模型目录，请先加载上游模型")
	}
	return json.MarshalIndent(map[string]any{"models": list}, "", "  ")
}

// WriteCodexCatalog writes the catalog next to config.toml and returns its path.
func WriteCodexCatalog(dir string, models []string, source string) (string, error) {
	b, err := CodexCatalogJSON(dir, models, source)
	if err != nil {
		return "", err
	}
	path := CodexCatalogPath(dir)
	return path, writeFileKeepMode(path, append(b, '\n'), 0o600)
}

// SetCodexCatalogTOML points config.toml at our catalog (path != "") or removes
// our pointer (path == ""), never touching a catalog the user configured.
func SetCodexCatalogTOML(content, dir, path string) string {
	lines, eol := splitLines(content)
	if path != "" {
		lines = setTopLevel(lines, "model_catalog_json", TOMLString(path))
	} else {
		lines = removeTopLevel(lines, "model_catalog_json", CodexCatalogPath(dir))
	}
	return joinLines(lines, eol)
}

// ApplyCodexCatalog publishes (on) or withdraws (off) the API's models in the
// Codex model picker of the Codex home dir. Runs after the switch wrote config.toml.
func ApplyCodexCatalog(dir string, on bool, models []string, source string) error {
	path := filepath.Join(dir, "config.toml")
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	catalog := ""
	if on {
		if catalog, err = WriteCodexCatalog(dir, models, source); err != nil {
			return err
		}
		// A stale cached list can hide the new entries until Codex refetches.
		_ = os.Remove(filepath.Join(dir, "models_cache.json"))
	}
	out := SetCodexCatalogTOML(string(b), dir, catalog)
	if out == string(b) {
		return nil
	}
	return writeFileKeepMode(path, []byte(out), 0o600)
}
