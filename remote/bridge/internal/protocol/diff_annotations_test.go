package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func diffReferenceFixture() ContextReference {
	return ContextReference{Kind: "diff", SessionKey: "codex:s", Label: "old.ts", Path: "src/old.ts", Side: "old", StartLine: refNumber(10), EndLine: refNumber(11), Text: "deleted 😀\nline", SourceRevision: strings.Repeat("a", 64), CapturedAt: 1, Annotation: annotationString("keep compatibility")}
}
func TestDiffAnnotationsRoundTripAndStrictCoordinates(t *testing.T) {
	ref := diffReferenceFixture()
	wire, _ := json.Marshal(ref)
	var got ContextReference
	if err := json.Unmarshal(wire, &got); err != nil || !reflect.DeepEqual(ref, got) {
		t.Fatal(err, string(wire))
	}
	if !HasDiffAnnotations([]ContextReference{got}) || HasContextAnnotations([]ContextReference{got}) {
		t.Fatal("diff requires its independent capability")
	}
	var base map[string]any
	_ = json.Unmarshal(wire, &base)
	cases := map[string]any{"side": "current", "sourceRevision": "HEAD", "path": "../key", "startLine": 0, "endLine": 12, "textBasis": "source", "messageId": "m", "projectPath": "/p", "blockIndex": 0, "annotation": strings.Repeat("中", 3000)}
	for key, value := range cases {
		copy := map[string]any{}
		for k, v := range base {
			copy[k] = v
		}
		copy[key] = value
		invalid, _ := json.Marshal(copy)
		if json.Unmarshal(invalid, &got) == nil {
			t.Fatal("accepted", key)
		}
	}
	for _, key := range []string{"side", "sourceRevision", "sessionKey", "path", "startLine", "endLine"} {
		copy := map[string]any{}
		for k, v := range base {
			copy[k] = v
		}
		delete(copy, key)
		invalid, _ := json.Marshal(copy)
		if json.Unmarshal(invalid, &got) == nil {
			t.Fatal("missing", key)
		}
	}
	cloned := CloneContextReferences([]ContextReference{ref})
	*cloned[0].StartLine = 99
	*cloned[0].Annotation = "changed"
	if *ref.StartLine == 99 || *ref.Annotation == "changed" {
		t.Fatal("shared pointers")
	}
}
