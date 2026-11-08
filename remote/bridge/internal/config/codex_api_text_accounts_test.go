package config

import (
	"path/filepath"
	"testing"
)

func TestNamedAPIClientTextAccountsValidateCloneAndFence(t *testing.T) {
	client := NamedAPIClient{
		ID: "client-text", Name: "Text client", AccountID: "account-anchor",
		AccountIDs:    []string{"account-text-a", "account-text-b"},
		PoolStrategy:  PoolStrategySingleAccount,
		PoolAccountID: "account-text-b",
		KeyDigest:     APIClientDigest("synthetic-text-client-key"),
	}
	if err := ValidateAPIClient(client); err != nil {
		t.Fatal(err)
	}
	clone := (Config{CodexAPI: CodexAPIService{Clients: []NamedAPIClient{client}}}).Clone()
	clone.CodexAPI.Clients[0].AccountIDs[0] = "mutated"
	if client.AccountIDs[0] != "account-text-a" {
		t.Fatal("text account membership leaked through Config.Clone")
	}
	for _, invalid := range [][]string{{"account-text-a", "account-text-a"}, {""}, {" bad"}} {
		copy := client
		copy.AccountIDs = invalid
		copy.PoolStrategy, copy.PoolAccountID = PoolStrategyAuto, ""
		if ValidateAPIClient(copy) == nil {
			t.Fatalf("invalid text account membership accepted: %q", invalid)
		}
	}
	for _, mutate := range []func(*NamedAPIClient){
		func(c *NamedAPIClient) { c.PoolStrategy = "AUTO"; c.PoolAccountID = "" },
		func(c *NamedAPIClient) { c.PoolStrategy = PoolStrategySingleAccount; c.PoolAccountID = "" },
		func(c *NamedAPIClient) {
			c.PoolStrategy = PoolStrategySingleAccount
			c.PoolAccountID = "account-anchor"
		},
		func(c *NamedAPIClient) { c.PoolStrategy = PoolStrategyAuto; c.PoolAccountID = "account-text-a" },
	} {
		copy := client
		mutate(&copy)
		if ValidateAPIClient(copy) == nil {
			t.Fatalf("invalid text pool policy accepted: %+v", copy)
		}
	}
	tooMany := client
	tooMany.PoolStrategy, tooMany.PoolAccountID = PoolStrategyAuto, ""
	tooMany.AccountIDs = make([]string, 65)
	for i := range tooMany.AccountIDs {
		tooMany.AccountIDs[i] = "text-member-" + string(rune('a'+i%26)) + string(rune('A'+i/26))
	}
	if ValidateAPIClient(tooMany) == nil {
		t.Fatal("unbounded text membership accepted")
	}

	store, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(c *Config) error {
		c.LocalAccounts = []LocalAccount{
			{ID: "account-anchor", SourceType: "api_key", Key: "anchor", BaseURL: "https://anchor.example/v1"},
			{ID: "account-text-a", SourceType: "api_key", Key: "text-a", BaseURL: "https://text.example/v1"},
			{ID: "account-text-b", SourceType: "api_key", Key: "text-b", BaseURL: "https://text.example/v1"},
			{ID: "account-unrelated", SourceType: "api_key", Key: "other", BaseURL: "https://other.example/v1"},
		}
		c.CodexAPI.Clients = []NamedAPIClient{client}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	base := store.Get().CodexAPI.Clients[0].AuthEpoch
	if err = store.Update(func(c *Config) error { c.LocalAccounts[3].Key = "other-rotated"; return nil }); err != nil {
		t.Fatal(err)
	}
	if store.Get().CodexAPI.Clients[0].AuthEpoch != base {
		t.Fatal("unrelated account rotation fenced text client")
	}
	if err = store.Update(func(c *Config) error { c.LocalAccounts[1].Key = "text-a-rotated"; return nil }); err != nil {
		t.Fatal(err)
	}
	memberEpoch := store.Get().CodexAPI.Clients[0].AuthEpoch
	if memberEpoch == base {
		t.Fatal("text member rotation did not fence client")
	}
	if err = store.Update(func(c *Config) error {
		c.CodexAPI.Clients[0].PoolStrategy = PoolStrategyAuto
		c.CodexAPI.Clients[0].PoolAccountID = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if store.Get().CodexAPI.Clients[0].AuthEpoch == memberEpoch {
		t.Fatal("text member policy change did not fence client")
	}
}

func TestNamedAPIClientEmptyTextPoolKeepsLegacyFixedSource(t *testing.T) {
	client := NamedAPIClient{ID: "legacy", Name: "Legacy", AccountID: "account-anchor", KeyDigest: APIClientDigest("legacy-key")}
	if err := ValidateAPIClient(client); err != nil {
		t.Fatal(err)
	}
	client.PoolStrategy = PoolStrategyAuto
	if ValidateAPIClient(client) == nil {
		t.Fatal("pool policy without explicit members was accepted")
	}
}
