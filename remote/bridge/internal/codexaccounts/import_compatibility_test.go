package codexaccounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestPortableCredentialImportShapes(t *testing.T) {
	for _, test := range []struct {
		name, raw, email, account, plan string
		count                           int
		web                             bool
	}{
		{"utf8-bom", "\ufeff" + fixture, "fixture@example.test", "explicit-fixture-account", "", 1, false},
		{"jsonl", "{\"access_token\":\"synthetic-one\"}\r\n\r\n{\"refreshToken\":\"synthetic-two\"}\n", "Codex account", "", "", 2, false},
		{"account-envelope", `{"accounts":[{"tokens":{"access_token":"synthetic-one"}},{"credentials":{"refreshToken":"synthetic-two"}}]}`, "Codex account", "", "", 2, false},
		{"sub2api", `{"type":"sub2api-data","version":1,"exported_at":"2026-10-10T00:00:00Z","proxies":[],"accounts":[{"name":"Team","platform":"openai","type":"oauth","credentials":{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","account_id":"team-1","chatgpt_account_id":"team-1","workspace_id":"team-1","email":"fixture@example.test","chatgpt_plan_type":"team","expires_at":4070908800,"subscription_expires_at":"2099-01-01T00:00:00Z"}}]}`, "fixture@example.test", "team-1", "team", 1, false},
		{"cockpit-managed", `{"email":"fixture@example.test","plan_type":"pro","account_id":"team-1","token_source_mode":"managed","tokens":{"access_token":"synthetic-access","refresh_token":"synthetic-refresh"}}`, "fixture@example.test", "team-1", "pro", 1, false},
		{"personal-token-alias", `{"atToken":"synthetic-access","accountEmail":"fixture@example.test","workspaceId":"team-1","planType":"pro"}`, "fixture@example.test", "team-1", "pro", 1, false},
		{"header-token", `{"credentials":{"headers":{"Authorization":"Bearer synthetic-access","ChatGPT-Account-Id":"team-1"},"email":"fixture@example.test"}}`, "fixture@example.test", "team-1", "", 1, false},
		{"token-alias", `{"token":"synthetic-access"}`, "Codex account", "", "", 1, false},
		{"web-session", `{"accessToken":"synthetic-session","user":{"email":"fixture@example.test"},"account":{"id":"team-1","planType":"pro","subscriptionExpiresAt":"2099-01-01T00:00:00Z"},"expires":"2099-01-01T00:00:00Z"}`, "fixture@example.test", "team-1", "pro", 1, true},
		{"session-object", `{"session":{"accessToken":"synthetic-session","user":{"email":"fixture@example.test"},"account":{"id":"team-1"}}}`, "fixture@example.test", "team-1", "", 1, true},
		{"session-string", `{"session_json":"{\"accessToken\":\"synthetic-session\",\"user\":{\"email\":\"fixture@example.test\"}}"}`, "fixture@example.test", "", "", 1, true},
		{"cockpit-web-session", `{"email":"fixture@example.test","token_source_mode":"chatgpt_web_session","tokens":{"access_token":"synthetic-session","refresh_token":"synthetic-unknown-client"}}`, "fixture@example.test", "", "", 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, err := ParseImport([]byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if len(p.records) != test.count {
				t.Fatalf("got %d accounts, want %d", len(p.records), test.count)
			}
			c := p.records[0]
			if c.Name != test.email || c.AccountID != test.account || c.Plan != test.plan || (c.TokenSourceMode == "chatgpt_web_session") != test.web {
				t.Fatalf("metadata mismatch: %+v", p.Preview())
			}
			raw, _ := json.Marshal(p.Preview())
			if strings.Contains(string(raw), "synthetic-") || strings.Contains(string(raw), "team-1") {
				t.Fatal("preview exposed credential or routing identity")
			}
		})
	}
}

func TestPortableImportDoesNotMixCredentialsOrRelaxJSONValidation(t *testing.T) {
	for _, raw := range []string{
		`{"accessToken":"first","atToken":"second"}`,
		`{"headers":{"Authorization":"Bearer first","authorization":"Bearer second"}}`,
		`{"access_token":"first","headers":{"Authorization":"Bearer second"}}`,
		`{"access_token":"first","tokens":{"refresh_token":"second"}}`,
		`{"tokens":{"access_token":"first"},"credentials":{"refresh_token":"second"}}`,
		`{"provider":"codex","metadata":{"provider":"claude","access_token":"first"}}`,
		`{"credentials":{"type":123,"access_token":"first"}}`,
		`{"tokens":{"access_token":"first","account_id":"first"},"accountId":"second"}`,
		`{"session":{"accessToken":"first"},"refreshToken":"second"}`,
		`{"session":{"accessToken":"first"},"session_json":"{\"accessToken\":\"second\"}"}`,
		`{"session_json":"{\"accessToken\":\"first\",\"accessToken\":\"second\"}"}`,
		`{"credentials":{"access_token":"first","chatgpt_account_id":"first","workspace_id":"second"}}`,
		`{"credentials":{"access_token":"first","account_id":{}},"account_id":{}}`,
		`{"access_token":"first","expires_at":{}}`,
		`{"access_token":"first","expires_at":true}`,
		`{"access_token":"first","expiresAt":"2099-01-01T00:00:00Z","expired":"2098-01-01T00:00:00Z"}`,
		`{"accessToken":"first","user":{"email":"fixture@example.test","token":"second"}}`,
		`{"accounts":[{"access_token":"first"}],"refresh_token":"second"}`,
		`{"accounts":[{"platform":"anthropic","type":"oauth","credentials":{"access_token":"first"}}]}`,
		`{"accounts":[{"platform":"openai","type":"apikey","credentials":{"access_token":"first"}}]}`,
		`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-key","tokens":{"access_token":"first"}}`,
		`{"auth_mode":"agentIdentity","access_token":"first"}`,
		`{"auth_mode":"agentidentity","access_token":"first"}`,
		`{"access_token":"first","account_id":"line\nsecond"}`,
		`{"access_token":"sk-api-key"}`,
		`{"refresh_token":"sk-api-key"}`,
		`{"session":{"refreshToken":"synthetic-web-refresh","user":{"email":"fixture@example.test"}}}`,
		`{"token_source_mode":"chatgpt_web_session","refresh_token":"synthetic-web-refresh"}`,
		`{"access_token":"first","token_source_mode":"unknown-client"}`,
		"{\"access_token\":\"first\"}\n{\"access_token\":\"second\",\"access_token\":\"third\"}",
		"{\"access_token\":\"first\"}\ntruncated",
		`{"access_token":"first"} {"access_token":"second"}`,
	} {
		if p, err := ParseImport([]byte(raw)); err != ErrInvalidImport || p != nil {
			t.Fatalf("accepted ambiguous or malformed import: %s", raw)
		}
	}
}

func TestImportFilesSharesBoundsAndPreservesAtomicCommit(t *testing.T) {
	first := []byte(`{"tokens":{"access_token":"synthetic-first"}}`)
	second := []byte("{\"accessToken\":\"synthetic-second\"}\n{\"refreshToken\":\"synthetic-third\"}")
	p, err := ParseImportFiles([][]byte{first, second}, "json")
	if err != nil || len(p.records) != 3 || p.Preview().Source != "files" {
		t.Fatal("mixed JSON documents did not produce one prepared import", err)
	}
	s, _, _ := newTestService(t, nil, nil)
	rows, err := s.Import(context.Background(), p, "")
	if err != nil || len(rows) != 3 || len(s.List()) != 3 {
		t.Fatal("batch did not commit", err)
	}
	for _, files := range [][][]byte{
		{first, []byte(`{"access_token":"one","access_token":"two"}`)},
		{first, nil},
		{[]byte(strings.Repeat(" ", MaxImportBytes/2) + string(first)), []byte(strings.Repeat(" ", MaxImportBytes/2) + string(first))},
	} {
		if prepared, err := ParseImportFiles(files, "json"); err != ErrInvalidImport || prepared != nil {
			t.Fatal("partial/oversize file selection accepted")
		}
	}
	files := make([][]byte, MaxAccounts)
	for i := range files {
		files[i] = []byte(fmt.Sprintf(`{"access_token":"synthetic-%d"}`, i))
	}
	if prepared, err := ParseImportFiles(files, "json"); err != nil || len(prepared.records) != MaxAccounts {
		t.Fatal("valid boundary failed", err)
	}
	files[0] = []byte(`[{"access_token":"synthetic-extra"},{"access_token":"synthetic-0"}]`)
	if _, err := ParseImportFiles(files, "json"); err != ErrInvalidImport {
		t.Fatal("account limit was applied per file")
	}
	// Duplicate credentials across files are rejected atomically by the vault;
	// a valid first document must not be saved when the second conflicts.
	duplicate, err := ParseImportFiles([][]byte{first, first}, "json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Import(context.Background(), duplicate, ""); err != ErrConflict || len(s.List()) != 3 {
		t.Fatal("duplicate batch partially committed", err)
	}
}

func TestExplicitTokenJSONStringsAndBOM(t *testing.T) {
	for _, kind := range []string{"access_token", "refresh_token"} {
		for _, input := range []string{"\ufeffsynthetic-first\r\nsynthetic-second", `["synthetic-first","synthetic-second"]`, `"synthetic-first"`} {
			p, err := ParseImportFiles([][]byte{[]byte(input)}, kind)
			if err != nil || len(p.records) == 0 {
				t.Fatal("explicit token form rejected", kind, err)
			}
		}
		for _, input := range []string{`["synthetic-first",42]`, `"first\nsecond"`, `{"token":"synthetic-first"}`, `["first","second"`, `"first" "second"`} {
			if _, err := ParseTokenImport([]byte(input), kind); err != ErrInvalidImport {
				t.Fatal("invalid explicit token form accepted")
			}
		}
	}
}

func TestWebSessionImportPersistsQuotaOnlyBoundary(t *testing.T) {
	var refreshes, usages atomic.Int32
	s, protector, path := newTestService(t, RefreshFunc(func(context.Context, *coreauth.Auth) (*coreauth.Auth, error) {
		refreshes.Add(1)
		return sdkFixture("synthetic-new", "synthetic-refresh"), nil
	}), nil)
	s.usageFetcher = UsageFetchFunc(func(_ context.Context, auth *coreauth.Auth) (UsageFetchResult, error) {
		usages.Add(1)
		if auth.Metadata["access_token"] != "synthetic-session" || auth.Metadata["account_id"] != "workspace" {
			t.Fatal("quota request lost session credential")
		}
		return UsageFetchResult{Snapshot: UsageSnapshot{Status: UsageStatusFresh}}, nil
	})
	p, err := ParseImport([]byte(`{"session":{"accessToken":"synthetic-session","refreshToken":"synthetic-web-refresh","user":{"email":"fixture@example.test"},"account":{"id":"workspace"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.Import(context.Background(), p, "")
	if err != nil || rows[0].Mode != "chatgpt_web_session" {
		t.Fatal("session mode missing", err)
	}
	checkBlocked := func(service *Service) {
		t.Helper()
		if _, err := service.SDKRecord(context.Background(), rows[0].ID); !errors.Is(err, ErrConflict) {
			t.Fatal("web session leaked to SDKRecord", err)
		}
		if _, err := service.ExecutionAvailability(rows[0].ID, rows[0].IdentityEpoch); !errors.Is(err, ErrConflict) {
			t.Fatal("web session became API executable", err)
		}
		records, err := service.SDKStore().List(context.Background())
		if err != nil || len(records) != 0 {
			t.Fatal("web session projected into SDKStore", err)
		}
		if _, err := service.Refresh(context.Background(), rows[0].ID); !errors.Is(err, ErrConflict) || refreshes.Load() != 0 {
			t.Fatal("web refresh was sent through Codex client", err)
		}
	}
	checkBlocked(s)
	if _, err := s.RefreshUsage(context.Background(), rows[0].ID); err != nil || usages.Load() != 1 {
		t.Fatal("session quota inspection was unavailable", err)
	}
	s.Close()
	reopened, err := New(Options{Path: path, Protector: protector})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, _ := reopened.Get(rows[0].ID); got.Mode != "chatgpt_web_session" {
		t.Fatal("restart discarded quota-only classification")
	}
	checkBlocked(reopened)
	// A provider expiry is still respected; quota-only does not mean the token
	// can be rotated using a different OAuth application's client identity.
	reopened.mu.Lock()
	c := reopened.vault.Accounts[rows[0].ID]
	c.ExpiresAt = time.Unix(1, 0)
	reopened.vault.Accounts[c.ID] = c
	reopened.mu.Unlock()
	if _, err := reopened.RefreshUsage(context.Background(), c.ID); err == nil || refreshes.Load() != 0 {
		t.Fatal("expired web session silently refreshed")
	}
}
