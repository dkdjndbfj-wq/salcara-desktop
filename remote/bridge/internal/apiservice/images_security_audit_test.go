package apiservice

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func TestAuditResponsesImageToolUsesClientNamespaceAliasBeforeACL(t *testing.T) {
	s, live := imageService(t)
	live.change(func(c *config.Config) {
		client := &c.CodexAPI.Clients[0]
		client.ModelPrefix = "team/"
		client.ModelAliases = map[string]string{"chat": "gpt-fixture", "picture": "gpt-image-2"}
		client.ModelAllowlist = []string{"gpt-fixture", "gpt-image-2"}
	})
	var calls int
	var upstream map[string]any
	s.Official = func(_ context.Context, _ config.LocalAccount, body []byte, _ bool, _ bool, _ func() bool) (*http.Response, error) {
		calls++
		_ = json.Unmarshal(body, &upstream)
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}

	w := callAPI(s, syntheticClientKey, `{"model":"team/chat","input":"fixture","tools":[{"type":"image_generation","model":"team/picture"}]}`)
	if w.Code != http.StatusOK || calls != 1 {
		t.Fatalf("namespaced image alias was not admitted: status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
	}
	tools, _ := upstream["tools"].([]any)
	tool, _ := tools[0].(map[string]any)
	if upstream["model"] != "gpt-fixture" || tool["model"] != "gpt-image-2" {
		t.Fatalf("visible aliases reached the official executor: %+v", upstream)
	}

	w = callAPI(s, syntheticClientKey, `{"model":"team/chat","input":"fixture","tools":[{"type":"image_generation","model":"gpt-image-2"}]}`)
	if w.Code != http.StatusForbidden || calls != 1 {
		t.Fatalf("unprefixed nested image model bypassed client namespace: status=%d calls=%d", w.Code, calls)
	}
}

func TestAuditStaleImageAdmissionCannotRaiseRetiredCredentialLimit(t *testing.T) {
	q := new(imageQueue)
	hold, err := q.acquire(context.Background(), "shared-credential", 1)
	if err != nil {
		t.Fatal(err)
	}

	unblock := make(chan struct{})
	entered := make(chan struct{}, 3)
	done := make(chan error, 3)
	start := func(limit int) {
		go func() {
			release, acquireErr := q.acquire(context.Background(), "shared-credential", limit)
			if acquireErr != nil {
				done <- acquireErr
				return
			}
			entered <- struct{}{}
			<-unblock
			release()
			done <- nil
		}()
	}
	start(1)
	start(1)
	waitImageQueue(t, q, 1, 2)

	// This represents a request that captured the old limit before a settings
	// retirement, but reached the shared queue after new-limit waiters.
	start(4)
	var bypassed bool
	select {
	case <-entered:
		bypassed = true
	case <-time.After(50 * time.Millisecond):
	}
	close(unblock)
	hold()
	for i := 0; i < 3; i++ {
		if waitErr := <-done; waitErr != nil {
			t.Fatal(waitErr)
		}
	}
	if bypassed {
		t.Fatal("stale admission raised the retired per-credential image limit")
	}
}
