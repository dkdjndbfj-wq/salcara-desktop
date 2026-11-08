package usageledger

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// Collector merges provider snapshots rather than adding stream events.
// Missing/invalid values stay nil; repeated terminal/usage events count once.
type Collector struct {
	tokens          Tokens
	state           string
	terminal        bool
	multipleChoices bool
	providerCost    *Money
	serviceTier     string
}

func (c *Collector) Tokens() Tokens       { return sanitizeTokens(c.tokens) }
func (c *Collector) ProviderCost() *Money { return cloneMoney(c.providerCost) }
func (c *Collector) ServiceTier() string  { return c.serviceTier }
func (c *Collector) State() string {
	if c.state == "" {
		return "unknown"
	}
	return c.state
}

// Final means the upstream supplied a known terminal usage snapshot; an
// incomplete result may be final usage without representing completed work.
// A failed stream has no trusted final snapshot even if more events follow.
func (c *Collector) Final() bool { return c.terminal }

func object(raw []byte) map[string]json.RawMessage {
	var v map[string]json.RawMessage
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}
func textField(v map[string]json.RawMessage, key string) string {
	var s string
	_ = json.Unmarshal(v[key], &s)
	return s
}
func number(v map[string]json.RawMessage, key string) *int64 {
	raw := bytes.TrimSpace(v[key])
	if len(raw) == 0 || len(raw) > 13 {
		return nil
	}
	n, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil || n < 0 || n > 1_000_000_000_000 {
		return nil
	}
	return &n
}
func merge(dst **int64, v *int64) {
	if v != nil {
		*dst = v
	}
}

func (c *Collector) usage(raw []byte) {
	if len(raw) == 0 || !uniqueJSON(raw) {
		return
	}
	v := object(raw)
	if v == nil {
		return
	}
	c.metadata(v)
	input := number(v, "input_tokens")
	if input == nil {
		input = number(v, "prompt_tokens")
	}
	output := number(v, "output_tokens")
	if output == nil {
		output = number(v, "completion_tokens")
	}
	merge(&c.tokens.Input, input)
	merge(&c.tokens.Output, output)
	merge(&c.tokens.Total, number(v, "total_tokens"))
	merge(&c.tokens.CachedInput, number(v, "cache_read_input_tokens"))
	merge(&c.tokens.CacheCreationInput, number(v, "cache_creation_input_tokens"))
	merge(&c.tokens.CachedInput, number(object(v["input_tokens_details"]), "cached_tokens"))
	// OpenAI's documented cache-write class is part of total input, not an
	// additional token charge: https://developers.openai.com/api/docs/guides/prompt-caching
	merge(&c.tokens.CacheCreationInput, number(object(v["input_tokens_details"]), "cache_write_tokens"))
	merge(&c.tokens.CachedInput, number(object(v["prompt_tokens_details"]), "cached_tokens"))
	merge(&c.tokens.ReasoningOutput, number(object(v["output_tokens_details"]), "reasoning_tokens"))
	merge(&c.tokens.ReasoningOutput, number(object(v["completion_tokens_details"]), "reasoning_tokens"))
}

// ObserveJSON is called only on the original upstream payload, before any
// translator can synthesize usage. A single frame/body is bounded by gateway.
func (c *Collector) ObserveJSON(raw []byte) {
	if len(raw) == 0 || len(raw) > 64<<20 || !uniqueJSON(raw) {
		return
	}
	v := object(raw)
	if v == nil {
		return
	}
	c.metadata(v)
	kind := textField(v, "type")
	c.usage(v["usage"])
	response := object(v["response"])
	if response != nil {
		c.metadata(response)
		c.usage(response["usage"])
	}
	message := object(v["message"])
	if message != nil {
		c.metadata(message)
		c.usage(message["usage"])
	}
	state := textField(v, "status")
	if response != nil {
		state = textField(response, "status")
	}
	rawError, hasError := v["error"]
	if kind == "error" || kind == "response.failed" || state == "failed" || hasError && !bytes.Equal(bytes.TrimSpace(rawError), []byte("null")) {
		c.state = "failed"
		c.terminal = false
		return
	}
	// A success event cannot repair an earlier explicit terminal failure or
	// incomplete result. Continue merging actual usage above without upgrading
	// the request outcome or treating post-failure usage as a final snapshot.
	if c.state == "failed" || c.state == "incomplete" && c.terminal {
		return
	}
	switch kind {
	case "response.incomplete":
		c.state = "incomplete"
		c.terminal = true
		return
	case "response.completed":
		c.state = "completed"
		c.terminal = true
		return
	case "message_stop":
		if c.state != "incomplete" && c.state != "failed" {
			c.state = "completed"
		}
		c.terminal = true
		return
	}
	switch state {
	case "completed":
		c.state = "completed"
		c.terminal = true
	case "incomplete":
		c.state = "incomplete"
		c.terminal = true
	case "cancelled":
		c.state = state
		c.terminal = true
	}
	if textField(v, "object") == "response.compaction" {
		c.state = "completed"
		c.terminal = true
	}
	var choices []map[string]json.RawMessage
	if json.Unmarshal(v["choices"], &choices) == nil {
		if len(choices) > 1 {
			c.multipleChoices = true
		}
		for _, choice := range choices {
			if index := number(choice, "index"); index != nil && *index > 0 {
				c.multipleChoices = true
			}
		}
		// Sparse streaming choice frames do not prove every choice terminated.
		// Keep their termination unknown rather than promoting the first result.
		if c.multipleChoices {
			c.state = "unknown"
			c.terminal = false
			return
		}
		for _, choice := range choices {
			switch textField(choice, "finish_reason") {
			case "stop", "tool_calls", "function_call":
				c.state = "completed"
				if textField(v, "object") != "chat.completion.chunk" {
					c.terminal = true
				}
			case "length", "content_filter":
				c.state = "incomplete"
				if textField(v, "object") != "chat.completion.chunk" {
					c.terminal = true
				}
			}
		}
	}
	stop := textField(v, "stop_reason")
	if delta := object(v["delta"]); delta != nil {
		if next := textField(delta, "stop_reason"); next != "" {
			stop = next
		}
	}
	switch stop {
	case "end_turn", "tool_use", "stop_sequence":
		c.state = "completed"
		if kind == "message" {
			c.terminal = true
		}
	case "max_tokens", "refusal", "model_context_window_exceeded":
		c.state = "incomplete"
		if kind == "message" {
			c.terminal = true
		}
	}
}

func (c *Collector) ObserveEvent(data []byte) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		if c.state == "failed" || c.state == "incomplete" && c.terminal {
			return
		}
		if c.state == "" {
			c.state = "completed"
		}
		c.terminal = true
		return
	}
	c.ObserveJSON(data)
}

func (c *Collector) ObserveSSE(raw []byte) {
	// Non-stream callers already hold a gateway-bounded response. Stream callers
	// use ObserveEvent directly and never aggregate the complete response here.
	var data [][]byte
	dispatch := func() {
		if len(data) > 0 {
			c.ObserveEvent(bytes.Join(data, []byte("\n")))
			data = nil
		}
	}
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 {
			dispatch()
		} else if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" ")))
		}
	}
	dispatch()
}
