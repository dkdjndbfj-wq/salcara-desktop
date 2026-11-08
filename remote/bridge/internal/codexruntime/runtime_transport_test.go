package codexruntime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/codexaccounts"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type runtimeTransportFunc func(*http.Request) (*http.Response, error)

func (f runtimeTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRuntimeTransportOverridesCallerHookAndRejectsSDKProxy(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := runtimeTransportFunc(func(*http.Request) (*http.Response, error) {
		foreignCalls.Add(1)
		return nil, errors.New("foreign transport must not receive credentials")
	})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer provider.Close()
	runtime := New(t.TempDir())
	defer runtime.Close()
	ctx := context.WithValue(context.Background(), sdkRoundTripperContextKey, http.RoundTripper(foreign))
	safeCtx, err := runtime.withSDKTransport(ctx, syntheticAuth("context-override"))
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := safeCtx.Value(sdkRoundTripperContextKey).(http.RoundTripper)
	if !ok || transport == nil {
		t.Fatal("runtime did not install its private SDK transport")
	}
	resp, err := (&http.Client{Transport: transport}).Get(provider.URL + "/fixture")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if foreignCalls.Load() != 0 {
		t.Fatalf("caller transport overrode runtime credential path: calls=%d", foreignCalls.Load())
	}
	proxied := syntheticAuth("proxy-bypass")
	proxied.ProxyURL = "http://127.0.0.1:65530"
	if _, err = runtime.withSDKTransport(context.Background(), proxied); err != ErrUnavailable {
		t.Fatalf("SDK auth proxy bypass was accepted: %v", err)
	}
}

func TestRuntimeTransportRejectsSameAndCrossOriginRedirects(t *testing.T) {
	const syntheticToken = "synthetic-runtime-transport-token"
	for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect} {
		for _, crossOrigin := range []bool{false, true} {
			name := fmt.Sprintf("status-%d/cross-origin-%t", status, crossOrigin)
			t.Run(name, func(t *testing.T) {
				var trapCalls atomic.Int32
				trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					trapCalls.Add(1)
					w.WriteHeader(http.StatusNoContent)
				}))
				defer trap.Close()
				var source *httptest.Server
				source = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					if req.URL.Path == "/credential-trap" {
						trapCalls.Add(1)
						w.WriteHeader(http.StatusNoContent)
						return
					}
					target := "/credential-trap"
					if crossOrigin {
						target = trap.URL + "/credential-trap"
					}
					w.Header().Set("Location", target)
					w.WriteHeader(status)
				}))
				defer source.Close()

				transport := newRuntimeHTTPTransport()
				defer transport.CloseIdleConnections()
				client := &http.Client{Transport: runtimeRedirectGuard{base: transport}}
				req, err := http.NewRequest(http.MethodPost, source.URL+"/source", strings.NewReader("refresh_token=synthetic-private"))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer "+syntheticToken)
				resp, err := client.Do(req)
				if resp != nil && resp.Body != nil {
					_ = resp.Body.Close()
				}
				if !errors.Is(err, errRuntimeRedirect) {
					t.Fatalf("redirect was not rejected by transport: response=%v err=%v", resp, err)
				}
				if trapCalls.Load() != 0 {
					t.Fatalf("redirect target received credential-bearing request: calls=%d", trapCalls.Load())
				}
			})
		}
	}
}

func TestPinnedSDKTextPathsUseRuntimeRedirectGuard(t *testing.T) {
	const syntheticToken = "synthetic-text-redirect-token"
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "stream"}[stream], func(t *testing.T) {
			var trapCalls atomic.Int32
			trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				trapCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"response":{"id":"credential-trap"}}`)
			}))
			defer trap.Close()
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Location", trap.URL+"/credential-trap")
				w.WriteHeader(http.StatusFound)
			}))
			defer source.Close()

			runtime := New(t.TempDir())
			defer runtime.Close()
			auth := &coreauth.Auth{Provider: "codex", Attributes: map[string]string{"base_url": source.URL, "api_key": syntheticToken}}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			response, err := runtime.Execute(ctx, auth, []byte(`{"model":"gpt-fixture","input":"fixture"}`), stream, false)
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			if err == nil {
				t.Fatal("redirecting text upstream returned success")
			}
			if trapCalls.Load() != 0 {
				t.Fatalf("pinned SDK followed authenticated text redirect: calls=%d err=%v", trapCalls.Load(), err)
			}
		})
	}
}

func TestRuntimeRefreshUsesBoundedNoRedirectConnector(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		var trapCalls atomic.Int32
		trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			trapCalls.Add(1)
			w.WriteHeader(http.StatusNoContent)
		}))
		defer trap.Close()
		source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Location", trap.URL+"/credential-trap")
			w.WriteHeader(http.StatusTemporaryRedirect)
		}))
		defer source.Close()
		runtime := New(t.TempDir())
		defer runtime.Close()
		runtime.refreshTokenURL = source.URL + "/token"
		refreshed, err := runtime.Refresh(context.Background(), syntheticAuth("refresh-redirect"))
		if refreshed != nil || err == nil {
			t.Fatalf("redirecting refresh returned credentials: record=%v err=%v", refreshed, err)
		}
		if trapCalls.Load() != 0 {
			t.Fatalf("refresh form was replayed to redirect target: calls=%d", trapCalls.Load())
		}
	})

	t.Run("success", func(t *testing.T) {
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"fixture@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"account-fixture"}}`))
		idToken := "fixture." + payload + ".signature"
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method != http.MethodPost || req.URL.Path != "/token" {
				t.Errorf("unexpected refresh request: %s %s", req.Method, req.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if err := req.ParseForm(); err != nil || req.Form.Get("refresh_token") != "synthetic-not-real-refresh-refresh-success" || req.Form.Get("client_id") != legacyCodexClientID {
				t.Errorf("unexpected bounded refresh form")
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"access_token":"synthetic-access-rotated","refresh_token":"synthetic-refresh-rotated","id_token":%q,"expires_in":3600}`, idToken)
		}))
		defer provider.Close()
		runtime := New(t.TempDir())
		defer runtime.Close()
		runtime.refreshTokenURL = provider.URL + "/token"
		refreshed, err := runtime.Refresh(context.Background(), syntheticAuth("refresh-success"))
		if err != nil {
			t.Fatal(err)
		}
		if refreshed == nil || refreshed.Metadata["access_token"] != "synthetic-access-rotated" || refreshed.Metadata["refresh_token"] != "synthetic-refresh-rotated" || refreshed.Metadata["account_id"] != "account-fixture" || refreshed.Metadata["email"] != "fixture@example.test" {
			t.Fatalf("refresh response was not converted to protected SDK record: %#v", refreshed)
		}
	})

	t.Run("invalid-grant", func(t *testing.T) {
		provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"synthetic private provider text"}`)
		}))
		defer provider.Close()
		runtime := New(t.TempDir())
		defer runtime.Close()
		runtime.refreshTokenURL = provider.URL + "/token"
		refreshed, err := runtime.Refresh(context.Background(), syntheticAuth("refresh-invalid"))
		if refreshed != nil || !errors.Is(err, codexaccounts.ErrNeedsLogin) || strings.Contains(err.Error(), "private") {
			t.Fatalf("invalid_grant was not mapped to a sanitized login requirement: record=%v err=%v", refreshed, err)
		}
	})
}
