package hubclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFailedConnectionUsesBackoffRatherThanRetryingTightLoop(t *testing.T) {
	var calls atomic.Int32
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer u.Close()
	c, _ := newTestClient(t, u.URL, "synthetic-key", nil)
	c.o.BackoffMin = 100 * time.Millisecond
	c.o.BackoffMax = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	c.Run(ctx)
	if n := calls.Load(); n < 1 || n > 5 {
		t.Fatalf("backoff not respected: %d requests in 250ms", n)
	}
}

func TestStationErrorBodyCannotEchoSecretsIntoLogs(t *testing.T) {
	r := &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader(`{"error":"secret-fixture-credential private-command"}`))}
	err := checkResp(r)
	if err == nil || strings.Contains(err.Error(), "secret-fixture") || strings.Contains(err.Error(), "private-command") {
		t.Fatal("station body reflected")
	}
}
