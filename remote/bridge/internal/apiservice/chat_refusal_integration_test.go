package apiservice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

const namedRefusalTerminal = `{"type":"response.completed","response":{"id":"resp-refusal","created_at":1,"model":"gpt-fixture","status":"completed","output":[{"id":"msg-refusal","type":"message","role":"assistant","content":[{"type":"refusal","refusal":"synthetic refusal"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`

func TestNamedChatRefusalSurvivesNonStreamAndStreamingSDKTranslation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			s, _ := fixtureService(t)
			s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
				frames := "data: " + namedRefusalTerminal + "\n\n"
				if stream {
					frames = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-refusal\",\"created_at\":1,\"model\":\"gpt-fixture\"}}\n\n" +
						"data: {\"type\":\"response.refusal.delta\",\"item_id\":\"msg-refusal\",\"delta\":\"synthetic \"}\n\n" +
						"data: {\"type\":\"response.refusal.delta\",\"item_id\":\"msg-refusal\",\"delta\":\"refusal\"}\n\n" + frames
				}
				return successfulResponse(io.NopCloser(strings.NewReader(frames)), true), nil
			}
			body := `{"model":"friendly","messages":[{"role":"user","content":"synthetic"}]}`
			if stream {
				body = strings.TrimSuffix(body, "}") + `,"stream":true}`
			}
			w := callChatAPI(s, syntheticClientKey, body)
			if w.Code != 200 {
				t.Fatal("refusal translation failed", w.Code, w.Body)
			}
			if !stream {
				var response struct {
					Choices []struct {
						Message struct {
							Refusal string `json:"refusal"`
						} `json:"message"`
					} `json:"choices"`
				}
				if json.Unmarshal(w.Body.Bytes(), &response) != nil || len(response.Choices) != 1 || response.Choices[0].Message.Refusal != "synthetic refusal" {
					t.Fatal("nonstream Chat silently dropped refusal", w.Body)
				}
				return
			}
			var refusal strings.Builder
			for _, frame := range strings.Split(w.Body.String(), "\n\n") {
				raw := strings.TrimPrefix(frame, "data: ")
				if raw == "" || raw == "[DONE]" {
					continue
				}
				var chunk struct {
					Choices []struct {
						Delta struct {
							Refusal string `json:"refusal"`
						} `json:"delta"`
					} `json:"choices"`
				}
				if json.Unmarshal([]byte(raw), &chunk) != nil {
					t.Fatal("invalid Chat SSE", frame)
				}
				for _, choice := range chunk.Choices {
					refusal.WriteString(choice.Delta.Refusal)
				}
			}
			if refusal.String() != "synthetic refusal" || strings.Count(w.Body.String(), "data: [DONE]") != 1 {
				t.Fatal("stream Chat dropped or duplicated refusal", w.Body)
			}
		})
	}
}
