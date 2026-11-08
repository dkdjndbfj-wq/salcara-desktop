package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync/atomic"
	"testing"

	"salcara/bridge/internal/config"
)

func TestModelDraftUsesUnsavedPolicyAndHeadersWithoutPersisting(t *testing.T) {
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "Bearer synthetic-draft-key" {
			t.Error("model read changed credential or method")
		}
		if r.Header.Get("X-Channel") != "draft" || r.Header.Get("X-Old-Channel") != "" {
			t.Error("model discovery ignored draft channel headers")
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"saved-only"},{"id":"draft-only"},{"id":"denied"}]}`))
	}))
	defer u.Close()
	s, h := newTestServer(t)
	id := createLocal(t, s, h, "api", "draft", "synthetic-draft-key", u.URL)
	updateSourceFixture(t, s, id, func(a *config.LocalAccount) {
		a.ModelAllowlist = []string{"saved-only"}
		a.Models, a.Model, a.ModelsManual = []string{"saved-only"}, "saved-only", true
		a.ExtraHeaders = map[string]string{"X-Channel": "saved", "X-Old-Channel": "must-be-replaced"}
	})
	before := s.d.Store.Get()
	diskBefore, err := os.ReadFile(s.d.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	seq := s.beginModelProbe(id)
	w := localRequest(s, h, http.MethodPost, "/api/local/models", map[string]any{
		"id": id, "draftOnly": true, "wire": "chat", "modelAllowlist": []string{"draft-only", "denied"}, "modelDenylist": []string{"denied"},
		"extraHeaders": map[string]string{"X-Channel": "draft", "Authorization": "Bearer must-not-win"},
	})
	if w.Code != 200 {
		t.Fatalf("draft read: %d %s", w.Code, w.Body)
	}
	var reply struct {
		Models []string `json:"models"`
	}
	if json.Unmarshal(w.Body.Bytes(), &reply) != nil || !reflect.DeepEqual(reply.Models, []string{"draft-only"}) {
		t.Fatal("saved policy replaced unsaved model rules")
	}
	if !reflect.DeepEqual(before, s.d.Store.Get()) || s.modelProbeSeq[id] != seq {
		t.Fatal("draft read committed state or canceled a card refresh")
	}
	diskAfter, _ := os.ReadFile(s.d.Store.Path())
	if string(diskBefore) != string(diskAfter) || calls.Load() != 1 {
		t.Fatal("draft read wrote configuration or retried")
	}
}

func TestModelDraftEmptyResultDoesNotRevokeSavedModels(t *testing.T) {
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) }))
	defer u.Close()
	s, h := newTestServer(t)
	id := createLocal(t, s, h, "api", "draft", "synthetic-draft-key", u.URL)
	updateSourceFixture(t, s, id, func(a *config.LocalAccount) { a.Model, a.Models = "saved", []string{"saved"} })
	before := s.d.Store.Get()
	w := localRequest(s, h, http.MethodPost, "/api/local/models", map[string]any{"id": id, "draftOnly": true})
	if w.Code != 502 || !reflect.DeepEqual(before, s.d.Store.Get()) {
		t.Fatal("empty draft read revoked a saved model")
	}
	// The explicit card refresh retains its existing authoritative behavior.
	w = localRequest(s, h, http.MethodPost, "/api/local/models", map[string]any{"id": id})
	a, _ := s.d.Store.Get().LocalAccount(id)
	if w.Code != 502 || len(a.Models) != 0 || a.Model != "" {
		t.Fatal("card refresh no longer commits the empty directory")
	}
}
