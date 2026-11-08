package launcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
)

// Opt-in proof of Claude's actual --settings precedence using this launcher's
// pin builder. Only an isolated profile and loopback synthetic API are used.
func TestClaudeNativeLauncherAPIPinPrecedence(t *testing.T) {
	exe := os.Getenv("SALCARA_CLAUDE_E2E_EXE")
	if exe == "" {
		t.Skip("set SALCARA_CLAUDE_E2E_EXE to opt in")
	}
	if info, err := os.Stat(exe); err != nil || info.IsDir() {
		t.Fatal("native Claude fixture executable is missing")
	}
	s, profile, _ := switchFixture(t, "claude", "claude")
	s.FindTools = func(context.Context, map[string]string) []Tool {
		return []Tool{{ID: "claude", Kind: "claude", Name: "Claude Code", Path: exe, Available: true}}
	}
	for _, key := range []string{"HOME", "USERPROFILE"} {
		t.Setenv(key, profile)
	}
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	t.Setenv("DISABLE_AUTOUPDATER", "1")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	t.Setenv("ANTHROPIC_CUSTOM_HEADERS", "Authorization: synthetic-stale")
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-stale")
	project := t.TempDir()
	// Native Claude may finish before a short-lived Git helper releases cwd on
	// Windows. Retry cleanup only for this fixture's generated project; never
	// touch an installation or a real user project.
	if !filepath.IsAbs(project) || filepath.Dir(project) != filepath.Dir(profile) {
		t.Fatal("fixture project escaped its temporary root")
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := os.RemoveAll(project)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("native fixture project cleanup: %v", err)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	retained := map[string]string{}
	for _, file := range []struct{ root, name, body string }{
		{profile, "settings.json", `{"env":{"CLAUDE_CODE_USE_FOUNDRY":"1","ANTHROPIC_BASE_URL":"http://127.0.0.1:1","ANTHROPIC_API_KEY":"synthetic-old-user"}}`},
		{project, ".claude/settings.json", `{"env":{"CLAUDE_CODE_USE_BEDROCK":"1","ANTHROPIC_BASE_URL":"http://127.0.0.1:2","ANTHROPIC_API_KEY":"synthetic-old-project"}}`},
		{project, ".claude/settings.local.json", `{"env":{"CLAUDE_CODE_USE_VERTEX":"1","ANTHROPIC_BASE_URL":"http://127.0.0.1:3","ANTHROPIC_AUTH_TOKEN":"synthetic-old-local"}}`},
	} {
		path := fixtureFile(t, file.root, file.name, file.body)
		retained[path] = digest([]byte(file.body))
	}
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/hello":
			fmt.Fprint(w, "{}")
			return
		case "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-4-6","type":"model","display_name":"Synthetic model"}],"has_more":false}`)
			return
		case "/v1/messages/count_tokens":
			fmt.Fprint(w, `{"input_tokens":100}`)
			return
		case "/v1/messages":
		default:
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-native-selected" || r.Header.Get("x-api-key") != "" {
			t.Error("native request did not use the selected exclusive API credential")
		}
		var request struct {
			Model  string
			Stream bool
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&request); err != nil {
			t.Error("invalid native API payload")
		}
		if request.Model != "claude-sonnet-4-6" {
			t.Error("native request lost selected model")
		}
		calls.Add(1)
		const reply = "native-launch-pin-reply"
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"msg_native_pin","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":%q}],"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":10}}`, reply)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		data := func(event map[string]any) {
			encoded, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], encoded)
		}
		data(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_native_pin", "type": "message", "role": "assistant", "model": request.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 100, "output_tokens": 0}}})
		data(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		data(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": reply}})
		data(map[string]any{"type": "content_block_stop", "index": 0})
		data(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 10}})
		data(map[string]any{"type": "message_stop"})
	}))
	defer upstream.Close()
	a := account("claude", "synthetic-native", "synthetic-native-selected")
	a.BaseURL, a.Model = upstream.URL, "claude-sonnet-4-6"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	p, err := s.PrepareSwitch(ctx, a, "claude", project, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Args = []string{"--print", "native-launch-pin-prompt", "--output-format", "json", "--model", a.Model, "--max-turns", "1", "--no-session-persistence", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`}
	var output []byte
	s.Start = func(ctx context.Context, launch Plan) (int, error) {
		defer agents.RemoveClaudeRuntimeSettings(launch.ClaudeSettingsPin)
		cmd := exec.CommandContext(ctx, launch.Tool.Path, launch.Args...)
		cmd.Dir, cmd.Env = launch.Workspace, launch.Environment
		configureProcess(cmd, false)
		output, err = cmd.CombinedOutput()
		if err != nil {
			return 0, err
		}
		return cmd.Process.Pid, nil
	}
	if _, err := s.startWithClaudeAPIPin(ctx, p, a); err != nil {
		t.Fatalf("native fixture failed (synthetic API calls=%d): %v; output=%s", calls.Load(), err, output)
	}
	if calls.Load() == 0 || !strings.Contains(string(output), "native-launch-pin-reply") {
		t.Fatal("native launcher did not complete through the selected API")
	}
	assertRetained(t, retained)
	pins, err := filepath.Glob(filepath.Join(os.TempDir(), "salcara-claude-runtime-*"))
	if err != nil || len(pins) != 0 {
		t.Fatal("native launcher left runtime credentials")
	}
	t.Log("native Claude --settings pinned the selected API/model over ambient, user, project and local-project routing")
}
