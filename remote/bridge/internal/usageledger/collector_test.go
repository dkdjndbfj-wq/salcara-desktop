package usageledger

import "testing"

func expectCount(t *testing.T, value *int64, want int64) {
	t.Helper()
	if value == nil || *value != want {
		t.Fatalf("token value %v want %d", value, want)
	}
}

func TestCollectorUsesActualResponsesChatAndMessagesUsage(t *testing.T) {
	for _, fixture := range []struct {
		name, raw     string
		input, output int64
	}{
		{"responses", `{"object":"response","status":"completed","usage":{"input_tokens":11,"output_tokens":7,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}`, 11, 7},
		{"chat", `{"object":"chat.completion","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":0,"total_tokens":5}}`, 5, 0},
		{"messages", `{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":4,"cache_read_input_tokens":3}}`, 8, 4},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			var c Collector
			c.ObserveJSON([]byte(fixture.raw))
			got := c.Tokens()
			expectCount(t, got.Input, fixture.input)
			expectCount(t, got.Output, fixture.output)
			if c.State() != "completed" || !c.Final() {
				t.Fatal("final provider result not recognized")
			}
		})
	}
}

func TestStreamUsageSnapshotsMergeWithoutAddingDuplicateEvents(t *testing.T) {
	var c Collector
	c.ObserveEvent([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":0}}}`))
	delta := []byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":8}}`)
	for i := 0; i < 1000; i++ {
		c.ObserveEvent(delta)
	}
	c.ObserveEvent([]byte(`{"type":"message_stop"}`))
	tokens := c.Tokens()
	expectCount(t, tokens.Input, 12)
	expectCount(t, tokens.Output, 8)
	if tokens.Total != nil || !c.Final() {
		t.Fatal("unknown total was synthesized or stream final lost")
	}
	var response Collector
	for i := 0; i < 3; i++ {
		response.ObserveEvent([]byte(`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":2,"output_tokens":1}}}`))
	}
	expectCount(t, response.Tokens().Input, 2)
	expectCount(t, response.Tokens().Output, 1)
}

func TestUnknownAndAmbiguousUsageIsNeverZero(t *testing.T) {
	for _, raw := range []string{`{"status":"completed"}`, `{"usage":{"input_tokens":null,"output_tokens":-1}}`, `{"usage":{"input_tokens":1.2,"output_tokens":"3"}}`, `{"usage":{"input_tokens":2,"input_tokens":4}}`, `{"usage":{"input_tokens":2,"Input_Tokens":4}}`, `{"usage":{"input_tokens":1000000000001}}`} {
		var c Collector
		c.ObserveJSON([]byte(raw))
		tokens := c.Tokens()
		if tokens.Input != nil || tokens.Output != nil || tokens.Total != nil {
			t.Fatalf("unverified usage accepted: %s", raw)
		}
	}
	var c Collector
	c.ObserveJSON([]byte(`{"usage":{"input_tokens":0}}`))
	expectCount(t, c.Tokens().Input, 0)
}

func TestIncompleteAndFailedEventsKeepOnlyObservedUsage(t *testing.T) {
	var c Collector
	c.ObserveEvent([]byte(`{"type":"response.incomplete","response":{"status":"incomplete","usage":{"input_tokens":9,"output_tokens":2}}}`))
	expectCount(t, c.Tokens().Output, 2)
	if c.State() != "incomplete" || !c.Final() {
		t.Fatal("incomplete response lost")
	}
	c.ObserveEvent([]byte(`{"type":"error","error":{"message":"private provider text"}}`))
	if c.State() != "failed" || c.Final() {
		t.Fatal("failed stream presented as final completion")
	}
	var chat Collector
	chat.ObserveSSE([]byte("data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"finish_reason\":\"stop\"}]}\n\ndata: {\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"))
	expectCount(t, chat.Tokens().Input, 4)
	if !chat.Final() {
		t.Fatal("DONE terminator not observed")
	}
}

func TestMultipleChoicesDoNotPretendTheFirstFinishedChoiceCompletedAll(t *testing.T) {
	for _, initial := range []string{
		`{"object":"chat.completion.chunk","choices":[{"index":0,"finish_reason":"stop"},{"index":1,"finish_reason":null}]}`,
		`{"object":"chat.completion.chunk","choices":[{"index":1,"finish_reason":null}]}`,
	} {
		var c Collector
		c.ObserveEvent([]byte(initial))
		c.ObserveEvent([]byte(`{"object":"chat.completion.chunk","choices":[{"index":0,"finish_reason":"stop"}]}`))
		if c.State() != "unknown" || c.Final() {
			t.Fatal("one choice incorrectly completed a multi-choice stream")
		}
		c.ObserveEvent([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":5}}`))
		c.ObserveEvent([]byte("[DONE]"))
		if c.State() != "unknown" {
			t.Fatal("terminator asserted an unsupported choice result")
		}
		expectCount(t, c.Tokens().Output, 5)
	}
}

func TestCollectorTerminalFailureCannotBeRepairedByLaterSuccess(t *testing.T) {
	for _, failure := range []string{
		`{"type":"error","error":{"message":"private"},"usage":{"input_tokens":5}}`,
		`{"type":"response.failed","response":{"status":"failed","usage":{"input_tokens":5}}}`,
		`{"object":"response","status":"failed","usage":{"input_tokens":5}}`,
		`{"type":"response.completed","response":{"status":"failed","usage":{"input_tokens":5}}}`,
	} {
		for _, success := range []string{
			`{"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":3}}}`,
			`{"type":"message_stop"}`,
			`{"object":"chat.completion","choices":[{"finish_reason":"stop"}],"usage":{"output_tokens":3}}`,
			`[DONE]`,
		} {
			var c Collector
			c.ObserveEvent([]byte(failure))
			c.ObserveEvent([]byte(success))
			if c.State() != "failed" || c.Final() {
				t.Fatalf("failure upgraded by success: %s -> %s", failure, success)
			}
			expectCount(t, c.Tokens().Input, 5)
		}
	}
}

func TestCollectorIncompleteTerminalKeepsItsOutcomeAndActualFinalUsage(t *testing.T) {
	for _, incomplete := range []string{
		`{"type":"response.incomplete","response":{"status":"incomplete","usage":{"input_tokens":5,"output_tokens":2}}}`,
		`{"object":"response","status":"incomplete","usage":{"input_tokens":5,"output_tokens":2}}`,
		`{"type":"message","stop_reason":"max_tokens","usage":{"input_tokens":5,"output_tokens":2}}`,
	} {
		var c Collector
		c.ObserveJSON([]byte(incomplete))
		c.ObserveJSON([]byte(`{"type":"response.completed","response":{"status":"completed","usage":{"output_tokens":3}}}`))
		c.ObserveEvent([]byte(`[DONE]`))
		if c.State() != "incomplete" || !c.Final() {
			t.Fatal("incomplete terminal upgraded to successful completion or lost actual terminal usage")
		}
		expectCount(t, c.Tokens().Input, 5)
		expectCount(t, c.Tokens().Output, 3)
		if c.Tokens().Total != nil {
			t.Fatal("unknown total inferred")
		}
		c.ObserveJSON([]byte(`{"type":"error","error":{"message":"private"}}`))
		if c.State() != "failed" || c.Final() {
			t.Fatal("explicit error did not invalidate final usage claim")
		}
	}
}
