package codexruntime

import (
	"bytes"
	"encoding/json"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
)

type quotaReserveError struct{}

func (quotaReserveError) Error() string             { return "额度状态未知、已耗尽或保留额度不足" }
func (quotaReserveError) LocalAdmissionStatus() int { return 503 }

var ErrQuotaReserve error = quotaReserveError{}

func reserveModels(body []byte) ([]string, bool) {
	if len(body) > 64<<20 || !gateway.ValidResponsesEnvelope(body) {
		return nil, false
	}
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return nil, false
	}
	var model string
	if json.Unmarshal(request["model"], &model) != nil || !config.ValidCatalogModel(model) {
		return nil, false
	}
	models := []string{model}
	seen := map[string]bool{model: true}
	var tools []map[string]json.RawMessage
	if raw, ok := request["tools"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if json.Unmarshal(raw, &tools) != nil {
			return nil, false
		}
	}
	for _, tool := range tools {
		var kind string
		_ = json.Unmarshal(tool["type"], &kind)
		if kind != "image_generation" {
			continue
		}
		model = "gpt-image-2"
		if raw, ok := tool["model"]; ok {
			if json.Unmarshal(raw, &model) != nil || !config.ValidCatalogModel(model) {
				return nil, false
			}
		}
		if !seen[model] {
			models = append(models, model)
			seen[model] = true
		}
	}
	return models, true
}
