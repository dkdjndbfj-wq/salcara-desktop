// Loopback-only integration fixture. Never accepts a real provider URL/key.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/gateway"
)

func main() {
	protocol := "chat"
	if len(os.Args) > 1 {
		protocol = os.Args[1]
	}
	if protocol != "chat" && protocol != "anthropic" {
		panic("only synthetic protocols")
	}
	var mu sync.Mutex
	calls, modelOK, patchCalls, historyOK := 0, true, 0, false
	var toolNames []string
	u := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in struct {
			Model string           `json:"model"`
			Tools []map[string]any `json:"tools"`
		}
		_ = json.Unmarshal(body, &in)
		mu.Lock()
		calls++
		n := calls
		modelOK = modelOK && in.Model == "non-openai-fixture"
		historyOK = historyOK || strings.Contains(string(body), "fixture-patch-marker") || strings.Contains(string(body), "protocol-smoke-")
		mu.Unlock()
		if r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		toolName := ""
		if n == 2 {
			for _, tool := range in.Tools {
				name, _ := tool["name"].(string)
				if f, ok := tool["function"].(map[string]any); ok {
					name, _ = f["name"].(string)
				}
				mu.Lock()
				toolNames = append(toolNames, name)
				mu.Unlock()
				if strings.HasSuffix(name, "apply_patch") || (toolName == "" && name == "shell_command") {
					toolName = name
					if strings.HasSuffix(name, "apply_patch") {
						break
					}
				}
			}
		}
		patch := "*** Begin Patch\n*** Add File: fixture-result.txt\n+fixture-patch-marker\n*** End Patch"
		arguments := map[string]string{"input": patch}
		if toolName == "shell_command" {
			arguments = map[string]string{"command": "Get-Location"}
		}
		if toolName != "" {
			mu.Lock()
			patchCalls++
			mu.Unlock()
		}
		data := func(value any) { b, _ := json.Marshal(value); fmt.Fprintf(w, "data: %s\n\n", b) }
		if protocol == "chat" {
			delta := map[string]any{"role": "assistant", "content": "Synthetic Grok-compatible reply"}
			finish := "stop"
			if toolName != "" {
				args, _ := json.Marshal(arguments)
				delta["content"] = nil
				delta["tool_calls"] = []any{map[string]any{"index": 0, "id": "fixture-call", "type": "function", "function": map[string]any{"name": toolName, "arguments": string(args)}}}
				finish = "tool_calls"
			}
			data(map[string]any{"id": "fixture-chat", "object": "chat.completion.chunk", "created": 1, "model": in.Model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}})
			data(map[string]any{"id": "fixture-chat", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}})
			data(map[string]any{"id": "fixture-chat", "choices": []any{}, "usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 10, "total_tokens": 110}})
			fmt.Fprint(w, "data: [DONE]\n\n")
		} else {
			data(map[string]any{"type": "message_start", "message": map[string]any{"id": "fixture-msg", "role": "assistant", "type": "message", "model": in.Model, "content": []any{}, "usage": map[string]int{"input_tokens": 100, "output_tokens": 0}}})
			block := map[string]any{"type": "text", "text": ""}
			delta := map[string]any{"type": "text_delta", "text": "Synthetic Claude-compatible reply"}
			reason := "end_turn"
			if toolName != "" {
				block = map[string]any{"type": "tool_use", "id": "fixture-call", "name": toolName, "input": map[string]any{}}
				args, _ := json.Marshal(arguments)
				delta = map[string]any{"type": "input_json_delta", "partial_json": string(args)}
				reason = "tool_use"
			}
			data(map[string]any{"type": "content_block_start", "index": 0, "content_block": block})
			data(map[string]any{"type": "content_block_delta", "index": 0, "delta": delta})
			data(map[string]any{"type": "content_block_stop", "index": 0})
			data(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": reason, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 10}})
			data(map[string]any{"type": "message_stop"})
		}
	}))
	defer u.Close()
	a := config.LocalAccount{ID: "fixture", Kind: "codex", Model: "non-openai-fixture", Protocol: protocol, BaseURL: u.URL, Key: "synthetic-upstream-credential", AuthMode: "bearer"}
	mux := http.NewServeMux()
	mux.Handle("/gateway/", gateway.New(func() string { return "synthetic-loopback-credential" }, func(string) (config.LocalAccount, bool) { return a, true }))
	mux.HandleFunc("/fixture-stats", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"calls": calls, "modelOK": modelOK, "patchCalls": patchCalls, "historyOK": historyOK, "toolNames": toolNames})
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	fmt.Println("http://" + ln.Addr().String())
	if err := http.Serve(ln, mux); err != nil {
		panic(err)
	}
}
