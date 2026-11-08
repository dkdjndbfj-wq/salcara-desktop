package codexaccounts

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestImportCustomHeadersPreservesExplicitWorkspace(t *testing.T) {
	for _, test := range []struct {
		name, raw string
		web       bool
	}{
		{"cockpit-custom-headers", `{"personal_access_token":"at-synthetic-import","email":"fixture@example.test","custom_headers":{"ChatGPT-Account-Id":"workspace-synthetic"}}`, false},
		{"camel-custom-headers", `{"atToken":"at-synthetic-import","customHeaders":{"ChatGPT-Account-Id":"workspace-synthetic"}}`, false},
		{"case-insensitive", `{"accessToken":"at-synthetic-import","custom_headers":{"CHATGPT-ACCOUNT-ID":"workspace-synthetic"}}`, false},
		{"nested-credentials", `{"credentials":{"customHeaders":{"Authorization":"Bearer at-synthetic-import","ChatGPT-Account-Id":"workspace-synthetic"}}}`, false},
		{"nested-metadata", `{"provider":"codex","metadata":{"custom_headers":{"Authorization":"Bearer at-synthetic-import","ChatGPT-Account-Id":"workspace-synthetic"}}}`, false},
		{"nested-auth-json", `{"tokens":{"access_token":"at-synthetic-import"},"custom_headers":{"ChatGPT-Account-Id":"workspace-synthetic"}}`, false},
		{"complementary-headers", `{"headers":{"Authorization":"Bearer at-synthetic-import"},"custom_headers":{"ChatGPT-Account-Id":"workspace-synthetic"}}`, false},
		{"canonical-headers", `{"headers":{"Authorization":"Bearer at-synthetic-import","ChatGPT-Account-Id":"workspace-synthetic"}}`, false},
		{"web-session", `{"session":{"accessToken":"at-synthetic-import","user":{"email":"fixture@example.test"},"custom_headers":{"ChatGPT-Account-Id":"workspace-synthetic"}}}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := ParseImport([]byte(test.raw))
			if err != nil || len(prepared.records) != 1 {
				t.Fatal("valid portable header import rejected", err)
			}
			c := prepared.records[0]
			if c.AccessToken != "at-synthetic-import" || c.AccountID != "workspace-synthetic" || (c.TokenSourceMode == "chatgpt_web_session") != test.web {
				t.Fatal("portable credential, workspace, or session boundary lost")
			}
			raw, err := json.Marshal(prepared.Preview())
			if err != nil || strings.Contains(string(raw), "at-synthetic-import") || strings.Contains(string(raw), "workspace-synthetic") {
				t.Fatal("preview disclosed credential header data", err)
			}
			if got := c.sdk().Metadata["account_id"]; got != "workspace-synthetic" {
				t.Fatal("workspace selector was discarded before SDK projection")
			}
		})
	}
}

func TestImportCustomHeadersRejectsAmbiguousOrMixedCredentials(t *testing.T) {
	for _, raw := range []string{
		`{"access_token":"synthetic","custom_headers":null}`,
		`{"access_token":"synthetic","customHeaders":[]}`,
		`{"access_token":"synthetic","custom_headers":{"ChatGPT-Account-Id":42}}`,
		`{"custom_headers":{"Authorization":"Basic synthetic"}}`,
		`{"custom_headers":{"Authorization":"Bearer sk-api-key"}}`,
		`{"custom_headers":{"Authorization":"Bearer first","Authorization":"Bearer second"}}`,
		`{"custom_headers":{"Authorization":"Bearer first","authorization":"Bearer first"}}`,
		`{"headers":{"Authorization":"Bearer first"},"custom_headers":{"Authorization":"Bearer first"}}`,
		`{"custom_headers":{"Authorization":"Bearer first"},"customHeaders":{"Authorization":"Bearer second"}}`,
		`{"access_token":"first","custom_headers":{"Authorization":"Bearer second"}}`,
		`{"access_token":"first","account_id":"workspace-first","customHeaders":{"ChatGPT-Account-Id":"workspace-second"}}`,
		`{"access_token":"first","custom_headers":{"ChatGPT-Account-Id":"workspace-first","chatgpt-account-id":"workspace-first"}}`,
		`{"access_token":"first","headers":{"ChatGPT-Account-Id":"workspace-first"},"customHeaders":{"ChatGPT-Account-Id":"workspace-second"}}`,
		`{"tokens":{"access_token":"first","account_id":"workspace-first"},"custom_headers":{"ChatGPT-Account-Id":"workspace-second"}}`,
		`{"tokens":{"access_token":"first"},"custom_headers":{"Authorization":"Bearer second"}}`,
		`{"access_token":"first","credentials":{"custom_headers":{"Authorization":"Bearer second"}}}`,
		`{"session":{"accessToken":"first"},"custom_headers":{"Authorization":"Bearer second"}}`,
		`{"accounts":[{"access_token":"first"}],"customHeaders":{"Authorization":"Bearer second"}}`,
		`{"access_token":"first","profile":{"custom_headers":{"Authorization":"Bearer second"}}}`,
	} {
		if prepared, err := ParseImport([]byte(raw)); err != ErrInvalidImport || prepared != nil {
			t.Fatal("ambiguous or mixed custom header import accepted")
		}
	}
}

func TestImportCustomHeadersDoesNotProjectArbitraryRequestHeaders(t *testing.T) {
	prepared, err := ParseImport([]byte(`{"access_token":"at-synthetic-import","custom_headers":{"ChatGPT-Account-Id":"workspace-synthetic","Host":"forbidden-host.example.test","Cookie":"synthetic-cookie-secret","X-Api-Key":"synthetic-api-key"},"customHeaders":{"X-Trace-Id":"synthetic-trace-id"}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []any{prepared.Preview(), prepared.records[0].sdk()} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"custom_headers", "customHeaders", "forbidden-host.example.test", "synthetic-cookie-secret", "synthetic-api-key", "synthetic-trace-id"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatal("arbitrary imported request header escaped normalization")
			}
		}
	}
}

func TestImportCredentialKindMarkersPreserveOAuthDefaults(t *testing.T) {
	for _, raw := range []string{
		`{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"access_token":"synthetic-oauth"}}`,
		`{"auth_mode":"oauth","openai_api_key":null,"agent_identity":null,"tokens":{"access_token":"synthetic-oauth"}}`,
		`{"access_token":"synthetic-oauth","OPENAI_API_KEY":"","openai_api_key":" \t ","agent_identity":null,"agentIdentity":""}`,
		`{"access_token":"synthetic-oauth","OPENAI_API_KEY":null,"openai_api_key":"","agent_identity":" \t ","agentIdentity":null}`,
		`{"credentials":{"access_token":"synthetic-oauth","openai_api_key":"","agentIdentity":null}}`,
		`{"tokens":{"access_token":"synthetic-oauth","OPENAI_API_KEY":null,"agent_identity":null},"openai_api_key":"","agentIdentity":""}`,
		`{"session":{"accessToken":"synthetic-oauth","agent_identity":null},"agentIdentity":""}`,
		`{"accounts":[{"access_token":"synthetic-oauth"}],"OPENAI_API_KEY":null,"openai_api_key":"","agent_identity":null}`,
		`{"access_token":"synthetic-oauth","profile":{"email":"fixture@example.test","openai_api_key":null,"agent_identity":null}}`,
	} {
		prepared, err := ParseImport([]byte(raw))
		if err != nil || len(prepared.records) != 1 || prepared.records[0].AccessToken != "synthetic-oauth" {
			t.Fatal("OAuth export defaults were mistaken for another credential kind", err)
		}
	}
}

func TestImportCredentialKindMarkersRejectMixedAndMalformedRecords(t *testing.T) {
	for _, key := range []string{"OPENAI_API_KEY", "openai_api_key", "agent_identity", "agentIdentity"} {
		for _, value := range []any{"synthetic-other-credential", true, 0, []any{}, map[string]any{}, map[string]any{"agent_runtime_id": "synthetic-agent", "agent_private_key": "synthetic-key"}} {
			record := map[string]any{"auth_mode": "oauth", "access_token": "synthetic-oauth", key: value}
			for _, outer := range []any{
				record,
				map[string]any{"tokens": record},
				map[string]any{"credentials": record},
				map[string]any{"metadata": record},
				map[string]any{"session": record},
				map[string]any{"accounts": []any{record}},
				map[string]any{"accounts": []any{map[string]any{"access_token": "synthetic-oauth"}}, key: value},
				map[string]any{"access_token": "synthetic-oauth", "profile": map[string]any{key: value}},
			} {
				raw, err := json.Marshal(outer)
				if err != nil {
					t.Fatal(err)
				}
				if prepared, err := ParseImport(raw); err != ErrInvalidImport || prepared != nil {
					t.Fatalf("mixed or malformed %s marker accepted", key)
				}
			}
		}
	}
	for _, raw := range []string{
		`{"access_token":"synthetic-oauth","OPENAI_API_KEY":null,"openai_api_key":"synthetic-api-key"}`,
		`{"access_token":"synthetic-oauth","OPENAI_API_KEY":"synthetic-api-key","openai_api_key":""}`,
		`{"access_token":"synthetic-oauth","agent_identity":null,"agentIdentity":{"agent_runtime_id":"synthetic-agent"}}`,
		`{"access_token":"synthetic-oauth","agent_identity":{"agent_runtime_id":"synthetic-agent"},"agentIdentity":""}`,
		`{"tokens":{"access_token":"synthetic-oauth","openai_api_key":"synthetic-api-key"},"OPENAI_API_KEY":null}`,
		`{"credentials":{"access_token":"synthetic-oauth","agentIdentity":null},"agent_identity":{"agent_runtime_id":"synthetic-agent"}}`,
		`{"auth_mode":"apikey","OPENAI_API_KEY":"","access_token":"synthetic-oauth"}`,
		`{"authMode":"agentIdentity","agent_identity":null,"access_token":"synthetic-oauth"}`,
		`{"access_token":"synthetic-oauth","openai_api_key":null,"openai_api_key":"synthetic-api-key"}`,
	} {
		if prepared, err := ParseImport([]byte(raw)); err != ErrInvalidImport || prepared != nil {
			t.Fatal("empty marker alias concealed a different credential kind")
		}
	}
}
