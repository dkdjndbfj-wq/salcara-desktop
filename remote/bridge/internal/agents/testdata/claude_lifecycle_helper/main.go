package main

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// A synthetic executable used only by lifecycle tests. It has no provider,
// network, credentials or real Claude configuration implementation.
func main() {
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			_, _ = io.WriteString(os.Stdout, "9.9.9\n")
			return
		}
	}
	dir := os.Getenv("SALCARA_CLAUDE_LIFECYCLE_DIR")
	if os.Getenv("SALCARA_CLAUDE_LIFECYCLE_SLOW_TASKKILL") == "1" && len(os.Args) > 1 && os.Args[1] == "/T" {
		pendingPID := filepath.Join(dir, "taskkill-started.tmp")
		if os.WriteFile(pendingPID, []byte(strconv.Itoa(os.Getpid())), 0600) != nil {
			os.Exit(5)
		}
		if os.Rename(pendingPID, filepath.Join(dir, "taskkill-started")) != nil {
			os.Exit(5)
		}
		// Production exec.CommandContext must terminate this owned fixture.
		// Do not interpret /PID or terminate another process from this helper.
		time.Sleep(30 * time.Second)
		return
	}
	role := os.Getenv("SALCARA_CLAUDE_LIFECYCLE_ROLE")
	if len(os.Args) > 1 && os.Args[1] == "--owned-child" {
		role = "child"
	}
	if role == "child" || role == "closed-output-alive" {
		if role == "closed-output-alive" {
			_ = os.Stdout.Close()
			_ = os.Stderr.Close()
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		return
	}
	if role == "eof-parent" {
		exe, _ := os.Executable()
		child := exec.Command(exe, "--owned-child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if child.Start() != nil {
			os.Exit(2)
		}
		// Publish only after the whole PID has been written and closed. The
		// polling test must never see an empty or partially written identifier.
		pendingPID := filepath.Join(dir, "child-pid.tmp")
		if os.WriteFile(pendingPID, []byte(strconv.Itoa(child.Process.Pid)), 0o600) != nil {
			_ = child.Process.Kill()
			os.Exit(3)
		}
		if os.Rename(pendingPID, filepath.Join(dir, "child-pid")) != nil {
			_ = child.Process.Kill()
			os.Exit(3)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		return
	}
	if role == "exit-after-byte" {
		_, _ = os.Stdin.Read(make([]byte, 1))
		return
	}
	if role == "normal" {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			encoder := json.NewEncoder(os.Stdout)
			_ = encoder.Encode(map[string]any{"type": "assistant", "message": map[string]any{"content": []map[string]any{{"type": "text", "text": "synthetic final answer"}}}})
			_ = encoder.Encode(map[string]any{"type": "result", "subtype": "success", "result": "synthetic final answer"})
		}
		return
	}
	os.Exit(4)
}
