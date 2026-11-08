package agents

import (
	"encoding/json"

	"salcara/bridge/internal/protocol"
)

// Native Codex UserInput has separate mention and skill variants. Keep only
// explicit skill entries, with their byte-exact catalog identity. Do not read
// files, require current catalog availability, or infer skills from the prompt:
// historical references must survive a removed skill and an offline host.
func codexUserInputSkills(raw json.RawMessage) []protocol.SkillReference {
	var parts []cxUserInput
	if json.Unmarshal(raw, &parts) != nil {
		return nil
	}
	var refs []protocol.SkillReference
	seen := map[protocol.SkillReference]bool{}
	for _, part := range parts {
		ref := protocol.SkillReference{Name: part.Name, Path: part.Path}
		if part.Type != "skill" || ref.Path == "" || seen[ref] || ValidateTurnFeatures(TurnOptions{Skills: []protocol.SkillReference{ref}}) != nil {
			continue
		}
		refs = append(refs, ref)
		seen[ref] = true
		if len(refs) == 8 {
			break
		}
	}
	return refs
}
