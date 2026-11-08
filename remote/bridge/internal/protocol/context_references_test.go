package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestContextReferenceWireRejectsNullAndForeignOptionalFields(t *testing.T) {
	valid := `{"kind":"message","sessionKey":"codex:s","label":"source","text":"x","capturedAt":1,"messageId":"m","startOffset":0,"endOffset":1}`
	for _, suffix := range []string{`,"path":null}`, `,"path":""}`, `,"startLine":null}`, `,"unexpected":true}`} {
		var ref ContextReference
		if json.Unmarshal([]byte(strings.TrimSuffix(valid, "}")+suffix), &ref) == nil {
			t.Fatal("ambiguous wire shape accepted", suffix)
		}
	}
	var ref ContextReference
	if json.Unmarshal([]byte(valid), &ref) != nil {
		t.Fatal("valid wire reference rejected")
	}
}

func refNumber(n int64) *int64 { return &n }
func sampleMessageReference() ContextReference {
	return ContextReference{Kind: "message", SessionKey: "codex:source", Label: "消息", Text: "A😀中", MessageID: "exact-id", StartOffset: refNumber(2), EndOffset: refNumber(6), CapturedAt: 1}
}

func TestContextReferenceValidationPreservesUTF16AndExactSnapshots(t *testing.T) {
	r := sampleMessageReference()
	if err := ValidateContextReferences([]ContextReference{r}); err != nil {
		t.Fatal(err)
	}
	copy := CloneContextReferences([]ContextReference{r})
	*copy[0].StartOffset = 0
	if *r.StartOffset != 2 {
		t.Fatal("snapshot retained mutable offset pointer")
	}
	f := ContextReference{Kind: "file", SessionKey: "claude:source", Label: "src/a.ts", Text: "const a = 1\n// exact", Path: "src/a.ts", StartLine: refNumber(7), EndLine: refNumber(8), CapturedAt: 1}
	if err := ValidateContextReferences([]ContextReference{f}); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*ContextReference){
		func(r *ContextReference) { r.Path = "../secret" }, func(r *ContextReference) { r.Path = "C:/secret" }, func(r *ContextReference) { r.Path = "/secret" },
		func(r *ContextReference) { r.Path = "src\\a.ts" }, func(r *ContextReference) { r.Path = "src/ a.ts" }, func(r *ContextReference) { r.Path = "src/a?.ts" },
		func(r *ContextReference) { r.EndLine = refNumber(9) }, func(r *ContextReference) { r.StartOffset = refNumber(0) },
	} {
		bad := f
		mutate(&bad)
		if ValidateContextReferences([]ContextReference{bad}) == nil {
			t.Fatalf("invalid file reference accepted: %+v", bad)
		}
	}
	for _, mutate := range []func(*ContextReference){
		func(r *ContextReference) { r.EndOffset = refNumber(5) }, func(r *ContextReference) { r.StartOffset = nil }, func(r *ContextReference) { r.Path = "a" },
		func(r *ContextReference) { r.SessionKey = "codex:one|two" }, func(r *ContextReference) { r.Label = "spoof\u202e" }, func(r *ContextReference) { r.CapturedAt = 0 },
		func(r *ContextReference) { r.Kind = "task" }, func(r *ContextReference) { r.Text = "\x00" }, func(r *ContextReference) { r.MessageID = strings.Repeat("x", 257) },
	} {
		bad := r
		mutate(&bad)
		if ValidateContextReferences([]ContextReference{bad}) == nil {
			t.Fatalf("invalid message reference accepted: %+v", bad)
		}
	}
}

func TestContextReferenceBudgetsAreUTF8Bytes(t *testing.T) {
	r := sampleMessageReference()
	r.Text = strings.Repeat("x", 32<<10)
	r.StartOffset = refNumber(0)
	r.EndOffset = refNumber(32 << 10)
	if err := ValidateContextReferences([]ContextReference{r, r}); err != nil {
		t.Fatal(err)
	}
	if ValidateContextReferences([]ContextReference{r, r, r}) == nil {
		t.Fatal("aggregate budget not enforced")
	}
	r.Text += "中"
	r.EndOffset = refNumber(32<<10 + 1)
	if ValidateContextReferences([]ContextReference{r}) == nil {
		t.Fatal("single snapshot byte budget not enforced")
	}
	r = sampleMessageReference()
	refs := make([]ContextReference, 9)
	for i := range refs {
		refs[i] = r
	}
	if ValidateContextReferences(refs) == nil {
		t.Fatal("count budget not enforced")
	}
}

func TestProjectFileReferenceHasRealProjectSourceWithoutInventedSession(t *testing.T) {
	raw := `{"kind":"file","projectPath":"C:/workspace/project","label":"main.ts","text":"source","path":"main.ts","startLine":1,"endLine":1,"capturedAt":1}`
	var ref ContextReference
	if err := json.Unmarshal([]byte(raw), &ref); err != nil || ref.SessionKey != "" || ref.ProjectPath != "C:/workspace/project" {
		t.Fatal("new-task source lost", err)
	}
	encoded, _ := json.Marshal(ref)
	if strings.Contains(string(encoded), "sessionKey") {
		t.Fatal("invented session serialized")
	}
	for _, extra := range []string{`,"sessionKey":"codex:fake"}`, `,"sessionKey":null}`} {
		if json.Unmarshal([]byte(strings.TrimSuffix(raw, "}")+extra), &ref) == nil {
			t.Fatal("ambiguous file source accepted")
		}
	}
	message := sampleMessageReference()
	message.ProjectPath = "C:/project"
	if ValidateContextReferences([]ContextReference{message}) == nil {
		t.Fatal("message accepted project source")
	}
}
