// Package launcher manages local BYOK profiles independently of remote sessions.
package launcher

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"salcara/bridge/internal/agents"
)

type Tool struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Available bool   `json:"available"`
	Path      string `json:"path,omitempty"`
	Custom    bool   `json:"custom"`
	Family    string `json:"-"`
	AppID     string `json:"-"`
}

var toolNames = map[string]string{
	"codex": "Codex CLI", "codex-desktop": "Codex Desktop",
	"claude": "Claude Code", "claude-desktop": "Claude Desktop",
}

func ValidTarget(id string) bool { _, ok := toolNames[id]; return ok }

func Discover(ctx context.Context, paths map[string]string) []Tool {
	packages := platformPackages(ctx)
	var running map[string]Tool
	list := []Tool{}
	for _, id := range []string{"codex-desktop", "codex", "claude-desktop", "claude"} {
		t := Tool{ID: id, Name: toolNames[id], Kind: strings.Split(id, "-")[0]}
		t.Custom = paths[id] != ""
		if !strings.HasSuffix(id, "-desktop") {
			t.Path, t.Available = agents.LocalExecutable(id, paths[id])
		} else if paths[id] != "" {
			t.Path = desktopExecutable(paths[id], t.Kind)
			t.Available = fileExists(t.Path)
		} else if pkg, ok := packages[t.Kind]; ok {
			t.Path, t.Family, t.AppID = pkg.Path, pkg.Family, pkg.AppID
			t.Available = fileExists(t.Path)
		} else {
			for _, p := range desktopCandidates(t.Kind) {
				if exe := desktopExecutable(p, t.Kind); fileExists(exe) {
					t.Path, t.Available = exe, true
					break
				}
			}
		}
		if !t.Available && !t.Custom && strings.HasSuffix(id, "-desktop") {
			if running == nil {
				running = runningDesktopTools(ctx)
			}
			if live, ok := running[t.Kind]; ok && fileExists(live.Path) {
				t.Path, t.Family, t.AppID, t.Available = live.Path, live.Family, live.AppID, true
			}
		}
		// Custom Store paths must retain package identity as well.
		if t.Custom && strings.HasSuffix(id, "-desktop") {
			if pkg, ok := packages[t.Kind]; ok && strings.EqualFold(t.Path, pkg.Path) {
				t.Family, t.AppID = pkg.Family, pkg.AppID
			}
		}
		list = append(list, t)
	}
	// A nonstandard desktop installation can still provide its bundled CLI.
	// Resolve from the detected app location instead of relying on PATH.
	for i := range list {
		if list[i].ID != "codex" || list[i].Available || list[i].Custom {
			continue
		}
		for _, desktop := range list {
			if desktop.ID != "codex-desktop" || !desktop.Available {
				continue
			}
			for _, p := range bundledCLICandidates(desktop.Path) {
				if fileExists(p) {
					list[i].Path, list[i].Available = p, true
					break
				}
			}
		}
	}
	return list
}

func bundledCLICandidates(exe string) []string {
	dir := filepath.Dir(exe)
	if runtime.GOOS == "darwin" {
		return []string{filepath.Join(dir, "..", "Resources", "codex"), filepath.Join(dir, "..", "Resources", "bin", "codex")}
	}
	if runtime.GOOS == "windows" {
		return []string{filepath.Join(dir, "resources", "codex.exe"), filepath.Join(dir, "resources", "bin", "codex.exe")}
	}
	return nil
}

func desktopExecutable(path, kind string) string {
	if runtime.GOOS == "darwin" && strings.HasSuffix(strings.ToLower(path), ".app") {
		name := "Codex"
		if kind == "claude" {
			name = "Claude"
		}
		return filepath.Join(path, "Contents", "MacOS", name)
	}
	return path
}

func desktopCandidates(kind string) []string {
	home, _ := os.UserHomeDir()
	name := "Codex"
	if kind == "claude" {
		name = "Claude"
	}
	switch runtime.GOOS {
	case "darwin":
		return []string{filepath.Join("/Applications", name+".app"), filepath.Join(home, "Applications", name+".app")}
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			local = filepath.Join(home, "AppData", "Local")
		}
		out := []string{}
		if kind == "claude" {
			entries, _ := filepath.Glob(filepath.Join(local, "AnthropicClaude", "app-*", "claude.exe"))
			// Compare numeric version parts rather than app-1.9 sorting after app-1.10.
			sort.Slice(entries, func(i, j int) bool {
				return versionLess(filepath.Base(filepath.Dir(entries[j])), filepath.Base(filepath.Dir(entries[i])))
			})
			out = append(out, entries...)
			out = append(out, filepath.Join(local, "AnthropicClaude", "claude.exe"))
		}
		return append(out, filepath.Join(local, "Programs", name, name+".exe"), filepath.Join(os.Getenv("ProgramFiles"), name, name+".exe"))
	default:
		return []string{filepath.Join(home, ".local", "bin", strings.ToLower(name)+"-desktop"), filepath.Join("/opt", strings.ToLower(name), strings.ToLower(name))}
	}
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	s, err := os.Stat(path)
	return err == nil && !s.IsDir()
}

func versionLess(a, b string) bool {
	// Numeric components padded to fixed length for the known Squirrel layout.
	pad := func(s string) string {
		parts := strings.FieldsFunc(strings.TrimPrefix(s, "app-"), func(r rune) bool { return r < '0' || r > '9' })
		for i, p := range parts {
			if len(p) < 10 {
				parts[i] = strings.Repeat("0", 10-len(p)) + p
			}
		}
		return strings.Join(parts, ".")
	}
	return pad(a) < pad(b)
}
