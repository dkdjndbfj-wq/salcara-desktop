package mcpapproval

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type channelWriter chan []byte

func (w channelWriter) Write(b []byte) (int, error) {
	w <- append([]byte(nil), b...)
	return len(b), nil
}

type helperHarness struct {
	in   *io.PipeWriter
	out  channelWriter
	done chan error
}

func startHelper(t *testing.T, endpoint string) *helperHarness {
	t.Helper()
	in, writer := io.Pipe()
	h := &helperHarness{in: writer, out: make(channelWriter, 128), done: make(chan error, 1)}
	go func() {
		err := Serve(in, h.out, endpoint, "synthetic-token", "synthetic-session")
		_ = in.Close()
		h.done <- err
	}()
	t.Cleanup(func() { _ = writer.Close() })
	return h
}

func (h *helperHarness) send(t *testing.T, s string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := io.WriteString(h.in, s+"\n"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("helper input stalled")
	}
}

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for helper")
	}
	var zero T
	return zero
}

func approvalCall(id, tool string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"tools/call","params":{"name":"approve","arguments":{"tool_name":%q,"input":{},"tool_use_id":"synthetic-tool"}}}`, id, tool)
}

func cancelCall(id string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%s}}`, id)
}

func requireNoOutput(t *testing.T, ch channelWriter) {
	t.Helper()
	select {
	case out := <-ch:
		t.Fatalf("unexpected output: %s", out)
	default:
	}
}

func TestCancellationTargetsExactRequestAndSuppressesLateAllow(t *testing.T) {
	started, cancelled := make(chan string, 4), make(chan string, 4)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Tool string `json:"tool_name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		started <- req.Tool
		select {
		case <-r.Context().Done():
			cancelled <- req.Tool
		case <-release:
		}
		// Simulate a bridge finishing concurrently with host cancellation.
		_, _ = io.WriteString(w, `{"behavior":"allow","updatedInput":{}}`)
	}))
	t.Cleanup(srv.Close)
	h := startHelper(t, srv.URL)
	h.send(t, approvalCall(`1`, "numeric"))
	h.send(t, approvalCall(`"1"`, "string"))
	_ = receive(t, started)
	_ = receive(t, started)
	h.send(t, cancelCall(`1`))
	if got := receive(t, cancelled); got != "numeric" {
		t.Fatalf("cancelled wrong ID domain: %s", got)
	}
	close(release)
	var reply struct {
		ID     json.RawMessage
		Result json.RawMessage
	}
	if err := json.Unmarshal(receive(t, h.out), &reply); err != nil {
		t.Fatal(err)
	}
	if string(reply.ID) != `"1"` || !bytes.Contains(reply.Result, []byte(`allow`)) {
		t.Fatalf("wrong surviving result: %+v", reply)
	}
	_ = h.in.Close()
	if err := receive(t, h.done); err != nil {
		t.Fatal(err)
	}
	requireNoOutput(t, h.out)
}

func TestEOFImmediatelyCancelsPendingHTTP(t *testing.T) {
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	h := startHelper(t, srv.URL)
	h.send(t, approvalCall(`1`, "Bash"))
	_ = receive(t, started)
	_ = h.in.Close()
	if err := receive(t, h.done); err != nil {
		t.Fatal(err)
	}
	_ = receive(t, cancelled)
	requireNoOutput(t, h.out)
}

func TestOversizedInputCancelsExistingApproval(t *testing.T) {
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	h := startHelper(t, srv.URL)
	h.send(t, approvalCall(`1`, "Bash"))
	_ = receive(t, started)
	// Serve closes the reader on the hard limit, so a pipe writer may receive
	// ErrClosedPipe for the excess suffix. Neither outcome is a valid request.
	written := make(chan error, 1)
	go func() { _, err := io.WriteString(h.in, strings.Repeat("x", maxMessageBytes+1)); written <- err }()
	if err := receive(t, h.done); !errors.Is(err, errMessageTooLarge) {
		t.Fatalf("hard limit: %v", err)
	}
	_ = receive(t, written)
	_ = receive(t, cancelled)
	requireNoOutput(t, h.out)
}

func TestDuplicateInflightIDClosesAndCancelsWithoutAmbiguousReply(t *testing.T) {
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	h := startHelper(t, srv.URL)
	h.send(t, approvalCall(`"same"`, "Bash"))
	_ = receive(t, started)
	h.send(t, approvalCall(`"same"`, "Write"))
	if err := receive(t, h.done); err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("duplicate accepted: %v", err)
	}
	_ = receive(t, cancelled)
	if calls.Load() != 1 {
		t.Fatal("duplicate request reached bridge")
	}
	requireNoOutput(t, h.out)
}

func TestCompletedRequestIDCannotBeReused(t *testing.T) {
	h := startHelper(t, "http://127.0.0.1:1")
	h.send(t, `{"jsonrpc":"2.0","id":"same","method":"ping"}`)
	_ = receive(t, h.out)
	h.send(t, cancelCall(`"same"`))
	h.send(t, approvalCall(`"same"`, "Bash"))
	if err := receive(t, h.done); err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("completed ID reused: %v", err)
	}
	requireNoOutput(t, h.out)
}

func TestCancelledRequestIDCannotBeReused(t *testing.T) {
	started, cancelled := make(chan struct{}, 1), make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	h := startHelper(t, srv.URL)
	h.send(t, approvalCall(`"old"`, "Bash"))
	_ = receive(t, started)
	h.send(t, cancelCall(`"old"`))
	_ = receive(t, cancelled)
	h.send(t, approvalCall(`"old"`, "Write"))
	if err := receive(t, h.done); err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("cancelled ID reused: %v", err)
	}
	requireNoOutput(t, h.out)
}

func TestConcurrencyLimitDoesNotBlockCancellation(t *testing.T) {
	started, cancelled := make(chan struct{}, maxInFlight), make(chan struct{}, maxInFlight)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		<-r.Context().Done()
		cancelled <- struct{}{}
	}))
	t.Cleanup(srv.Close)
	h := startHelper(t, srv.URL)
	for i := 0; i < maxInFlight; i++ {
		h.send(t, approvalCall(fmt.Sprint(i), "Bash"))
	}
	for i := 0; i < maxInFlight; i++ {
		_ = receive(t, started)
	}
	h.send(t, approvalCall(`999`, "Write"))
	if reply := receive(t, h.out); !bytes.Contains(reply, []byte(`too many pending`)) {
		t.Fatalf("missing capacity rejection: %s", reply)
	}
	h.send(t, cancelCall(`0`))
	_ = receive(t, cancelled)
	_ = h.in.Close()
	if err := receive(t, h.done); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxInFlight; i++ {
		_ = receive(t, cancelled)
	}
	if calls.Load() != maxInFlight {
		t.Fatal("capacity rejection reached HTTP handler")
	}
	requireNoOutput(t, h.out)
}

func TestMalformedAndUnknownCancellationDoesNotCancelOtherRequest(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var cancelled atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			cancelled.Store(true)
		}
		_, _ = io.WriteString(w, `{"behavior":"deny","message":"synthetic"}`)
	}))
	t.Cleanup(srv.Close)
	h := startHelper(t, srv.URL)
	h.send(t, approvalCall(`1`, "Bash"))
	_ = receive(t, started)
	for _, raw := range []string{
		cancelCall(`2`), cancelCall(`"1"`), cancelCall(`null`), cancelCall(`{}`),
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2,"requestId":1}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"RequestID":2,"requestId":1}}`,
	} {
		h.send(t, raw)
	}
	close(release)
	_ = receive(t, h.out)
	_ = h.in.Close()
	if err := receive(t, h.done); err != nil {
		t.Fatal(err)
	}
	if cancelled.Load() {
		t.Fatal("invalid cancellation affected pending approval")
	}
}

func TestToolsCallNotificationHasNoSideEffects(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(srv.Close)
	var out bytes.Buffer
	in := strings.Replace(approvalCall(`1`, "Bash"), `"id":1,`, "", 1)
	if err := Serve(strings.NewReader(in), &out, srv.URL, "synthetic", "synthetic"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || out.Len() != 0 {
		t.Fatal("notification launched an approval or produced a reply")
	}
}

func TestInputLineHasHardLimitBeforeNewline(t *testing.T) {
	for _, suffix := range []string{"", "\n"} {
		var out bytes.Buffer
		in := strings.Repeat(" ", maxMessageBytes+1) + suffix
		if err := Serve(strings.NewReader(in), &out, "http://127.0.0.1:1", "synthetic", "synthetic"); !errors.Is(err, errMessageTooLarge) {
			t.Fatalf("oversized line accepted: %v", err)
		}
		if out.Len() != 0 {
			t.Fatal("oversized input emitted a response")
		}
	}
	line := strings.Repeat(" ", maxMessageBytes-1) + "\n"
	if got, err := boundedLine(bufio.NewReaderSize(strings.NewReader(line), 1024)); err != nil || len(got) != maxMessageBytes {
		t.Fatalf("exact bound rejected: %d %v", len(got), err)
	}
}

func TestRequestIDCanonicalization(t *testing.T) {
	for _, raw := range []string{`null`, `true`, `{}`, `[]`, `1.0`, `1e0`, `"` + strings.Repeat("x", maxRequestIDBytes) + `"`} {
		if _, ok := requestKey(json.RawMessage(raw)); ok {
			t.Fatalf("invalid ID accepted: %s", raw)
		}
	}
	a, _ := requestKey(json.RawMessage(`9007199254740992`))
	b, _ := requestKey(json.RawMessage(`9007199254740993`))
	if a == b {
		t.Fatal("large IDs collided")
	}
	a, _ = requestKey(json.RawMessage(`"same"`))
	b, _ = requestKey(json.RawMessage(`"s\u0061me"`))
	if a != b {
		t.Fatal("escaped equivalent string IDs differ")
	}
	a, _ = requestKey(json.RawMessage(`-0`))
	b, _ = requestKey(json.RawMessage(`0`))
	if a != b {
		t.Fatal("zero IDs differ")
	}
}

func TestHTTPTimeoutReturnsDenyInsteadOfHangingHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.Copy(io.Discard, r.Body); <-r.Context().Done() }))
	t.Cleanup(srv.Close)
	client := &http.Client{Timeout: 30 * time.Millisecond}
	result := callApprove(context.Background(), client, srv.URL, "synthetic", "synthetic", json.RawMessage(`{"name":"approve","arguments":{"tool_name":"Bash","input":{}}}`))
	b, _ := json.Marshal(result)
	if !bytes.Contains(b, []byte(`\"behavior\":\"deny\"`)) {
		t.Fatalf("timeout did not deny: %s", b)
	}
}

func TestOversizedDecisionCannotProduceAllow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"behavior":"allow","updatedInput":{"value":"`+strings.Repeat("x", maxDecisionBytes)+`"}}`)
	}))
	t.Cleanup(srv.Close)
	result := callApprove(context.Background(), srv.Client(), srv.URL, "synthetic", "synthetic", json.RawMessage(`{"name":"approve","arguments":{"tool_name":"Bash","input":{}}}`))
	b, _ := json.Marshal(result)
	if !bytes.Contains(b, []byte(`\"behavior\":\"deny\"`)) {
		t.Fatal("oversized decision allowed")
	}
}

func TestSessionRequestHistoryIsBounded(t *testing.T) {
	var in strings.Builder
	for i := 0; i <= maxSessionRequests; i++ {
		fmt.Fprintf(&in, "{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"ping\"}\n", i)
	}
	err := Serve(strings.NewReader(in.String()), io.Discard, "http://127.0.0.1:1", "synthetic", "synthetic")
	if err == nil || !strings.Contains(err.Error(), "session request limit") {
		t.Fatalf("unbounded session history: %v", err)
	}
}

func TestRedirectCannotForwardPermissionToken(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		_, _ = io.WriteString(w, `{"behavior":"allow"}`)
	}))
	t.Cleanup(target.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)
	h := startHelper(t, srv.URL)
	h.send(t, approvalCall(`1`, "Bash"))
	if out := receive(t, h.out); !bytes.Contains(out, []byte(`\"behavior\":\"deny\"`)) {
		t.Fatalf("redirect should deny: %s", out)
	}
	_ = h.in.Close()
	if err := receive(t, h.done); err != nil {
		t.Fatal(err)
	}
	if redirected.Load() != 0 {
		t.Fatal("permission request followed redirect")
	}
}
