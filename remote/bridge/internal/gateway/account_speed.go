package gateway

import (
	"encoding/json"

	"salcara/bridge/internal/config"
)

// Apply after translation so adapters cannot discard the requested tier. The
// original prompt, model, reasoning, tool definitions and unknown fields are
// retained as raw JSON; a provider rejection is returned without paid retries.
func applyAccountSpeed(body []byte, a config.LocalAccount) ([]byte, error) {
	tier := config.AccountServiceTier(a.SpeedMode)
	protocol := a.Protocol
	if protocol == "" {
		protocol = config.NativeProtocol(a.Kind)
	}
	if tier == "" || protocol != "responses" && protocol != "chat" {
		return body, nil
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	if request == nil {
		return body, nil
	}
	request["service_tier"], _ = json.Marshal(tier)
	return json.Marshal(request)
}
