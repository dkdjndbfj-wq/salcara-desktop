package protocol

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func annotationString(text string) *string { return &text }

func annotationFixture() ContextReference {
	r := sampleMessageReference()
	r.Annotation = annotationString("请解释这一段\n只问选中的文字。")
	r.TextBasis = "rendered"
	r.BlockIndex = refNumber(2)
	r.SourceRevision = strings.Repeat("a", 64)
	return r
}

func TestContextAnnotationRoundTripAndClonePreserveCoordinatesAndOptionalComment(t *testing.T) {
	for _, basis := range []string{"source", "rendered"} {
		for _, comment := range []*string{nil, annotationString(""), annotationString("question\n😀\tline")} {
			r := annotationFixture()
			r.TextBasis, r.Annotation = basis, comment
			if basis == "source" {
				r.BlockIndex = nil
			}
			wire, err := json.Marshal(r)
			var decoded ContextReference
			if err != nil || json.Unmarshal(wire, &decoded) != nil || !reflect.DeepEqual(decoded, r) {
				t.Fatalf("selection lost in round trip: %s", wire)
			}
			if !HasContextAnnotations([]ContextReference{decoded}) {
				t.Fatal("comment-less selection lost its capability requirement")
			}
		}
	}
	r := annotationFixture()
	cloned := CloneContextReferences([]ContextReference{r})
	*cloned[0].Annotation = "changed"
	*cloned[0].BlockIndex = 99
	if *r.Annotation == "changed" || *r.BlockIndex == 99 {
		t.Fatal("selection retained mutable comment/block pointers")
	}
	if HasContextAnnotations([]ContextReference{sampleMessageReference()}) {
		t.Fatal("legacy references require a new capability")
	}
}

func TestContextAnnotationRejectsAmbiguousWireShapes(t *testing.T) {
	raw, _ := json.Marshal(annotationFixture())
	var valid map[string]any
	_ = json.Unmarshal(raw, &valid)
	cases := map[string]func(map[string]any){
		"unknown":           func(r map[string]any) { r["execute"] = true },
		"comment-null":      func(r map[string]any) { r["annotation"] = nil },
		"comment-number":    func(r map[string]any) { r["annotation"] = 7 },
		"comment-control":   func(r map[string]any) { r["annotation"] = "bad\x00" },
		"missing-basis":     func(r map[string]any) { delete(r, "textBasis") },
		"empty-basis":       func(r map[string]any) { r["textBasis"] = "" },
		"null-basis":        func(r map[string]any) { r["textBasis"] = nil },
		"unknown-basis":     func(r map[string]any) { r["textBasis"] = "markdown" },
		"missing-revision":  func(r map[string]any) { delete(r, "sourceRevision") },
		"empty-revision":    func(r map[string]any) { r["sourceRevision"] = "" },
		"null-revision":     func(r map[string]any) { r["sourceRevision"] = nil },
		"upper-revision":    func(r map[string]any) { r["sourceRevision"] = strings.Repeat("A", 64) },
		"short-revision":    func(r map[string]any) { r["sourceRevision"] = strings.Repeat("a", 63) },
		"non-hex-revision":  func(r map[string]any) { r["sourceRevision"] = strings.Repeat("g", 64) },
		"missing-block":     func(r map[string]any) { delete(r, "blockIndex") },
		"null-block":        func(r map[string]any) { r["blockIndex"] = nil },
		"negative-block":    func(r map[string]any) { r["blockIndex"] = -1 },
		"large-block":       func(r map[string]any) { r["blockIndex"] = 100001 },
		"fraction-block":    func(r map[string]any) { r["blockIndex"] = 1.5 },
		"source-with-block": func(r map[string]any) { r["textBasis"] = "source" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			value := map[string]any{}
			for k, v := range valid {
				value[k] = v
			}
			mutate(value)
			wire, _ := json.Marshal(value)
			var decoded ContextReference
			if json.Unmarshal(wire, &decoded) == nil {
				t.Fatalf("invalid annotated reference accepted: %s", wire)
			}
		})
	}
	file := `{"kind":"file","projectPath":"C:/project","label":"file","text":"x","path":"a.txt","startLine":1,"endLine":1,"capturedAt":1`
	for _, extra := range []string{`,"annotation":""}`, `,"textBasis":""}`, `,"sourceRevision":""}`, `,"blockIndex":0}`} {
		var decoded ContextReference
		if json.Unmarshal([]byte(file+extra), &decoded) == nil {
			t.Fatal("file reference accepted selection-only field", extra)
		}
	}
}

func TestContextAnnotationBudgetIncludesUTF8CommentBytes(t *testing.T) {
	r := annotationFixture()
	r.Text = strings.Repeat("x", 24<<10)
	r.StartOffset, r.EndOffset = refNumber(0), refNumber(24<<10)
	r.Annotation = annotationString(strings.Repeat("中", 2730) + "ab") // exactly 8 KiB
	if err := ValidateContextReferences([]ContextReference{r, r}); err != nil {
		t.Fatal("valid 64 KiB aggregate rejected", err)
	}
	r.Annotation = annotationString(*r.Annotation + "x")
	if ValidateContextReferences([]ContextReference{r}) == nil {
		t.Fatal("comment byte budget not enforced")
	}
	r.Annotation = annotationString(strings.Repeat("x", 8<<10))
	r.Text += "x"
	r.EndOffset = refNumber(24<<10 + 1)
	if ValidateContextReferences([]ContextReference{r, r}) == nil {
		t.Fatal("aggregate snapshot + comment budget not enforced")
	}
	bad := annotationFixture()
	bad.Annotation = annotationString(string([]byte{0xff}))
	if ValidateContextReferences([]ContextReference{bad}) == nil {
		t.Fatal("invalid UTF8 comment accepted")
	}
}
