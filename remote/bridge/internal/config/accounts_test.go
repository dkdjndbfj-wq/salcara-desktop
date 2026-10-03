package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestAPIBasePreservesPrefixes(t *testing.T) {
	for _, tc := range []struct{ in, root, v1 string }{
		{" salcara.top/v1/ ", "https://salcara.top", "https://salcara.top/v1"},
		{"http://localhost:8080/gateway/v1", "http://localhost:8080/gateway", "http://localhost:8080/gateway/v1"},
		{"https://example.com/proxy", "https://example.com/proxy", "https://example.com/proxy/v1"},
	} {
		root, v1, err := APIBase(tc.in)
		if err != nil || root != tc.root || v1 != tc.v1 {
			t.Fatalf("%q => %q %q %v", tc.in, root, v1, err)
		}
	}
	for _, in := range []string{"", "ftp://example.com", "https://key:secret@example.com", "https://example.com?key=secret", "https://example.com#secret"} {
		if _, _, err := APIBase(in); err == nil {
			t.Errorf("accepted unsafe URL %q", in)
		}
	}
}

func TestLocalAccountValidation(t *testing.T) {
	base := LocalAccount{ID: NewUUID(), Name: "我的 API", Kind: "codex", BaseURL: "salcara.top/v1", Key: "test-key", Model: "coding-model"}
	if err := ValidateLocalAccount(&base); err != nil {
		t.Fatal(err)
	}
	if base.BaseURL != "https://salcara.top" || base.AuthMode != "bearer" {
		t.Fatal(base)
	}
	for _, id := range []string{"..", "../escape", `a\b`, "a:b", ""} {
		a := base
		a.ID = id
		if ValidateLocalAccount(&a) == nil {
			t.Errorf("accepted id %q", id)
		}
	}
	a := base
	a.Target = "claude-desktop"
	if ValidateLocalAccount(&a) == nil {
		t.Fatal("accepted mismatched target")
	}
	a = base
	a.Model = "model\ninjection"
	if ValidateLocalAccount(&a) == nil {
		t.Fatal("accepted multiline model")
	}
}

func TestLocalMigrationAndClone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := Config{RelayRoot: "https://relay.test", AccountKey: "remote-key", CodexKey: "cx-key", ClaudeKey: "cl-key", CodexModel: "cx-model", ClaudeModel: "cl-model"}
	b, _ := json.Marshal(old)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	c := st.Get()
	if len(c.LocalAccounts) != 2 || !c.LocalAccountsReady || c.AccountKey != old.AccountKey {
		t.Fatal("migration lost remote credentials", c)
	}
	cx, ok := c.ActiveLocalAccount("codex")
	if !ok || cx.Key != "cx-key" || cx.Model != "cx-model" {
		t.Fatal(cx)
	}
	cl, ok := c.ActiveLocalAccount("claude")
	if !ok || cl.Key != "cl-key" {
		t.Fatal(cl)
	}
	if err := st.Update(func(c *Config) error {
		c.LocalAccounts[0].Models = []string{"one"}
		c.LocalToolPaths = map[string]string{"codex": "original"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	copy := st.Get()
	copy.LocalAccounts[0].Models[0] = "changed"
	copy.LocalToolPaths["codex"] = "changed"
	if st.Get().LocalAccounts[0].Models[0] != "one" || st.Get().LocalToolPaths["codex"] != "original" {
		t.Fatal("shallow clone")
	}
	if err := st.Update(func(c *Config) error {
		c.LocalAccounts = nil
		c.ActiveCodexAccount = ""
		c.ActiveClaudeAccount = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Get().LocalAccounts) != 0 {
		t.Fatal("deleted migrated accounts reappeared")
	}
}
