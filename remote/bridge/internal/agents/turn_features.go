package agents

import (
	"context"
	"errors"
	"strings"

	"salcara/bridge/internal/protocol"
)

// FeatureProvider discovers native turn features for one admitted working
// directory. An existing session ID must be resolved within that adapter's
// authoritative history/live state; a supplied cwd cannot retarget a session.
type FeatureProvider interface {
	Features(context.Context, string, string) (protocol.AgentFeatures, error)
}

func (o TurnOptions) HasFeatures() bool { return o.CollaborationMode != "" || len(o.Skills) != 0 }

// ValidateTurnFeatures checks wire shape only. Each adapter must also resolve
// the exact selected catalog entries immediately before native turn admission.
func ValidateTurnFeatures(o TurnOptions) error {
	if err := protocol.ValidateContextReferences(o.ContextReferences); err != nil {
		return err
	}
	if o.CollaborationMode != "" && o.CollaborationMode != "default" && o.CollaborationMode != "plan" {
		return errors.New("agent_feature_invalid: 计划模式无效")
	}
	if len(o.Skills) > 8 {
		return errors.New("agent_feature_invalid: 一次最多选择 8 个技能")
	}
	for _, skill := range o.Skills {
		if !validSkillDisplayMetadata(skill) {
			return errors.New("agent_feature_invalid: 技能显示信息无效")
		}
		if skill.Name == "" || len(skill.Name) > 160 || strings.TrimSpace(skill.Name) != skill.Name || strings.ContainsAny(skill.Name, "\r\n\t\x00 $<>") || len(skill.Path) > 4096 || strings.ContainsAny(skill.Path, "\r\n\x00") {
			return errors.New("agent_feature_invalid: 技能引用无效")
		}
	}
	return nil
}
