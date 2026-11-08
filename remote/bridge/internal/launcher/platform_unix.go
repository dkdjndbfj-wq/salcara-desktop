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

func startPlatform(ctx context.Context, p Plan) (pid int, launchErr error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if p.RuntimeDir == "" {
		p.RuntimeDir = p.ProfileDir
	}
	if isDesktop(p.Tool.ID) {
		LaunchPhase(ctx, "start", "创建工具进程", "创建桌面应用进程")
		return startDirectContext(ctx, p, false)
	}
	path := filepath.Join(p.RuntimeDir, "launch-cli.command")
	if p.ClaudeSettingsPin != "" {
		// The script also exports the API environment for a singleton Terminal.
		// Keep it in the same private directory, unique to this launch.
		path = filepath.Join(filepath.Dir(p.ClaudeSettingsPin), "launch.command")
		defer func() {
			if launchErr != nil {
				_ = os.Remove(path)
			}
		}()
	}
	script := unixCLILaunchScript(p, path)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		return 0, err
	}
	if runtime.GOOS == "darwin" {
		LaunchPhase(ctx, "start", "创建工具进程", "向 Terminal 提交工具启动请求")
		cmd := exec.CommandContext(ctx, "open", "-a", "Terminal", path)
		if err := cmd.Run(); err != nil {
			return 0, errors.New("系统未能打开 Terminal")
		}
		// open reports request acceptance by LaunchServices, not the tool's PID
		// or a successful model request. Keep that narrower meaning explicit.
		LaunchPhase(ctx, "confirm", "等待进程启动确认", "系统已接收 Terminal 启动请求，工具进程状态未验证")
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
			LaunchPhase(ctx, "start", "创建工具进程", "创建桌面终端进程")
			pid, err := startDirectContext(ctx, terminal, true)
			if err == nil {
				LaunchPhase(ctx, "confirm", "等待进程启动确认", "终端进程已启动，终端内工具状态未验证")
			}
			return pid, err
		}
	}
	return 0, errors.New("没有找到可用于启动工具的桌面终端")
}

func unixCLILaunchScript(p Plan, path string) string {
	// Terminal on macOS is a singleton and does not reliably inherit an open
	// caller's environment. Put only this profile's tool environment in its script.
	script := "#!/bin/sh\n"
	if p.ClaudeSettingsPin != "" {
		// Do not exec the CLI: the supervisor must keep the pin through reloads
		// and remove only its own files when the child finishes, including errors.
		script += "cleanup_claude_api() {\n  rm -f -- " + shLiteral(path) + " " + shLiteral(p.ClaudeSettingsPin) + "\n  rmdir -- " + shLiteral(filepath.Dir(p.ClaudeSettingsPin)) + " 2>/dev/null || :\n}\ntrap cleanup_claude_api EXIT\n"
	}
	for _, key := range []string{"OPENAI_API_KEY", "OPENAI_BASE_URL", "SUB2API_API_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "CLAUDE_CODE_OAUTH_TOKEN", "ELECTRON_RUN_AS_NODE"} {
		script += "unset " + key + "\n"
	}
	for _, kv := range p.Environment {
		k, v, _ := strings.Cut(kv, "=")
		if k == "PATH" || k == "CODEX_HOME" || k == "CLAUDE_CONFIG_DIR" || strings.HasPrefix(k, "ANTHROPIC_") || p.Tool.Kind == "claude" && strings.HasPrefix(k, "CLAUDE_CODE_") {
			script += "export " + k + "=" + shLiteral(v) + "\n"
		}
	}
	script += "cd " + shLiteral(p.Workspace) + " || exit 1\n"
	if p.ClaudeSettingsPin == "" {
		script += "exec "
	}
	script += shLiteral(p.Tool.Path)
	for _, a := range p.Args {
		script += " " + shLiteral(a)
	}
	return script + "\n"
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
