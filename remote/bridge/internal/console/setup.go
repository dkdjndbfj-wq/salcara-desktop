package console

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/launcher"
)

// Environment check and one-click install of the command-line tools that
// phone remote coding runs on this computer (Claude Code, Codex CLI) and their
// prerequisites. Installers are the vendors' official scripts; nothing from the
// browser is ever put on a command line.

type setupItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Role        string `json:"role"`
	Installed   bool   `json:"installed"`
	Path        string `json:"path,omitempty"`
	Version     string `json:"version,omitempty"`
	Source      string `json:"source,omitempty"` // e.g. "codex-app": the CLI bundled inside the Codex app
	Installable bool   `json:"installable"`
	Required    bool   `json:"required"`
	Download    string `json:"download,omitempty"`
	Note        string `json:"note,omitempty"`
}

type setupJob struct {
	Owner     string `json:"-"` // paired direct identity; never exposed over HTTP
	RequestID string `json:"-"`
	ID        string `json:"id"`
	Tool      string `json:"tool"`
	State     string `json:"state"` // running | done | failed
	Log       string `json:"log"`
	Started   int64  `json:"started"`
	Finished  int64  `json:"finished,omitempty"`
	Error     string `json:"error,omitempty"`
}

type setupRunner struct {
	mu  sync.Mutex
	job *setupJob
	buf bytes.Buffer
	// Process-bound pairing identities cannot be replayed after restart.
	// Retain accepted direct requests even after the visible job is replaced.
	receipts map[string]*setupJob
}

var setupState = &setupRunner{}

func (s *Server) setupRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/setup", s.handleSetupStatus)
	m.HandleFunc("POST /api/setup/install", s.handleSetupInstall)
	m.HandleFunc("GET /api/setup/job", s.handleSetupJob)
}

var versionRe = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.]+)?`)

type setupVersionEntry struct {
	mod     time.Time
	version string
	pending bool
}

var setupVersions = struct {
	sync.Mutex
	entries map[string]setupVersionEntry
}{entries: map[string]setupVersionEntry{}}

// Executing a shim or asking Windows for a version is never on the HTTP
// rendering path. Cache even unsuccessful checks until the binary changes.
func setupVersionSnapshot(exe string) (string, bool) {
	info, err := os.Stat(exe)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	mod := info.ModTime()
	setupVersions.Lock()
	if cached, ok := setupVersions.entries[exe]; ok && cached.mod.Equal(mod) {
		setupVersions.Unlock()
		return cached.version, cached.pending
	}
	setupVersions.entries[exe] = setupVersionEntry{mod: mod, pending: true}
	setupVersions.Unlock()
	go func() {
		version := toolVersion(context.Background(), exe)
		setupVersions.Lock()
		if entry := setupVersions.entries[exe]; entry.mod.Equal(mod) {
			setupVersions.entries[exe] = setupVersionEntry{mod: mod, version: version}
		}
		setupVersions.Unlock()
	}()
	return "", true
}

func toolVersion(ctx context.Context, exe string) string {
	// Version text is decorative. Never make the Environment page wait for a
	// broken shim, PowerShell prompt or a sleeping package install.
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--version")
	agents.PrepareChild(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return versionRe.FindString(string(out))
}

func gitPath() (string, bool) {
	if p, err := exec.LookPath("git"); err == nil {
		return p, true
	}
	if runtime.GOOS == "windows" {
		for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs")} {
			if base == "" {
				continue
			}
			p := filepath.Join(base, "Git", "cmd", "git.exe")
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p, true
			}
		}
	}
	return "", false
}

func bundledCodex(path string) bool {
	p := strings.ToLower(strings.ReplaceAll(path, `\`, "/"))
	return strings.Contains(p, "/resources/") || strings.Contains(p, ".app/contents/")
}

func (s *Server) setupItems() ([]setupItem, bool) {
	tools, pending := s.d.Local.InventorySnapshot(s.d.Store.Get().LocalToolPaths)
	byID := map[string]launcher.Tool{}
	for _, t := range tools {
		byID[t.ID] = t
	}
	items := []setupItem{}
	if runtime.GOOS == "windows" {
		p, ok := gitPath()
		items = append(items, setupItem{ID: "git", Name: "Git", Role: "Claude Code 运行命令需要它", Installed: ok, Path: p, Installable: !ok, Required: true,
			Download: "https://git-scm.com/download/win"})
	}
	claude := byID["claude"]
	items = append(items, setupItem{ID: "claude", Name: "Claude Code", Role: "手机继续 Claude Code / Claude Desktop 的会话", Installed: claude.Available, Path: claude.Path, Installable: true, Required: true})
	sdkItem, sdkPending := setupClaudeSDKSnapshot()
	items, pending = append(items, sdkItem), pending || sdkPending
	codex := byID["codex"]
	ci := setupItem{ID: "codex", Name: "Codex CLI", Role: "手机继续 Codex / Codex App 的对话", Installed: codex.Available, Path: codex.Path, Installable: true, Required: true}
	if codex.Available && bundledCodex(codex.Path) {
		ci.Source, ci.Note = "codex-app", "正在使用 Codex App 自带的版本，可以直接用；单独安装后更新更及时"
	}
	items = append(items, ci)
	for _, entry := range []struct{ id, name, url string }{
		{"deepseek-harness", "DeepSeek Harness", "https://github.com/deepseek-ai/deepseek-harness"},
		{"hermes", "Hermes Agent", "https://hermes-agent.nousresearch.com"},
	} {
		t := byID[entry.id]
		note := ""
		if entry.id == "deepseek-harness" {
			note = "官方开发预览版；npm 安装需要 Node.js 24.9 或更高版本"
		}
		items = append(items, setupItem{ID: entry.id, Name: entry.name, Role: "可选 Agent · 独立 API 配置", Installed: t.Available, Path: t.Path, Installable: true, Download: entry.url, Note: note})
	}
	cd := byID["codex-desktop"]
	items = append(items, setupItem{ID: "codex-desktop", Name: "Codex App", Role: "桌面应用（可选）", Installed: cd.Available, Path: cd.Path, Download: "https://chatgpt.com/codex"})
	cl := byID["claude-desktop"]
	items = append(items, setupItem{ID: "claude-desktop", Name: "Claude Desktop", Role: "桌面应用（可选）", Installed: cl.Available, Path: cl.Path, Download: "https://claude.ai/download"})
	for i := range items {
		if items[i].Installed && items[i].Path != "" && (items[i].ID == "claude" || items[i].ID == "codex" || items[i].ID == "git") {
			var versionPending bool
			items[i].Version, versionPending = setupVersionSnapshot(items[i].Path)
			pending = pending || versionPending
		}
	}
	return items, pending
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" {
		s.d.Local.InvalidateInventory()
		invalidateSetupClaudeSDK()
	}
	setupState.mu.Lock()
	var job *setupJob
	if setupState.job != nil {
		j := *setupState.job
		j.Log = tailString(setupState.buf.String(), 4000)
		job = &j
	}
	setupState.mu.Unlock()
	items, pending := s.setupItems()
	s.publishPluginTools(items, pending)
	writeJSON(w, map[string]any{"os": runtime.GOOS, "items": items, "job": job, "discoveryPending": pending})
}

// installCommand returns the official installer for a tool on this OS.
func installCommand(tool string) (*exec.Cmd, error) {
	if tool == "claude-sdk" {
		return agents.ClaudeSessionSDKInstallCommand()
	}
	ps := func(script string) *exec.Cmd {
		return exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
			"[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; "+script)
	}
	sh := func(script string) *exec.Cmd { return exec.Command("/bin/bash", "-lc", script) }
	switch runtime.GOOS + "/" + tool {
	case "windows/deepseek-harness":
		return ps("$ErrorActionPreference = 'Stop'; Get-Command npm -ErrorAction Stop | Out-Null; npm install --global --engine-strict @deepseek-ai/dsh@0.2.2-rc.1; if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }"), nil
	case "darwin/deepseek-harness", "linux/deepseek-harness":
		return sh("npm install --global --engine-strict @deepseek-ai/dsh@0.2.2-rc.1"), nil
	case "windows/hermes":
		return ps("$ErrorActionPreference = 'Stop'; & ([scriptblock]::Create((irm https://hermes-agent.nousresearch.com/install.ps1))) -NonInteractive"), nil
	case "darwin/hermes", "linux/hermes":
		return sh("curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash"), nil
	case "windows/claude":
		return ps("irm https://claude.ai/install.ps1 | iex"), nil
	case "windows/codex":
		return ps("irm https://chatgpt.com/codex/install.ps1 | iex"), nil
	case "windows/git":
		return exec.Command("winget", "install", "--id", "Git.Git", "-e", "--source", "winget", "--silent",
			"--accept-package-agreements", "--accept-source-agreements"), nil
	case "darwin/claude", "linux/claude":
		return sh("curl -fsSL https://claude.ai/install.sh | bash"), nil
	case "darwin/codex", "linux/codex":
		return sh("curl -fsSL https://chatgpt.com/codex/install.sh | sh"), nil
	}
	return nil, errors.New("这个系统暂不支持一键安装，请按官网说明安装")
}

type jobWriter struct{ r *setupRunner }

func (w jobWriter) Write(p []byte) (int, error) {
	w.r.mu.Lock()
	defer w.r.mu.Unlock()
	w.r.buf.Write(p)
	if w.r.buf.Len() > 256<<10 {
		tail := tailString(w.r.buf.String(), 64<<10)
		w.r.buf.Reset()
		w.r.buf.WriteString(tail)
	}
	return len(p), nil
}

func tailString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func (s *Server) handleSetupInstall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tool string `json:"tool"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	job, status, err := s.startSetupInstall(in.Tool, "", "", r.Context().Err)
	if err != nil {
		writeErr(w, status, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "job": job})
}
func (s *Server) handleSetupJob(w http.ResponseWriter, r *http.Request) {
	setupState.mu.Lock()
	defer setupState.mu.Unlock()
	if setupState.job == nil {
		writeJSON(w, map[string]any{"job": nil})
		return
	}
	j := *setupState.job
	j.Log = tailString(setupState.buf.String(), 6000)
	writeJSON(w, map[string]any{"job": j})
}
