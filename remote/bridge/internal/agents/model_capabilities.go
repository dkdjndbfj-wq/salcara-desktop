package agents

import (
	"context"
	"strings"

	"salcara/bridge/internal/protocol"
)

// ModelCatalogReader is optional so old/custom agents retain their existing interface.
type ModelCatalogReader interface {
	ModelCatalog(context.Context) (protocol.ModelCatalog, error)
}

type codexModelPage struct {
	Data []struct {
		ID                        string `json:"id"`
		Model                     string `json:"model"`
		Hidden                    bool   `json:"hidden"`
		SupportedReasoningEfforts *[]struct {
			ReasoningEffort string `json:"reasoningEffort"`
		} `json:"supportedReasoningEfforts"`
		InputModalities []string `json:"inputModalities"`
	} `json:"data"`
	NextCursor string `json:"nextCursor"`
}

func appendCodexModelPage(out *protocol.ModelCatalog, page codexModelPage) {
	for _, model := range page.Data {
		id := model.Model
		if id == "" {
			id = model.ID
		}
		if model.Hidden || id == "" || len(id) > 200 || strings.ContainsAny(id, "\r\n\x00") {
			continue
		}
		if _, exists := out.ModelCapabilities[id]; exists {
			continue
		}
		capability := protocol.ModelCapability{Source: "codex-model-list"}
		if model.SupportedReasoningEfforts != nil {
			capability.ReasoningKnown = true
			capability.ReasoningEfforts = []string{}
			seen := map[string]bool{}
			for _, entry := range *model.SupportedReasoningEfforts {
				if protocol.ValidModelEffort(entry.ReasoningEffort) && entry.ReasoningEffort != "" && !seen[entry.ReasoningEffort] {
					seen[entry.ReasoningEffort] = true
					capability.ReasoningEfforts = append(capability.ReasoningEfforts, entry.ReasoningEffort)
				}
			}
		}
		for _, modality := range model.InputModalities {
			if modality == "text" || modality == "image" {
				capability.InputModalities = append(capability.InputModalities, modality)
			}
		}
		out.Models = append(out.Models, id)
		out.ModelCapabilities[id] = capability
	}
}
