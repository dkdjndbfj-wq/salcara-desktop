package codexruntime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

type retryAfterFixtureError struct{ delay time.Duration }

func (e retryAfterFixtureError) Error() string {
	return `{"error":{"code":"rate_limit","message":"private provider text"}}`
}

func TestNativeWebsocketHandshakePreservesRetryAfter(t *testing.T) {
	r := nativeWSFixture(t, http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Header().Set("Retry-After", "5400")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"message":"private reason"}}`))
	}))
	_, err := r.executeWebSocket(context.Background(), syntheticAuth("A"), []byte(`{"model":"gpt-fixture","input":[]}`))
	var safe providerExecutionError
	if !errors.As(err, &safe) || safe.RetryAfter() == nil || *safe.RetryAfter() != 90*time.Minute {
		t.Fatalf("handshake dropped reset: %v", err)
	}
}
func (e retryAfterFixtureError) StatusCode() int            { return 429 }
func (e retryAfterFixtureError) RetryAfter() *time.Duration { return &e.delay }

func TestProviderErrorRetainsBoundedRetryAfter(t *testing.T) {
	for _, tc := range []struct{ delay, want time.Duration }{{90 * time.Minute, 90 * time.Minute}, {100 * time.Hour, 24 * time.Hour}, {-time.Hour, 0}, {0, 0}} {
		err := normalizeProviderError(fmt.Errorf("sdk wrapper: %w", retryAfterFixtureError{tc.delay}))
		var safe providerExecutionError
		if !errors.As(err, &safe) || safe.StatusCode() != 429 || safe.Error() != ErrUnavailable.Error() {
			t.Fatalf("lost status or leaked provider diagnostic: %v", err)
		}
		got := safe.RetryAfter()
		if tc.want == 0 {
			if got != nil {
				t.Fatalf("negative/zero delay preserved: %v", got)
			}
		} else if got == nil || *got != tc.want {
			t.Fatalf("delay=%v want=%s", got, tc.want)
		}
	}
}
