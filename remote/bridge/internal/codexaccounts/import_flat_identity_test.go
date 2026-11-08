package codexaccounts

import (
	"encoding/json"
	"testing"
)

func TestImportRejectsFlattenedAgentIdentityAcrossCredentialShapes(t *testing.T) {
	for _, key := range []string{"agent_runtime_id", "agentRuntimeId", "agent_private_key", "agentPrivateKey"} {
		for _, material := range []any{"synthetic-agent-material", true, 42, []any{}, map[string]any{}} {
			record := map[string]any{"access_token": "synthetic-oauth-access", key: material}
			for _, shape := range []struct {
				name string
				data any
			}{
				{"flat", record},
				{"tokens", map[string]any{"tokens": record}},
				{"credentials", map[string]any{"credentials": record}},
				{"metadata", map[string]any{"metadata": record}},
				{"session", map[string]any{"session": record}},
				{"accounts-record", map[string]any{"accounts": []any{record}}},
				{"accounts-wrapper", map[string]any{"accounts": []any{map[string]any{"access_token": "synthetic-oauth-access"}}, key: material}},
				{"profile", map[string]any{"access_token": "synthetic-oauth-access", "profile": map[string]any{key: material}}},
			} {
				raw, err := json.Marshal(shape.data)
				if err != nil {
					t.Fatal(err)
				}
				if prepared, err := ParseImport(raw); err != ErrInvalidImport || prepared != nil {
					t.Fatalf("%s accepted flattened identity field %s as OAuth", shape.name, key)
				}
			}
		}
	}
}

func TestImportFlattenedAgentIdentityEmptyAliasCannotHideMaterial(t *testing.T) {
	for _, raw := range []string{
		`{"access_token":"synthetic-oauth-access","agent_runtime_id":null,"agentRuntimeId":"synthetic-runtime"}`,
		`{"access_token":"synthetic-oauth-access","agent_runtime_id":"synthetic-runtime","agentRuntimeId":""}`,
		`{"access_token":"synthetic-oauth-access","agent_private_key":"","agentPrivateKey":"synthetic-private-key"}`,
		`{"access_token":"synthetic-oauth-access","agent_private_key":"synthetic-private-key","agentPrivateKey":null}`,
		`{"credentials":{"access_token":"synthetic-oauth-access","agentRuntimeId":null},"agent_runtime_id":"synthetic-runtime"}`,
		`{"agentPrivateKey":"","tokens":{"access_token":"synthetic-oauth-access","agent_private_key":"synthetic-private-key"}}`,
		`{"session_json":"{\"accessToken\":\"synthetic-oauth-access\",\"agent_runtime_id\":\"synthetic-runtime\",\"agent_private_key\":\"synthetic-private-key\"}"}`,
	} {
		if prepared, err := ParseImport([]byte(raw)); err != ErrInvalidImport || prepared != nil {
			t.Fatal("empty identity alias concealed populated identity material")
		}
	}
}

func TestImportFlattenedAgentIdentityDefaultsPreserveOAuthExports(t *testing.T) {
	for _, raw := range []string{
		`{"auth_mode":"oauth","access_token":"synthetic-oauth-access","agent_runtime_id":null,"agentRuntimeId":"","agent_private_key":" \t ","agentPrivateKey":null}`,
		`{"agent_runtime_id":"","tokens":{"access_token":"synthetic-oauth-access","agentRuntimeId":null,"agent_private_key":"","agentPrivateKey":null}}`,
		`{"credentials":{"access_token":"synthetic-oauth-access","agent_runtime_id":null,"agentPrivateKey":""}}`,
		`{"session":{"accessToken":"synthetic-oauth-access","agentRuntimeId":"","agent_private_key":null}}`,
		`{"accounts":[{"access_token":"synthetic-oauth-access","agent_runtime_id":null}],"agentPrivateKey":""}`,
		`{"access_token":"synthetic-oauth-access","profile":{"email":"fixture@example.test","agent_runtime_id":"","agent_private_key":null}}`,
	} {
		prepared, err := ParseImport([]byte(raw))
		if err != nil || len(prepared.records) != 1 || prepared.records[0].AccessToken != "synthetic-oauth-access" {
			t.Fatal("empty identity placeholders rejected an OAuth export", err)
		}
	}
}
