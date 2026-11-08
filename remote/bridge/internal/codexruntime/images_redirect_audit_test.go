package codexruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestAuditPinnedOfficialImageExecutorDoesNotFollowRedirects(t *testing.T) {
	const syntheticToken = "synthetic-image-redirect-token"
	var redirectTargetCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/images/generations":
			w.Header().Set("Location", "/credential-trap")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/credential-trap":
			redirectTargetCalls.Add(1)
			if r.Header.Get("Authorization") == "Bearer "+syntheticToken {
				w.Header().Set("X-Synthetic-Credential-Replayed", "true")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"fixture"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	runtime := New(t.TempDir())
	defer runtime.Close()
	auth := &coreauth.Auth{Provider: "codex", Attributes: map[string]string{"base_url": server.URL, "api_key": syntheticToken}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	response, err := runtime.ExecuteImages(ctx, auth, []byte(`{"model":"gpt-image-2","prompt":"fixture"}`), false, "/v1/images/generations")
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if redirectTargetCalls.Load() != 0 {
		t.Fatalf("pinned SDK followed an authenticated image redirect: calls=%d err=%v", redirectTargetCalls.Load(), err)
	}
}
