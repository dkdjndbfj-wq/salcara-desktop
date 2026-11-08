package console

import (
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestDefaultAPIAccountNameUsesOfflineSiteIdentity(t *testing.T) {
	for _, tc := range []struct{ base, want string }{
		{"https://api.salcara.top/v1", "salcara"},
		{"  API.EXAMPLE.COM/v1  ", "example"},
		{"https://www.api.example.co.uk/prefix/v1", "example"},
		{"https://api.eu.example.co.uk:8443/v1", "eu.example:8443"},
		{"https://api.company.github.io/v1", "company"},
		{"https://api.github.io/v1", "api"},
		{"https://www.example.com.cn", "example"},
		{"https://api.example.invalid/v1", "example.invalid"},
		{"https://co.uk", "co.uk"},
		{"http://localhost:47831/v1", "localhost:47831"},
		{"http://192.0.2.1:8080/v1", "192.0.2.1:8080"},
		{"http://[2001:db8::1]:8080/v1", "[2001:db8::1]:8080"},
		{"http://[::1]/v1", "[::1]"},
		{"https://api.example.com.:443/v1", "example:443"},
		{"https://gateway.example.com/secret-looking-path", "example"},
	} {
		t.Run(tc.base, func(t *testing.T) {
			got, err := defaultAPIAccountName(tc.base)
			if err != nil || got != tc.want {
				t.Fatalf("name = %q, err = %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, invalid := range []string{"", "https://user:secret@example.com/v1", "https://example.com?key=secret", "file:///private/key"} {
		if got, err := defaultAPIAccountName(invalid); err == nil || got != "" {
			t.Fatal("invalid or credential-bearing URL produced a display name")
		}
	}
	long := strings.Repeat("a", 50) + "." + strings.Repeat("b", 50)
	got, err := defaultAPIAccountName("https://" + long + ".example.com:9443")
	if err != nil || len([]rune(got)) > 80 || !strings.HasSuffix(got, ":9443") {
		t.Fatal("long generated name lost the account name limit or port")
	}
}

func TestDefaultAPIAccountNameDoesNotRenameExplicitOrOfficialAccounts(t *testing.T) {
	for _, a := range []config.LocalAccount{
		{Name: "  我的备用 Key  ", BaseURL: "https://different.example.com"},
		{Name: "", SourceType: "official_account", BaseURL: "https://chatgpt.com/backend-api/codex"},
		{Name: "", CredentialRef: "protected-reference", BaseURL: "https://chatgpt.com/backend-api/codex"},
	} {
		before := a.Name
		if err := applyDefaultAPIAccountName(&a); err != nil || a.Name != before {
			t.Fatal("default API name overwrote explicit or official identity")
		}
	}
}

func TestLocalSaveDefaultsBlankNameWithoutOverwritingCustomName(t *testing.T) {
	s, h := newTestServer(t)
	id := createLocal(t, s, h, "api", " \t ", "synthetic-name-key", "https://api.example.co.uk/v1")
	a, ok := s.d.Store.Get().LocalAccount(id)
	if !ok || a.Name != "example" {
		t.Fatal("blank name was not inferred on create")
	}
	for _, tc := range []struct {
		name *string
		base string
		want string
	}{
		{strPointer("我的备用"), "https://api.other.com", "我的备用"},
		{nil, "https://api.changed.com", "我的备用"},
		{strPointer("我的备用"), "https://api.again.com", "我的备用"},
		{strPointer("  "), "https://api.fresh.co.uk:9443", "fresh:9443"},
	} {
		body := map[string]any{"id": id, "kind": "api", "baseUrl": tc.base, "key": ""}
		if tc.name != nil {
			body["name"] = *tc.name
		}
		w := localRequest(s, h, "POST", "/api/local/accounts", body)
		if w.Code != 200 {
			t.Fatalf("edit failed: %d %s", w.Code, w.Body)
		}
		a, _ = s.d.Store.Get().LocalAccount(id)
		if a.Name != tc.want || a.Key != "synthetic-name-key" {
			t.Fatal("edit lost explicit name or retained credential")
		}
	}
	w := localRequest(s, h, "POST", "/api/local/accounts", map[string]any{"kind": "api", "name": "", "baseUrl": "https://user:secret@example.com", "key": "synthetic"})
	if w.Code != 400 || len(s.d.Store.Get().LocalAccounts) != 1 {
		t.Fatal("default name bypassed URL validation")
	}
}

func strPointer(s string) *string { return &s }
