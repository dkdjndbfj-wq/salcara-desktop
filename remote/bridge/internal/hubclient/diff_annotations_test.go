package hubclient

import (
	"context"
	"strings"
	"testing"
)

func TestDiffAnnotationsRequireIndependentCapabilityAndSurviveDispatch(t *testing.T) {
	for _, typ := range []string{"session.start", "session.send", "session.continue"} {
		for _, support := range []bool{false, true} {
			c, a, cwd := turnFeatureFixture(t)
			a.descriptor.Capabilities.ContextReferences = true
			a.descriptor.Capabilities.ContextDiffAnnotations = support
			ref := map[string]any{"kind": "diff", "sessionKey": "hermes:old", "label": "a.ts", "path": "a.ts", "side": "old", "startLine": 4, "endLine": 4, "text": "deleted", "sourceRevision": strings.Repeat("a", 64), "annotation": "review comment", "capturedAt": 1}
			cmd := map[string]any{"type": typ, "tool": "hermes", "sessionKey": "hermes:old", "cwd": cwd, "text": "review", "prompt": "review", "controlSurface": "cli", "expectedTurnId": "old", "operationId": "11111111-2222-4333-8444-555555555555", "contextReferences": []any{ref}}
			_, err := c.Dispatch(context.Background(), cmd)
			if !support {
				if err == nil || len(a.options)+len(a.started)+len(a.sent)+len(a.opened) > 0 {
					t.Fatal("legacy dispatch", typ, err)
				}
				continue
			}
			if err != nil || len(a.options) != 1 || len(a.options[0].ContextReferences) != 1 {
				t.Fatal(typ, err)
			}
			r := a.options[0].ContextReferences[0]
			if r.Kind != "diff" || r.Side != "old" || r.SourceRevision != strings.Repeat("a", 64) || r.Annotation == nil || *r.Annotation != "review comment" {
				t.Fatal("metadata lost", r)
			}
		}
	}
}
