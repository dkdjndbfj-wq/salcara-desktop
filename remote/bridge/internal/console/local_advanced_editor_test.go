package console

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/launcher"
)

func advancedEditorFixture(t *testing.T) (*Server, http.Handler, config.LocalAccount) {
	t.Helper()
	s, h := newTestServer(t)
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool { return nil }
	w := localRequest(s, h, http.MethodPost, "/api/local/accounts", map[string]any{
		"name": "advanced API", "kind": "api", "baseUrl": "https://advanced.example.test",
		"key": "synthetic-private-advanced-key", "model": "model-a", "models": []string{"model-a", "model-b"},
		"extraHeaders":       map[string]string{"X-Custom-Org": "fixture-org", "X-Channel-Token": "synthetic-header-value"},
		"capabilities":       map[string]any{"streaming": false, "functionCalling": true, "reasoning": false, "maxContextTokens": 32000},
		"systemPromptPrefix": "Use the configured project conventions.", "fallbackModel": "model-b", "maxRpm": 120, "maxTpm": 32000,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("save advanced account returned %d: %s", w.Code, w.Body)
	}
	var saved struct {
		Account config.LocalAccount `json:"account"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	a, ok := s.d.Store.Get().LocalAccount(saved.Account.ID)
	if !ok {
		t.Fatal("saved account missing")
	}
	assertAdvancedEditorValues(t, saved.Account, a)
	assertAdvancedEditorPrivateFields(t, w.Body.String(), a.Key)
	return s, h, a
}

func assertAdvancedEditorValues(t *testing.T, got, want config.LocalAccount) {
	t.Helper()
	if !reflect.DeepEqual(got.ExtraHeaders, want.ExtraHeaders) || !reflect.DeepEqual(got.Capabilities, want.Capabilities) ||
		got.SystemPromptPrefix != want.SystemPromptPrefix || got.FallbackModel != want.FallbackModel || got.MaxRPM != want.MaxRPM || got.MaxTPM != want.MaxTPM {
		t.Fatal("advanced editor values did not round-trip")
	}
}

func assertAdvancedEditorPrivateFields(t *testing.T, body, key string) {
	t.Helper()
	for _, forbidden := range []string{key, `"key":`, `"credentialRef":`, `"credentialEpoch":`} {
		if strings.Contains(body, forbidden) {
			t.Fatal("editor response exposed a protected credential field")
		}
	}
}

func TestAdvancedEditorReadbackAndLegacyRenamePreserveOptions(t *testing.T) {
	s, h, before := advancedEditorFixture(t)
	w := localRequest(s, h, http.MethodGet, "/api/local/accounts", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("read accounts returned %d", w.Code)
	}
	var list struct {
		Accounts []config.LocalAccount `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Accounts) != 1 {
		t.Fatal("account list did not return the saved account")
	}
	assertAdvancedEditorValues(t, list.Accounts[0], before)
	assertAdvancedEditorPrivateFields(t, w.Body.String(), before.Key)
	// An older client resubmits the basic form and omits advanced controls.
	w = localRequest(s, h, http.MethodPost, "/api/local/accounts", map[string]any{
		"id": before.ID, "name": "renamed API", "kind": before.Kind, "baseUrl": before.BaseURL,
		"key": "", "model": before.Model, "authMode": before.AuthMode, "wire": before.Wire,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("rename returned %d: %s", w.Code, w.Body)
	}
	after, _ := s.d.Store.Get().LocalAccount(before.ID)
	assertAdvancedEditorValues(t, after, before)
	want := before
	want.Name = "renamed API"
	if !reflect.DeepEqual(after, want) {
		t.Fatal("renaming an account changed unrelated configuration")
	}
	reopened, err := config.Open(s.d.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	persisted, _ := reopened.Get().LocalAccount(before.ID)
	assertAdvancedEditorValues(t, persisted, before)
	assertAdvancedEditorPrivateFields(t, w.Body.String(), before.Key)
}

func TestAdvancedEditorExplicitEmptyValuesClearOptions(t *testing.T) {
	s, h, before := advancedEditorFixture(t)
	w := localRequest(s, h, http.MethodPost, "/api/local/accounts", map[string]any{
		"id": before.ID, "name": before.Name, "kind": before.Kind, "baseUrl": before.BaseURL,
		"key": "", "model": before.Model, "authMode": before.AuthMode, "wire": before.Wire,
		"extraHeaders": map[string]string{}, "capabilities": map[string]any{},
		"systemPromptPrefix": "", "fallbackModel": "", "maxRpm": 0, "maxTpm": 0,
	})
	if w.Code != http.StatusOK {
		t.Fatalf("clear advanced options returned %d: %s", w.Code, w.Body)
	}
	after, _ := s.d.Store.Get().LocalAccount(before.ID)
	if len(after.ExtraHeaders) != 0 || !reflect.DeepEqual(after.Capabilities, config.AccountCapabilities{}) ||
		after.SystemPromptPrefix != "" || after.FallbackModel != "" || after.MaxRPM != 0 || after.MaxTPM != 0 {
		t.Fatal("explicit empty values did not clear advanced options")
	}
	if after.Key != before.Key || !reflect.DeepEqual(after.Models, before.Models) || after.Model != before.Model {
		t.Fatal("clearing advanced options changed the credential or model directory")
	}
	var saved struct {
		Account config.LocalAccount `json:"account"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	assertAdvancedEditorValues(t, saved.Account, after)
	assertAdvancedEditorPrivateFields(t, w.Body.String(), before.Key)
}
