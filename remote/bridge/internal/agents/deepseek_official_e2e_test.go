package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in: point at an isolated official npm/native dsh installation. The only
// provider is an in-process fixture; no real model API or user DSH_HOME is used.
func TestDeepSeekOfficialSDKWithSyntheticUpstream(t *testing.T) {
	runDeepSeekOfficialFixture(t, false)
}

func TestDeepSeekOfficialSDKInterruptWithSyntheticUpstream(t *testing.T) {
	runDeepSeekOfficialFixture(t, true)
}

func runDeepSeekOfficialFixture(t *testing.T, interruptOnly bool) {
	exe := os.Getenv("SALCARA_DSH_OFFICIAL_E2E_PATH")
	if exe == "" {
		t.Skip("requires an explicitly selected isolated official dsh installation")
	}
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		upper := strings.ToUpper(name)
		if strings.HasSuffix(upper, "_API_KEY") || strings.HasSuffix(upper, "_TOKEN") || strings.HasSuffix(upper, "_SECRET") {
			t.Setenv(name, "")
		}
	}
	var calls atomic.Int32
	var expectedKey atomic.Value
	expectedKey.Store("synthetic-dsh-e2e-key")
	var hold atomic.Bool
	hold.Store(interruptOnly)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/b/v1/chat/completions") || r.Header.Get("Authorization") != "Bearer "+expectedKey.Load().(string) {
			http.Error(w, "unexpected fixture route", 400)
			return
		}
		var request struct {
			Model    string          `json:"model"`
			Stream   bool            `json:"stream"`
			Messages json.RawMessage `json:"messages"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&request) != nil || (request.Model != "salcara-fixture" && request.Model != "salcara-fixture-2") {
			http.Error(w, "unexpected fixture model", 400)
			return
		}
		calls.Add(1)
		if calls.Load() > 1 && (!strings.Contains(string(request.Messages), "one short sentence") || !strings.Contains(string(request.Messages), "Synthetic DeepSeek SDK reply")) {
			t.Error("API change lost the original native conversation context")
			http.Error(w, "missing original context", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"salcara-fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Synthetic DeepSeek SDK reply\"},\"finish_reason\":null}]}\n\n")
		if hold.Load() {
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":\"salcara-fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":5,\"total_tokens\":9}}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	settings := Settings{DeepSeekPath: exe, Approval: "ask", Extensions: map[string]ExtensionSettings{deepSeekID: {Root: server.URL, Key: "synthetic-dsh-e2e-key"}}}
	state := t.TempDir()
	a := newDeepSeekAgent(func() Settings { return settings }, state)
	defer a.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if detected := a.Detect(ctx); !detected.Available || detected.Version == "" {
		t.Fatalf("official detection failed: %+v", detected)
	}
	id, err := a.StartWithOptions(ctx, t.TempDir(), "Reply with one short sentence. Do not call tools.", "", TurnOptions{Model: "salcara-fixture"})
	if err != nil {
		a.mu.Lock()
		var diagnostic string
		for _, s := range a.sessions {
			if s.conn != nil {
				diagnostic = s.conn.stderrText()
			}
		}
		a.mu.Unlock()
		t.Fatalf("official SDK startup: %v; diagnostic: %s", err, diagnostic)
	}
	completed := int32(0)
	for ctx.Err() == nil {
		info, events, err := a.Open(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Status == "failed" {
			t.Fatalf("official SDK failed: %+v", events)
		}
		if interruptOnly && info.ActiveTurnID != "" && calls.Load() == 1 {
			result, err := a.InterruptTurn(ctx, id, info.ActiveTurnID, "01234567-1234-1234-1234-123456789abc")
			if err != nil || result.Status != "stopped" {
				t.Fatal("official exact-turn shutdown", result, err)
			}
			return
		}
		if info.Status == "idle" {
			found := false
			for _, event := range events {
				found = found || event.Role == "assistant" && event.Text == "Synthetic DeepSeek SDK reply"
			}
			completed++
			if !found || calls.Load() != completed {
				t.Fatalf("official SDK output missing or extra provider requests (%d): %+v", calls.Load(), events)
			}
			if completed == 1 {
				next := settings.Extensions[deepSeekID]
				next.Key, next.Root = "synthetic-dsh-key-B", server.URL+"/b"
				settings.Extensions[deepSeekID] = next
				expectedKey.Store(next.Key)
				if err := a.SendWithOptions(ctx, id, "Reply again. Do not call tools.", TurnOptions{Model: "salcara-fixture-2"}); err != nil {
					t.Fatal("official model change/resume", err)
				}
				continue
			}
			if completed == 2 {
				a.Close()
				next := settings.Extensions[deepSeekID]
				next.Key, next.Root = "synthetic-dsh-e2e-key", server.URL
				settings.Extensions[deepSeekID] = next
				expectedKey.Store(next.Key)
				a = newDeepSeekAgent(func() Settings { return settings }, state)
				defer a.Close()
				if err := a.Send(ctx, id, "Reply after restoring the session. Do not call tools."); err != nil {
					t.Fatal("official persisted restore", err)
				}
				continue
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(ctx.Err())
}
