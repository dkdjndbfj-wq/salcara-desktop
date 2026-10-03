package hubclient

import (
	"context"
	"errors"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

// Unknown /models metadata cannot authorize a paid generation with an invented
// effort value. An explicit override needs current, exact runtime evidence.
func validateModelEffort(ctx context.Context, cfg config.Config, agent agents.Agent, model, effort string) error {
	if !protocol.ValidModelEffort(effort) {
		return errors.New("无效的推理强度")
	}
	if effort == "" {
		return nil
	}
	if _, managed, err := managedModelAccount(cfg, agent.ID()); err != nil {
		return err
	} else if managed {
		return errors.New("当前 API 未提供推理档位，请使用 Default")
	}
	if model == "" {
		return errors.New("请先选择模型再设置推理档位")
	}
	reader, ok := agent.(agents.ModelCatalogReader)
	if !ok || agent.ID() != "codex" {
		return errors.New("当前工具未提供推理档位，请使用 Default")
	}
	catalog, err := reader.ModelCatalog(ctx)
	if err != nil {
		return errors.New("读取模型档位失败，请刷新模型")
	}
	capability, exists := catalog.ModelCapabilities[model]
	if !exists || !containsKey(catalog.Models, model) || capability.Source != "codex-model-list" || !capability.ReasoningKnown || !containsKey(capability.ReasoningEfforts, effort) {
		return errors.New("这个模型不支持该推理档位，请刷新后重选")
	}
	return nil
}

// Unknown metadata retains the existing attachment transport; it is not proof
// of image support. Only an exact model explicitly reported as text-only may
// reject an image here, before the worker can start a paid turn.
func validateModelImages(ctx context.Context, cfg config.Config, agent agents.Agent, model, sessionID string, imageCount int) error {
	if imageCount == 0 || agent.ID() != "codex" {
		return nil
	}
	if _, managed, err := managedModelAccount(cfg, agent.ID()); err != nil {
		return err
	} else if managed {
		return nil
	}
	if model == "" && sessionID != "" {
		if known, ok := agent.(interface{ KnownSessionModel(string) string }); ok {
			model = known.KnownSessionModel(sessionID)
		}
	}
	if model == "" {
		return nil
	}
	reader, ok := agent.(agents.ModelCatalogReader)
	if !ok {
		return nil
	}
	catalog, err := reader.ModelCatalog(ctx)
	if err != nil {
		return nil
	}
	capability, exists := catalog.ModelCapabilities[model]
	if exists && containsKey(catalog.Models, model) && capability.Source == "codex-model-list" && containsKey(capability.InputModalities, "text") && !containsKey(capability.InputModalities, "image") {
		return errors.New("当前模型仅支持文字，请移除图片或更换模型")
	}
	return nil
}
