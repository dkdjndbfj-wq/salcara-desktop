package codexaccounts

import (
	"encoding/json"
	"testing"
)

// Pinned Cockpit be24ce6171279741c2cd141bc02bd9f380f4b67c has an explicit
// Sub2API OAuth access-token fixture with these two personal-access-token
// markers (codex_account_tests_identity_import_refresh.rs, test
// extract_candidate_from_sub2api_account_credentials). The values here are
// independent synthetic data; API keys and Agent Identity are not involved.
func TestImportReferenceSub2APIPersonalAccessTokenMode(t *testing.T) {
	p, err := ParseImport([]byte(`{
		"name":"Synthetic Sub2API OAuth",
		"platform":"openai","type":"oauth",
		"credentials":{
			"email":"fixture@example.test",
			"access_token":"at-synthetic-sub2api",
			"token_type":"Bearer",
			"auth_mode":"personal_access_token",
			"openai_auth_mode":"personal_access_token",
			"plan_type":"team",
			"chatgpt_account_id":"synthetic-workspace",
			"expires_at":"2099-01-01T00:00:00Z",
			"subscription_expires_at":"2098-12-01T00:00:00Z"
		}
	}`))
	if err != nil || p == nil {
		t.Fatalf("normal access-only OAuth export rejected: %v", err)
	}
	c := p.records[0]
	if c.AccessToken != "at-synthetic-sub2api" || c.AccountID != "synthetic-workspace" || c.Plan != "team" || c.RefreshToken != "" || c.TokenSourceMode != "" {
		t.Fatal("personal access token metadata or credential mode changed")
	}
}

func TestImportReferencePersonalAccessTokenModeAliases(t *testing.T) {
	for _, field := range []string{"auth_mode", "authMode", "openai_auth_mode", "openaiAuthMode"} {
		for _, nested := range []bool{false, true} {
			t.Run(field+map[bool]string{false: "-flat", true: "-nested"}[nested], func(t *testing.T) {
				record := map[string]any{
					"accessToken": "at-synthetic-personal", "refreshToken": "", "idToken": nil,
					"workspaceId": "synthetic-workspace", "planType": "team",
					field: " PERSONAL_ACCESS_TOKEN ",
				}
				input := record
				if nested {
					input = map[string]any{"platform": "openai", "type": "oauth", "credentials": record, "auth_mode": ""}
				}
				raw, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				p, err := ParseImport(raw)
				if err != nil || p == nil {
					t.Fatalf("valid personal access-token mode rejected: %v", err)
				}
				if p.records[0].AccessToken != "at-synthetic-personal" || p.records[0].AccountID != "synthetic-workspace" || p.records[0].Plan != "team" {
					t.Fatal("mode alias changed account metadata")
				}
			})
		}
	}
}

func TestImportReferencePersonalAccessTokenModeRejectsMixedRecords(t *testing.T) {
	for _, raw := range []string{
		`{"auth_mode":"personal_access_token","refresh_token":"synthetic-refresh"}`,
		`{"auth_mode":"personal_access_token","access_token":"at-synthetic","refresh_token":"synthetic-refresh"}`,
		`{"auth_mode":"personal_access_token","access_token":"at-synthetic","id_token":"synthetic-id"}`,
		`{"auth_mode":"personal_access_token","tokens":{"auth_mode":"","access_token":"at-synthetic","refresh_token":"synthetic-refresh"}}`,
		`{"auth_mode":"","credentials":{"auth_mode":"personal_access_token","access_token":"at-synthetic","id_token":"synthetic-id"}}`,
		`{"auth_mode":"oauth","openai_auth_mode":"personal_access_token","access_token":"at-synthetic","refresh_token":"synthetic-refresh"}`,
		`{"auth_mode":"personal_access_token","access_token":"at-synthetic","personal_access_token":"at-other"}`,
		`{"auth_mode":"personal_access_token","access_token":"at-synthetic","headers":{"Authorization":"Bearer at-other"}}`,
		`{"auth_mode":"personal_access_token","access_token":"at-synthetic","OPENAI_API_KEY":"sk-synthetic"}`,
		`{"auth_mode":"personal_access_token","access_token":"at-synthetic","agent_runtime_id":"synthetic-runtime"}`,
		`{"auth_mode":"personal_access_token","type":"oauth","access_token":"at-synthetic"}`,
		`{"auth_mode":"personal_access_token","platform":"anthropic","type":"oauth","access_token":"at-synthetic"}`,
	} {
		if p, err := ParseImport([]byte(raw)); err != ErrInvalidImport || p != nil {
			t.Fatal("mixed identity or conflicting access-token material accepted")
		}
	}
	for _, field := range []string{"auth_mode", "authMode", "openai_auth_mode", "openaiAuthMode"} {
		for _, mode := range []any{"apikey", "agentIdentity", "siwc", "unknown-provider", true, 1, map[string]any{}} {
			for _, nested := range []bool{false, true} {
				record := map[string]any{"access_token": "at-synthetic", field: mode}
				input := record
				if nested {
					input = map[string]any{"auth_mode": "", "credentials": record}
				}
				raw, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				if p, err := ParseImport(raw); err != ErrInvalidImport || p != nil {
					t.Fatalf("unsupported %s credential mode accepted", field)
				}
			}
		}
	}
}
