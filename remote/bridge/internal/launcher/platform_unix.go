//go:build !windows

package launcher

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func configureProcess(cmd *exec.Cmd, terminal bool)    {}
func platformPackages(context.Context) map[string]Tool { return map[string]Tool{} }
func shLiteral(s string) string                        { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func startPlatform(ctx context.Context, p Plan) (int, error) {
	if p.RuntimeDir == "" {
		p.RuntimeDir = p.ProfileDir
	}
	if isDesktop(p.Tool.ID) {
		return startDirect(p, false)
	}
	// Terminal on macOS is a singleton and does not reliably inherit an open
	// caller's environment. Put only this profile's tool environment in its script.
	script := "#!/bin/sh\n"
	for _, key := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "SUB2API_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "CLAUDE_CODE_OAUTH_TOKEN", "ELECTRON_RUN_AS_NODE"} {
		script += "unset " + key + "\n"
	}
	for _, kv := range p.Environment {
		k, v, _ := strings.Cut(kv, "=")
		if k == "PATH" || k == "CODEX_HOME" || k == "CLAUDE_CONFIG_DIR" || strings.HasPrefix(k, "ANTHROPIC_") {
			script += "export " + k + "=" + shLiteral(v) + "\n"
		}
	}
	script += "cd " + shLiteral(p.Workspace) + " || exit 1\nexec " + shLiteral(p.Tool.Path)
	for _, a := range p.Args {
		script += " " + shLiteral(a)
	}
	script += "\n"
	path := filepath.Join(p.RuntimeDir, "launch-cli.command")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return 0, err
	}
	if runtime.GOOS == "darwin" {
		cmd := exec.CommandContext(ctx, "open", "-a", "Terminal", path)
		if err := cmd.Run(); err != nil {
			return 0, errors.New("无法打开 Terminal，请手动运行本地实例目录中的 launch-cli.command")
		}
		return 0, nil
	}
	for _, name := range []string{"x-terminal-emulator", "gnome-terminal", "konsole", "xterm"} {
		if exe, err := exec.LookPath(name); err == nil {
			args := []string{"-e", path}
			if name == "gnome-terminal" {
				args = []string{"--", path}
			}
			terminal := p
			terminal.Tool.Path, terminal.Args = exe, args
			return startDirect(terminal, true)
		}
	}
	return 0, errors.New("没有找到桌面终端，请手动运行实例目录中的 launch-cli.command")
}

func originalAppData(t Tool) string {
	if runtime.GOOS == "darwin" {
		key, name := "CODEX_ELECTRON_USER_DATA_PATH", "Codex"
		if t.Kind == "claude" {
			key, name = "CLAUDE_USER_DATA_DIR", "Claude"
		}
		if os.Getenv(key) == "" {
			home, _ := os.UserHomeDir()
			return filepath.Join(home, "Library", "Application Support", name)
		}
	}
	return defaultAppData(t)
}

func stopPlatform(ctx context.Context, p Plan) error {
	if runtime.GOOS != "darwin" {
		return errors.New("此系统暂不支持安全自动退出桌面程序；请使用 CLI 切换，或先手动配置原程序")
	}
	app := strings.Split(p.Tool.Path, ".app/")[0] + ".app"
	quote := func(value string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"` }
	script := "if application " + quote(app) + " is running then tell application " + quote(app) + " to quit"
	if err := exec.CommandContext(ctx, "osascript", "-e", script).Run(); err != nil {
		return errors.New("原桌面程序未正常退出，请先手动退出后再切换")
	}
	return nil
}
