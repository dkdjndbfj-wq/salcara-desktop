package agents

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

// In-process metadata RPC fixtures cannot launch a process, resume a thread,
// read personal history or contact any inference provider.
type metadataRPCTransport struct {
	t      *testing.T
	conn   *rpcConn
	thread map[string]any
	fail   bool
	hook   func()
	calls  []string
}

func (s *metadataRPCTransport) Close() error { return nil }

func (s *metadataRPCTransport) Write(data []byte) (int, error) {
	var request struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return 0, err
	}
	s.calls = append(s.calls, request.Method)
	if request.Method != "thread/read" || request.Params["includeTurns"] != false || request.Params["threadId"] != "fixture-old" {
		s.t.Errorf("metadata lookup broadened into a non-metadata RPC: %+v", request)
	}
	response := map[string]any{"id": request.ID, "result": map[string]any{"thread": s.thread}}
	if s.fail {
		delete(response, "result")
		response["error"] = map[string]any{"code": -32601, "message": "SYNTHETIC_PRIVATE_RPC_STATE"}
	}
	if s.hook != nil {
		s.hook()
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return 0, err
	}
	s.conn.handleLine(encoded)
	return len(data), nil
}

func metadataCodexFixture(t *testing.T) (*codexAgent, *metadataRPCTransport, *recorder) {
	t.Helper()
	a, _, rec := newResumeSafetyAgent(t, cxThread{}, false)
	transport := &metadataRPCTransport{t: t, conn: a.conn, thread: map[string]any{
		"id": "fixture-old", "cwd": t.TempDir(), "updatedAt": int64(1730000000),
		"status": map[string]any{"type": "idle"}, "preview": "SYNTHETIC_PRIVATE_MESSAGE",
		"turns": []any{map[string]any{"items": []any{map[string]any{"text": "SYNTHETIC_PRIVATE_MESSAGE"}}}},
	}}
	a.conn.stdin = transport
	return a, transport, rec
}

func TestCodexSessionMetadataExactPureRead(t *testing.T) {
	a, transport, rec := metadataCodexFixture(t)
	info, err := a.SessionMetadata(context.Background(), "fixture-old")
	if err != nil {
		t.Fatal(err)
	}
	if info.SessionKey != "codex:fixture-old" || info.Tool != "codex" || info.Cwd != filepath.Clean(transport.thread["cwd"].(string)) || info.Status != "idle" || info.UpdatedAt != 1730000000000 || info.Controllable {
		t.Fatalf("incorrect exact metadata: %+v", info)
	}
	if !reflect.DeepEqual(transport.calls, []string{"thread/read"}) || len(a.threads) != 0 || len(rec.all()) != 0 {
		t.Fatalf("metadata lookup changed session ownership/history or emitted events: calls=%v threads=%d events=%d", transport.calls, len(a.threads), len(rec.all()))
	}
	encoded, _ := json.Marshal(info)
	if strings.Contains(string(encoded), "SYNTHETIC_PRIVATE_MESSAGE") || info.Title != "" || info.Model != "" {
		t.Fatal("metadata leaked or mapped conversation content")
	}
}

func TestCodexSessionMetadataFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*metadataRPCTransport)
	}{
		{"wrong-id", func(s *metadataRPCTransport) { s.thread["id"] = "fixture-another" }},
		{"ephemeral", func(s *metadataRPCTransport) { s.thread["ephemeral"] = true }},
		{"missing-cwd", func(s *metadataRPCTransport) { delete(s.thread, "cwd") }},
		{"relative-cwd", func(s *metadataRPCTransport) { s.thread["cwd"] = "relative/project" }},
		{"control-cwd", func(s *metadataRPCTransport) { s.thread["cwd"] = filepath.Join(t.TempDir(), "bad\nproject") }},
		{"unsupported-host", func(s *metadataRPCTransport) { s.fail = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, transport, rec := metadataCodexFixture(t)
			tc.change(transport)
			if _, err := a.SessionMetadata(context.Background(), "fixture-old"); err == nil {
				t.Fatal("unverified metadata accepted")
			} else if strings.Contains(err.Error(), "SYNTHETIC_PRIVATE") {
				t.Fatal("metadata lookup exported raw host content through an error")
			} else {
				stage := map[string]string{"wrong-id": "session_identity_mismatch", "ephemeral": "ephemeral_session", "unsupported-host": "native_metadata_read"}[tc.name]
				if stage == "" {
					stage = "invalid_working_directory"
				}
				if !errors.Is(err, errSessionMetadata) || !strings.Contains(err.Error(), "("+stage+")") {
					t.Fatalf("safe metadata failure stage lost: %v", err)
				}
			}
			if !reflect.DeepEqual(transport.calls, []string{"thread/read"}) || len(a.threads) != 0 || len(rec.all()) != 0 {
				t.Fatal("failure fell back to history/mutation")
			}
		})
	}
}

func TestCodexSessionMetadataCancellationAndConnectionFence(t *testing.T) {
	for _, boundary := range []string{"before", "after-reply", "connection-replaced", "closed"} {
		t.Run(boundary, func(t *testing.T) {
			a, transport, rec := metadataCodexFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch boundary {
			case "before":
				cancel()
			case "after-reply":
				transport.hook = cancel
			case "connection-replaced", "closed":
				if boundary == "closed" {
					t.Cleanup(func() {
						a.mu.Lock()
						a.closed = false // The synthetic fence did not close closeCh.
						a.mu.Unlock()
					})
				}
				transport.hook = func() {
					a.mu.Lock()
					defer a.mu.Unlock()
					if boundary == "closed" {
						a.closed = true
					} else {
						a.conn = &rpcConn{done: make(chan struct{})}
					}
				}
			}
			if _, err := a.SessionMetadata(ctx, "fixture-old"); err == nil {
				t.Fatal("canceled or stale metadata returned")
			}
			if boundary == "before" && len(transport.calls) != 0 {
				t.Fatal("pre-canceled request reached RPC")
			}
			if len(a.threads) != 0 || len(rec.all()) != 0 {
				t.Fatal("stale metadata changed state")
			}
		})
	}
}

func TestCodexSessionMetadataRejectsInvalidIDsWithoutRPC(t *testing.T) {
	a, transport, _ := metadataCodexFixture(t)
	for _, id := range []string{"", "../fixture-old", "codex:fixture-old", strings.Repeat("a", 257)} {
		if _, err := a.SessionMetadata(context.Background(), id); err == nil {
			t.Fatalf("invalid session ID accepted: %q", id)
		}
	}
	if len(transport.calls) != 0 {
		t.Fatal("invalid IDs reached the transport")
	}
}

func metadataClaudeFixture(t *testing.T, id string, data []byte) (*claudeAgent, string, *recorder) {
	t.Helper()
	home := t.TempDir()
	project := filepath.Join(home, "projects", "fixture-project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, id+".jsonl")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings { return Settings{} }, "", home)
	t.Cleanup(a.Close)
	return a, path, rec
}

func metadataClaudeRow(t *testing.T, fields map[string]any) string {
	t.Helper()
	data, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func TestClaudeSessionMetadataExactOlderSessionAndBoundedSpans(t *testing.T) {
	first, latest := t.TempDir(), t.TempDir()
	head := metadataClaudeRow(t, map[string]any{"sessionId": "fixture-old", "cwd": first,
		"message": map[string]any{"content": "SYNTHETIC_PRIVATE_MESSAGE"}, "model": "SYNTHETIC_PRIVATE_MODEL"})
	// A mismatched identity in the middle MUST NOT be scanned: this lookup
	// reads bounded metadata spans rather than the full transcript.
	middle := metadataClaudeRow(t, map[string]any{"sessionId": "fixture-other", "cwd": first})
	tail := metadataClaudeRow(t, map[string]any{"session_id": "fixture-old", "cwd": latest, "entrypoint": "claude-desktop"})
	a, path, rec := metadataClaudeFixture(t, "fixture-old", []byte(head+strings.Repeat("X", 300<<10)+"\n"+middle+strings.Repeat("X", 300<<10)+"\n"+tail))
	oldTime := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 101; i++ {
		other := filepath.Join(filepath.Dir(path), "newer-"+strconv.Itoa(i)+".jsonl")
		if err := os.WriteFile(other, []byte("SYNTHETIC_UNRELATED_HISTORY\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	info, err := a.SessionMetadata(context.Background(), "fixture-old")
	if err != nil {
		t.Fatal(err)
	}
	if info.SessionKey != "claude:fixture-old" || info.Cwd != latest || info.Client != "Claude Desktop" || info.UpdatedAt != oldTime.UnixMilli() || info.Controllable {
		t.Fatalf("wrong bounded metadata: %+v", info)
	}
	if len(a.hist.cache) != 0 || len(a.live) != 0 || len(a.procs) != 0 || len(rec.all()) != 0 {
		t.Fatal("metadata lookup populated history or launched a worker")
	}
	encoded, _ := json.Marshal(info)
	if strings.Contains(string(encoded), "SYNTHETIC_PRIVATE") || info.Title != "" || info.Model != "" {
		t.Fatal("metadata exposed transcript content")
	}
}

func TestClaudeSessionMetadataRejectsIdentityAndUnsafeCwd(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  map[string]any
	}{
		{"wrong-session-id", map[string]any{"sessionId": "fixture-other", "cwd": t.TempDir()}},
		{"wrong-stream-id", map[string]any{"session_id": "fixture-other", "cwd": t.TempDir()}},
		{"relative-cwd", map[string]any{"sessionId": "fixture-old", "cwd": "relative"}},
		{"missing-cwd", map[string]any{"sessionId": "fixture-old", "message": "SYNTHETIC_PRIVATE_MESSAGE"}},
		{"control-cwd", map[string]any{"sessionId": "fixture-old", "cwd": filepath.Join(t.TempDir(), "bad\nproject")}},
		{"overlong-cwd", map[string]any{"sessionId": "fixture-old", "cwd": filepath.Join(t.TempDir(), strings.Repeat("a", 4096))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _, _ := metadataClaudeFixture(t, "fixture-old", []byte(metadataClaudeRow(t, tc.row)))
			if _, err := a.SessionMetadata(context.Background(), "fixture-old"); err == nil {
				t.Fatal("invalid transcript metadata accepted")
			}
		})
	}
}

func TestClaudeSessionMetadataIgnoresChildMetadata(t *testing.T) {
	cwd := t.TempDir()
	data := metadataClaudeRow(t, map[string]any{"sessionId": "fixture-old", "cwd": cwd}) +
		metadataClaudeRow(t, map[string]any{"sessionId": "fixture-child", "cwd": t.TempDir(), "isSidechain": true}) +
		metadataClaudeRow(t, map[string]any{"sessionId": "fixture-child", "cwd": t.TempDir(), "parent_tool_use_id": "fixture-tool"})
	a, _, _ := metadataClaudeFixture(t, "fixture-old", []byte(data))
	info, err := a.SessionMetadata(context.Background(), "fixture-old")
	if err != nil || info.Cwd != cwd {
		t.Fatalf("child metadata was used for parent workspace: %+v %v", info, err)
	}
}

func TestClaudeSessionMetadataLiveFallbackIsMissingOnly(t *testing.T) {
	cwd := t.TempDir()
	a, path, _ := metadataClaudeFixture(t, "fixture-old", []byte("invalid synthetic transcript\n"))
	a.live["fixture-old"] = protocol.SessionInfo{SessionKey: "claude:fixture-old", Client: "Claude Code", Cwd: cwd,
		Title: "SYNTHETIC_PRIVATE_MESSAGE", Model: "SYNTHETIC_PRIVATE_MODEL", Status: "running", Controllable: true}
	if _, err := a.SessionMetadata(context.Background(), "fixture-old"); err == nil {
		t.Fatal("invalid transcript silently fell back to stale live metadata")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	info, err := a.SessionMetadata(context.Background(), "fixture-old")
	if err != nil || info.Cwd != cwd || info.Title != "" || info.Model != "" || info.Controllable {
		t.Fatalf("missing-only live metadata was not sanitized: %+v %v", info, err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SessionMetadata(context.Background(), "fixture-old"); err == nil {
		t.Fatal("non-regular transcript silently used live fallback")
	}
}

// Context hooks allow deterministic read-boundary tests without modifying
// production adapters or racing the scheduler against filesystem I/O.
type metadataHookContext struct {
	context.Context
	mu    sync.Mutex
	n     int
	at    int
	onErr func()
}

func (c *metadataHookContext) Err() error {
	c.mu.Lock()
	c.n++
	invoke := c.n == c.at
	c.mu.Unlock()
	if invoke {
		c.onErr()
	}
	return c.Context.Err()
}

func TestClaudeSessionMetadataCancellationAndSnapshotFence(t *testing.T) {
	for _, boundary := range []string{"before", "during-read", "changed-size", "changed-same-size", "closed"} {
		t.Run(boundary, func(t *testing.T) {
			cwd := t.TempDir()
			data := []byte(metadataClaudeRow(t, map[string]any{"sessionId": "fixture-old", "cwd": cwd}))
			a, path, rec := metadataClaudeFixture(t, "fixture-old", data)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var requestCtx context.Context = ctx
			if boundary == "before" {
				cancel()
			} else {
				requestCtx = &metadataHookContext{Context: ctx, at: 4, onErr: func() {
					switch boundary {
					case "during-read":
						cancel()
					case "changed-size":
						if err := os.WriteFile(path, append(append([]byte{}, data...), '\n'), 0o600); err != nil {
							t.Fatal(err)
						}
					case "changed-same-size":
						changed := time.Now().Add(time.Hour)
						if err := os.Chtimes(path, changed, changed); err != nil {
							t.Fatal(err)
						}
					case "closed":
						a.Close()
					}
				}}
			}
			if _, err := a.SessionMetadata(requestCtx, "fixture-old"); err == nil {
				t.Fatal("canceled, changed or stale metadata returned")
			} else if boundary == "before" || boundary == "during-read" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation was lost: %v", err)
				}
			}
			if len(a.hist.cache) != 0 || len(a.procs) != 0 || len(rec.all()) != 0 {
				t.Fatal("failed metadata changed runtime state")
			}
		})
	}
}

func TestClaudeSessionMetadataRejectsOutsideTranscriptLink(t *testing.T) {
	a, path, _ := metadataClaudeFixture(t, "fixture-old", nil)
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	data := []byte(metadataClaudeRow(t, map[string]any{"sessionId": "fixture-old", "cwd": t.TempDir()}))
	if err := os.WriteFile(outside, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skip("synthetic symlink fixture is unavailable on this OS")
	}
	if _, err := a.SessionMetadata(context.Background(), "fixture-old"); err == nil {
		t.Fatal("metadata namespace expanded through an outside symlink")
	}
}

func TestClaudeSessionMetadataRejectsPathIdentityReplacement(t *testing.T) {
	data := []byte(metadataClaudeRow(t, map[string]any{"sessionId": "fixture-old", "cwd": t.TempDir()}))
	a, path, _ := metadataClaudeFixture(t, "fixture-old", data)
	original := filepath.Join(filepath.Dir(path), "original.jsonl")
	other := filepath.Join(filepath.Dir(path), "replacement.jsonl")
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, data, 0o600); err != nil {
		t.Fatal(err)
	}
	// Equal contents, size and time are not sufficient: identity must match.
	stamp := time.Now().Add(-time.Hour)
	for _, p := range []string{original, other} {
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(original, path); err != nil {
		t.Skip("synthetic symlink fixture is unavailable on this OS")
	}
	ctx := &metadataHookContext{Context: context.Background(), at: 4, onErr: func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, path); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := a.SessionMetadata(ctx, "fixture-old"); err == nil {
		t.Fatal("a different final transcript identity was accepted")
	}
}
