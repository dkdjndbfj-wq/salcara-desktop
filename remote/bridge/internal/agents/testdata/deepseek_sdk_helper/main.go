// Synthetic official-wire fixture: no network, provider or user executable.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("dsh fixture 0.0.1")
		return
	}
	writer := json.NewEncoder(os.Stdout)
	write := func(v map[string]any) { v["jsonrpc"] = "2.0"; _ = writer.Encode(v) }
	reply := func(id any, result any) { write(map[string]any{"id": id, "result": result}) }
	var session, prompt string
	var waiting any
	var seq int
	finished := false
	event := func(kind string, data any) {
		seq++
		write(map[string]any{"method": "session.event", "params": map[string]any{"sessionId": session, "event": map[string]any{"seq": seq, "time": 1700000000000, "type": kind, "data": data}}})
	}
	complete := func() {
		event("assistant/message", map[string]any{"turn": 1, "step": 1, "message": map[string]any{"id": "answer", "role": "assistant", "content": []any{map[string]string{"type": "text", "text": "fixture complete"}}}, "usage": map[string]int{"inputTokens": 3, "outputTokens": 2}})
		event("turn/end", map[string]any{"turn": 1, "reason": map[string]string{"kind": "completed"}})
		write(map[string]any{"method": "session.status", "params": map[string]string{"sessionId": session, "status": "idle"}})
		finished = true
		if waiting != nil {
			reply(waiting, map[string]any{})
			waiting = nil
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 4<<20)
	for scanner.Scan() {
		var frame struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      any             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			Result  json.RawMessage `json:"result"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil || frame.JSONRPC != "2.0" {
			os.Exit(21)
		}
		if frame.Method == "" {
			if frame.ID == "approval-fixture" {
				complete()
			}
			continue
		}
		if frame.Method == "initialize" || frame.Method == "session/prompt" {
			// The synthetic child observes the actual persisted gate, so this
			// catches regressions that start/send before writing the restart fence.
			data, err := os.ReadFile(filepath.Join(os.Getenv("DSH_HOME"), "..", "session.json"))
			var checkpoint struct {
				RuntimePending bool `json:"runtimePending"`
				Info           struct {
					Status string `json:"status"`
				} `json:"info"`
			}
			if err != nil || json.Unmarshal(data, &checkpoint) != nil || !checkpoint.RuntimePending || checkpoint.Info.Status != "running" {
				fmt.Fprintln(os.Stderr, "missing durable runtime fence before execution")
				os.Exit(24)
			}
		}
		switch frame.Method {
		case "initialize":
			var p struct {
				Provider string `json:"provider"`
				Approval bool   `json:"approvalRequests"`
			}
			_ = json.Unmarshal(frame.Params, &p)
			if p.Provider != "salcara" || !p.Approval || os.Getenv("SALCARA_DSH_API_KEY") == "" {
				os.Exit(22)
			}
			reply(frame.ID, map[string]any{"serverInfo": map[string]string{"name": "deepseek-harness-sdk-runtime", "version": "fixture"}})
		case "session/resume":
			var p struct {
				SessionID string `json:"sessionId"`
			}
			_ = json.Unmarshal(frame.Params, &p)
			if _, err := os.Stat(filepath.Join(os.Getenv("DSH_HOME"), p.SessionID)); err != nil {
				os.Exit(23)
			}
			reply(frame.ID, map[string]any{})
		case "session/prompt":
			var p struct {
				SessionID string `json:"sessionId"`
				Content   []struct {
					Text string `json:"text"`
				} `json:"contentBlocks"`
			}
			_ = json.Unmarshal(frame.Params, &p)
			session = p.SessionID
			prompt = p.Content[0].Text
			finished = false
			_ = os.WriteFile(filepath.Join(os.Getenv("DSH_HOME"), session), []byte("native-session-fixture"), 0600)
			reply(frame.ID, map[string]string{"messageId": "fixture-input"})
			write(map[string]any{"method": "session.event", "params": map[string]any{"sessionId": "foreign-session", "event": map[string]any{"type": "assistant/message", "data": map[string]any{"message": map[string]any{"id": "foreign", "role": "assistant", "content": []any{map[string]string{"type": "text", "text": "MUST NOT LEAK"}}}}}}})
			write(map[string]any{"method": "session.status", "params": map[string]string{"sessionId": session, "status": "running"}})
			event("turn/start", map[string]int{"turn": 1})
			event("user/message", map[string]any{"id": "user-input", "role": "user", "source": map[string]string{"kind": "user"}, "content": []any{map[string]string{"type": "text", "text": prompt}}})
			if prompt == "approval" {
				write(map[string]any{"id": "approval-fixture", "method": "approval/request", "params": map[string]string{"sessionId": session, "toolName": "write", "reason": "Synthetic fixture"}})
			} else if prompt != "hold" {
				complete()
			}
		case "session/wait":
			if finished {
				reply(frame.ID, map[string]any{})
			} else {
				waiting = frame.ID
			}
		case "shutdown":
			reply(frame.ID, map[string]any{})
			return
		}
	}
}
