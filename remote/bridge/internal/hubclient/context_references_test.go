package hubclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func referenceFixture() map[string]any {
	return map[string]any{"kind": "message", "sessionKey": "hermes:source", "label": "source message", "text": "😀", "messageId": "exact", "startOffset": 0, "endOffset": 2, "capturedAt": 1}
}

func TestContextReferencesSurviveAllNewTurnRoutesWithoutFeatureCapability(t *testing.T) {
	for _, typ := range []string{"session.start", "session.send", "session.continue"} {
		t.Run(typ, func(t *testing.T) {
			c, a, cwd := turnFeatureFixture(t)
			a.descriptor.Capabilities.Features = false
			a.descriptor.Capabilities.ContextReferences = true
			cmd := map[string]any{"type": typ, "tool": "hermes", "sessionKey": "hermes:old", "cwd": cwd, "text": "body", "prompt": "body", "controlSurface": "cli", "expectedTurnId": "old", "operationId": "11111111-2222-4333-8444-555555555555", "contextReferences": []any{referenceFixture()}}
			if _, err := c.Dispatch(context.Background(), cmd); err != nil {
				t.Fatal(err)
			}
			if len(a.options) != 1 || len(a.options[0].ContextReferences) != 1 || a.options[0].ContextReferences[0].Text != "😀" {
				t.Fatal("reference was silently discarded", a.options)
			}
		})
	}
}

func TestReferenceOnlyTurnsRemainStructuredAndOrdinaryEmptyTurnsAreRejected(t *testing.T) {
	for _, typ := range []string{"session.start", "session.send"} {
		for _, referenced := range []bool{false, true} {
			c, a, cwd := turnFeatureFixture(t)
			a.descriptor.Capabilities.ContextReferences = true
			cmd := map[string]any{"type": typ, "tool": "hermes", "sessionKey": "hermes:old", "cwd": cwd, "text": "", "prompt": "", "controlSurface": "cli"}
			if referenced {
				cmd["contextReferences"] = []any{referenceFixture()}
			}
			_, err := c.Dispatch(context.Background(), cmd)
			if referenced && (err != nil || len(a.options) != 1 || len(a.options[0].ContextReferences) != 1) {
				t.Fatalf("%s reference-only turn failed: %v", typ, err)
			}
			if !referenced && (err == nil || len(a.options) > 0) {
				t.Fatalf("%s empty turn executed", typ)
			}
		}
	}
}

func TestContextReferencesFailClosedBeforeSideEffects(t *testing.T) {
	for _, variant := range []string{"capability", "null", "type", "unknown", "cross-tool", "offset", "native", "steer", "oversized-body"} {
		t.Run(variant, func(t *testing.T) {
			c, a, _ := turnFeatureFixture(t)
			a.descriptor.Capabilities.ContextReferences = true
			r := referenceFixture()
			cmd := map[string]any{"type": "session.send", "sessionKey": "hermes:old", "text": "body", "controlSurface": "cli", "contextReferences": []any{r}}
			switch variant {
			case "capability":
				a.descriptor.Capabilities.ContextReferences = false
			case "null":
				cmd["contextReferences"] = nil
			case "type":
				cmd["contextReferences"] = "bad"
			case "unknown":
				r["execute"] = true
			case "cross-tool":
				r["sessionKey"] = "claude:source"
			case "offset":
				r["endOffset"] = 1
			case "native":
				cmd["controlSurface"] = "desktop"
			case "steer":
				cmd["type"] = "session.steer"
			case "oversized-body":
				cmd["text"] = strings.Repeat("x", (64<<10)+1)
			}
			if _, err := c.Dispatch(context.Background(), cmd); err == nil {
				t.Fatal("invalid/unsupported reference admitted")
			}
			if len(a.options)+len(a.started)+len(a.sent)+len(a.opened) > 0 {
				t.Fatal("rejection followed a side effect")
			}
		})
	}
}

func TestContextReferenceQuoteHeavyCommandRefusesBeforeDispatchWithoutTrimming(t *testing.T) {
	c, a, _ := turnFeatureFixture(t)
	a.descriptor.Capabilities.ContextReferences = true
	r := referenceFixture()
	r["text"] = strings.Repeat(`"`, 32<<10)
	r["endOffset"] = 32 << 10
	cmd := map[string]any{"type": "session.send", "sessionKey": "hermes:old", "text": strings.Repeat(`"`, 64<<10), "contextReferences": []any{r, r}}
	if _, err := c.Dispatch(context.Background(), cmd); err == nil || !strings.Contains(err.Error(), "input_too_large") {
		t.Fatal("escaped wire budget was not refused", err)
	}
	if len(a.options)+len(a.sent)+len(a.opened) > 0 {
		t.Fatal("oversized request executed")
	}
	if len(cmd["text"].(string)) != 64<<10 || len(r["text"].(string)) != 32<<10 {
		t.Fatal("input silently truncated")
	}
	// HTML is plain UTF8 in the phone's JSON.stringify output, not six bytes
	// per '<'. Counting must not reject this otherwise small wire command.
	cmd["text"] = strings.Repeat("<", 64<<10)
	r["text"] = strings.Repeat("<", 32<<10)
	if _, err := commandTurnFeatures(cmd); err != nil {
		t.Fatal("Go HTML escaping distorted phone wire budget", err)
	}
}

func TestContextReferenceEventBudgetAccountsForMaximumSnapshots(t *testing.T) {
	var refs []protocol.ContextReference
	r := referenceFixture()
	r["text"] = strings.Repeat("x", 32<<10)
	r["endOffset"] = 32 << 10
	b, _ := json.Marshal([]any{r, r})
	if json.Unmarshal(b, &refs) != nil {
		t.Fatal("fixture")
	}
	e := protocol.Event{Type: "message", Role: "user", Text: strings.Repeat("x", 64<<10), ContextReferences: refs}
	if eventWeight(e) <= maxEventWeight || eventWeight(e) > eventWeightLimit(e) {
		t.Fatal("valid maximum snapshots lost to event budget", eventWeight(e), eventWeightLimit(e))
	}
	without := e
	without.ContextReferences = nil
	if eventWeight(e) <= eventWeight(without)+64<<10 {
		t.Fatal("reference bytes missing from retained budget")
	}
	if eventWeightLimit(without) != maxEventWeight {
		t.Fatal("unrelated event limit changed")
	}
	without.Skills = []protocol.SkillReference{{Name: "review", Path: strings.Repeat("a", 4096)}}
	if eventWeightLimit(without) != maxReferencedEventWeight {
		t.Fatal("skill receipt metadata lost to old event limit")
	}
}
