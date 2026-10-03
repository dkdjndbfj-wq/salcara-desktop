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
	"salcara/bridge/internal/config"
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
	ID       string `json:"id"`
	Tool     string `json:"tool"`
	State    string `json:"state"` // running | done | failed
	Log      string `json:"log"`
	Started  int64  `json:"started"`
	Finished int64  `json:"finished,omitempty"`
	Error    string `json:"error,omitempty"`
}

type setupRunner struct {
	mu  sync.Mutex
	job *setupJob
	buf bytes.Buffer
}

var setupState = &setupRunner{}

func (s *Server) setupRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/setup", s.handleSetupStatus)
	m.HandleFunc("POST /api/setup/install", s.handleSetupInstall)
	m.HandleFunc("GET /api/setup/job", s.handleSetupJob)
}

var versionRe = regexp.MustCompile(`\d+\.\d+(?:\.\d+)?(?:[-+][0-9A-Za-z.]+)?`)

func toolVersion(ctx context.Context, exe string) string {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
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

func (s *Server) setupItems(ctx context.Context) []setupItem {
	tools := s.d.Local.Tools(ctx, s.d.Store.Get().LocalToolPaths)
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
	codex := byID["codex"]
	ci := setupItem{ID: "codex", Name: "Codex CLI", Role: "手机继续 Codex / Codex App 的对话", Installed: codex.Available, Path: codex.Path, Installable: true, Required: true}
	if codex.Available && bundledCodex(codex.Path) {
		ci.Source, ci.Note = "codex-app", "正在使用 Codex App 自带的版本，可以直接用；单独安装后更新更及时"
	}
	items = append(items, ci)
	cd := byID["codex-desktop"]
	items = append(items, setupItem{ID: "codex-desktop", Name: "Codex App", Role: "桌面应用（可选）", Installed: cd.Available, Path: cd.Path, Download: "https://chatgpt.com/codex"})
	cl := byID["claude-desktop"]
	items = append(items, setupItem{ID: "claude-desktop", Name: "Claude Desktop", Role: "桌面应用（可选）", Installed: cl.Available, Path: cl.Path, Download: "https://claude.ai/download"})
	var wg sync.WaitGroup
	for i := range items {
		if items[i].Installed && items[i].Path != "" && (items[i].ID == "claude" || items[i].ID == "codex" || items[i].ID == "git") {
			wg.Add(1)
			go func(it *setupItem) { defer wg.Done(); it.Version = toolVersion(ctx, it.Path) }(&items[i])
		}
	}
	wg.Wait()
	return items
}

func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := ctxTimeout(r, 20*time.Second)
	defer cancel()
	setupState.mu.Lock()
	var job *setupJob
	if setupState.job != nil {
		j := *setupState.job
		j.Log = tailString(setupState.buf.String(), 4000)
		job = &j
	}
	setupState.mu.Unlock()
	writeJSON(w, map[string]any{"os": runtime.GOOS, "items": s.setupItems(ctx), "job": job})
}

// installCommand returns the official installer for a tool on this OS.
func installCommand(tool string) (*exec.Cmd, error) {
	ps := func(script string) *exec.Cmd {
		return exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command",
			"[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12; "+script)
	}
	sh := func(script string) *exec.Cmd { return exec.Command("/bin/bash", "-lc", script) }
	switch runtime.GOOS + "/" + tool {
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
	if in.Tool != "claude" && in.Tool != "codex" && in.Tool != "git" {
		writeErr(w, 400, "不支持的工具")
		return
	}
	cmd, err := installCommand(in.Tool)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	setupState.mu.Lock()
	if setupState.job != nil && setupState.job.State == "running" {
		setupState.mu.Unlock()
		writeErr(w, 409, "正在安装其他工具，请等它完成")
		return
	}
	job := &setupJob{ID: config.RandomToken(6), Tool: in.Tool, State: "running", Started: time.Now().UnixMilli()}
	setupState.job = job
	setupState.buf.Reset()
	setupState.mu.Unlock()

	agents.PrepareChild(cmd)
	cmd.Stdout, cmd.Stderr = jobWriter{setupState}, jobWriter{setupState}
	cmd.Env = append(os.Environ(), "CI=1", "NO_COLOR=1")
	if err := cmd.Start(); err != nil {
		setupState.mu.Lock()
		job.State, job.Error, job.Finished = "failed", "无法启动安装程序："+err.Error(), time.Now().UnixMilli()
		setupState.mu.Unlock()
		writeErr(w, 500, job.Error)
		return
	}
	s.log.Printf("setup: installing %s", in.Tool)
	go func() {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		var err error
		select {
		case err = <-done:
		case <-time.After(15 * time.Minute):
			_ = cmd.Process.Kill()
			err = errors.New("安装超时")
		}
		setupState.mu.Lock()
		job.Finished = time.Now().UnixMilli()
		if err != nil {
			job.State, job.Error = "failed", err.Error()
		} else {
			job.State = "done"
		}
		setupState.mu.Unlock()
		s.log.Printf("setup: %s finished state=%s", in.Tool, job.State)
	}()
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
