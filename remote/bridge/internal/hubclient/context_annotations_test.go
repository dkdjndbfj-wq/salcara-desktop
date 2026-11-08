package hubclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func annotatedReferenceFixture() map[string]any {
	r := referenceFixture()
	r["annotation"] = "请解释\n这个选区"
	r["textBasis"] = "rendered"
	r["blockIndex"] = 0
	r["sourceRevision"] = strings.Repeat("a", 64)
	return r
}

func TestContextAnnotationsRequireIndependentCapabilityOnAllNewTurnRoutes(t *testing.T) {
	for _, typ := range []string{"session.start", "session.send", "session.continue"} {
		for _, supported := range []bool{false, true} {
			c, a, cwd := turnFeatureFixture(t)
			a.descriptor.Capabilities.Features = false
			a.descriptor.Capabilities.ContextReferences = true
			a.descriptor.Capabilities.ContextAnnotations = supported
			cmd := map[string]any{"type": typ, "tool": "hermes", "sessionKey": "hermes:old", "cwd": cwd, "text": "", "prompt": "", "controlSurface": "cli", "expectedTurnId": "old", "operationId": "11111111-2222-4333-8444-555555555555", "contextReferences": []any{annotatedReferenceFixture()}}
			if typ == "session.continue" {
				// Continue keeps its explicit text/CAS contract. The main composer
				// uses session.send for a selection without an additional message.
				cmd["text"] = "继续处理这条批注"
			}
			_, err := c.Dispatch(context.Background(), cmd)
			if !supported {
				if err == nil || len(a.options)+len(a.started)+len(a.sent)+len(a.opened) > 0 {
					t.Fatalf("%s sent annotations to a legacy reference-only adapter: %v", typ, err)
				}
				continue
			}
			if err != nil || len(a.options) != 1 || len(a.options[0].ContextReferences) != 1 {
				t.Fatalf("%s annotation route failed: %v", typ, err)
			}
			r := a.options[0].ContextReferences[0]
			if r.TextBasis != "rendered" || r.BlockIndex == nil || *r.BlockIndex != 0 || r.Annotation == nil || *r.Annotation != "请解释\n这个选区" || r.SourceRevision != strings.Repeat("a", 64) {
				t.Fatalf("%s annotation/coordinates silently discarded: %+v", typ, r)
			}
		}
	}
}

func TestContextAnnotationsCapabilityCannotOutliveReferenceTransport(t *testing.T) {
	_, a, _ := turnFeatureFixture(t)
	caps := a.descriptor.Capabilities
	caps.ContextAnnotations, caps.ContextReferences = true, false
	if publishedRemoteCapabilities(a, caps).ContextAnnotations {
		t.Fatal("annotation capability published without reference capability")
	}
	caps.ContextReferences, caps.Send = true, false
	if publishedRemoteCapabilities(a, caps).ContextAnnotations {
		t.Fatal("annotation capability published on read-only adapter")
	}
	caps.Send = true
	if !publishedRemoteCapabilities(a, caps).ContextAnnotations {
		t.Fatal("supported annotation adapter suppressed")
	}
}

func TestContextAnnotationEventBudgetIncludesCommentAndLocationMetadata(t *testing.T) {
	r := annotatedReferenceFixture()
	r["text"], r["endOffset"] = strings.Repeat("x", 24<<10), 24<<10
	r["annotation"] = strings.Repeat("\t", 8<<10)
	raw, _ := json.Marshal([]any{r, r})
	var refs []protocol.ContextReference
	if err := json.Unmarshal(raw, &refs); err != nil {
		t.Fatal(err)
	}
	e := protocol.Event{Type: "message", Role: "user", Text: strings.Repeat("x", 64<<10), ContextReferences: refs}
	if eventWeight(e) > eventWeightLimit(e) {
		t.Fatal("maximum valid annotation event lost to budget")
	}
	without := e
	without.ContextReferences = protocol.CloneContextReferences(refs)
	for i := range without.ContextReferences {
		without.ContextReferences[i].Annotation = nil
		without.ContextReferences[i].TextBasis = ""
		without.ContextReferences[i].BlockIndex = nil
		without.ContextReferences[i].SourceRevision = ""
	}
	if eventWeight(e)-eventWeight(without) < 6*(16<<10+2*64) {
		t.Fatal("comment/revision bytes omitted from queue weight")
	}
}
