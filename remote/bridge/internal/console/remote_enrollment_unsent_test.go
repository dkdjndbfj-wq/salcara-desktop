package console

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestRemoteAPIEnrollmentIdentityFailureReopensOnlyUnsentAttempt(t *testing.T) {
	f := newEnrollmentFixture(t)
	s, h := newTestServer(t)
	_, body := enrollmentTestRequest(t, s, h, f.apiKey)
	base := http.DefaultTransport
	http.DefaultTransport = enrollmentFixtureTransport(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		grantIssued := f.issuerCount > 0
		f.mu.Unlock()
		if grantIssued && strings.Contains(r.URL.Path, "/ingress/") {
			return nil, errors.New("synthetic identity failure before register")
		}
		return base.RoundTrip(r)
	})
	w := localRequest(s, h, "POST", "/api/remote/discover", body)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"enrollment_not_sent"`) {
		t.Fatalf("non-delivery was not identified: %d %s", w.Code, w.Body)
	}
	cfg := s.d.Store.Get()
	if len(cfg.RemoteEnrollmentAttempts) != 1 || cfg.RemoteEnrollmentAttempts[0].Submitted || f.registerCount != 0 {
		t.Fatal("known-unsent registration became an unknown submission")
	}
	identity := cfg.RemoteEnrollmentAttempts[0].Connection
	http.DefaultTransport = base
	w = localRequest(s, h, "POST", "/api/remote/discover", body)
	if w.Code != 200 || f.registerCount != 1 || s.d.Store.Get().DeviceID != identity.DeviceID || s.d.Store.Get().DeviceSecret != identity.DeviceSecret {
		t.Fatalf("explicit retry did not reuse the prepared identity: %d %s", w.Code, w.Body)
	}
}

func TestUnsentEnrollmentCleanupRequiresExactFreshAttempt(t *testing.T) {
	s, h := newTestServer(t)
	id, _ := enrollmentTestRequest(t, s, h, "synthetic-key")
	before := s.d.Store.Get()
	account, _ := before.LocalAccount(id)
	revision := before.AccountSourceRevision(account)
	attempt := config.RemoteEnrollmentAttempt{AccountID: id, RequestID: config.NewUUID(), Connection: config.RemoteConnection{HubURL: enrollmentFixtureHub, DeviceID: config.NewUUID(), DeviceSecret: "synthetic-device-secret", HubInstanceID: strings.Repeat("a", 32), IdentityPublicKey: "synthetic-key"}}
	attempt.SourceProof = enrollmentSourceProof(account, attempt.Connection.DeviceSecret)
	for _, change := range []string{"none", "earlier-unknown", "request", "epoch", "identity", "source", "target", "missing"} {
		t.Run(change, func(t *testing.T) {
			current, fresh := before.Clone(), attempt
			pending := attempt
			pending.Submitted = true
			current.RemoteEnrollmentAttempts = []config.RemoteEnrollmentAttempt{pending}
			switch change {
			case "earlier-unknown":
				fresh.Submitted = true
			case "request":
				current.RemoteEnrollmentAttempts[0].RequestID = config.NewUUID()
			case "epoch":
				current.RemoteConnectionEpoch = config.NewUUID()
			case "identity":
				current.DeviceID = "another-device"
			case "source":
				current.LocalAccounts[0].Key = "changed-key"
			case "target":
				current.RemoteEnrollmentAttempts[0].Connection.DeviceSecret = "changed-secret"
			case "missing":
				current.RemoteEnrollmentAttempts = nil
			}
			err := clearUnsentEnrollmentAttempt(&current, before, id, &revision, fresh)
			if change == "none" {
				if err != nil || current.RemoteEnrollmentAttempts[0].Submitted {
					t.Fatal("exact fresh attempt remained locked", err)
				}
			} else if err == nil || len(current.RemoteEnrollmentAttempts) > 0 && !current.RemoteEnrollmentAttempts[0].Submitted {
				t.Fatal("stale cleanup cleared a newer or uncertain attempt")
			}
		})
	}
}

func TestUnsentEnrollmentRetryPreservesEarlierUnknownResult(t *testing.T) {
	f := newEnrollmentFixture(t)
	f.dropAck, f.noCommit = true, true
	s, h := newTestServer(t)
	_, body := enrollmentTestRequest(t, s, h, f.apiKey)
	w := localRequest(s, h, "POST", "/api/remote/discover", body)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"enrollment_result_unknown"`) {
		t.Fatal("fixture did not lose the first reply")
	}
	first := s.d.Store.Get().RemoteEnrollmentAttempts[0]
	base := http.DefaultTransport
	http.DefaultTransport = enrollmentFixtureTransport(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		retryGranted := f.issuerCount >= 2
		f.mu.Unlock()
		if retryGranted && strings.Contains(r.URL.Path, "/ingress/") {
			return nil, errors.New("synthetic retry identity unavailable")
		}
		return base.RoundTrip(r)
	})
	body["retryEnrollment"] = true
	w = localRequest(s, h, "POST", "/api/remote/discover", body)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"enrollment_result_unknown"`) {
		t.Fatalf("earlier uncertain result was cleared: %d %s", w.Code, w.Body)
	}
	pending := s.d.Store.Get().RemoteEnrollmentAttempts
	if len(pending) != 1 || !pending[0].Submitted || pending[0].Connection.DeviceID != first.Connection.DeviceID || f.registerCount != 1 {
		t.Fatal("retry non-delivery discarded an uncertain identity or resent mutation")
	}
}
