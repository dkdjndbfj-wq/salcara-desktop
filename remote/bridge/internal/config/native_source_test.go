package config

import "testing"

func TestNativeDesktopIdentityIgnoresPhoneSourceAndFencesAppliedLease(t *testing.T) {
	a := LocalAccount{ID: "desktop", Kind: "api", Name: "Fixture A", BaseURL: "https://fixture-a.invalid", Key: "synthetic-a"}
	b := LocalAccount{ID: "worker", Kind: "api", Name: "Fixture B", BaseURL: "https://fixture-b.invalid", Key: "synthetic-b"}
	cfg := Config{LocalAccounts: []LocalAccount{a, b}, RemoteAPI: map[string]RemoteAPI{"codex": {AccountID: b.ID}}}
	if cfg.NativeDesktopSourceIdentity() != "" {
		t.Fatal("phone-only selection adopted as desktop")
	}
	cfg.RecordAppliedAPI("codex-desktop", a, "fixture")
	first := cfg.NativeDesktopSourceIdentity()
	if len(first) != 64 {
		t.Fatal("expected opaque applied identity")
	}
	delete(cfg.RemoteAPI, "codex")
	if cfg.NativeDesktopSourceIdentity() != first {
		t.Fatal("CLI source changed desktop identity")
	}
	latest := cfg.ToolAPIApplied["codex"]
	latest.LaunchLease = "renewed-fixture-lease"
	cfg.ToolAPIApplied["codex"] = latest
	if cfg.NativeDesktopSourceIdentity() == first {
		t.Fatal("renewed source silently reused desktop lease")
	}
	delete(cfg.ToolAPIApplied, "codex")
	if cfg.NativeDesktopSourceIdentity() != "" {
		t.Fatal("removed source remained authorized")
	}
}
