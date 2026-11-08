package config

import (
	"path/filepath"
	"testing"
)

func TestNamedAPIClientImageAccountsValidateCloneAndFence(t *testing.T) {
	client := NamedAPIClient{
		ID: "client-image", Name: "Image client", AccountID: "account-main",
		ImageAccountIDs: []string{"account-image-a", "account-image-b"},
		KeyDigest:       APIClientDigest("synthetic-image-client-key"),
	}
	if err := ValidateAPIClient(client); err != nil {
		t.Fatal(err)
	}
	clone := (Config{CodexAPI: CodexAPIService{Clients: []NamedAPIClient{client}}}).Clone()
	clone.CodexAPI.Clients[0].ImageAccountIDs[0] = "mutated"
	if client.ImageAccountIDs[0] != "account-image-a" {
		t.Fatal("image account membership leaked through Config.Clone")
	}
	for _, invalid := range [][]string{{"account-image-a", "account-image-a"}, {""}, {" bad"}} {
		copy := client
		copy.ImageAccountIDs = invalid
		if ValidateAPIClient(copy) == nil {
			t.Fatalf("invalid image account membership accepted: %q", invalid)
		}
	}
	tooMany := client
	tooMany.ImageAccountIDs = make([]string, 65)
	for i := range tooMany.ImageAccountIDs {
		tooMany.ImageAccountIDs[i] = "image-member-" + string(rune('a'+i%26)) + string(rune('A'+i/26))
	}
	if ValidateAPIClient(tooMany) == nil {
		t.Fatal("unbounded image membership accepted")
	}

	store, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(func(c *Config) error {
		c.LocalAccounts = []LocalAccount{
			{ID: "account-main", SourceType: "api_key", Key: "main", BaseURL: "https://main.example/v1"},
			{ID: "account-image-a", SourceType: "api_key", Key: "image-a", BaseURL: "https://image.example/v1"},
			{ID: "account-image-b", SourceType: "api_key", Key: "image-b", BaseURL: "https://image.example/v1"},
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
		t.Fatal("unrelated account rotation fenced image client")
	}
	if err = store.Update(func(c *Config) error { c.LocalAccounts[1].Key = "image-a-rotated"; return nil }); err != nil {
		t.Fatal(err)
	}
	memberEpoch := store.Get().CodexAPI.Clients[0].AuthEpoch
	if memberEpoch == base {
		t.Fatal("image member rotation did not fence client")
	}
	if err = store.Update(func(c *Config) error { c.CodexAPI.Clients[0].ImageAccountIDs = []string{"account-image-b"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if store.Get().CodexAPI.Clients[0].AuthEpoch == memberEpoch {
		t.Fatal("image member policy change did not fence client")
	}
}
