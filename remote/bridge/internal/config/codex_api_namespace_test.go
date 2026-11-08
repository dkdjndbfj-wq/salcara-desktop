package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func namespaceFixtureClient() NamedAPIClient {
	return NamedAPIClient{
		ID:             "namespace-client",
		Name:           "Namespace fixture",
		AccountID:      "namespace-account",
		KeyDigest:      APIClientDigest("synthetic-namespace-key"),
		ModelPrefix:    "team-a/",
		ModelAllowlist: []string{"gpt-actual", "second-alias"},
		ModelDenylist:  []string{"gpt-denied"},
		ModelAliases: map[string]string{
			"friendly":     "gpt-actual",
			"first-alias":  "second-alias",
			"second-alias": "gpt-denied",
			"escape":       "gpt-denied",
		},
	}
}

func TestResolveAPIClientModelRequiresExactNamespaceAndResolvesAliasOnce(t *testing.T) {
	client := namespaceFixtureClient()
	for visible, want := range map[string]string{
		"team-a/gpt-actual":  "gpt-actual",
		"team-a/friendly":    "gpt-actual",
		"team-a/first-alias": "second-alias",
	} {
		if got, ok := ResolveAPIClientModel(client, visible); !ok || got != want {
			t.Fatalf("ResolveAPIClientModel(%q) = %q, %v; want %q, true", visible, got, ok, want)
		}
	}
	for _, visible := range []string{
		"gpt-actual",                 // missing assigned namespace
		"team-b/gpt-actual",          // another client's namespace
		"team-a/",                    // stripping must not create an empty ID
		"team-a/team-a/gpt-actual",   // a doubled public namespace is invalid
		"team-a/escape",              // ACL applies to the denied alias target
		"team-a/second-alias",        // alias resolves once to a denied target
		" team-a/gpt-actual",         // surrounding whitespace is not normalized
		"team-a/gpt-actual\nignored", // control characters are invalid
	} {
		if got, ok := ResolveAPIClientModel(client, visible); ok || got != "" {
			t.Fatalf("unsupported or denied model %q resolved to %q", visible, got)
		}
	}
}

func TestAPIClientVisibleModelAppliesNamespaceWithoutChoosingAlias(t *testing.T) {
	client := namespaceFixtureClient()
	for publicName, want := range map[string]string{"gpt-actual": "team-a/gpt-actual", "friendly": "team-a/friendly"} {
		if got, ok := APIClientVisibleModel(client, publicName); !ok || got != want {
			t.Fatalf("APIClientVisibleModel(%q) = %q, %v; want %q, true", publicName, got, ok, want)
		}
	}
	for _, publicName := range []string{"", "team-a/friendly", strings.Repeat("x", 200)} {
		if got, ok := APIClientVisibleModel(client, publicName); ok || got != "" {
			t.Fatalf("invalid public model %q became %q", publicName, got)
		}
	}
}

func TestValidateAPIClientModelPrefix(t *testing.T) {
	base := namespaceFixtureClient()
	if err := ValidateAPIClient(base); err != nil {
		t.Fatal(err)
	}
	for _, prefix := range []string{"/", "team", " team/", "team//", "team*/", strings.Repeat("x", 64) + "/"} {
		client := base
		client.ModelPrefix = prefix
		if ValidateAPIClient(client) == nil {
			t.Fatalf("invalid model namespace accepted: %q", prefix)
		}
	}
	client := base
	client.ModelAliases = map[string]string{"team-a/friendly": "gpt-actual"}
	if ValidateAPIClient(client) == nil {
		t.Fatal("alias containing the assigned public namespace was accepted")
	}
}

func TestCodexAPIImageSettingsFenceServiceAuthorization(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	initial := store.Get().CodexAPI.AuthEpoch
	if err := store.Update(func(c *Config) error {
		c.CodexAPI.ImagesMode = "openai"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	afterMode := store.Get().CodexAPI.AuthEpoch
	if afterMode == initial {
		t.Fatal("image mode change did not fence existing service authorization")
	}
	if err := store.Update(func(c *Config) error {
		c.CodexAPI.MaxConcurrentImageRequests = 2
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if store.Get().CodexAPI.AuthEpoch == afterMode {
		t.Fatal("image concurrency change did not fence existing service authorization")
	}
}
