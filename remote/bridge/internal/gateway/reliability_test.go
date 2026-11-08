package gateway

import (
	"net/http"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func TestRetryAfterHeadersAreBoundedAndFinite(t *testing.T) {
	for _, tc := range []struct {
		header, value string
		want          time.Duration
	}{
		{"Retry-After", "5400", 90 * time.Minute},
		{"Retry-After", "999999999999999999999999999999", 24 * time.Hour},
		{"Retry-After", "+Inf", 0}, {"Retry-After", "NaN", 0}, {"Retry-After", "-1", 0},
		{"X-Ratelimit-Reset-Requests", "1h30m", 90 * time.Minute},
		{"Retry-After", time.Now().Add(48 * time.Hour).UTC().Format(http.TimeFormat), 24 * time.Hour},
	} {
		t.Run(tc.value, func(t *testing.T) {
			got := retryAfter(&http.Response{Header: http.Header{tc.header: {tc.value}}})
			if got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestPoolRetainsLongProviderReset(t *testing.T) {
	p := NewAccountPool()
	now := time.Now()
	p.now = func() time.Time { return now }
	a := poolFixture("long-reset", 1)
	_, release, err := p.Select("codex", []config.LocalAccount{a}, "coding", "gpt-5", "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	p.MarkFailureFor(a.ID, config.AccountFingerprint(a), 90*time.Minute)
	now = now.Add(6 * time.Minute)
	if _, release, err = p.Select("codex", []config.LocalAccount{a}, "coding", "gpt-5", ""); err == nil {
		release()
		t.Fatal("long provider reset truncated to old 5m cap")
	}
	now = now.Add(85 * time.Minute)
	if _, release, err = p.Select("codex", []config.LocalAccount{a}, "coding", "gpt-5", ""); err != nil {
		t.Fatal(err)
	}
	release()
}

func TestReplayGuardOnlyInspectsStructuredResourceReferences(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","input":"Explain the string file_id and encrypted_content"}`,
		`{"model":"m","messages":[{"role":"user","content":"file_id: example"}]}`,
		`{"model":"m","input":[{"type":"input_image","image_url":"data:image/png;base64,eA=="}]}`,
		`{"model":"m","tools":[{"type":"function","name":"lookup"}],"input":"hi"}`,
	} {
		if !replayableTextBody([]byte(body)) {
			t.Fatalf("ordinary self-contained input rejected: %s", body)
		}
	}
	for _, body := range []string{
		`{"model":"m","messages":[{"content":[{"file_id":"private"}]}]}`,
		`{"model":"m","input":[{"nested":{"vector_store_ids":["private"]}}]}`,
		`{"model":"m","input":[{"container":{"id":"private"}}]}`,
		`{"model":"m","input":[{"encrypted_content":"cipher"}]}`,
		`{"model":"m","previous_response_id":"private"}`,
	} {
		if replayableTextBody([]byte(body)) {
			t.Fatalf("private reference allowed: %s", body)
		}
	}
}
