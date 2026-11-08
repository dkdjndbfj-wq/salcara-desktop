package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ValidResponsesEnvelope checks the shared HTTP/WS envelope for ambiguous JSON
// interpretations without dropping or rewriting unknown request parameters.
func ValidResponsesEnvelope(raw []byte) bool {
	_, ok := webSocketEnvelope(raw)
	return ok
}

// Decode the envelope only after rejecting duplicate JSON keys everywhere and
// case-confusable protocol fields. An HTTP body or WS frame must not authorize
// one interpretation of model/history while forwarding another to an SDK.
func webSocketEnvelope(raw []byte) (map[string]any, bool) {
	if !chatUniqueJSON(raw) {
		return nil, false
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var in map[string]any
	if d.Decode(&in) != nil || in == nil {
		return nil, false
	}
	for key := range in {
		for _, canonical := range []string{"type", "model", "input", "previous_response_id", "generate", "stream", "stream_id", "background", "conversation", "instructions", "tools", "tool_choice", "parallel_tool_calls", "max_output_tokens", "reasoning", "include", "store", "metadata", "service_tier"} {
			if key != canonical && strings.EqualFold(key, canonical) {
				return nil, false
			}
		}
	}
	return in, true
}
