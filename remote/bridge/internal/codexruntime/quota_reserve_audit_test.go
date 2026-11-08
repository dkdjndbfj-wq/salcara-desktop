package codexruntime

import (
	"context"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestAuditFixedOfficialExecutionFailsClosedWithoutFreshQuota(t *testing.T) {
	for _, image := range []bool{false, true} {
		name := "responses"
		if image {
			name = "images"
		}
		t.Run(name, func(t *testing.T) {
			executions := 0
			stub := &stubExecutor{execute: func(context.Context, *coreauth.Auth, executor.Request, executor.Options) (executor.Response, error) {
				executions++
				return executor.Response{Payload: []byte(`{"data":[{"b64_json":"fixture"}]}`)}, nil
			}}
			e, account := handlerFixture(t, "2099-01-01T00:00:00Z", stub)
			account.QuotaReservePercent = 25
			var err error
			if image {
				_, err = e.DoImages(context.Background(), account, []byte(`{"model":"gpt-image-2","prompt":"fixture"}`), false, "/v1/images/generations", func() bool { return true })
			} else {
				_, err = e.Do(context.Background(), account, []byte(`{"model":"gpt-fixture","input":"fixture"}`), false, false, func() bool { return true })
			}
			if err == nil || executions != 0 {
				t.Fatalf("fixed official route bypassed unknown quota reserve: executions=%d err=%v", executions, err)
			}
		})
	}
}
