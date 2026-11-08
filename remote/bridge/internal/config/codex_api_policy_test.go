package config

import "testing"

func TestClientPolicyValidationAndSnapshots(t *testing.T) {
	client := NamedAPIClient{ID: "client-fixture", Name: "Fixture", AccountID: "account-fixture", KeyDigest: APIClientDigest("synthetic-client"), ModelAllowlist: []string{"allowed", "denied"}, ModelDenylist: []string{"denied"}}
	if err := ValidateAPIClient(client); err != nil {
		t.Fatal(err)
	}
	if !APIClientModelAllowed(client, "allowed") || APIClientModelAllowed(client, "denied") || APIClientModelAllowed(client, "unlisted") {
		t.Fatal("client allow/deny policy incorrect")
	}
	c := Config{CodexAPI: CodexAPIService{Clients: []NamedAPIClient{client}}}
	clone := c.Clone()
	clone.CodexAPI.Clients[0].ModelAllowlist[0] = "changed"
	clone.CodexAPI.Clients[0].ModelDenylist[0] = "changed"
	if c.CodexAPI.Clients[0].ModelAllowlist[0] != "allowed" || c.CodexAPI.Clients[0].ModelDenylist[0] != "denied" {
		t.Fatal("client permissions leaked through config snapshot")
	}
	for _, models := range [][]string{{"duplicate", "duplicate"}, {" with-space"}, {"bad\nmodel"}, {""}} {
		client.ModelAllowlist = models
		if ValidateAPIClient(client) == nil {
			t.Fatalf("invalid permission accepted: %q", models)
		}
	}
}
