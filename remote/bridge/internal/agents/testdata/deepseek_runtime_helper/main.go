// Synthetic process used only by runtime prerequisite tests. Configuration and
// call records stay beside the copied executable in the test's temporary tree.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type configuration struct {
	Mode         string `json:"mode"`
	Log          string `json:"log"`
	NodeOutput   string `json:"nodeOutput"`
	NodeStderr   string `json:"nodeStderr"`
	NodeExit     int    `json:"nodeExit"`
	NodeSleepMS  int    `json:"nodeSleepMS"`
	EntryOutput  string `json:"entryOutput"`
	EntryStderr  string `json:"entryStderr"`
	EntryExit    int    `json:"entryExit"`
	EntrySleepMS int    `json:"entrySleepMS"`
}

func main() {
	// A newly copied synthetic executable can take longer to enter main than
	// the product's deliberately short Node probe budget. Tests use this one
	// setup-only invocation to establish process readiness without probing a
	// runtime, reading its scenario, recording calls or evaluating any entry.
	if len(os.Args) == 2 && os.Args[1] == "--salcara-fixture-ready" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		os.Exit(90)
	}
	raw, err := os.ReadFile(exe + ".json")
	var cfg configuration
	if err != nil || json.Unmarshal(raw, &cfg) != nil {
		os.Exit(91)
	}
	args := os.Args[1:]
	kind := "entry-start"
	output, stderr, code, sleepMS := cfg.EntryOutput, cfg.EntryStderr, cfg.EntryExit, cfg.EntrySleepMS
	if cfg.Mode == "node" && len(args) == 1 && args[0] == "--version" {
		kind = "node-version"
		output, stderr, code, sleepMS = cfg.NodeOutput, cfg.NodeStderr, cfg.NodeExit, cfg.NodeSleepMS
	} else if len(args) > 0 && args[len(args)-1] == "--version" {
		kind = "entry-version"
		if cfg.Mode == "native" {
			kind = "native-version"
		}
	}
	if cfg.Log != "" {
		f, err := os.OpenFile(cfg.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(92)
		}
		_ = json.NewEncoder(f).Encode(struct {
			Kind string   `json:"kind"`
			Args []string `json:"args"`
		}{kind, args})
		_ = f.Close()
	}
	if sleepMS > 0 {
		time.Sleep(time.Duration(sleepMS) * time.Millisecond)
	}
	// No shell, script interpreter, provider, network or user configuration is
	// used. Even an "entry-start" record never evaluates the supplied JS file.
	_, _ = fmt.Fprint(os.Stdout, output)
	_, _ = fmt.Fprint(os.Stderr, strings.ReplaceAll(stderr, "\r\n", "\n"))
	os.Exit(code)
}
