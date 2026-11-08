package apiservice

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

func TestSSEResumeWriterKeepsLinesAcrossWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	state := &StreamState{}
	rw := NewSSEResumeWriter(rec, state)

	if _, err := rw.Write([]byte("data: {\"response_id\":\"resume-")); err != nil {
		t.Fatal(err)
	}
	if got := state.LastResponseID(); got != "" {
		t.Fatalf("parsed incomplete JSON line as %q", got)
	}
	if _, err := rw.Write([]byte("123\"}\n\n")); err != nil {
		t.Fatal(err)
	}
	if got := state.LastResponseID(); got != "resume-123" {
		t.Fatalf("response id = %q, want resume-123", got)
	}

	// The completion marker is split in the same way a network write can split
	// any other SSE field.
	if _, err := rw.Write([]byte("data: [DO")); err != nil {
		t.Fatal(err)
	}
	if state.Completed() {
		t.Fatal("split completion marker was accepted too early")
	}
	if _, err := rw.Write([]byte("NE]\n\n")); err != nil {
		t.Fatal(err)
	}
	if !state.Completed() {
		t.Fatal("completion marker was lost across writes")
	}
}

func TestSSEResumeWriterFinishParsesUnterminatedMarker(t *testing.T) {
	rec := httptest.NewRecorder()
	state := &StreamState{}
	rw := NewSSEResumeWriter(rec, state)
	if _, err := rw.Write([]byte("data: [DONE]")); err != nil {
		t.Fatal(err)
	}
	if state.Completed() {
		t.Fatal("unterminated marker was parsed before Finish")
	}
	rw.Finish()
	if !state.Completed() {
		t.Fatal("Finish did not parse the final SSE line")
	}
}

func TestSSEResumeWriterRecognizesDoneEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	state := &StreamState{}
	rw := NewSSEResumeWriter(rec, state)
	if _, err := rw.Write([]byte("event: done\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if !state.Completed() {
		t.Fatal("event: done was not recognized")
	}
}

func TestSSEResumeWriterKeepsEventIDAcrossWrites(t *testing.T) {
	rec := httptest.NewRecorder()
	state := &StreamState{}
	rw := NewSSEResumeWriter(rec, state)
	if _, err := rw.Write([]byte("id: event-")); err != nil {
		t.Fatal(err)
	}
	if got := state.LastResponseID(); got != "" {
		t.Fatalf("parsed incomplete id field as %q", got)
	}
	if _, err := rw.Write([]byte("42\n")); err != nil {
		t.Fatal(err)
	}
	if got := state.LastResponseID(); got != "event-42" {
		t.Fatalf("event id = %q, want event-42", got)
	}
}

func TestSSEResumeWriterSkipsOversizedMetadataLine(t *testing.T) {
	rec := httptest.NewRecorder()
	state := &StreamState{}
	rw := NewSSEResumeWriter(rec, state)
	chunk := append([]byte("data: "), bytes.Repeat([]byte("x"), maxSSELineBytes)...)
	chunk = append(chunk, []byte("\nid: keep-me\n")...)
	if _, err := rw.Write(chunk); err != nil {
		t.Fatal(err)
	}
	if got := state.LastResponseID(); got != "keep-me" {
		t.Fatalf("event id after oversized line = %q, want keep-me", got)
	}
	if got := rec.Body.String(); got != string(chunk) {
		t.Fatal("metadata size limit changed forwarded response bytes")
	}
}
