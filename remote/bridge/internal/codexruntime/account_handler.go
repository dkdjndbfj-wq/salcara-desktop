package codexruntime

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"salcara/bridge/internal/codexaccounts"
	"salcara/bridge/internal/config"
)

type AccountExecutor struct {
	Accounts *codexaccounts.Service
	Runtime  *Runtime
}

func (e AccountExecutor) Do(ctx context.Context, a config.LocalAccount, body []byte, stream, compact bool, current func() bool) (*http.Response, error) {
	return e.do(ctx, a, body, current, func(ctx context.Context, record *coreauth.Auth) (*http.Response, error) {
		return e.Runtime.Execute(ctx, record, body, stream, compact)
	})
}

func (e AccountExecutor) DoImages(ctx context.Context, a config.LocalAccount, body []byte, stream bool, path string, current func() bool) (*http.Response, error) {
	return e.do(ctx, a, body, current, func(ctx context.Context, record *coreauth.Auth) (*http.Response, error) {
		return e.Runtime.ExecuteImages(ctx, record, body, stream, path)
	})
}

// Text and image routes share the same refresh, identity, persistence and final
// authorization fences. SDK image execution does not obtain its own account.
func (e AccountExecutor) do(ctx context.Context, a config.LocalAccount, body []byte, current func() bool, execute func(context.Context, *coreauth.Auth) (*http.Response, error)) (*http.Response, error) {
	if e.Accounts == nil || e.Runtime == nil || a.CredentialRef == "" || a.SourceType != "official_account" || a.ProviderID != "openai" || current == nil {
		return nil, ErrUnavailable
	}
	// Reject a request whose client/source/config lease was already revoked
	// before doing even bootstrap or credential-refresh work.
	if !current() {
		return nil, codexaccounts.ErrConflict
	}
	if err := e.Runtime.Ready(ctx); err != nil {
		return nil, err
	}
	if !current() {
		return nil, codexaccounts.ErrConflict
	}
	availability, err := e.Accounts.ExecutionAvailability(a.CredentialRef, a.CredentialEpoch)
	if err != nil {
		return nil, err
	}
	if availability.RequiresRefresh {
		if availability.PendingSave {
			return nil, codexaccounts.ErrSaveFailed
		}
		if availability.NeedsLogin {
			return nil, codexaccounts.ErrNeedsLogin
		}
		if !availability.RefreshEligible {
			return nil, codexaccounts.ErrRefreshFailed
		}
	}
	if availability.RequiresRefresh || availability.RefreshRecommended {
		if !current() {
			return nil, codexaccounts.ErrConflict
		}
		if _, err = e.Accounts.Refresh(ctx, a.CredentialRef); err != nil && !availability.AccessUsable {
			return nil, err
		}
	}
	call := func() (*http.Response, error) {
		// Re-read after bootstrap/refresh. A reimport/deletion/key change while
		// those await cannot execute the earlier command with new credentials.
		availability, err := e.Accounts.ExecutionAvailability(a.CredentialRef, a.CredentialEpoch)
		if err != nil {
			return nil, err
		}
		// A usable old access token may expire during a failed refresh wait.
		// Neither that token nor an already-expired refresh result can execute.
		if availability.RequiresRefresh {
			if availability.PendingSave {
				return nil, codexaccounts.ErrSaveFailed
			}
			if availability.NeedsLogin {
				return nil, codexaccounts.ErrNeedsLogin
			}
			return nil, codexaccounts.ErrRefreshFailed
		}
		record, err := e.Accounts.SDKRecord(ctx, a.CredentialRef)
		if err != nil {
			return nil, err
		}
		if record.Attributes["salcara_identity_epoch"] != strconv.FormatUint(a.CredentialEpoch, 10) {
			return nil, codexaccounts.ErrConflict
		}
		if !current() {
			return nil, codexaccounts.ErrConflict
		}
		// Refresh invalidates cached usage. Check here on every attempt, after
		// re-reading the owned credential, so stale usage cannot authorize retry.
		if a.QuotaReservePercent > 0 {
			models, valid := reserveModels(body)
			if !valid {
				return nil, ErrQuotaReserve
			}
			for _, model := range models {
				quota := e.Accounts.PoolQuota(a.CredentialRef, a.CredentialEpoch, model)
				if !quota.Fresh || !quota.Allowed || quota.RemainingPercent <= float64(a.QuotaReservePercent) {
					return nil, ErrQuotaReserve
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !current() {
			return nil, codexaccounts.ErrConflict
		}
		response, err := execute(ctx, record)
		if err != nil {
			return nil, err
		}
		// Streaming Runtime.Execute does not return until the first payload is
		// available. Recheck after that wait, while the response is still private;
		// a revoked request must not commit headers or its first byte. Once this
		// function returns a response, the existing draining semantics apply.
		if !current() {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			return nil, codexaccounts.ErrConflict
		}
		return response, nil
	}
	response, err := call()
	var status executor.StatusError
	if errors.As(err, &status) && status.StatusCode() == http.StatusUnauthorized {
		// Only retry a refused authentication before headers/output. Never
		// replay a partly streamed request or switch accounts on 429/5xx.
		if !current() {
			return nil, codexaccounts.ErrConflict
		}
		if _, refreshErr := e.Accounts.Refresh(ctx, a.CredentialRef); refreshErr != nil {
			return nil, refreshErr
		}
		return call()
	}
	return response, err
}
