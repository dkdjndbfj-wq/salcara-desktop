package toolcfg

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
	"salcara/bridge/internal/atomicfile"
)

// Native formats verified against deepseek-ai/deepseek-harness and
// NousResearch/hermes-agent. Only the Salcara provider/connection is changed.
func AdditionalHome(kind string) string {
	key, folder := "DSH_HOME", ".dsh"
	if kind == "hermes" {
		key, folder = "HERMES_HOME", ".hermes"
	}
	if dir := strings.TrimSpace(os.Getenv(key)); dir != "" {
		if dir == "~" || strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, "~\\") {
			home, _ := os.UserHomeDir()
			dir = filepath.Join(home, strings.TrimLeft(strings.TrimPrefix(dir, "~"), "/\\"))
		}
		if abs, err := filepath.Abs(dir); err == nil {
			return abs
		}
	}
	home, _ := os.UserHomeDir()
	if kind == "hermes" && runtime.GOOS == "windows" && os.Getenv("LOCALAPPDATA") != "" {
		return filepath.Join(os.Getenv("LOCALAPPDATA"), "hermes")
	}
	return filepath.Join(home, folder)
}

func AdditionalPaths(kind, home string) []string {
	if kind == "deepseek-harness" {
		return []string{filepath.Join(home, "profiles", "web", "cordis.patch.yml"), filepath.Join(home, ".credentials.yaml")}
	}
	if kind == "hermes" {
		return []string{filepath.Join(home, "config.yaml")}
	}
	return nil
}

func readAgentYAML(path string, kind yaml.Kind) (*yaml.Node, error) {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	root := &yaml.Node{Kind: kind}
	if len(b) == 0 {
		return root, nil
	}
	var doc yaml.Node
	if yaml.Unmarshal(b, &doc) != nil || len(doc.Content) != 1 || doc.Content[0].Kind != kind {
		return nil, errors.New("原 Agent YAML 配置格式无效，未覆盖")
	}
	var check any
	if doc.Decode(&check) != nil {
		return nil, errors.New("原 Agent YAML 配置包含重复或无效字段，未覆盖")
	}
	return doc.Content[0], nil
}

func yamlValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
func putYAML(m *yaml.Node, key string, value any) {
	n := &yaml.Node{}
	_ = n.Encode(value)
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = n
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, n)
}
func removeYAML(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}
func yamlMap(m *yaml.Node, key string) (*yaml.Node, error) {
	n := yamlValue(m, key)
	if n == nil {
		n = &yaml.Node{Kind: yaml.MappingNode}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, n)
	}
	if n.Kind != yaml.MappingNode {
		return nil, errors.New("原 Agent 配置字段类型不兼容，未覆盖")
	}
	return n, nil
}
func patchConfig(root *yaml.Node, id string) (*yaml.Node, error) {
	var found *yaml.Node
	for _, n := range root.Content {
		if n.Kind != yaml.MappingNode {
			return nil, errors.New("原 Harness patch 格式无效，未覆盖")
		}
		if value := yamlValue(n, "id"); value != nil && value.Value == id {
			if found != nil {
				return nil, errors.New("原 Harness patch 包含重复插件，未覆盖")
			}
			found = n
		}
	}
	if found == nil {
		found = &yaml.Node{Kind: yaml.MappingNode}
		putYAML(found, "id", id)
		root.Content = append(root.Content, found)
	}
	return yamlMap(found, "config")
}
func writeAgentYAML(path string, root *yaml.Node) error {
	b, err := yaml.Marshal(root)
	if err != nil {
		return err
	}
	return atomicfile.WriteFileKeepMode(path, b, 0600)
}

// The machine-wide patch is applied after the Web profile in the official
// launcher. Refuse conflicting overrides rather than report a false success.
func checkHarnessHomePatch(home string) error {
	root, err := readAgentYAML(filepath.Join(home, "cordis.patch.yml"), yaml.SequenceNode)
	if err != nil {
		return err
	}
	var conflicts func(*yaml.Node) bool
	conflicts = func(n *yaml.Node) bool {
		if n.Kind == yaml.MappingNode {
			for _, key := range []string{"id", "name"} {
				if value := yamlValue(n, key); value != nil {
					for _, id := range []string{"llm-pi-ai", "agent-default-model", "credentials"} {
						if value.Value == id || value.Value == "@deepseek-ai/dsh-"+id || value.Value == "@deepseek-ai/dsh-credentials-local" {
							return true
						}
					}
				}
			}
		}
		for _, child := range n.Content {
			if conflicts(child) {
				return true
			}
		}
		return false
	}
	if conflicts(root) {
		return errors.New("Harness 主目录的 cordis.patch.yml 覆盖了模型或凭据配置，请先在原工具调整该覆盖后再应用")
	}
	return nil
}

// ApplyAdditional writes a loopback route lease, never an upstream provider key
// in production. The caller owns the multi-file backup/rollback transaction.
func ApplyAdditional(kind, home, base, key, model string, models []string) error {
	if model == "" || key == "" {
		return errors.New("请先选择有效 API 和默认模型")
	}
	paths := AdditionalPaths(kind, home)
	if kind == "hermes" {
		root, err := readAgentYAML(paths[0], yaml.MappingNode)
		if err != nil {
			return err
		}
		// Hermes still accepts the legacy `model: provider/model` scalar.
		// Upgrade that supported shape before writing the explicit connection.
		if old := yamlValue(root, "model"); old != nil && old.Kind == yaml.ScalarNode && old.Tag == "!!str" {
			putYAML(root, "model", map[string]string{"default": old.Value})
			m := yamlValue(root, "model")
			m.HeadComment, m.LineComment, m.FootComment = old.HeadComment, old.LineComment, old.FootComment
		}
		m, err := yamlMap(root, "model")
		if err != nil {
			return err
		}
		putYAML(m, "provider", "custom")
		putYAML(m, "base_url", base)
		putYAML(m, "api_key", key)
		putYAML(m, "default", model)
		return writeAgentYAML(paths[0], root)
	}
	if kind != "deepseek-harness" {
		return errors.New("不支持的 Agent 配置")
	}
	if err := checkHarnessHomePatch(home); err != nil {
		return err
	}
	root, err := readAgentYAML(paths[0], yaml.SequenceNode)
	if err != nil {
		return err
	}
	credentials, err := readAgentYAML(paths[1], yaml.MappingNode)
	if err != nil {
		return err
	}
	if v := yamlValue(credentials, "version"); v != nil && v.Value != "1" {
		return errors.New("不支持的 Harness 凭据版本")
	}
	refs, err := yamlMap(credentials, "refs")
	if err != nil {
		return err
	}
	putYAML(credentials, "version", 1)
	putYAML(refs, "SALCARA_API_KEY", key)
	llm, err := patchConfig(root, "llm-pi-ai")
	if err != nil {
		return err
	}
	providers, err := yamlMap(llm, "providers")
	if err != nil {
		return err
	}
	entries := []map[string]string{}
	seen := map[string]bool{}
	for _, id := range append([]string{model}, models...) {
		if id != "" && !seen[id] {
			entries = append(entries, map[string]string{"id": id})
			seen[id] = true
		}
	}
	putYAML(providers, "salcara", map[string]any{"apiKeyEnv": "SALCARA_API_KEY", "api": "openai-completions", "baseURL": base, "models": entries})
	defaults, err := patchConfig(root, "agent-default-model")
	if err != nil {
		return err
	}
	putYAML(defaults, "provider", "salcara")
	putYAML(defaults, "model", model)
	removeYAML(defaults, "reasoningEffort") // old provider's effort may be invalid for the new route
	if err := writeAgentYAML(paths[1], credentials); err != nil {
		return err
	}
	return writeAgentYAML(paths[0], root)
}
