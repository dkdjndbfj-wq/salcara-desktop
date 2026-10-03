package launcher

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/toolcfg"
)

// PreserveCurrentToolModel is used by the no-model-picker Agent workflow. It
// keeps the original app's configured selection; a fresh installation can use
// the already discovered catalog as a bootstrap, without asking for a model.
// No chat database, authentication token or session is returned to the browser.
func PreserveCurrentToolModel(a config.LocalAccount, target string) (config.LocalAccount, error) {
	if !ValidTarget(target) || target == "claude-desktop" {
		return a, errors.New("此工具需要自己的配置适配器")
	}
	path := toolcfg.CodexConfigPath()
	if strings.HasPrefix(target, "claude") {
		path = toolcfg.ClaudeSettingsPath()
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return a, nil
	}
	if err != nil {
		return a, errors.New("无法读取原工具的模型设置；未切换 API")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(b) > 1<<20 {
		return a, errors.New("原工具配置过大或无法读取；未切换 API")
	}
	model := ""
	if strings.HasPrefix(target, "codex") {
		model = toolcfg.CurrentCodexModel(string(b))
	} else if len(b) > 0 {
		var saved struct {
			Model string                     `json:"model"`
			Env   map[string]json.RawMessage `json:"env"`
		}
		if json.Unmarshal(b, &saved) != nil {
			return a, errors.New("原 Claude Code 配置无法解析；未切换 API")
		}
		model = saved.Model
		if model == "" {
			if raw, ok := saved.Env["ANTHROPIC_MODEL"]; ok && json.Unmarshal(raw, &model) != nil {
				return a, errors.New("原 Claude Code 模型设置无法解析；未切换 API")
			}
		}
	}
	model = strings.TrimSpace(model)
	if len(model) > 200 || strings.ContainsAny(model, "\r\n\x00") {
		return a, errors.New("原工具的模型配置无效；未切换 API")
	}
	if model != "" {
		a.Model = model
		a.Protocol = config.InferProtocol(model, a.Wire)
		if a.Protocol == "" {
			a.Protocol = config.NativeProtocol(strings.Split(target, "-")[0])
		}
	}
	return a, nil
}
