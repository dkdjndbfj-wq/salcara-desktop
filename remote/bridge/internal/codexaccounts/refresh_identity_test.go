package codexaccounts

import (
	"context"
	"errors"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestRefreshCannotChangeProviderAccountBehindSelectedIdentity(t *testing.T) {
	s, _, _ := newTestService(t, RefreshFunc(func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
		a.Metadata["access_token"] = "synthetic-access-different-account"
		a.Metadata["refresh_token"] = "synthetic-refresh-different-account"
		a.Metadata["account_id"] = "different-provider-account"
		return a, nil
	}), nil)
	before := importFixture(t, s)
	after, err := s.Refresh(context.Background(), before.ID)
	if !errors.Is(err, ErrNeedsLogin) || after.Status != StatusNeedsLogin || after.Epoch <= before.Epoch || after.IdentityEpoch != before.IdentityEpoch {
		t.Fatalf("identity replacement accepted: %+v %v", after, err)
	}
	record, err := s.SDKRecord(context.Background(), before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Metadata["access_token"] != "synthetic-access-old" || record.Metadata["account_id"] != "explicit-fixture-account" {
		t.Fatal("another provider identity published")
	}
	if record.Metadata["refresh_token"] != "" {
		t.Fatal("conflicting refresh credential still retryable")
	}
}
