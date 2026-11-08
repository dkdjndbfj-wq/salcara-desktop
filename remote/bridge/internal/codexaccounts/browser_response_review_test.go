package codexaccounts

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"
)

type browserLateEOFReader struct {
	ctx    context.Context
	reader *strings.Reader
	waited chan struct{}
}

func (r *browserLateEOFReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		// A buffered response can deliver a clean EOF after the login deadline.
		// The exchange must check cancellation even when ReadAll returns nil.
		<-r.ctx.Done()
		close(r.waited)
	}
	return n, err
}

func (r *browserLateEOFReader) Close() error { return nil }

func TestBrowserLateTokenBodyCannotCompleteExpiredServiceLogin(t *testing.T) {
	waited := make(chan struct{})
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: &browserLateEOFReader{
				ctx: req.Context(), reader: strings.NewReader(`{"access_token":"synthetic-late-browser-access","refresh_token":"synthetic-late-browser-refresh"}`), waited: waited,
			},
			Request: req,
		}, nil
	})}
	connector, err := NewBrowserConnector(BrowserOptions{
		Timeout: 500 * time.Millisecond,
		FixtureEndpoints: &BrowserFixtureEndpoints{
			AuthURL: "http://127.0.0.1:1/authorize", TokenURL: "http://127.0.0.1:1/token",
			RedirectURI: "http://127.0.0.1:0/auth/callback", Client: client,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, _, _ := newTestService(t, nil, connector)
	login, err := service.StartLogin(context.Background(), LoginRequest{})
	if err != nil {
		t.Fatal(err)
	}
	progress := waitBrowserProgress(t, service, login.ID)
	authorize, err := url.Parse(progress.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	query := authorize.Query()
	callbackURL := query.Get("redirect_uri") + "?code=synthetic-late-code&state=" + url.QueryEscape(query.Get("state"))
	if err := service.SubmitLoginCallback(login.ID, callbackURL); err != nil {
		t.Fatal(err)
	}
	finished := waitLogin(t, service, login.ID)
	select {
	case <-waited:
	default:
		t.Fatal("fixture did not deliver a clean EOF after the flow deadline")
	}
	if finished.Status != "failed" || finished.ErrorCode != "expired" || finished.Account != nil || len(service.List()) != 0 {
		t.Fatalf("expired browser response: status=%s errorCode=%s accountReturned=%t persisted=%d", finished.Status, finished.ErrorCode, finished.Account != nil, len(service.List()))
	}
}

func TestBrowserTokenBodyDeadlinePreservesExpiredError(t *testing.T) {
	bodyStarted := make(chan struct{})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"synthetic-partial`)
		w.(http.Flusher).Flush()
		close(bodyStarted)
		<-r.Context().Done()
	}))
	defer provider.Close()
	connector, err := NewBrowserConnector(BrowserOptions{
		FixtureEndpoints: &BrowserFixtureEndpoints{
			AuthURL: provider.URL + "/authorize", TokenURL: provider.URL + "/token",
			RedirectURI: "http://127.0.0.1:0/auth/callback", Client: provider.Client(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err = connector.exchange(ctx, "http://127.0.0.1:1455/auth/callback", "synthetic-code", "synthetic-verifier")
	select {
	case <-bodyStarted:
	default:
		t.Fatal("test did not reach token response body")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("token response body deadline returned %v instead of expired", err)
	}
}

// Exercise the actual loopback handler and connector shutdown together: an
// accepted callback must reach the browser even when token exchange is fast.
func TestBrowserAcceptedCallbackResponseReachesBrowser(t *testing.T) {
	previousProcs := runtime.GOMAXPROCS(4)
	defer runtime.GOMAXPROCS(previousProcs)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"synthetic-browser-response-access","refresh_token":"synthetic-browser-response-refresh"}`)
	}))
	defer provider.Close()
	connector, err := NewBrowserConnector(BrowserOptions{
		Timeout: 3 * time.Second,
		FixtureEndpoints: &BrowserFixtureEndpoints{
			AuthURL: provider.URL + "/authorize", TokenURL: provider.URL + "/token",
			RedirectURI: "http://127.0.0.1:0/auth/callback", Client: provider.Client(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 3 * time.Second, Transport: transport}
	var lostResponses int
	var firstFailure string
	const attempts = 256
	for i := 0; i < attempts; i++ {
		progress := make(chan LoginProgress, 1)
		done := make(chan browserLoginOutcome, 1)
		go func() {
			record, loginErr := connector.LoginWithProgress(context.Background(), func(p LoginProgress) { progress <- p })
			done <- browserLoginOutcome{record: record, err: loginErr}
		}()
		var p LoginProgress
		select {
		case p = <-progress:
		case <-time.After(4 * time.Second):
			t.Fatal("browser connector did not publish progress")
		}
		authorize, parseErr := url.Parse(p.AuthorizationURL)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		query := authorize.Query()
		callbackURL := query.Get("redirect_uri") + "?code=synthetic-response-code&state=" + url.QueryEscape(query.Get("state"))
		response, callbackErr := client.Get(callbackURL)
		if callbackErr != nil {
			lostResponses++
			if firstFailure == "" {
				firstFailure = callbackErr.Error()
			}
		} else {
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK || readErr != nil || !strings.Contains(string(body), "Authorization received") {
				lostResponses++
				if firstFailure == "" {
					firstFailure = "callback response was incomplete"
				}
			}
		}
		outcome := <-done
		if outcome.err != nil || outcome.record == nil {
			t.Fatalf("attempt %d login failed: %v", i, outcome.err)
		}
	}
	if lostResponses != 0 {
		t.Fatalf("%d/%d accepted callbacks lost their HTTP response despite successful login; first failure: %s", lostResponses, attempts, firstFailure)
	}
}
