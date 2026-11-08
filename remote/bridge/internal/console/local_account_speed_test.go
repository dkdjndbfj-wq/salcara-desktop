package console

import (
	"encoding/json"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestAccountSpeedNarrowSaveAndEditorRoundTrip(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	for _, mode := range []string{"fast", "ultrafast", "standard"} {
		before := s.d.Store.Get()
		a, _ := before.LocalAccount(id)
		w := localRequest(s, h, http.MethodPost, "/api/local/accounts/speed", map[string]any{"id": id, "sourceRevision": before.AccountSourceRevision(a), "speedMode": mode})
		if w.Code != http.StatusOK {
			t.Fatalf("save: %d %s", w.Code, w.Body)
		}
		after := s.d.Store.Get()
		saved, _ := after.LocalAccount(id)
		want := a
		want.SpeedMode = mode
		if !reflect.DeepEqual(saved, want) {
			t.Fatal("narrow speed update changed other account fields")
		}
		var reply struct {
			OK        bool   `json:"ok"`
			ID        string `json:"id"`
			SpeedMode string `json:"speedMode"`
			Revision  string `json:"sourceRevision"`
		}
		if json.Unmarshal(w.Body.Bytes(), &reply) != nil || !reply.OK || reply.ID != id || reply.SpeedMode != mode || reply.Revision != after.AccountSourceRevision(saved) {
			t.Fatal("speed save lacks a committed acknowledgement")
		}
		if strings.Contains(w.Body.String(), a.Key) || strings.Contains(w.Body.String(), config.AccountFingerprint(a)) {
			t.Fatal("speed reply disclosed private source data")
		}
		// An old full editor must retain the tier it does not send.
		w = localRequest(s, h, http.MethodPost, "/api/local/accounts", map[string]any{"id": id, "name": a.Name, "kind": a.Kind, "baseUrl": a.BaseURL, "model": a.Model})
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"speedMode":"`+mode+`"`) {
			t.Fatalf("old editor lost tier: %d %s", w.Code, w.Body)
		}
		persisted, err := config.Open(s.d.Store.Path())
		if err != nil {
			t.Fatal(err)
		}
		loaded, _ := persisted.Get().LocalAccount(id)
		if loaded.SpeedMode != mode {
			t.Fatal("tier was not persisted")
		}
	}
	// The complete editor can change the field too, with canonical readback.
	a, _ := s.d.Store.Get().LocalAccount(id)
	w := localRequest(s, h, http.MethodPost, "/api/local/accounts", map[string]any{"id": id, "name": a.Name, "kind": a.Kind, "baseUrl": a.BaseURL, "model": a.Model, "speedMode": "fast"})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"speedMode":"fast"`) {
		t.Fatal("complete editor cannot save tier")
	}
}

func TestAccountSpeedRejectsStaleInvalidOrBroadWrites(t *testing.T) {
	for _, mutate := range []func(map[string]any){
		func(in map[string]any) { delete(in, "sourceRevision") },
		func(in map[string]any) { in["sourceRevision"] = "stale" },
		func(in map[string]any) { in["sourceRevision"] = nil },
		func(in map[string]any) { in["speedMode"] = "ultra" },
		func(in map[string]any) { in["speedMode"] = "" },
		func(in map[string]any) { in["key"] = "replacement" },
	} {
		s, h, id := sourceRevisionFixture(t)
		before := s.d.Store.Get()
		a, _ := before.LocalAccount(id)
		in := map[string]any{"id": id, "sourceRevision": before.AccountSourceRevision(a), "speedMode": "fast"}
		mutate(in)
		w := localRequest(s, h, http.MethodPost, "/api/local/accounts/speed", in)
		if w.Code < 400 || !reflect.DeepEqual(before, s.d.Store.Get()) {
			t.Fatal("invalid speed write modified configuration")
		}
	}
	s, h, id := sourceRevisionFixture(t)
	before := s.d.Store.Get()
	a, _ := before.LocalAccount(id)
	in := map[string]any{"id": id, "sourceRevision": before.AccountSourceRevision(a), "speedMode": "fast"}
	if localRequest(s, h, http.MethodPost, "/api/local/accounts/speed", in).Code != 200 {
		t.Fatal("first save failed")
	}
	in["speedMode"] = "ultrafast"
	if localRequest(s, h, http.MethodPost, "/api/local/accounts/speed", in).Code != http.StatusConflict {
		t.Fatal("stale editor overwrote tier")
	}
}

func TestAccountEditorRevisionCannotUndoConcurrentSpeedChoice(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	before := s.d.Store.Get()
	a, _ := before.LocalAccount(id)
	revision := before.AccountSourceRevision(a)
	w := localRequest(s, h, http.MethodPost, "/api/local/accounts/speed", map[string]any{"id": id, "sourceRevision": revision, "speedMode": "fast"})
	if w.Code != http.StatusOK {
		t.Fatal("speed setup failed")
	}
	current := s.d.Store.Get()
	for _, stale := range []any{revision, "", nil} {
		w = localRequest(s, h, http.MethodPost, "/api/local/accounts", map[string]any{"id": id, "sourceRevision": stale, "name": a.Name, "kind": a.Kind, "baseUrl": a.BaseURL, "speedMode": "standard"})
		if w.Code != http.StatusConflict || !reflect.DeepEqual(current, s.d.Store.Get()) {
			t.Fatalf("stale editor changed account: %d", w.Code)
		}
	}
	a, _ = current.LocalAccount(id)
	w = localRequest(s, h, http.MethodPost, "/api/local/accounts", map[string]any{"id": id, "sourceRevision": current.AccountSourceRevision(a), "name": "new title", "kind": a.Kind, "baseUrl": a.BaseURL, "speedMode": a.SpeedMode})
	if w.Code != http.StatusOK {
		t.Fatalf("current editor rejected: %d %s", w.Code, w.Body)
	}
}

func TestAccountSpeedDiskFailureKeepsPreviousPolicy(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	before := s.d.Store.Get()
	a, _ := before.LocalAccount(id)
	// The store is a synthetic temporary fixture, never user configuration.
	if err := os.Remove(s.d.Store.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.d.Store.Path(), 0700); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, http.MethodPost, "/api/local/accounts/speed", map[string]any{"id": id, "sourceRevision": before.AccountSourceRevision(a), "speedMode": "ultrafast"})
	if w.Code < 400 || !reflect.DeepEqual(before, s.d.Store.Get()) {
		t.Fatal("failed disk write published the new tier")
	}
}

func TestAccountSpeedConcurrentSelectionsHaveOneWinner(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	before := s.d.Store.Get()
	a, _ := before.LocalAccount(id)
	start, done := make(chan struct{}), make(chan int, 2)
	for _, mode := range []string{"fast", "ultrafast"} {
		go func() {
			<-start
			w := localRequest(s, h, http.MethodPost, "/api/local/accounts/speed", map[string]any{"id": id, "sourceRevision": before.AccountSourceRevision(a), "speedMode": mode})
			done <- w.Code
		}()
	}
	close(start)
	codes := map[int]int{}
	codes[<-done]++
	codes[<-done]++
	if codes[http.StatusOK] != 1 || codes[http.StatusConflict] != 1 {
		t.Fatalf("concurrent speed writes: %v", codes)
	}
}
