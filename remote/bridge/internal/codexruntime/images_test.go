package codexruntime

import (
	"context"
	"io"
	"strings"
	"testing"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestAccountImagesUseProtectedExecutorOptionsAndRefreshBeforeRetry(t *testing.T) {
	var calls, refreshes int
	stub := &stubExecutor{refresh: func(_ context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
		refreshes++
		return rotatedSDK(a), nil
	}}
	stub.execute = func(_ context.Context, a *coreauth.Auth, req executor.Request, opts executor.Options) (executor.Response, error) {
		calls++
		if req.Model != "gpt-image-2" || opts.SourceFormat.String() != "openai-image" || opts.ResponseFormat.String() != "openai-image" || opts.Metadata[executor.RequestPathMetadataKey] != "/v1/images/generations" || opts.Headers.Get("Content-Type") != "application/json" {
			t.Error("image request entered text/default SDK path")
		}
		if calls == 1 {
			return executor.Response{}, syntheticStatusError{code: 401}
		}
		if a.Metadata["access_token"] != "synthetic-access-rotated" {
			t.Error("retry used stale access")
		}
		return executor.Response{Payload: []byte(`{"data":[{"b64_json":"fixture"}]}`)}, nil
	}
	e, a := handlerFixture(t, "2099-01-01T00:00:00Z", stub)
	r, err := e.DoImages(context.Background(), a, []byte(`{"model":"gpt-image-2","prompt":"fixture"}`), false, "/v1/images/generations", func() bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if calls != 2 || refreshes != 1 || !strings.Contains(string(body), "fixture") {
		t.Fatal("image fixed-account retry failed")
	}
	if _, err = e.DoImages(context.Background(), a, []byte(`{"model":"gpt-image-2"}`), false, "/v1/images/edits", func() bool { return false }); err == nil || calls != 2 {
		t.Fatal("revoked image entered executor")
	}
}
