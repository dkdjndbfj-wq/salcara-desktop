package console

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

func listedAccountTags(t *testing.T, s *Server, h http.Handler, id, officialID string) accountTagDisplay {
	t.Helper()
	w := localRequest(s, h, "GET", "/api/local/accounts", nil)
	var out struct {
		Accounts []struct {
			ID          string
			TagMetadata accountTagDisplay
		}
		OfficialAccountTags map[string]accountTagDisplay
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatal("account tags not readable")
	}
	if officialID != "" {
		return out.OfficialAccountTags[officialID]
	}
	for _, row := range out.Accounts {
		if row.ID == id {
			return row.TagMetadata
		}
	}
	t.Fatal("missing account metadata")
	return accountTagDisplay{}
}

func TestAccountTagsPureMetadataAndCAS(t *testing.T) {
	s, h, _, id, clientID := memberRevisionFixture(t)
	before := s.d.Store.Get()
	a, _ := before.LocalAccount(id)
	metadata := listedAccountTags(t, s, h, id, "")
	body := map[string]any{"id": id, "kind": "local", "tags": []string{" 常用 ", "开发", "常用"}, "expectedSourceRevision": before.AccountSourceRevision(a), "expectedTagsRevision": metadata.Revision}
	w := localRequest(s, h, "POST", "/api/local/accounts/tags", body)
	if w.Code != 200 {
		t.Fatalf("tags: %d %s", w.Code, w.Body)
	}
	after := s.d.Store.Get()
	if !reflect.DeepEqual(before.LocalAccounts, after.LocalAccounts) || !reflect.DeepEqual(before.CodexAPI, after.CodexAPI) || before.UsageSourceRevision != after.UsageSourceRevision || !reflect.DeepEqual(before.ToolAPIApplied, after.ToolAPIApplied) {
		t.Fatal("tag edit changed source, credentials, memberships or launch authorization")
	}
	oldClient, _ := before.APIClient(clientID)
	nextClient, _ := after.APIClient(clientID)
	if apiClientRevision(before, oldClient) != apiClientRevision(after, nextClient) || before.AccountSourceRevision(a) != after.AccountSourceRevision(a) {
		t.Fatal("metadata invalidated source or membership revision")
	}
	next := listedAccountTags(t, s, h, id, "")
	if !reflect.DeepEqual(next.Tags, []string{"常用", "开发"}) || next.Revision == metadata.Revision {
		t.Fatal("tags not normalized or versioned")
	}
	if stale := localRequest(s, h, "POST", "/api/local/accounts/tags", body); stale.Code != http.StatusConflict {
		t.Fatal("stale metadata overwritten")
	}
	body["tags"] = []string{}
	body["expectedTagsRevision"] = next.Revision
	if clear := localRequest(s, h, "POST", "/api/local/accounts/tags", body); clear.Code != 200 {
		t.Fatalf("clear: %d", clear.Code)
	}
	if len(listedAccountTags(t, s, h, id, "").Tags) != 0 {
		t.Fatal("tags not cleared")
	}
	// Even an A -> B -> A tag cycle cannot accept the earlier confirmation.
	body["expectedTagsRevision"] = metadata.Revision
	if stale := localRequest(s, h, "POST", "/api/local/accounts/tags", body); stale.Code != http.StatusConflict {
		t.Fatal("metadata ABA accepted")
	}
}

func TestAccountTagsStrictValidationAndReplacementFence(t *testing.T) {
	s, h, id := sourceRevisionFixture(t)
	c := s.d.Store.Get()
	a, _ := c.LocalAccount(id)
	metadata := listedAccountTags(t, s, h, id, "")
	base := map[string]any{"id": id, "kind": "local", "tags": []string{"开发"}, "expectedSourceRevision": c.AccountSourceRevision(a), "expectedTagsRevision": metadata.Revision}
	for _, change := range []map[string]any{
		{"tags": nil}, {"tags": []string{""}}, {"tags": []string{"a\nb"}}, {"tags": []string{"a\u202eb"}}, {"tags": []string{"a,b"}}, {"tags": []string{"1234567890123456789012345"}},
		{"expectedSourceRevision": ""}, {"expectedTagsRevision": ""}, {"key": "must-not-save"}, {"identityEpoch": 9}, {"kind": "unsupported"},
	} {
		body := make(map[string]any)
		for key, value := range base {
			body[key] = value
		}
		for key, value := range change {
			body[key] = value
		}
		w := localRequest(s, h, "POST", "/api/local/accounts/tags", body)
		if w.Code != 400 && w.Code != 409 {
			t.Fatalf("accepted invalid tags: %v (%d)", change, w.Code)
		}
	}
	if err := s.d.Store.Update(func(c *config.Config) error {
		for i := range c.LocalAccounts {
			if c.LocalAccounts[i].ID == id {
				c.LocalAccounts[i].Key = "replaced-synthetic-key"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := localRequest(s, h, "POST", "/api/local/accounts/tags", base); w.Code != 409 {
		t.Fatal("same-ID source replacement accepted")
	}
	if len(s.d.Store.Get().AccountTags) != 0 {
		t.Fatal("invalid/stale tags persisted")
	}
}

func TestOfficialAccountTagsDoNotAttachAndStayWithIdentity(t *testing.T) {
	s, h := officialConsole(t)
	s.d.Store.SetRuntimeGatewayKey("synthetic-tags-runtime")
	p, err := codexaccounts.ParseImport([]byte(officialFixture))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.d.CodexAccounts.Import(context.Background(), p, "")
	if err != nil {
		t.Fatal(err)
	}
	row := rows[0]
	before, _ := s.d.CodexAccounts.Get(row.ID)
	metadata := listedAccountTags(t, s, h, "", row.ID)
	body := map[string]any{"id": row.ID, "kind": "official", "identityEpoch": row.IdentityEpoch, "tags": []string{"未关联"}, "expectedTagsRevision": metadata.Revision}
	w := localRequest(s, h, "POST", "/api/local/accounts/tags", body)
	if w.Code != 200 {
		t.Fatalf("official tags: %d %s", w.Code, w.Body)
	}
	after, _ := s.d.CodexAccounts.Get(row.ID)
	if !reflect.DeepEqual(before, after) || len(s.d.Store.Get().LocalAccounts) != 0 || len(s.d.Store.Get().CodexAPI.Clients) != 0 {
		t.Fatal("tag metadata changed protected account or attached it")
	}
	if got := listedAccountTags(t, s, h, "", row.ID); !reflect.DeepEqual(got.Tags, []string{"未关联"}) {
		t.Fatal("vault-only tag projection missing")
	}
	body["identityEpoch"] = row.IdentityEpoch + 1
	if w := localRequest(s, h, "POST", "/api/local/accounts/tags", body); w.Code != 409 {
		t.Fatal("wrong official identity accepted")
	}
	c := s.d.Store.Get()
	if got := accountTagsDisplay(c, "official:"+row.ID, row.IdentityEpoch+1, ""); len(got.Tags) != 0 {
		t.Fatal("old identity tags leaked to replacement")
	}
	assertNoOfficialSecrets(t, w.Body.String())
}

func TestAccountTagMetadataDeepCopy(t *testing.T) {
	c := config.Config{AccountTags: map[string]config.AccountTagMetadata{"local:a": {Tags: []string{"a"}, Revision: 1}}}
	clone := c.Clone()
	row := clone.AccountTags["local:a"]
	row.Tags[0] = "changed"
	clone.AccountTags["other"] = row
	if c.AccountTags["local:a"].Tags[0] != "a" || len(c.AccountTags) != 1 {
		t.Fatal("metadata escaped Store snapshot")
	}
}

func TestAPIMemberReceiptSuppliesExactCommittedRevision(t *testing.T) {
	s, h, _, id, clientID := memberRevisionFixture(t)
	body := memberRevisionRequest(t, s, h, id, clientID)
	w := localRequest(s, h, "POST", "/api/local/codex-api/members", body)
	var receipt struct{ ClientRevision string }
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &receipt) != nil {
		t.Fatal("missing member receipt")
	}
	c := s.d.Store.Get()
	client, _ := c.APIClient(clientID)
	if receipt.ClientRevision == "" || receipt.ClientRevision != apiClientRevision(c, client) || receipt.ClientRevision == body["expectedClientRevision"] {
		t.Fatal("receipt is not committed revision")
	}
	body["enabled"] = true
	body["expectedClientRevision"] = receipt.ClientRevision
	if next := localRequest(s, h, "POST", "/api/local/codex-api/members", body); next.Code != 200 {
		t.Fatalf("cannot chain CAS: %d %s", next.Code, next.Body)
	}
}
