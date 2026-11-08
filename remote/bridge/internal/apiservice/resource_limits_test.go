package apiservice

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

type cancellableBody struct {
	entered   chan struct{}
	release   chan struct{}
	reader    *strings.Reader
	readOnce  sync.Once
	closeOnce sync.Once
	closed    atomic.Bool
}

func newCancellableBody() *cancellableBody {
	return &cancellableBody{entered: make(chan struct{}), release: make(chan struct{}), reader: strings.NewReader(`{"model":"gpt-fixture","input":"synthetic"}`)}
}
func (b *cancellableBody) Read(p []byte) (int, error) {
	b.readOnce.Do(func() { close(b.entered); <-b.release })
	return b.reader.Read(p)
}
func (b *cancellableBody) Close() error {
	b.closed.Store(true)
	b.closeOnce.Do(func() { close(b.release) })
	return nil
}

type countingBody struct{ reads atomic.Int32 }

func (b *countingBody) Read([]byte) (int, error) { b.reads.Add(1); return 0, io.EOF }
func (*countingBody) Close() error               { return nil }

func TestSlowBodyOccupiesClientAdmissionBeforeReadingRejectedBodies(t *testing.T) {
	s, _ := fixtureService(t)
	body := newCancellableBody()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	w := httptest.NewRecorder()
	go func() { defer close(done); s.ServeHTTP(w, apiRequest(ctx, syntheticClientKey, body)) }()
	<-body.entered
	stats := s.Snapshot()
	if len(stats) != 1 || stats[0].Inflight != 1 || stats[0].Requests != 1 {
		t.Fatalf("slow body not admitted/tracked: %+v", stats)
	}
	rejectedBody := &countingBody{}
	rejected := httptest.NewRecorder()
	s.ServeHTTP(rejected, apiRequest(context.Background(), syntheticClientKey, rejectedBody))
	if rejected.Code != 429 || rejectedBody.reads.Load() != 0 {
		t.Fatalf("rejection read body: status=%d reads=%d", rejected.Code, rejectedBody.reads.Load())
	}
	cancel()
	awaitDone(t, done)
	if w.Code != 408 || !body.closed.Load() {
		t.Fatalf("cancel did not close body/release slot: %d", w.Code)
	}
	if stats = s.Snapshot(); stats[0].Inflight != 0 || stats[0].Failed != 1 {
		t.Fatalf("cancel stats: %+v", stats)
	}
}

func TestSlowBodiesShareGlobalAdmissionBound(t *testing.T) {
	s, live := fixtureService(t)
	s.maxInflight = 2
	thirdKey := "synthetic-third-client-key"
	live.change(func(c *config.Config) {
		c.CodexAPI.Clients = append(c.CodexAPI.Clients, config.NamedAPIClient{ID: "client-C", Name: "C", AccountID: "account-A", KeyDigest: config.APIClientDigest(thirdKey), MaxConcurrency: 1})
	})
	var cancels []context.CancelFunc
	var dones []chan struct{}
	for _, key := range []string{syntheticClientKey, syntheticKeyB} {
		body := newCancellableBody()
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		done := make(chan struct{})
		dones = append(dones, done)
		go func() { defer close(done); s.ServeHTTP(httptest.NewRecorder(), apiRequest(ctx, key, body)) }()
		<-body.entered
	}
	rejectedBody := &countingBody{}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, apiRequest(context.Background(), thirdKey, rejectedBody))
	if w.Code != 429 || rejectedBody.reads.Load() != 0 {
		t.Fatalf("global cap: %d reads=%d", w.Code, rejectedBody.reads.Load())
	}
	for i, cancel := range cancels {
		cancel()
		awaitDone(t, dones[i])
	}
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	if w = callAPI(s, thirdKey, `{"model":"gpt-fixture","input":"synthetic"}`); w.Code != 200 {
		t.Fatalf("global slot leaked: %d %s", w.Code, w.Body)
	}
}

func TestBodyReadTimeoutReleasesAdmissionWithoutForwarding(t *testing.T) {
	s, _ := fixtureService(t)
	s.bodyReadTimeout = 25 * time.Millisecond
	var calls atomic.Int32
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		calls.Add(1)
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	body := newCancellableBody()
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { defer close(done); s.ServeHTTP(w, apiRequest(context.Background(), syntheticClientKey, body)) }()
	awaitDone(t, done)
	if w.Code != 408 || calls.Load() != 0 || !body.closed.Load() {
		t.Fatalf("body timeout: status=%d calls=%d closed=%v", w.Code, calls.Load(), body.closed.Load())
	}
	if next := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"synthetic"}`); next.Code != 200 {
		t.Fatalf("timeout slot leaked: %d", next.Code)
	}
}

func TestSocketSlowBodyHasReadDeadlineAndReturnsTimeout(t *testing.T) {
	s, _ := fixtureService(t)
	s.bodyReadTimeout = 40 * time.Millisecond
	server := httptest.NewServer(s)
	defer server.Close()
	connection, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
	_, err = fmt.Fprintf(connection, "POST /codex-api/v1/responses HTTP/1.1\r\nHost: fixture\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\nContent-Type: application/json\r\n\r\n{", syntheticClientKey)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := connection.Read(buf)
	if err != nil || !strings.Contains(string(buf[:n]), "408 Request Timeout") {
		t.Fatalf("socket upload timeout: %q err=%v", string(buf[:n]), err)
	}
}

type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu        sync.Mutex
	deadlines []time.Time
}

func (w *deadlineRecorder) SetReadDeadline(deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadlines = append(w.deadlines, deadline)
	return nil
}
func (w *deadlineRecorder) deadlineCleared() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.deadlines) >= 2 && w.deadlines[len(w.deadlines)-1].IsZero()
}

func TestBodyDeadlineClearsAndRevokedAlreadyForwardedStreamDrains(t *testing.T) {
	s, live := fixtureService(t)
	s.bodyReadTimeout = 20 * time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	producerDone := make(chan struct{})
	w := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	s.Official = func(ctx context.Context, _ config.LocalAccount, _ []byte, stream, compact bool, _ func() bool) (*http.Response, error) {
		if !stream || compact || !w.deadlineCleared() {
			t.Error("body deadline leaked to downstream")
		}
		reader, writer := io.Pipe()
		go func() {
			defer close(producerDone)
			defer writer.Close()
			_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
			close(entered)
			<-release
			if ctx.Err() != nil {
				t.Error("body timer cancelled an admitted stream")
			}
			_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
		}()
		return successfulResponse(reader, true), nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.ServeHTTP(w, apiRequest(context.Background(), syntheticClientKey, strings.NewReader(`{"model":"gpt-fixture","stream":true,"input":"synthetic"}`)))
	}()
	<-entered
	live.change(func(c *config.Config) { c.CodexAPI.Clients = c.CodexAPI.Clients[1:] })
	stats := s.Snapshot()
	if len(stats) != 1 || stats[0].Inflight != 1 {
		t.Fatalf("revoked active stream stats evicted: %+v", stats)
	}
	select {
	case <-done:
		t.Fatal("revocation interrupted already-forwarded stream")
	case <-time.After(60 * time.Millisecond):
	}
	close(release)
	awaitDone(t, done)
	awaitDone(t, producerDone)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("stream terminal lost: %d %s", w.Code, w.Body)
	}
	if stats = s.Snapshot(); len(stats) != 0 {
		t.Fatalf("revoked idle stats retained: %+v", stats)
	}
}

func TestStatsEvictRevokedIdleClientsDuringChurn(t *testing.T) {
	s, live := fixtureService(t)
	s.Official = func(context.Context, config.LocalAccount, []byte, bool, bool, func() bool) (*http.Response, error) {
		return successfulResponse(io.NopCloser(strings.NewReader(fixtureResponse)), false), nil
	}
	for i := 0; i < 80; i++ {
		id := fmt.Sprintf("fixture-client-%d", i)
		live.change(func(c *config.Config) {
			c.CodexAPI.Clients = []config.NamedAPIClient{{ID: id, Name: id, AccountID: "account-A", KeyDigest: config.APIClientDigest(syntheticClientKey)}}
		})
		if w := callAPI(s, syntheticClientKey, `{"model":"gpt-fixture","input":"synthetic"}`); w.Code != 200 {
			t.Fatalf("churn request: %d", w.Code)
		}
		if stats := s.Snapshot(); len(stats) != 1 || stats[0].ClientID != id || stats[0].Inflight != 0 {
			t.Fatalf("revoked stats accumulated: %+v", stats)
		}
	}
	live.change(func(c *config.Config) { c.CodexAPI.Clients = nil })
	if stats := s.Snapshot(); len(stats) != 0 {
		t.Fatalf("last revoked stats retained: %+v", stats)
	}
}
