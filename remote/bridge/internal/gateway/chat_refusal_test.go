package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

const codexChatRefusalTerminal = `{"type":"response.completed","response":{"id":"resp_refusal_fixture","created_at":1700000000,"model":"gpt-fixture","status":"completed","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},"output":[{"id":"msg_refusal_fixture","type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I can’t help with that request."}]}]}}`

func TestRepairCodexChatNonStreamPreservesRefusal(t *testing.T) {
	var translatorState any
	translated := translator.TranslateNonStream(context.Background(), translator.FormatCodex, translator.FormatOpenAI, "gpt-fixture", nil, nil, []byte(codexChatRefusalTerminal), &translatorState)
	if bytes.Contains(translated, []byte(`"refusal"`)) || !bytes.Contains(translated, []byte(`"content":null`)) {
		t.Fatal("fixture no longer reproduces the pinned SDK refusal loss")
	}
	repaired := repairCodexChatNonStream([]byte(codexChatRefusalTerminal), translated)
	var response struct {
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
				Refusal string  `json:"refusal"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
			Total      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(repaired, &response) != nil || len(response.Choices) != 1 {
		t.Fatalf("invalid repaired Chat response: %s", repaired)
	}
	choice := response.Choices[0]
	if choice.Message.Content != nil || choice.Message.Refusal != "I can’t help with that request." || choice.FinishReason != "stop" {
		t.Fatalf("refusal was not represented exactly: %+v", choice)
	}
	if response.Usage.Prompt != 3 || response.Usage.Completion != 2 || response.Usage.Total != 5 {
		t.Fatal("repair changed translated usage")
	}
}

func TestRepairCodexChatNonStreamLeavesOrdinaryTranslationExactAndFailsClosed(t *testing.T) {
	ordinary := []byte(`{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}}`)
	translated := []byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	if got := repairCodexChatNonStream(ordinary, translated); !bytes.Equal(got, translated) {
		t.Fatal("ordinary Chat translation was rewritten")
	}
	for _, invalid := range [][]byte{nil, []byte(`{}`), []byte(`{"choices":[]}`), []byte(`{"choices":[{"message":null}]}`)} {
		if got := repairCodexChatNonStream([]byte(codexChatRefusalTerminal), invalid); got != nil {
			t.Fatalf("refusal repair accepted an unusable translated envelope: %s", got)
		}
	}
	malformed := []byte(`{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"refusal","refusal":7}]}]}}`)
	if got := repairCodexChatNonStream(malformed, translated); got != nil {
		t.Fatal("malformed refusal was silently represented as success")
	}
}

func TestTranslateCodexChatRefusalStreamingDeltaAndTerminalDoNotDuplicate(t *testing.T) {
	state := &chatRefusalStreamState{}
	events := []struct {
		raw       string
		wantDelta string
	}{
		{`{"type":"response.created","response":{"id":"resp_stream_fixture","created_at":1700000001,"model":"gpt-fixture"}}`, ""},
		{`{"type":"response.refusal.delta","item_id":"msg_stream_fixture","output_index":0,"content_index":0,"delta":"I can’t "}`, "I can’t "},
		{`{"type":"response.refusal.delta","item_id":"msg_stream_fixture","output_index":0,"content_index":0,"delta":"help."}`, "help."},
		{`{"type":"response.refusal.done","item_id":"msg_stream_fixture","output_index":0,"content_index":0,"refusal":"I can’t help."}`, ""},
		{`{"type":"response.completed","response":{"id":"resp_stream_fixture","created_at":1700000001,"model":"gpt-fixture","output":[{"id":"msg_stream_fixture","type":"message","content":[{"type":"refusal","refusal":"I can’t help."}]}]}}`, ""},
	}
	for _, event := range events {
		chunks, ok := translateCodexChatRefusal([]byte(event.raw), "gpt-fallback", state)
		if !ok {
			t.Fatalf("valid refusal event rejected: %s", event.raw)
		}
		if event.wantDelta == "" {
			if len(chunks) != 0 {
				t.Fatalf("event duplicated refusal content: %s", chunks[0])
			}
			continue
		}
		if len(chunks) != 1 {
			t.Fatalf("refusal delta produced %d chunks", len(chunks))
		}
		assertChatRefusalChunk(t, chunks[0], event.wantDelta, "resp_stream_fixture", "gpt-fixture", 1700000001)
	}
}

func TestTranslateCodexChatRefusalFallsBackToDoneOrTerminalSnapshot(t *testing.T) {
	for name, finalEvent := range map[string]string{
		"done":     `{"type":"response.refusal.done","item_id":"msg_fixture","output_index":0,"content_index":0,"refusal":"complete refusal"}`,
		"terminal": `{"type":"response.completed","response":{"id":"resp_fixture","created_at":1700000002,"model":"gpt-fixture","output":[{"type":"message","content":[{"type":"refusal","refusal":"complete refusal"}]}]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			state := &chatRefusalStreamState{}
			created := []byte(`{"type":"response.created","response":{"id":"resp_fixture","created_at":1700000002,"model":"gpt-fixture"}}`)
			if chunks, ok := translateCodexChatRefusal(created, "gpt-fallback", state); !ok || len(chunks) != 0 {
				t.Fatal("response.created metadata was not accepted")
			}
			chunks, ok := translateCodexChatRefusal([]byte(finalEvent), "gpt-fallback", state)
			if !ok || len(chunks) != 1 {
				t.Fatalf("final refusal fallback failed: %v %q", ok, chunks)
			}
			assertChatRefusalChunk(t, chunks[0], "complete refusal", "resp_fixture", "gpt-fixture", 1700000002)
		})
	}
}

func TestTranslateCodexChatRefusalRejectsInconsistentOrUnboundedState(t *testing.T) {
	state := &chatRefusalStreamState{}
	created := []byte(`{"type":"response.created","response":{"id":"resp_fixture","created_at":1,"model":"gpt-fixture"}}`)
	if _, ok := translateCodexChatRefusal(created, "gpt-fallback", state); !ok {
		t.Fatal("response.created metadata was rejected")
	}
	if _, ok := translateCodexChatRefusal([]byte(`{"type":"response.refusal.delta","item_id":"msg","output_index":0,"content_index":0,"delta":"first"}`), "gpt-fallback", state); !ok {
		t.Fatal("initial refusal delta was rejected")
	}
	if _, ok := translateCodexChatRefusal([]byte(`{"type":"response.refusal.done","item_id":"msg","output_index":0,"content_index":0,"refusal":"different"}`), "gpt-fallback", state); ok {
		t.Fatal("inconsistent final refusal silently completed")
	}
	state = &chatRefusalStreamState{}
	_, _ = translateCodexChatRefusal(created, "gpt-fallback", state)
	oversized, _ := json.Marshal(map[string]any{"type": "response.refusal.delta", "item_id": "msg", "output_index": 0, "content_index": 0, "delta": bytes.Repeat([]byte{'x'}, maxChatRefusalBytes+1)})
	if _, ok := translateCodexChatRefusal(oversized, "gpt-fallback", state); ok {
		t.Fatal("unbounded refusal stream state was accepted")
	}
}

func assertChatRefusalChunk(t *testing.T, raw []byte, want, id, model string, created int64) {
	t.Helper()
	var chunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Created int64  `json:"created"`
		Choices []struct {
			Delta struct {
				Role    string `json:"role"`
				Refusal string `json:"refusal"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &chunk) != nil || len(chunk.Choices) != 1 {
		t.Fatalf("invalid Chat refusal chunk: %s", raw)
	}
	if chunk.ID != id || chunk.Model != model || chunk.Created != created || chunk.Choices[0].Delta.Role != "assistant" || chunk.Choices[0].Delta.Refusal != want || chunk.Choices[0].FinishReason != nil {
		t.Fatalf("unexpected Chat refusal chunk: %+v", chunk)
	}
}
