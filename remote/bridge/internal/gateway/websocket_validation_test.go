package gateway

import (
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func TestWebSocketRejectsAmbiguousEnvelopeAndUnsupportedStateWithoutInference(t *testing.T) {
	var calls atomic.Int32
	h := fixtureGateway(t, "codex", "responses", wsCompletedFixture, "text/event-stream", func(*http.Request, map[string]any) { calls.Add(1) })
	c := wsFixture(t, h)
	for _, test := range []struct{ raw, code string }{
		{`{"type":"response.create","model":"denied","model":"chosen-model","input":[]}`, "invalid_event"},
		{`{"type":"response.create","model":"chosen-model","Model":"other","input":[]}`, "invalid_event"},
		{`{"type":"response.create","model":"chosen-model","Previous_Response_ID":"other","input":[]}`, "invalid_event"},
		{`{"type":"response.create","model":"chosen-model","input":[{"role":"user","role":"assistant"}]}`, "invalid_event"},
		{`{"type":"response.create","model":"chosen-model","generate":null,"input":[]}`, "invalid_generate"},
		{`{"type":"response.create","model":"chosen-model","generate":"false","input":[]}`, "invalid_generate"},
		{`{"type":"response.create","model":"chosen-model","stream":false,"input":[]}`, "invalid_stream"},
		{`{"type":"response.create","model":"chosen-model","background":true,"input":[]}`, "unsupported_background"},
		{`{"type":"response.create","model":"chosen-model","conversation":"external-state","input":[]}`, "unsupported_conversation"},
	} {
		if err := c.WriteMessage(websocket.TextMessage, []byte(test.raw)); err != nil {
			t.Fatal(err)
		}
		event := wsUntil(t, c, "error")
		if event["error"].(map[string]any)["code"] != test.code {
			t.Fatalf("incorrect envelope rejection: %s: %v", test.raw, event)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid envelope caused inference")
	}
}

func TestWebSocketModelResolverRetainsClientLiteralAcrossContinuation(t *testing.T) {
	var calls atomic.Int32
	h := fixtureGateway(t, "codex", "responses", wsCompletedFixture, "text/event-stream", func(_ *http.Request, input map[string]any) {
		calls.Add(1)
		if input["model"] != "chosen-model" {
			t.Errorf("upstream model changed: %v", input["model"])
		}
	})
	h.WebSocketModel = func(literal string) (string, bool) { return "chosen-model", literal == "friendly" }
	c := wsFixture(t, h)
	wsSend(t, c, map[string]any{"type": "response.create", "model": "friendly", "generate": false, "input": "first prompt"})
	warm := wsUntil(t, c, "response.completed")["response"].(map[string]any)
	wsSend(t, c, map[string]any{"type": "response.create", "previous_response_id": warm["id"], "input": []any{}})
	first := wsUntil(t, c, "response.completed")["response"].(map[string]any)
	wsSend(t, c, map[string]any{"type": "response.create", "previous_response_id": first["id"], "input": "next prompt"})
	wsUntil(t, c, "response.completed")
	if calls.Load() != 2 {
		t.Fatal("client model literal lost between turns")
	}
}
