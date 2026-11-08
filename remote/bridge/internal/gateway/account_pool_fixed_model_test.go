package gateway

import (
	"testing"

	"salcara/bridge/internal/config"
)

func TestAdmitFixedModelChecksPolicyQuotaAndSharedCredentialCapacity(t *testing.T) {
	p := NewAccountPool()
	a := officialPoolFixture("image-a")
	a.Models = []string{"gpt-parent"}
	a.ModelAllowlist = []string{"gpt-parent", "gpt-image-2"}
	a.QuotaReservePercent = 10
	a.MaxConcurrency = 1
	p.SetRoutingProbe(func(ref string, epoch uint64, model string) PoolRoutingObservation {
		if ref != a.CredentialRef || epoch != a.CredentialEpoch {
			t.Fatalf("wrong quota identity: %q/%d", ref, epoch)
		}
		allowed := model == "gpt-parent" || model == "gpt-image-2"
		return PoolRoutingObservation{Current: true, QuotaFresh: true, QuotaAllowed: allowed, RemainingPercent: 50}
	})
	release, err := p.AdmitFixedModel(a, []config.LocalAccount{a}, "gpt-parent", "gpt-image-2")
	if err != nil {
		t.Fatal(err)
	}
	if err = p.ValidateFixedModels(a, "gpt-parent", "gpt-image-2"); err != nil {
		t.Fatalf("an existing socket lease could not revalidate its models: %v", err)
	}
	alias := a
	alias.ID = "image-alias"
	if _, err = p.AdmitFixedModel(alias, []config.LocalAccount{a, alias}, "gpt-parent", "gpt-image-2"); err != ErrCredentialCapacity {
		t.Fatalf("shared credential concurrency bypassed: %v", err)
	}
	release()

	denied := a
	denied.ModelDenylist = []string{"gpt-image-2"}
	if _, err = p.AdmitFixedModel(denied, []config.LocalAccount{denied}, "gpt-parent", "gpt-image-2"); err == nil {
		t.Fatal("image model policy bypassed")
	}
	p.SetRoutingProbe(func(string, uint64, string) PoolRoutingObservation { return PoolRoutingObservation{} })
	if _, err = p.AdmitFixedModel(a, []config.LocalAccount{a}, "gpt-parent", "gpt-image-2"); err != errPoolQuotaReserve {
		t.Fatalf("unknown positive reserve did not fail closed: %v", err)
	}
}
