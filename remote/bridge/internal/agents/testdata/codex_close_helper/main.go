package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Owned fixture only: it never contacts a provider or loads a user config.
func main() {
	dir := os.Getenv("SALCARA_CODEX_CLOSE_DIR")
	role := os.Getenv("SALCARA_CODEX_CLOSE_ROLE")
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(2)
		}
		if req.Method == "initialize" && (role == "init-barrier" || role == "notify-barrier") {
			// Publish only a complete marker. The test observes this concurrently;
			// WriteFile exposes an empty/truncated file before the PID is written.
			pending := filepath.Join(dir, "init-owned-pid.pending")
			if os.WriteFile(pending, []byte(strconv.Itoa(os.Getpid())), 0o600) != nil || os.Rename(pending, filepath.Join(dir, "init-owned-pid")) != nil {
				os.Exit(3)
			}
			if role == "notify-barrier" {
				_ = encoder.Encode(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": "synthetic-thread", "turn": map[string]any{"id": "synthetic-turn", "status": "inProgress"}}})
			} else {
				deadline := time.Now().Add(20 * time.Second)
				for time.Now().Before(deadline) {
					if _, err := os.Stat(filepath.Join(dir, "release-init")); err == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
		}
		if len(req.ID) != 0 {
			_ = encoder.Encode(map[string]any{"id": req.ID, "result": map[string]any{}})
		}
	}
}
