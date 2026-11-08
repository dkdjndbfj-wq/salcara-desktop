package agents

import "testing"

func TestAggregationDesktopSuffixDoesNotInventSharedAPIFamily(t *testing.T) {
	a := declaredRegistryAgent()
	a.id, a.descriptor.ID, a.descriptor.Tool, a.descriptor.APIFamily = "hermes-desktop", "hermes-desktop", "hermes-desktop", ""
	d, valid := CanonicalDescriptor(a)
	if !valid || d.ID != a.id || d.Tool != a.id || d.APIFamily != a.id {
		t.Fatalf("default family crossed extension identity: valid=%v descriptor=%+v", valid, d)
	}
	m := NewManagerWithOptions(nil, func() Settings { return Settings{} }, registryOptions(t, a))
	defer m.Close()
	if got := m.Get(a.id); got != a {
		t.Fatal("a valid desktop-suffix CLI namespace was rejected")
	}
	a.descriptor.APIFamily = "hermes"
	d, valid = CanonicalDescriptor(a)
	if !valid || d.APIFamily != "hermes" || d.Tool != a.id {
		t.Fatal("explicit family sharing changed or rejected the executor identity")
	}
	a.descriptor.APIFamily = "another-desktop"
	d, valid = CanonicalDescriptor(a)
	if !valid || d.APIFamily != "another-desktop" {
		t.Fatal("an explicitly named extension family was truncated")
	}
}
