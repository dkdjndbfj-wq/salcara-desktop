package usageledger

import "testing"

func TestImageUsageRequiresResultAndPreservesUnknownTokens(t *testing.T) {
	for _, raw := range []string{`{}`, `{"data":[]}`, `{"data":[{}]}`, `{"type":"image_generation.partial_image","b64_json":"fixture"}`} {
		var c Collector
		c.ObserveImageJSON([]byte(raw))
		if c.State() == "completed" || c.Final() || c.Tokens().Total != nil {
			t.Fatal("partial/empty image became final zero usage")
		}
	}
	var c Collector
	c.ObserveImageJSON([]byte(`{"data":[{"b64_json":"fixture"}],"usage":{"total_tokens":12}}`))
	if c.State() != "completed" || !c.Final() || c.Tokens().Total == nil || *c.Tokens().Total != 12 || c.Tokens().Input != nil {
		t.Fatal("genuine image usage lost")
	}
	c.ObserveImageJSON([]byte(`{"type":"error","error":{"message":"fixture"}}`))
	c.ObserveImageJSON([]byte(`{"type":"image_generation.completed","b64_json":"fixture","usage":{"total_tokens":12}}`))
	if c.State() != "failed" || c.Final() {
		t.Fatal("late image completion revived failed usage")
	}
}
