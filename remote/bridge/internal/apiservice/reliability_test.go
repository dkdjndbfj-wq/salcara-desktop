package apiservice

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/accounthealth"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
	"salcara/bridge/internal/usageledger"
)

type reliabilityError struct {
	status int
	code   string
	delay  time.Duration
}

func (e reliabilityError) Error() string              { return "synthetic-private-diagnostic" }
func (e reliabilityError) StatusCode() int            { return e.status }
func (e reliabilityError) ExecutionCode() string      { return e.code }
func (e reliabilityError) RetryAfter() *time.Duration { return &e.delay }

type reliabilityAdmissionError struct{}

func (reliabilityAdmissionError) Error() string             { return "local admission" }
func (reliabilityAdmissionError) LocalAdmissionStatus() int { return 503 }

const reliableResult = `{"id":"result","object":"response","status":"completed","model":"gpt-fixture","output":[],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}`

func TestReliableTextFailoverUsesOnlyKnownRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		calls  int
		status int
	}{
		{"rate limit", reliabilityError{429, "", time.Hour}, 2, 200},
		{"auth refusal", reliabilityError{401, "", 0}, 2, 200},
		{"model capacity", reliabilityError{503, "model_capacity", time.Hour}, 2, 200},
		{"ambiguous 503", reliabilityError{503, "", 0}, 1, 503},
		{"transport result unknown", errors.New("network gone"), 1, 502},
		{"local admission", reliabilityAdmissionError{}, 1, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, live := fixtureService(t)
			enableTextPoolFixture(s, live, []string{"account-A", "account-B"}, config.PoolStrategyAuto, "")
			var accounts []string
			var observed []usageledger.Observation
			s.Observe = func(o usageledger.Observation) { observed = append(observed, o) }
			active := 0
			s.BeginUsage = func(string, uint64) func() { active++; return func() { active-- } }
			s.Official = func(_ context.Context, a config.LocalAccount, _ []byte, _, _ bool, current func() bool) (*http.Response, error) {
				if !current() {
					t.Fatal("invalid account reached dispatch")
				}
				accounts = append(accounts, a.ID)
				if len(accounts) == 1 {
					return nil, tc.err
				}
				return successfulResponse(io.NopCloser(strings.NewReader(reliableResult)), false), nil
			}
			w := callAPI(s, syntheticClientKey, `{"model":"friendly","input":"ordinary text"}`)
			if w.Code != tc.status || len(accounts) != tc.calls {
				t.Fatalf("status=%d calls=%v body=%s", w.Code, accounts, w.Body)
			}
			if strings.Contains(w.Body.String(), "synthetic-private") || active != 0 {
				t.Fatalf("private error or leaked usage: %s active=%d", w.Body, active)
			}
			if len(observed) != 1 {
				t.Fatalf("observations=%d", len(observed))
			}
			o := observed[0]
			if tc.calls == 2 {
				if accounts[0] == accounts[1] || len(o.Attempts) != 2 || o.Attempts[0].AccountID != accounts[0] || o.Attempts[1].AccountID != accounts[1] || o.State != "completed" || o.RoutingReason != "failover" {
					t.Fatalf("attempts not matching execution: %+v %v", o, accounts)
				}
				if o.Tokens.Total == nil || *o.Tokens.Total != 7 {
					t.Fatalf("incorrect final tokens: %+v", o.Tokens)
				}
				if o.Attempts[1].Reason != "failover" || o.Attempts[0].Reason == "pool_selected" {
					t.Fatalf("attempt reasons disagree with routing: %+v", o.Attempts)
				}
			}
			if tc.name == "rate limit" && time.Until(s.Health.CooldownUntil(accounts[0])) < 59*time.Minute {
				t.Fatal("provider reset truncated")
			}
			if tc.name == "model capacity" && !s.Health.Available(accounts[0]) {
				t.Fatal("one model cooled whole credential")
			}
			if tc.name == "local admission" && (len(o.Attempts) != 0 || !s.Health.Available(accounts[0])) {
				t.Fatalf("local admission blamed provider: %+v", o)
			}
		})
	}
}

func TestReliableDuplicateCredentialAliasesAreNotRetried(t *testing.T) {
	s, live := fixtureService(t)
	live.change(func(c *config.Config) { c.LocalAccounts[1].CredentialRef = c.LocalAccounts[0].CredentialRef })
	enableTextPoolFixture(s, live, []string{"account-A", "account-B"}, config.PoolStrategyAuto, "")
	calls := 0
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls++
		return nil, reliabilityError{429, "", 0}
	}
	w := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"same credential aliases"}`)
	if w.Code != 429 || calls != 1 {
		t.Fatalf("same credential was retried under another card: %d calls=%d", w.Code, calls)
	}
}

func TestReliableHTTPRejectsAcrossCredentialsWithPrivateResources(t *testing.T) {
	for _, fragment := range []string{
		`"input":[{"type":"input_file","file_id":"file-private"}]`,
		`"input":[{"type":"message","content":[{"type":"input_image","file_id":"file-private"}]}]`,
		`"input":[{"type":"reasoning","encrypted_content":"cipher"}]`,
		`"input":[{"type":"message","content":[{"nested":{"vector_store_ids":["vs-private"]}}]}]`,
		`"input":"test","tools":[{"type":"web_search"}]`,
		`"input":"test","background":true`,
	} {
		t.Run(fragment, func(t *testing.T) {
			s, live := fixtureService(t)
			enableTextPoolFixture(s, live, []string{"account-A", "account-B"}, config.PoolStrategyAuto, "")
			calls := 0
			s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
				calls++
				return nil, reliabilityError{429, "", 0}
			}
			w := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture",`+fragment+`}`)
			if w.Code != 429 || calls != 1 {
				t.Fatalf("private resource replayed: %d calls=%d %s", w.Code, calls, w.Body)
			}
		})
	}
}

func TestReliableHTTPStreamFailureNeverReplaysAndRecordsHealthWithoutLedger(t *testing.T) {
	s, live := fixtureService(t)
	enableTextPoolFixture(s, live, []string{"account-A", "account-B"}, config.PoolStrategyAuto, "")
	var accounts []string
	s.Official = func(_ context.Context, a config.LocalAccount, _ []byte, _, _ bool, _ func() bool) (*http.Response, error) {
		accounts = append(accounts, a.ID)
		return successfulResponse(io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")), true), nil
	}
	w := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"test","stream":true}`)
	if w.Code != 200 || len(accounts) != 1 || s.Health.Available(accounts[0]) {
		t.Fatalf("truncation treated as success or replayed: status=%d accounts=%v", w.Code, accounts)
	}
}

func TestReliableWebSocketFailoverMatchesAttemptsAndKeepsPrivateHistoryPinned(t *testing.T) {
	s, live := fixtureService(t)
	enableTextPoolFixture(s, live, []string{"account-A", "account-B"}, config.PoolStrategyAuto, "")
	observations := make(chan usageledger.Observation, 2)
	s.Observe = func(o usageledger.Observation) { observations <- o }
	var calls atomic.Int32
	s.Official = func(_ context.Context, a config.LocalAccount, _ []byte, _, _ bool, current func() bool) (*http.Response, error) {
		if !current() {
			t.Error("socket selected stale credential")
		}
		n := calls.Add(1)
		if n == 1 || n == 3 {
			return nil, reliabilityError{429, "", 0}
		}
		return successfulResponse(io.NopCloser(strings.NewReader(namedCompleted(a.ID))), true), nil
	}
	c := namedSocket(t, namedSocketServer(t, s), syntheticClientKey)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": "gpt-fixture", "input": "initial"})
	event := namedSocketEvent(t, c, "response.completed")
	var first usageledger.Observation
	select {
	case first = <-observations:
	case <-time.After(3 * time.Second):
		t.Fatal("missing socket observation")
	}
	response := event["response"].(map[string]any)
	if calls.Load() != 2 || len(first.Attempts) != 2 || first.Attempts[0].AccountID == first.Attempts[1].AccountID || response["id"] != first.Attempts[1].AccountID || first.AccountID != first.Attempts[1].AccountID {
		t.Fatalf("socket selection overwritten: calls=%d obs=%+v event=%v", calls.Load(), first, event)
	}
	// The completed response carries encrypted reasoning. Its expanded second
	// turn may run on the same account but cannot fail over to another account.
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "previous_response_id": response["id"], "input": []any{map[string]any{"role": "user", "content": "continue"}}})
	namedSocketEvent(t, c, "error")
	select {
	case second := <-observations:
		if calls.Load() != 3 || len(second.Attempts) != 1 || second.Attempts[0].AccountID != first.AccountID {
			t.Fatalf("private history replayed: calls=%d %+v", calls.Load(), second)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing second socket observation")
	}
}

func TestReliableTimeoutAndCancellationDoNotReplay(t *testing.T) {
	for _, mode := range []string{"headers", "first body", "stream idle", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, live := fixtureService(t)
			enableTextPoolFixture(s, live, []string{"account-A", "account-B"}, config.PoolStrategyAuto, "")
			s.firstByteTimeout = 80 * time.Millisecond
			s.streamIdleTimeout = 50 * time.Millisecond
			var o usageledger.Observation
			s.Observe = func(v usageledger.Observation) { o = v }
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.Official = func(upctx context.Context, _ config.LocalAccount, _ []byte, _, _ bool, _ func() bool) (*http.Response, error) {
				calls++
				if mode == "headers" || mode == "cancel" {
					if mode == "cancel" {
						cancel()
					}
					<-upctx.Done()
					return nil, upctx.Err()
				}
				r, w := io.Pipe()
				go func() {
					defer w.Close()
					if mode == "stream idle" {
						_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
					}
					<-upctx.Done()
				}()
				return successfulResponse(r, mode == "stream idle"), nil
			}
			body := `{"model":"gpt-fixture","input":"wait","stream":` + map[bool]string{true: "true", false: "false"}[mode == "stream idle"] + `}`
			w := httptest.NewRecorder()
			s.ServeHTTP(w, apiRequest(ctx, syntheticClientKey, strings.NewReader(body)))
			if calls != 1 || len(o.Attempts) != 1 {
				t.Fatalf("timeout replay: calls=%d %+v", calls, o)
			}
			if mode == "cancel" {
				if o.State != "cancelled" || !s.Health.Available(o.AccountID) {
					t.Fatalf("client cancellation blamed provider: %+v", o)
				}
				for _, state := range s.Pool.Snapshot(live.get().LocalAccounts) {
					if state.CoolingDown {
						t.Fatal("client cancellation cooled credential budget")
					}
				}
			} else {
				reason := "headers_timeout"
				if mode == "stream idle" {
					reason = "stream_idle_timeout"
				}
				if o.Attempts[0].Reason != reason || o.State == "completed" || s.Health.Available(o.AccountID) {
					t.Fatalf("wrong timeout state: %+v", o)
				}
				found := false
				for _, state := range s.Pool.Snapshot(live.get().LocalAccounts) {
					if state.AccountID == o.AccountID {
						found = state.CoolingDown
					}
				}
				if !found {
					t.Fatal("timeout omitted credential-wide cooldown")
				}
			}
		})
	}
}

func TestReliablePolicyStableAcrossHealthRanking(t *testing.T) {
	s, live := fixtureService(t)
	enableTextPoolFixture(s, live, []string{"account-A", "account-B"}, config.PoolStrategyAuto, "")
	c := live.get()
	client := c.CodexAPI.Clients[0]
	a, _, release, status := s.selectNamedTextAccount(c, client, "gpt-fixture", "sticky-session", func() bool { return true })
	if status != 0 {
		t.Fatal(status)
	}
	release()
	other := "account-A"
	if a.ID == other {
		other = "account-B"
	}
	for i := 0; i < 8; i++ {
		s.Health.Record(other, accounthealth.Result{Kind: accounthealth.OK, Latency: time.Millisecond})
	}
	b, _, release, status := s.selectNamedTextAccount(c, client, "gpt-fixture", "sticky-session", func() bool { return true })
	if status != 0 {
		t.Fatal(status)
	}
	release()
	if b.ID != a.ID {
		t.Fatalf("health ranking broke sticky session: %s -> %s", a.ID, b.ID)
	}
	accounts, _ := namedTextPoolCandidates(c, client)
	one := namedTextPoolPolicy(client, accounts)
	accounts[0], accounts[1] = accounts[1], accounts[0]
	if namedTextPoolPolicy(client, accounts).Revision != one.Revision {
		t.Fatal("candidate order changed policy revision")
	}
	// A delayed old generation must not poison its replacement.
	old := c.LocalAccounts[0]
	live.change(func(c *config.Config) { c.LocalAccounts[0].CredentialEpoch++ })
	s.recordExecutionOutcome(old, usageledger.Observation{Attempts: []usageledger.Attempt{{State: "failed", Reason: "rate_limited", UpstreamHTTPStatus: 429}}})
	newAccount := live.get().LocalAccounts[0]
	scope, _ := gateway.CredentialBudgetIdentity(newAccount)
	s.Health.Bind(newAccount.ID, scope)
	if !s.Health.Available(newAccount.ID) {
		t.Fatal("stale result poisoned replacement")
	}
}

func TestReliableThirdPartyHTTPStatusFailover(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
		want   int
	}{
		{429, "rate_limit", 200}, {503, "model_capacity", 200}, {503, "unavailable", 503},
	} {
		t.Run(fmt.Sprint(tc.status, tc.code), func(t *testing.T) {
			s, live := fixtureService(t)
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", "5400")
					w.WriteHeader(tc.status)
					_, _ = fmt.Fprintf(w, `{"error":{"code":%q}}`, tc.code)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, reliableResult)
			}))
			defer upstream.Close()
			live.change(func(c *config.Config) {
				for i := range c.LocalAccounts {
					a := &c.LocalAccounts[i]
					a.SourceType = "reverse_proxy"
					a.CredentialRef = ""
					a.Key = fmt.Sprintf("synthetic-key-%d", i)
					a.BaseURL = upstream.URL
					a.Protocol = "responses"
				}
			})
			enableTextPoolFixture(s, live, []string{"account-A", "account-B"}, config.PoolStrategyAuto, "")
			var observed usageledger.Observation
			s.Observe = func(o usageledger.Observation) { observed = o }
			w := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"complete input"}`)
			if w.Code != tc.want {
				t.Fatalf("status=%d body=%s", w.Code, w.Body)
			}
			wantCalls := int32(1)
			if tc.want == 200 {
				wantCalls = 2
			}
			if calls.Load() != wantCalls || len(observed.Attempts) != int(wantCalls) || observed.Attempts[0].RetryAfterMS != int64((90*time.Minute)/time.Millisecond) {
				t.Fatalf("bad attempt/Retry-After: calls=%d %+v", calls.Load(), observed)
			}
		})
	}
}

func TestReliableRetriesAreBoundedAndRevocationStopsNextDispatch(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprint(revoke), func(t *testing.T) {
			s, live := fixtureService(t)
			live.change(func(c *config.Config) {
				for i := 2; i < 5; i++ {
					a := c.LocalAccounts[0]
					a.ID = fmt.Sprintf("account-%d", i)
					a.CredentialRef = fmt.Sprintf("credential-%d", i)
					c.LocalAccounts = append(c.LocalAccounts, a)
				}
			})
			enableTextPoolFixture(s, live, []string{"account-A", "account-B", "account-2", "account-3", "account-4"}, config.PoolStrategyAuto, "")
			calls := 0
			s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
				calls++
				if revoke {
					live.change(func(c *config.Config) { c.CodexAPI.Clients[0].Disabled = true })
				}
				return nil, reliabilityError{429, "", 0}
			}
			var o usageledger.Observation
			s.Observe = func(v usageledger.Observation) { o = v }
			w := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"bounded attempts"}`)
			want := 3
			if revoke {
				want = 1
			}
			if w.Code != 429 || calls != want || len(o.Attempts) != want {
				t.Fatalf("retry budget/revocation failed: status=%d calls=%d %+v", w.Code, calls, o)
			}
		})
	}
}

func TestReliableWebSocketPrivateHistoryDoesNotMigrateDuringInitialSelection(t *testing.T) {
	s, live := fixtureService(t)
	enableTextPoolFixture(s, live, []string{"account-A", "account-B"}, config.PoolStrategyAuto, "")
	observations := make(chan usageledger.Observation, 1)
	s.Observe = func(o usageledger.Observation) { observations <- o }
	var calls atomic.Int32
	s.Official = func(_ context.Context, a config.LocalAccount, _ []byte, _, _ bool, _ func() bool) (*http.Response, error) {
		calls.Add(1)
		return successfulResponse(io.NopCloser(strings.NewReader(namedCompleted(a.ID))), true), nil
	}
	c := namedSocket(t, namedSocketServer(t, s), syntheticClientKey)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "model": "gpt-fixture", "input": "first"})
	event := namedSocketEvent(t, c, "response.completed")
	var first usageledger.Observation
	select {
	case first = <-observations:
	case <-time.After(3 * time.Second):
		t.Fatal("missing first observation")
	}
	s.Health.Record(first.AccountID, accounthealth.Result{Kind: accounthealth.RateLimit, Status: 429})
	response := event["response"].(map[string]any)
	sendNamedFrame(t, c, map[string]any{"type": "response.create", "previous_response_id": response["id"], "input": "continue"})
	rejected := namedSocketEvent(t, c, "error")
	if rejected["error"].(map[string]any)["code"] != "previous_account_unavailable" || calls.Load() != 1 {
		t.Fatalf("private history reached different credential: calls=%d %v", calls.Load(), rejected)
	}
}
