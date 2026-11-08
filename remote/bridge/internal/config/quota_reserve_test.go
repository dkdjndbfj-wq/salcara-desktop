package config

import "testing"

func TestQuotaReserveRequiresVerifiedSourceAndChangesAuthorization(t *testing.T) {
	a := LocalAccount{ID: "quota-fixture", Name: "Fixture", Kind: "api", SourceType: "official_account", ProviderID: "openai", CredentialRef: "fixture-vault", CredentialEpoch: 1, BaseURL: "https://chatgpt.com/backend-api/codex", Key: "protected-fixture", Wire: "responses"}
	if err := ValidateLocalAccount(&a); err != nil {
		t.Fatal(err)
	}
	before := AccountFingerprint(a)
	a.QuotaReservePercent = 20
	if err := ValidateLocalAccount(&a); err != nil {
		t.Fatal(err)
	}
	if AccountFingerprint(a) == before {
		t.Fatal("quota policy did not invalidate existing source authorization")
	}
	for _, invalid := range []int{-1, 100, 101} {
		a.QuotaReservePercent = invalid
		if ValidateLocalAccount(&a) == nil {
			t.Errorf("invalid reserve accepted: %d", invalid)
		}
	}
	a.QuotaReservePercent, a.CredentialRef, a.SourceType = 20, "", "api_key"
	if ValidateLocalAccount(&a) == nil {
		t.Fatal("unsupported quota source silently accepted reservation")
	}
}
