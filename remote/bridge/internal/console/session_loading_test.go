package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"salcara/bridge/internal/protocol"
	"testing"
)

func TestLocalSessionLoadingDoesNotNeedRemoteDevice(t *testing.T) {
	// No Hub, credentials or remote transport exists in this fixture.
	s := &Server{}
	s.SetManager(sessionManager{a: &sessionAgent{info: protocol.SessionInfo{SessionKey: "codex:local", Tool: "codex", Title: "Local fixture"}}})
	w := httptest.NewRecorder()
	s.handleSessions(w, httptest.NewRequest("GET", "/api/sessions", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("local read failed: %d", w.Code)
	}
	var body struct {
		Sessions []protocol.SessionInfo `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Sessions) != 1 || body.Sessions[0].SessionKey != "codex:local" {
		t.Fatalf("local history unavailable: %v", err)
	}
}

func TestLocalSessionStartupIsNotAnEmptyHistory(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleSessions(w, httptest.NewRequest("GET", "/api/sessions", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("unready reader returned %d", w.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body["error"] == nil || body["sessions"] != nil {
		t.Fatalf("startup failure masked as empty: %v", err)
	}
}
