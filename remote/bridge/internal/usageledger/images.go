package usageledger

import (
	"bytes"
	"encoding/json"
)

// Image outcomes use their own terminal envelope. An HTTP 200, a partial image,
// or an empty data array does not prove completion. Only usage is retained.
func (c *Collector) ObserveImageJSON(raw []byte) {
	if len(raw) == 0 || len(raw) > 128<<20 || !uniqueJSON(raw) {
		return
	}
	v := object(raw)
	if v == nil {
		return
	}
	c.metadata(v)
	// Merge genuine usage without the generic text-protocol terminal rules.
	// A response.completed/status:completed marker alone says nothing about
	// an image result, and large image bodies can exceed the text body bound.
	c.usage(v["usage"])
	response := object(v["response"])
	if response != nil {
		c.metadata(response)
		c.usage(response["usage"])
	}
	kind, state := textField(v, "type"), textField(v, "status")
	if response != nil {
		state = textField(response, "status")
	}
	rawError, hasError := v["error"]
	if kind == "error" || kind == "response.failed" || kind == "image_generation.failed" || kind == "image_edit.failed" || state == "failed" || hasError && !bytes.Equal(bytes.TrimSpace(rawError), []byte("null")) {
		c.state, c.terminal = "failed", false
		return
	}
	if c.state == "failed" || c.state == "incomplete" {
		return
	}
	if state == "incomplete" || kind == "response.incomplete" || kind == "image_generation.incomplete" || kind == "image_edit.incomplete" {
		c.state, c.terminal = "incomplete", false
		return
	}
	if kind == "image_generation.completed" || kind == "image_edit.completed" {
		if imageResult(v) {
			c.state, c.terminal = "completed", true
		}
		return
	}
	if kind != "" {
		return // Partial images and unrelated protocol events are not results.
	}
	var data []map[string]json.RawMessage
	if json.Unmarshal(v["data"], &data) != nil || len(data) == 0 {
		return
	}
	for _, result := range data {
		if !imageResult(result) {
			return
		}
	}
	c.state, c.terminal = "completed", true
}

func imageResult(v map[string]json.RawMessage) bool {
	return textField(v, "b64_json") != "" || textField(v, "url") != ""
}
