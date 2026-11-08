package pluginhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "synthetic-plugin-test-token-long-enough"

func hooksFixture(t *testing.T, fn http.HandlerFunc) *Hooks {
	t.Helper()
	server := httptest.NewServer(fn)
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	port, _ := strconv.Atoi(u.Port())
	h := &Hooks{}
	if err := h.Configure(port, testToken); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Disconnect(testToken) })
	return h
}
func TestRequestHooksKeepAuthorizationAndPolicyFields(t *testing.T) {
	h := hooksFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if bytes.Contains(raw, []byte("secret-key")) || r.Header.Get("X-Plugin-Token") != testToken {
			t.Error("credential boundary")
		}
		fmt.Fprint(w, `{"body":{"model":"forbidden","stream":true,"tools":[{"type":"image_generation"}],"input":"changed prompt","max_output_tokens":999},"headers":{"Authorization":"evil","Host":"evil","X-Salcara-Plugin-Note":"ok"}}`)
	})
	raw := json.RawMessage(`{"model":"allowed","stream":false,"max_output_tokens":16,"input":"original","tools":[]}`)
	body, headers, err := h.RunRequestHooks(context.Background(), "codex", raw, map[string]string{"Authorization": "secret-key"})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(body, &got)
	if got["input"] != "changed prompt" || got["model"] != "allowed" || got["stream"] != false || got["max_output_tokens"] != float64(16) || len(got["tools"].([]any)) != 0 {
		t.Fatalf("changed policy: %s", body)
	}
	if headers["Authorization"] != "secret-key" || headers["Host"] != "" || headers["X-Salcara-Plugin-Note"] != "ok" {
		t.Fatalf("unsafe headers: %v", headers)
	}
}
func TestFailureTimeoutAndMalformedResponsePreserveData(t *testing.T) {
	for _, mode := range []string{"http", "json", "timeout", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			h := hooksFixture(t, func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "http":
					w.WriteHeader(500)
				case "json":
					fmt.Fprint(w, `{"body":[]}`)
				case "timeout":
					select {
					case <-r.Context().Done():
					case <-time.After(100 * time.Millisecond):
					}
				case "oversize":
					fmt.Fprint(w, strings.Repeat("x", MaxPayloadBytes+1))
				}
			})
			h.client.Timeout = 40 * time.Millisecond
			before := json.RawMessage(`{"model":"m","input":"keep"}`)
			got, _, err := h.RunRequestHooks(context.Background(), "codex", before, nil)
			if err != nil || !bytes.Equal(got, before) {
				t.Fatalf("request not preserved: %s %v", got, err)
			}
			if mode != "timeout" {
				status, body, err := h.RunResponseHooks(context.Background(), "codex", before, 200, json.RawMessage(`{"output":[]}`))
				if err != nil || status != 200 || string(body) != `{"output":[]}` {
					t.Fatalf("response not preserved: %s", body)
				}
			}
		})
	}
}
func TestLeaseRenewalReplacementAndStaleCompletion(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h := hooksFixture(t, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		fmt.Fprint(w, `{"body":{"input":"late"}}`)
	})
	before := json.RawMessage(`{"input":"original"}`)
	done := make(chan []byte, 1)
	go func() { body, _, _ := h.RunRequestHooks(context.Background(), "codex", before, nil); done <- body }()
	<-started
	ep, _, _ := h.snapshot()
	if err := h.Configure(ep.port, "different-desktop-plugin-token"); err != nil {
		t.Fatal(err)
	}
	if h.Disconnect(testToken) {
		t.Fatal("old desktop disconnected new lease")
	}
	close(release)
	if body := <-done; !bytes.Equal(body, before) {
		t.Fatal("old runtime mutated request")
	}
	h.mu.Lock()
	h.current.expires = time.Now().Add(-time.Second)
	h.mu.Unlock()
	if h.Enabled() {
		t.Fatal("expired lease enabled")
	}
	if err := h.Configure(ep.port, "different-desktop-plugin-token"); err != nil || !h.Enabled() {
		t.Fatal("renew failed")
	}
	h.Disconnect("different-desktop-plugin-token")
}
func TestStreamObservationsDoNotBufferForwardingAndAreBounded(t *testing.T) {
	events := make(chan string, 100)
	h := hooksFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		events <- string(raw)
		fmt.Fprint(w, `{}`)
	})
	dest := httptest.NewRecorder()
	writer := h.ObserveStream(dest, context.Background(), "codex")
	frame := "event: delta\ndata: {\"type\":\"delta\",\"text\":\"hello\"}\n\n"
	for _, part := range []string{frame[:11], frame[11:]} {
		_, _ = writer.Write([]byte(part))
	}
	writer.(http.Flusher).Flush()
	if dest.Body.String() != frame || !dest.Flushed {
		t.Fatal("stream forwarding changed")
	}
	select {
	case event := <-events:
		if !strings.Contains(event, `"event":"stream"`) || !strings.Contains(event, "hello") {
			t.Fatal(event)
		}
	case <-time.After(time.Second):
		t.Fatal("no stream event")
	}
	sw := writer.(*streamWriter)
	large := "data: " + strings.Repeat("x", maxEventBytes*2) + "\n\n"
	_, _ = writer.Write([]byte(large))
	_, _ = writer.Write([]byte(frame))
	if len(sw.buffer) > maxEventBytes || sw.skip {
		t.Fatal("unbounded frame retention")
	}
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("valid event after oversize lost")
	}
	if dest.Body.Len() != len(frame)*2+len(large) {
		t.Fatal("oversize event damaged transport")
	}
}
func TestEventQueueDoesNotBlockRequestAndDoesNotGrow(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	h := hooksFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		once.Do(func() { close(entered) })
		<-release
		fmt.Fprint(w, `{}`)
	})
	h.FireEvent(context.Background(), "usage", map[string]int{"tokens": 1})
	<-entered
	started := time.Now()
	for i := 0; i < 1000; i++ {
		h.FireEvent(context.Background(), "usage", map[string]int{"tokens": i})
	}
	if time.Since(started) > time.Second {
		t.Fatal("enqueue blocked")
	}
	h.mu.Lock()
	queued := len(h.queue)
	h.mu.Unlock()
	if queued > maxQueuedEvents {
		t.Fatalf("queue %d", queued)
	}
	h.Disconnect(testToken)
	close(release)
}

func TestMalformedPromptAndInvalidAnnotationAreIgnored(t *testing.T) {
	original := json.RawMessage(`{"model":"m","input":"original"}`)
	if actual := mergePrompt(original, json.RawMessage(`{"input":123}`)); !bytes.Equal(actual, original) {
		t.Fatalf("malformed prompt applied: %s", actual)
	}
	headers := SafeHeaders(map[string]string{"X-Salcara-Plugin-Foo:bad": "value", "X-Salcara-Plugin-Note": "bad\x01value", "X-Salcara-Plugin-Good": "value"})
	if len(headers) != 1 || headers["X-Salcara-Plugin-Good"] != "value" {
		t.Fatalf("invalid header accepted: %v", headers)
	}
}
