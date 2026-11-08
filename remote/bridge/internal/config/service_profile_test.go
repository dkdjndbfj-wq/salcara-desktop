package config

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func serviceProfileStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetRuntimeGatewayKey("synthetic-profile-process")
	if err := store.Update(func(c *Config) error {
		c.LocalAccounts = []LocalAccount{{ID: "profile-account", Name: "Fixture", Kind: "api", BaseURL: "https://fixture.invalid", Key: "synthetic-upstream", Model: "gpt-fixture", Models: []string{"gpt-fixture"}}}
		c.CodexAPI.Enabled, c.CodexAPI.RuntimeLease = true, c.GatewayRuntimeKey
		c.CodexAPI.Clients = []NamedAPIClient{{ID: "profile-client", Name: "Fixture", AccountID: "profile-account", KeyDigest: APIClientDigest("synthetic-manual-key"), ModelPrefix: "team/", ModelAliases: map[string]string{"friendly": "gpt-fixture"}, ModelAllowlist: []string{"gpt-fixture"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestCodexServiceProfileProspectiveKeyNeedsRuntimeCommit(t *testing.T) {
	store := serviceProfileStore(t)
	before := store.Get()
	client := before.CodexAPI.Clients[0]
	key := before.CodexServiceProfileKeyFor(client, before.CodexAPI.AuthEpoch, "profile-lease")
	if !strings.HasPrefix(key, "sp-salcara-") || before.CodexServiceProfileCredential() != "" {
		t.Fatal("prospective key armed a runtime binding")
	}
	if _, ok := before.ResolveCodexServiceProfileClient(key); ok {
		t.Fatal("uncommitted key authenticated")
	}
	diskBefore, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatal(err)
	}
	path := store.path
	store.path = t.TempDir() // Runtime arm/revoke must not need writable settings.
	if !store.ArmCodexServiceProfile(client, before.CodexAPI.AuthEpoch, "profile-lease", "profile-generation", "team/friendly") {
		t.Fatal("current profile did not arm")
	}
	current := store.Get()
	if current.CodexServiceProfileCredential() != key || current.CodexServiceProfile.Generation != "profile-generation" || current.CodexServiceProfile.Model != "team/friendly" {
		t.Fatal("committed binding lost its exact disk generation or visible model")
	}
	if resolved, ok := current.ResolveCodexServiceProfileClient(key); !ok || !reflect.DeepEqual(resolved, client) {
		t.Fatal("committed key did not resolve its exact client")
	}
	if _, ok := current.ResolveCodexServiceProfileClient("synthetic-manual-key"); ok {
		t.Fatal("manual key fell back into service profile authentication")
	}
	if !reflect.DeepEqual(before.CodexAPI.Clients, current.CodexAPI.Clients) || !reflect.DeepEqual(before.ToolAPIApplied, current.ToolAPIApplied) {
		t.Fatal("service profile changed manual clients or Workbench ownership")
	}
	if store.RevokeCodexServiceProfile("") || store.RevokeCodexServiceProfile("another-lease") || !store.RevokeCodexServiceProfile("profile-lease") || store.Get().CodexServiceProfileCredential() != "" {
		t.Fatal("conditional runtime revocation failed")
	}
	store.path = path
	diskAfter, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(diskBefore, diskAfter) {
		t.Fatal("runtime authorization wrote persistent settings")
	}
}

func TestCodexServiceProfileStoreOwnershipAndConditionalRevoke(t *testing.T) {
	store := serviceProfileStore(t)
	c := store.Get()
	client := c.CodexAPI.Clients[0]
	if !store.ArmCodexServiceProfile(client, c.CodexAPI.AuthEpoch, "first-lease", "first-generation", "team/friendly") {
		t.Fatal("initial arm failed")
	}
	first := store.Get()
	if !store.ArmCodexServiceProfile(client, c.CodexAPI.AuthEpoch, "second-lease", "second-generation", "team/gpt-fixture") {
		t.Fatal("replacement arm failed")
	}
	second := store.Get()
	if store.RevokeCodexServiceProfile("first-lease") || second.CodexServiceProfileCredential() == first.CodexServiceProfileCredential() {
		t.Fatal("old apply owned a newer binding")
	}
	if err := store.Update(func(next *Config) error {
		next.CodexServiceProfile = first.CodexServiceProfile
		next.CodexModel = "unrelated-workbench-choice"
		return nil
	}); err != nil || store.Get().CodexServiceProfile != second.CodexServiceProfile {
		t.Fatal("ordinary Update replaced runtime profile ownership")
	}
	raw, err := os.ReadFile(store.Path())
	if err != nil || bytes.Contains(raw, []byte("second-lease")) || bytes.Contains(raw, []byte("second-generation")) || bytes.Contains(raw, []byte("CodexServiceProfile")) {
		t.Fatal("process-local profile was persisted")
	}
	reopened, err := Open(store.Path())
	if err != nil || reopened.Get().CodexServiceProfile != (ServiceProfileBinding{}) {
		t.Fatal("profile authorization survived reopening persistent settings")
	}
}

func TestCodexServiceProfileRejectsStaleSnapshotsAndInvalidMetadata(t *testing.T) {
	for _, variant := range []string{"client", "service", "lease", "generation", "model", "namespace", "disabled", "images-only", "runtime"} {
		t.Run(variant, func(t *testing.T) {
			store := serviceProfileStore(t)
			c := store.Get()
			client, epoch, lease, generation, model := c.CodexAPI.Clients[0], c.CodexAPI.AuthEpoch, "lease", "generation", "team/friendly"
			switch variant {
			case "client":
				client.ModelAliases["friendly"] = "gpt-other"
			case "service":
				epoch = "stale-service-epoch"
			case "lease":
				lease = ""
			case "generation":
				generation = ""
			case "model":
				model = "team/gpt-forbidden"
			case "namespace":
				model = "friendly"
			case "disabled":
				if _, ok := store.RetireCodexAPIClientRuntime(client); !ok {
					t.Fatal("fixture retirement failed")
				}
			case "images-only":
				if err := store.Update(func(next *Config) error { next.CodexAPI.ImagesMode = "images_only"; return nil }); err != nil {
					t.Fatal(err)
				}
				c = store.Get()
				epoch = c.CodexAPI.AuthEpoch
			case "runtime":
				store.SetRuntimeGatewayKey("replacement-process")
			}
			if store.ArmCodexServiceProfile(client, epoch, lease, generation, model) || store.Get().CodexServiceProfileCredential() != "" {
				t.Fatal("invalid or stale profile armed")
			}
		})
	}
}

func TestCodexServiceProfileLifecycleInvalidationCannotRevive(t *testing.T) {
	for _, variant := range []string{"stop", "client-disable", "client-rotate", "account-credential", "official-account-credential", "restart"} {
		t.Run(variant, func(t *testing.T) {
			store := serviceProfileStore(t)
			if variant == "official-account-credential" {
				if err := store.Update(func(next *Config) error {
					next.LocalAccounts[0].CredentialRef = "synthetic-vault-reference"
					next.LocalAccounts[0].CredentialEpoch = 1
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			c := store.Get()
			client := c.CodexAPI.Clients[0]
			if !store.ArmCodexServiceProfile(client, c.CodexAPI.AuthEpoch, "old-lease", "old-generation", "team/friendly") {
				t.Fatal("fixture arm failed")
			}
			armed := store.Get()
			switch variant {
			case "stop":
				store.RetireCodexAPIServiceRuntime(c.CodexAPI.AuthEpoch)
			case "client-disable":
				store.RetireCodexAPIClientRuntime(client)
			case "restart":
				store.SetRuntimeGatewayKey("next-synthetic-process")
			default:
				if err := store.Update(func(next *Config) error {
					if variant == "client-rotate" {
						next.CodexAPI.Clients[0].KeyDigest = APIClientDigest("replacement-synthetic-key")
					} else if variant == "official-account-credential" {
						next.LocalAccounts[0].CredentialEpoch++
					} else {
						next.LocalAccounts[0].Key = "replacement-synthetic-upstream"
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if next := store.Get(); next.CodexServiceProfileCredential() != "" || next.CodexServiceProfile != (ServiceProfileBinding{}) {
				t.Fatal("authorization transition retained old profile")
			}
			if err := store.Update(func(next *Config) error {
				next.CodexAPI.Enabled, next.CodexAPI.RuntimeLease = true, next.GatewayRuntimeKey
				next.CodexAPI.Clients = armed.CodexAPI.Clients
				next.LocalAccounts = armed.LocalAccounts
				next.CodexServiceProfile = armed.CodexServiceProfile
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if next := store.Get(); next.CodexServiceProfileCredential() != "" {
				t.Fatal("restoring earlier settings revived an old profile")
			}
			if store.ArmCodexServiceProfile(client, c.CodexAPI.AuthEpoch, "old-lease", "old-generation", "team/friendly") {
				t.Fatal("captured pre-revocation snapshot rearmed")
			}
		})
	}
}
