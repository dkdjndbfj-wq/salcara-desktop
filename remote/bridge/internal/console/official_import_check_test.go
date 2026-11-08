package console

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"salcara/bridge/internal/codexaccounts"
)

func importCheckConsoleRecord(index int) string {
	return fmt.Sprintf(`{"type":"codex","email":"import-%d@example.test","access_token":"synthetic-import-access-%d","account_id":"synthetic-import-provider-%d","expired":"2099-01-01T00:00:00Z"}`, index, index, index)
}

func TestOfficialImportCheckIsReadOnlyAndUsesOriginalIndices(t *testing.T) {
	var calls atomic.Int32
	s, h := officialConsoleWithUsage(t, codexaccounts.UsageFetchFunc(func(context.Context, *coreauth.Auth) (codexaccounts.UsageFetchResult, error) {
		calls.Add(1)
		return codexaccounts.UsageFetchResult{Snapshot: codexaccounts.UsageSnapshot{Plan: codexaccounts.UsagePlan{Raw: "pro"}}}, nil
	}))
	before := s.d.Store.Get()
	w := localRequest(s, h, "POST", "/api/local/codex/import/check", map[string]any{"files": []string{importCheckConsoleRecord(0), `{"refresh_token":"synthetic-import-refresh-only","email":"refresh@example.test"}`}, "format": "json"})
	var result codexaccounts.ImportCheckResult
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Accounts) != 2 {
		t.Fatal(w.Code, w.Body)
	}
	if result.Accounts[0].Index != 0 || result.Accounts[0].Status != "verified" || result.Accounts[0].Plan.Family != "pro" || result.Accounts[1].Index != 1 || result.Accounts[1].Status != "needs_refresh" || calls.Load() != 1 {
		t.Fatal(result, calls.Load())
	}
	if len(s.d.CodexAccounts.List()) != 0 || !reflect.DeepEqual(before, s.d.Store.Get()) {
		t.Fatal("check imported or changed config")
	}
	for _, secret := range []string{"synthetic-import-access", "synthetic-import-refresh", "synthetic-import-provider", "access_token", "refresh_token"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("private import result")
		}
	}
}

func TestOfficialImportSelectionKeepsOriginalOrderAndExistingAccounts(t *testing.T) {
	s, h := officialConsole(t)
	existingID := importOfficialConsole(t, s, h)
	raw := "[" + importCheckConsoleRecord(0) + "," + importCheckConsoleRecord(1) + "," + importCheckConsoleRecord(2) + "]"
	w := localRequest(s, h, "POST", "/api/local/codex/import", map[string]any{"credentials": raw, "selection": []int{2, 0}})
	var result struct {
		Accounts []codexaccounts.Account `json:"accounts"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || len(result.Accounts) != 2 {
		t.Fatal(w.Code, w.Body)
	}
	if result.Accounts[0].Name != "import-0@example.test" || result.Accounts[1].Name != "import-2@example.test" {
		t.Fatal("selected indices were shifted", result)
	}
	if len(s.d.CodexAccounts.List()) != 3 {
		t.Fatal("wrong count")
	}
	if _, err := s.d.CodexAccounts.Get(existingID); err != nil {
		t.Fatal("selection removed existing account")
	}
	if !strings.Contains(raw, "synthetic-import-access-1") {
		t.Fatal("selection mutated source")
	}
}

func TestOfficialImportSelectionRejectsAmbiguityAndCannotBypassReplacement(t *testing.T) {
	s, h := officialConsole(t)
	existingID := importOfficialConsole(t, s, h)
	raw := "[" + importCheckConsoleRecord(0) + "," + importCheckConsoleRecord(1) + "]"
	encoded, _ := json.Marshal(raw)
	for _, selection := range []string{`[]`, `null`, `[null]`, `[0,0]`, `[-1]`, `[2]`, `["0"]`, `[0.5]`, `[0.0]`, `{}`, `[0,1,2]`} {
		body := `{"credentials":` + string(encoded) + `,"selection":` + selection + `}`
		w := do(h, "POST", "/api/local/codex/import", "127.0.0.1:47831", map[string]string{"Cookie": CookieName + "=" + s.Token(), "Content-Type": "application/json"}, body)
		if w.Code != 400 {
			t.Fatal("ambiguous selection accepted", selection, w.Code, w.Body)
		}
	}
	w := localRequest(s, h, "POST", "/api/local/codex/import", map[string]any{"credentials": raw, "selection": []int{1}, "replaceId": existingID})
	if w.Code != 400 {
		t.Fatal("selection bypassed original single-account replacement", w.Code, w.Body)
	}
	if rows := s.d.CodexAccounts.List(); len(rows) != 1 || rows[0].ID != existingID {
		t.Fatal("rejected selection altered existing vault")
	}
}

func TestOfficialImportChecksNeverSilentlyDropInvalidJSON(t *testing.T) {
	var calls atomic.Int32
	s, h := officialConsoleWithUsage(t, codexaccounts.UsageFetchFunc(func(context.Context, *coreauth.Auth) (codexaccounts.UsageFetchResult, error) {
		calls.Add(1)
		return codexaccounts.UsageFetchResult{}, nil
	}))
	for _, path := range []string{"/api/local/codex/import/check", "/api/local/codex/import/preview", "/api/local/codex/import"} {
		body := map[string]any{"files": []string{importCheckConsoleRecord(0), `{"access_token":"first","access_token":"second"}`}, "format": "json"}
		if path == "/api/local/codex/import" {
			body["selection"] = []int{0}
		}
		w := localRequest(s, h, "POST", path, body)
		if w.Code != 400 {
			t.Fatal("bad batch partly accepted", path, w.Code, w.Body)
		}
	}
	if calls.Load() != 0 || len(s.d.CodexAccounts.List()) != 0 {
		t.Fatal("invalid batch probed or stored")
	}
	for _, path := range []string{"/api/local/codex/import/check", "/api/local/codex/import/preview"} {
		w := localRequest(s, h, "POST", path, map[string]any{"credentials": importCheckConsoleRecord(0), "selection": []int{0}})
		if w.Code != 400 {
			t.Fatal("preview/check allowed index-reordering selection")
		}
	}
}

func TestOfficialImportCheckDoesNotReturnProviderErrorOrIdentity(t *testing.T) {
	s, h := officialConsoleWithUsage(t, codexaccounts.UsageFetchFunc(func(context.Context, *coreauth.Auth) (codexaccounts.UsageFetchResult, error) {
		return codexaccounts.UsageFetchResult{}, errors.New("synthetic-import-access-0 synthetic-import-provider-0 private-error-body")
	}))
	w := localRequest(s, h, "POST", "/api/local/codex/import/check", map[string]any{"credentials": importCheckConsoleRecord(0)})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"unknown"`) || !strings.Contains(w.Body.String(), `"code":"unavailable"`) {
		t.Fatal(w.Code, w.Body)
	}
	for _, secret := range []string{"synthetic-import-access-0", "synthetic-import-provider-0", "private-error-body"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("private provider error exposed")
		}
	}
}
