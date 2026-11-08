package codexaccounts

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestWebSessionCannotConsumeExistingResetHandle(t *testing.T) {
	var calls atomic.Int32
	s, id := resetTestService(t, ResetCreditConsumeFunc(func(context.Context, *coreauth.Auth, string) error {
		calls.Add(1)
		return nil
	}), nil)
	defer s.Close()
	request, err := s.PrepareResetCredit(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a legacy persisted record with an existing handle and a newly
	// recognized web-session marker. Even that handle cannot authorize a write.
	s.mu.Lock()
	c := s.vault.Accounts[id]
	c.TokenSourceMode = "chatgpt_web_session"
	s.vault.Accounts[id] = c
	s.mu.Unlock()
	if _, err := s.PrepareResetCredit(context.Background(), id); !errors.Is(err, ErrResetUnavailable) {
		t.Fatal("web session reused a mutation handle", err)
	}
	if _, err := s.ConsumeResetCredit(context.Background(), id, request.RedeemRequestID); !errors.Is(err, ErrResetUnavailable) || calls.Load() != 0 {
		t.Fatal("web session issued a provider mutation", err)
	}
}
