package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
)

func TestProtectedOfficialSocketRebuildsCompleteConnectionHistory(t *testing.T) {
	a := officialAccountFixture()
	a.ModelDenylist = []string{"gpt-denied"}
	h := New(func() string { return localTestKey }, func(string) (config.LocalAccount, bool) { return a, true })
	var calls atomic.Int32
	h.Official = func(_ context.Context, selected config.LocalAccount, raw []byte, stream, compact bool, current func() bool) (*http.Response, error) {
		count := calls.Add(1)
		if selected.CredentialRef != a.CredentialRef || !stream || compact || !current() {
			t.Error("socket did not use pinned protected executor")
		}
		var request map[string]any
		if json.Unmarshal(raw, &request) != nil {
			t.Error("invalid rebuilt body")
		}
		if request["previous_response_id"] != nil || request["type"] != nil {
			t.Error("SDK received local continuation fields")
		}
		if count == 2 && (!strings.Contains(string(raw), "first question") || !strings.Contains(string(raw), "encrypted-fixture") || !strings.Contains(string(raw), "tool-output-fixture") || !strings.Contains(string(raw), "follow-up") || request["model"] != "gpt-native-hint") {
			t.Error("official socket lost complete original context")
		}
		payload := `{"type":"response.completed","response":{"id":"official-socket-fixture","object":"response","status":"completed","model":"gpt-native-hint","output":[{"type":"reasoning","encrypted_content":"encrypted-fixture"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"tool-output-fixture"}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: " + payload + "\n\n"))}, nil
	}
	c := wsFixture(t, h)
	if err := c.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-denied", "generate": false, "input": "rejected"}); err != nil {
		t.Fatal(err)
	}
	if event := wsUntil(t, c, "error"); event["error"].(map[string]any)["code"] != "invalid_model" || calls.Load() != 0 {
		t.Fatal("warmup bypassed model policy")
	}
	if err := c.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-native-hint", "generate": false, "input": "first question"}); err != nil {
		t.Fatal(err)
	}
	warm := wsUntil(t, c, "response.completed")["response"].(map[string]any)
	if calls.Load() != 0 {
		t.Fatal("official warmup billed provider")
	}
	if err := c.WriteJSON(map[string]any{"type": "response.create", "previous_response_id": warm["id"], "input": []any{}}); err != nil {
		t.Fatal(err)
	}
	first := wsUntil(t, c, "response.completed")["response"].(map[string]any)
	if err := c.WriteJSON(map[string]any{"type": "response.create", "previous_response_id": first["id"], "input": "follow-up"}); err != nil {
		t.Fatal(err)
	}
	wsUntil(t, c, "response.completed")
	other := wsFixture(t, h)
	if err := other.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-native-hint", "previous_response_id": first["id"], "input": "cross-connection"}); err != nil {
		t.Fatal(err)
	}
	if event := wsUntil(t, other, "error"); event["error"].(map[string]any)["code"] != "previous_response_not_found" || calls.Load() != 2 {
		t.Fatal("official history crossed connections")
	}
}
