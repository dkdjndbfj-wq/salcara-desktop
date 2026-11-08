package usageledger

import "testing"

func TestImageTerminalRejectsUnrelatedAndPartialResultsButKeepsActualUsage(t *testing.T) {
	for _, raw := range []string{
		`{"status":"completed","data":[],"usage":{"total_tokens":12}}`,
		`{"type":"response.completed","data":[{"b64_json":"fixture"}],"usage":{"total_tokens":12}}`,
		`{"type":"image_generation.partial_image","data":[{"b64_json":"fixture"}],"usage":{"total_tokens":12}}`,
		`{"type":"image_generation.completed","usage":{"total_tokens":12}}`,
		`{"data":[{"b64_json":"fixture"},{}],"usage":{"total_tokens":12}}`,
	} {
		var c Collector
		c.ObserveImageJSON([]byte(raw))
		if c.State() == "completed" || c.Final() || c.Tokens().Total == nil || *c.Tokens().Total != 12 || c.Tokens().Input != nil {
			t.Fatalf("image endpoint fabricated completion or dropped observed usage: state=%s final=%v tokens=%+v", c.State(), c.Final(), c.Tokens())
		}
	}
}

func TestImageTerminalFailuresCannotBeRepairedByLateImageOutput(t *testing.T) {
	for _, failure := range []string{
		`{"type":"image_generation.failed","usage":{"total_tokens":3}}`,
		`{"type":"image_edit.failed","usage":{"total_tokens":3}}`,
		`{"status":"failed","usage":{"total_tokens":3}}`,
		`{"type":"error","error":{"message":"fixture"},"usage":{"total_tokens":3}}`,
	} {
		var c Collector
		c.ObserveImageJSON([]byte(failure))
		c.ObserveImageJSON([]byte(`{"type":"image_generation.completed","b64_json":"fixture","usage":{"total_tokens":12}}`))
		if c.State() != "failed" || c.Final() || c.Tokens().Total == nil || *c.Tokens().Total != 12 {
			t.Fatal("failure was replaced by a successful image or usage was guessed")
		}
	}
	var incomplete Collector
	incomplete.ObserveImageJSON([]byte(`{"status":"incomplete","usage":{"total_tokens":3}}`))
	incomplete.ObserveImageJSON([]byte(`{"data":[{"url":"https://fixture.invalid/image"}],"usage":{"total_tokens":12}}`))
	if incomplete.State() != "incomplete" || incomplete.Final() {
		t.Fatal("incomplete image endpoint became completed by a later unrelated body")
	}
}

func TestImageTerminalAcceptsGenuineResultOnceWithoutInventingTokens(t *testing.T) {
	for _, raw := range []string{
		`{"data":[{"b64_json":"fixture"}],"usage":{"total_tokens":12}}`,
		`{"type":"image_generation.completed","b64_json":"fixture","usage":{"total_tokens":12}}`,
		`{"type":"image_edit.completed","url":"https://fixture.invalid/image","usage":{"total_tokens":12}}`,
	} {
		var c Collector
		c.ObserveImageJSON([]byte(raw))
		c.ObserveImageJSON([]byte(raw))
		if c.State() != "completed" || !c.Final() || c.Tokens().Total == nil || *c.Tokens().Total != 12 || c.Tokens().Input != nil || c.Tokens().Output != nil {
			t.Fatal("actual image completion was lost or token snapshots were added")
		}
	}
}
