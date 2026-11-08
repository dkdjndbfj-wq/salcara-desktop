package config

import "testing"

func TestClientPrefixesAreLiteralDenyFirstAndCloned(t *testing.T) {
	c := NamedAPIClient{ID: "client-fixture", Name: "Fixture", AccountID: "account-fixture", KeyDigest: APIClientDigest("synthetic"),
		ModelAllowlist: []string{"other-exact", "gpt-blocked-exact"}, ModelAllowPrefixes: []string{"gpt-"},
		ModelDenylist: []string{"gpt-denied"}, ModelDenyPrefixes: []string{"gpt-blocked"}}
	if err := ValidateAPIClient(c); err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]bool{"gpt-next": true, "other-exact": true, "gpt-denied": false, "gpt-blocked-exact": false, "other": false, "GPT-next": false, "gpt": false, "": false, " gpt-next": false} {
		if got := APIClientModelAllowed(c, model); got != want {
			t.Errorf("%q: got %v want %v", model, got, want)
		}
	}
	clone := (Config{CodexAPI: CodexAPIService{Clients: []NamedAPIClient{c}}}).Clone()
	clone.CodexAPI.Clients[0].ModelAllowPrefixes[0] = "other-"
	clone.CodexAPI.Clients[0].ModelDenyPrefixes[0] = "other-"
	if c.ModelAllowPrefixes[0] != "gpt-" || c.ModelDenyPrefixes[0] != "gpt-blocked" {
		t.Fatal("snapshot changed original prefix rules")
	}
	c.ModelAllowlist, c.ModelAllowPrefixes = nil, nil
	if !APIClientModelAllowed(c, "other") || APIClientModelAllowed(c, "gpt-blocked") {
		t.Fatal("deny-only rules must keep other models available")
	}
	for _, bad := range [][]string{{""}, {"gpt-*"}, {"?"}, {"gpt-[x]"}, {" gpt-"}, {"gpt-", "gpt-"}, {"bad\n"}} {
		c.ModelAllowPrefixes = bad
		if ValidateAPIClient(c) == nil {
			t.Errorf("invalid allow prefix accepted: %q", bad)
		}
		c.ModelAllowPrefixes, c.ModelDenyPrefixes = nil, bad
		if ValidateAPIClient(c) == nil {
			t.Errorf("invalid deny prefix accepted: %q", bad)
		}
	}
}
