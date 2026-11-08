package agents

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"salcara/bridge/internal/protocol"
)

type codexModePreset struct {
	Mode   string `json:"mode"`
	Model  string `json:"model"`
	Effort string `json:"reasoning_effort"`
}

func featureMethodUnavailable(err error) bool {
	var rpc *rpcError
	return errors.As(err, &rpc) && rpc.Code == -32601
}

func sameFeatureCwd(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func readCodexFeatures(ctx context.Context, c *rpcConn, cwd string) (protocol.AgentFeatures, map[string]codexModePreset, error) {
	out := protocol.AgentFeatures{CollaborationModes: []string{}, Skills: []protocol.Skill{}}
	modes := map[string]codexModePreset{}
	if cwd == "" || !filepath.IsAbs(cwd) || strings.ContainsAny(cwd, "\r\n\x00") {
		return out, modes, errors.New("agent_features_unavailable: 无法确认技能所在项目")
	}
	var presets struct {
		Data []codexModePreset `json:"data"`
	}
	if err := c.Call(ctx, "collaborationMode/list", map[string]any{}, &presets); err != nil {
		if !featureMethodUnavailable(err) {
			return out, modes, err
		}
	} else {
		if len(presets.Data) > 32 {
			return out, modes, errors.New("agent_features_unavailable: 模式目录过大")
		}
		for _, mode := range presets.Data {
			if mode.Mode != "default" && mode.Mode != "plan" {
				continue
			}
			if _, exists := modes[mode.Mode]; exists {
				continue
			}
			modes[mode.Mode] = mode
			out.CollaborationModes = append(out.CollaborationModes, mode.Mode)
		}
	}
	var catalog struct {
		Data []struct {
			Cwd    string `json:"cwd"`
			Skills []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Path        string `json:"path"`
				Enabled     bool   `json:"enabled"`
				Interface   struct {
					DisplayName string `json:"displayName"`
					BrandColor  string `json:"brandColor"`
					IconSmall   string `json:"iconSmall"`
				} `json:"interface"`
			} `json:"skills"`
		} `json:"data"`
	}
	if err := c.Call(ctx, "skills/list", map[string]any{"cwds": []string{cwd}, "forceReload": true}, &catalog); err != nil {
		if featureMethodUnavailable(err) {
			return out, modes, nil
		}
		return out, modes, err
	}
	if len(catalog.Data) > 16 {
		return out, modes, errors.New("agent_features_unavailable: 技能目录过大")
	}
	seen := map[string]bool{}
	iconBytes := 0
	for _, entry := range catalog.Data {
		if !sameFeatureCwd(entry.Cwd, cwd) {
			continue
		}
		out.SkillsSupported = true
		if len(entry.Skills) > protocol.MaxFeatureSkills {
			return out, modes, errors.New("agent_features_unavailable: 技能目录过大")
		}
		for _, skill := range entry.Skills {
			ref := protocol.SkillReference{Name: skill.Name, Path: skill.Path}
			if !skill.Enabled || !filepath.IsAbs(skill.Path) || ValidateTurnFeatures(TurnOptions{Skills: []protocol.SkillReference{ref}}) != nil {
				continue
			}
			key := skill.Name + "\x00" + skill.Path
			if seen[key] {
				continue
			}
			seen[key] = true
			entry := protocol.Skill{Name: skill.Name, Path: skill.Path, Description: truncHead(skill.Description, 1000)}
			if validSkillDisplayName(skill.Interface.DisplayName) {
				entry.DisplayName = skill.Interface.DisplayName
			}
			if skillBrandColor.MatchString(skill.Interface.BrandColor) {
				entry.BrandColor = skill.Interface.BrandColor
			}
			if iconBytes < 2<<20 {
				entry.IconSmall = readSkillCatalogIcon(skill.Path, skill.Interface.IconSmall)
				iconBytes += len(entry.IconSmall)
			}
			out.Skills = append(out.Skills, entry)
			if len(out.Skills) > protocol.MaxFeatureSkills {
				return out, modes, errors.New("agent_features_unavailable: 技能目录超过读取上限")
			}
		}
	}
	if !out.SkillsSupported {
		return out, modes, errors.New("agent_features_unavailable: Codex 返回了其他项目的技能目录")
	}
	return out, modes, nil
}

func (a *codexAgent) Features(ctx context.Context, cwd, sessionID string) (protocol.AgentFeatures, error) {
	if err := ctx.Err(); err != nil {
		return protocol.AgentFeatures{}, err
	}
	if sessionID != "" {
		info, err := a.SessionMetadata(ctx, sessionID)
		if err != nil {
			return protocol.AgentFeatures{}, err
		}
		if info.SessionKey != "codex:"+sessionID {
			return protocol.AgentFeatures{}, errors.New("会话身份不一致")
		}
		cwd = info.Cwd
	}
	c, err := a.ensure(ctx)
	if err != nil {
		return protocol.AgentFeatures{}, err
	}
	if err := validateCodexEffectiveProviderAt(ctx, c, settingsForContext(ctx, a.settings), cwd); err != nil {
		return protocol.AgentFeatures{}, err
	}
	out, _, err := readCodexFeatures(ctx, c, cwd)
	return out, err
}

func prepareCodexTurnFeatures(ctx context.Context, c *rpcConn, cwd, currentModel string, opts TurnOptions) (map[string]any, []protocol.SkillReference, error) {
	if err := ValidateTurnFeatures(opts); err != nil {
		return nil, nil, err
	}
	if !opts.HasFeatures() {
		return nil, nil, nil
	}
	catalog, modes, err := readCodexFeatures(ctx, c, cwd)
	if err != nil {
		return nil, nil, err
	}
	var modeInput map[string]any
	if opts.CollaborationMode != "" {
		preset, exists := modes[opts.CollaborationMode]
		if !exists {
			return nil, nil, errors.New("agent_feature_unavailable: 当前 Codex 未提供所选计划模式")
		}
		model := opts.Model
		if model == "" {
			model = currentModel
		}
		if model == "" {
			model = preset.Model
		}
		if model == "" || len(model) > 200 || strings.ContainsAny(model, "\r\n\x00") {
			return nil, nil, errors.New("agent_feature_unavailable: 无法确认计划模式使用的模型")
		}
		var effort any
		if opts.Effort != "" {
			effort = opts.Effort
		} else if preset.Effort != "" && protocol.ValidModelEffort(preset.Effort) {
			effort = preset.Effort
		}
		modeInput = map[string]any{"mode": opts.CollaborationMode, "settings": map[string]any{"model": model, "reasoning_effort": effort, "developer_instructions": nil}}
	}
	selected := []protocol.SkillReference{}
	seen := map[string]bool{}
	for _, requested := range opts.Skills {
		matches := []protocol.Skill{}
		for _, candidate := range catalog.Skills {
			if candidate.Name == requested.Name && (requested.Path == "" || candidate.Path == requested.Path) {
				matches = append(matches, candidate)
			}
		}
		if !catalog.SkillsSupported || len(matches) != 1 {
			return nil, nil, fmt.Errorf("agent_feature_unavailable: 技能 %s 已不可用或名称不唯一，请刷新", requested.Name)
		}
		key := matches[0].Name + "\x00" + matches[0].Path
		if seen[key] {
			return nil, nil, errors.New("agent_feature_invalid: 重复选择了同一技能")
		}
		seen[key] = true
		selected = append(selected, skillReferenceWithDisplay(matches[0]))
	}
	return modeInput, selected, nil
}

var _ FeatureProvider = (*codexAgent)(nil)
