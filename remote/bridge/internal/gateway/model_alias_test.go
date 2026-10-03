package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestReplaceSSEModelSupportsMultilineMetadataWithoutChangingToolPayload(t *testing.T) {
	from, to := "deepseek-chat", "claude-salcara-v1-1234567890"
	for _, newline := range []string{"\n", "\r\n"} {
		input := strings.Join([]string{
			"event: message_start", "id: fixture-1", ": keep comment",
			`data: {"type":"message_start",`,
			`data: "message":{"model":"deepseek-chat","content":[]}}`, "",
			"event: content_block_delta",
			`data: {"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"model\":\"deepseek-chat\"}"}}`, "", "",
		}, newline)
		result := replaceSSEModel([]byte(input), from, to)
		if !bytes.Contains(result, []byte(`"model":"`+to+`"`)) {
			t.Fatal("multiline model metadata did not retain native compatibility route")
		}
		if !bytes.Contains(result, []byte(`"partial_json":"{\"model\":\"deepseek-chat\"}"`)) {
			t.Fatal("arbitrary tool input was rewritten")
		}
		for _, preserved := range []string{"event: message_start", "id: fixture-1", ": keep comment", "event: content_block_delta"} {
			if !bytes.Contains(result, []byte(preserved)) {
				t.Fatal("SSE frame metadata was lost")
			}
		}
		for _, line := range strings.Split(string(result), "\n") {
			if strings.HasPrefix(line, "data: ") && !json.Valid([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data: ")))) {
				t.Fatal("rewritten data frame is not complete JSON")
			}
		}
	}
}

func TestReplaceSSEModelLeavesUnrelatedDataAndTerminalFramesExact(t *testing.T) {
	input := []byte("event: content_block_delta\ndata: {\"delta\":{\"text\":\"deepseek-chat\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\ndata: [DONE]\n\n")
	if result := replaceSSEModel(input, "deepseek-chat", "claude-salcara-v1-123"); !bytes.Equal(result, input) {
		t.Fatal("non-model SSE content was changed")
	}
}
