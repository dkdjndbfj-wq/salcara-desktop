//go:build windows

package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"salcara/bridge/internal/config"
)

func configureProcess(cmd *exec.Cmd, terminal bool) {
	if terminal {
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x10}
	} else {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	}
}

func powershell() string {
	if root := os.Getenv("SystemRoot"); root != "" {
		return filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	}
	return "powershell.exe"
}

var packageCache struct {
	sync.Mutex
	at    time.Time
	tools map[string]Tool
}

func platformPackages(ctx context.Context) map[string]Tool {
	packageCache.Lock()
	defer packageCache.Unlock()
	if packageCache.tools != nil && time.Since(packageCache.at) < 30*time.Second {
		return copyTools(packageCache.tools)
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	// Fixed package names; no user values are interpolated into this probe.
	script := `$ErrorActionPreference = 'Stop'
$items = @()
foreach ($name in @('OpenAI.Codex', 'Claude')) {
  $pkg = Get-AppxPackage -Name $name | Sort-Object Version -Descending | Select-Object -First 1
  if (-not $pkg -or -not $pkg.InstallLocation) { continue }
  $manifest = Get-AppxPackageManifest -Package $pkg
  foreach ($app in $manifest.Package.Applications.Application) {
    $relative = [string]$app.Executable
    if (-not $relative -or [IO.Path]::IsPathRooted($relative)) { continue }
    $file = [IO.Path]::GetFileName($relative)
    if ($file -notmatch '^(ChatGPT|Codex|Claude)\.exe$') { continue }
    $root = [IO.Path]::GetFullPath($pkg.InstallLocation).TrimEnd('\')
    $exe = [IO.Path]::GetFullPath((Join-Path $root $relative))
    if (-not $exe.StartsWith($root + '\', [StringComparison]::OrdinalIgnoreCase)) { continue }
    if (-not (Test-Path -LiteralPath $exe -PathType Leaf)) { continue }
    $kind = if ($name -eq 'Claude') { 'claude' } else { 'codex' }
    $items += @{kind=$kind; path=$exe; family=[string]$pkg.PackageFamilyName; appId=[string]$app.Id}
    break
  }
}
ConvertTo-Json -InputObject @($items) -Compress`
	cmd := exec.CommandContext(ctx, powershell(), "-NoProfile", "-NonInteractive", "-Command", script)
	configureProcess(cmd, false)
	b, err := cmd.Output()
	out := map[string]Tool{}
	if err == nil {
		var entries []struct {
			Kind   string
			Path   string
			Family string
			AppID  string
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(string(b)), "\ufeff")), &entries) == nil {
			for _, e := range entries {
				if e.Kind != "" && e.Family != "" && e.AppID != "" {
					out[e.Kind] = Tool{Path: e.Path, Family: e.Family, AppID: e.AppID}
				}
			}
		}
	}
	// Cache an unsuccessful read too. Restricted/slow Appx environments must
	// not spawn another PowerShell probe for every card and every click.
	packageCache.at, packageCache.tools = time.Now(), out
	return copyTools(out)
}

func copyTools(in map[string]Tool) map[string]Tool {
	out := make(map[string]Tool, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func psLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func startPlatform(ctx context.Context, p Plan) (int, error) {
	if p.RuntimeDir == "" {
		p.RuntimeDir = p.ProfileDir
	}
	if !isDesktop(p.Tool.ID) {
		args := []string{}
		for _, a := range p.Args {
			args = append(args, psLiteral(a))
		}
		script := "$ErrorActionPreference = 'Stop'\nSet-Location -LiteralPath " + psLiteral(p.Workspace) + "\n& " + psLiteral(p.Tool.Path) + " " + strings.Join(args, " ") + "\nif ($LASTEXITCODE) { Write-Host ('Tool exited: ' + $LASTEXITCODE) }\n"
		path := filepath.Join(p.RuntimeDir, "launch-cli.ps1")
		if err := os.WriteFile(path, append([]byte{0xef, 0xbb, 0xbf}, []byte(script)...), 0o600); err != nil {
			return 0, err
		}
		// exec.Cmd normally supplies NUL stdin/stdout. An interactive coding CLI
		// needs real console handles, so let ShellExecute create its own console.
		return startTerminal(ctx, p, path)
	}
	if p.Tool.Family == "" {
		// A plain CreateProcess of a Store app loses its package identity. Fail closed.
		if strings.Contains(strings.ToLower(p.Tool.Path), `\windowsapps\`) {
			return 0, errors.New("无法识别商店应用的包身份，请清空自定义路径后重新检测")
		}
		return startDirect(p, false)
	}
	return startPackaged(ctx, p)
}

func startTerminal(ctx context.Context, p Plan, path string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	args := []string{"-NoLogo", "-NoProfile", "-NoExit", "-ExecutionPolicy", "Bypass", "-File", path}
	quoted := []string{}
	for _, arg := range args {
		quoted = append(quoted, syscall.EscapeArg(arg))
	}
	script := "$ErrorActionPreference='Stop'; $psi=New-Object System.Diagnostics.ProcessStartInfo; $psi.FileName=" + psLiteral(powershell()) + "; $psi.Arguments=" + psLiteral(strings.Join(quoted, " ")) + "; $psi.WorkingDirectory=" + psLiteral(p.Workspace) + "; $psi.UseShellExecute=$true; $psi.WindowStyle='Normal'; $child=[System.Diagnostics.Process]::Start($psi); Write-Output $child.Id"
	cmd := exec.CommandContext(ctx, powershell(), "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = p.Environment
	configureProcess(cmd, false)
	b, err := cmd.Output()
	if err != nil {
		return 0, errors.New("无法打开交互终端，请检查 PowerShell 和程序路径")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, errors.New("终端没有返回启动确认")
	}
	return pid, nil
}

func startPackaged(ctx context.Context, p Plan) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 16*time.Second)
	defer cancel()
	id := config.RandomToken(8)
	dataPath := filepath.Join(p.RuntimeDir, ".launch-"+id+".json")
	scriptPath := filepath.Join(p.RuntimeDir, ".launch-"+id+".ps1")
	statusPath := filepath.Join(p.RuntimeDir, ".launch-"+id+".status")
	defer os.Remove(dataPath)
	defer os.Remove(scriptPath)
	defer os.Remove(statusPath)
	env := map[string]string{}
	allowed := map[string]bool{"CODEX_HOME": true, "CODEX_ELECTRON_USER_DATA_PATH": true, "OPENAI_API_KEY": true, "CLAUDE_CONFIG_DIR": true, "CLAUDE_USER_DATA_DIR": true, "ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true, "ANTHROPIC_BASE_URL": true, "ANTHROPIC_MODEL": true, "PATH": true}
	for _, kv := range p.Environment {
		k, v, _ := strings.Cut(kv, "=")
		if allowed[strings.ToUpper(k)] {
			env[k] = v
		}
	}
	argv := []string{}
	for _, arg := range p.Args {
		argv = append(argv, syscall.EscapeArg(arg))
	}
	b, _ := json.Marshal(map[string]any{"exe": p.Tool.Path, "args": strings.Join(argv, " "), "cwd": p.Workspace, "env": env, "status": statusPath})
	if err := os.WriteFile(dataPath, b, 0o600); err != nil {
		return 0, err
	}
	// The helper command line contains paths only, never an API key.
	script := `$ErrorActionPreference = 'Stop'
$d = Get-Content -LiteralPath ` + psLiteral(dataPath) + ` -Raw -Encoding UTF8 | ConvertFrom-Json
try {
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $d.exe
  $psi.Arguments = $d.args
  $psi.WorkingDirectory = $d.cwd
  $psi.UseShellExecute = $false
  foreach ($key in @('OPENAI_API_KEY','OPENAI_BASE_URL','SUB2API_API_KEY','ANTHROPIC_API_KEY','ANTHROPIC_AUTH_TOKEN','ANTHROPIC_BASE_URL','ANTHROPIC_MODEL','CLAUDE_CODE_OAUTH_TOKEN','ELECTRON_RUN_AS_NODE')) { $psi.EnvironmentVariables.Remove($key) }
  foreach ($entry in $d.env.PSObject.Properties) { $psi.EnvironmentVariables[$entry.Name] = [string]$entry.Value }
  $child = [System.Diagnostics.Process]::Start($psi)
  Start-Sleep -Milliseconds 1200
  if ($child.HasExited -and $child.ExitCode -ne 0) { throw 'Early exit' }
  [IO.File]::WriteAllText($d.status, [string]$child.Id)
} catch { [IO.File]::WriteAllText($d.status, 'FAILED') }
`
	if err := os.WriteFile(scriptPath, append([]byte{0xef, 0xbb, 0xbf}, []byte(script)...), 0o600); err != nil {
		return 0, err
	}
	innerArgs := "-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File " + syscall.EscapeArg(scriptPath)
	outer := "$ErrorActionPreference='Stop'; Invoke-CommandInDesktopPackage -PackageFamilyName " + psLiteral(p.Tool.Family) + " -AppId " + psLiteral(p.Tool.AppID) + " -PreventBreakaway -Command " + psLiteral(powershell()) + " -Args " + psLiteral(innerArgs)
	cmd := exec.CommandContext(ctx, powershell(), "-NoProfile", "-NonInteractive", "-Command", outer)
	configureProcess(cmd, false)
	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("商店应用包启动失败，请重新检测程序或使用 CLI")
	}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		b, err := os.ReadFile(statusPath)
		if err == nil {
			var pid int
			if _, err := fmt.Sscanf(string(b), "%d", &pid); err == nil && pid > 0 {
				return pid, nil
			}
			return 0, errors.New("桌面应用启动后立即退出，请检查应用版本和本地实例日志")
		}
		select {
		case <-ctx.Done():
			return 0, errors.New("没有收到桌面应用的启动确认，请检查程序自己的日志")
		case <-ticker.C:
		}
	}
}

func originalAppData(t Tool) string {
	path := defaultAppData(t)
	// MSIX redirects APPDATA into LocalCache. Use that original directory only
	// when it exists; normal Codex launch itself still receives no new userData flag.
	key := "CODEX_ELECTRON_USER_DATA_PATH"
	if t.Kind == "claude" {
		key = "CLAUDE_USER_DATA_DIR"
	}
	if os.Getenv(key) == "" && t.Family != "" {
		candidate := filepath.Join(os.Getenv("LOCALAPPDATA"), "Packages", t.Family, "LocalCache", "Roaming", filepath.Base(path))
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
	}
	return path
}

var userDataArg = regexp.MustCompile(`(?i)--user-data-dir(?:=|\s+)(?:"([^"]+)"|'([^']+)'|([^\s]+))`)

func originalProcess(command, appData string) bool {
	if strings.Contains(strings.ToLower(command), "--type=") {
		return false
	}
	match := userDataArg.FindStringSubmatch(command)
	if match == nil {
		return true
	}
	for _, path := range match[1:] {
		if path != "" {
			return strings.EqualFold(filepath.Clean(path), filepath.Clean(appData))
		}
	}
	return false
}

func stopPlatform(ctx context.Context, p Plan) error {
	ctx, cancel := context.WithTimeout(ctx, 22*time.Second)
	defer cancel()
	query := "$ErrorActionPreference='Stop'; $items=@(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -and [string]::Equals($_.ExecutablePath," + psLiteral(p.Tool.Path) + ",[StringComparison]::OrdinalIgnoreCase) } | Select-Object ProcessId,CommandLine); ConvertTo-Json -InputObject $items -Compress"
	cmd := exec.CommandContext(ctx, powershell(), "-NoProfile", "-NonInteractive", "-Command", query)
	configureProcess(cmd, false)
	b, err := cmd.Output()
	if err != nil {
		return errors.New("无法安全识别原工具进程，已停止切号，请先手动退出原工具")
	}
	var running []struct {
		ProcessID   int
		CommandLine string
	}
	if json.Unmarshal(b, &running) != nil {
		return errors.New("原工具进程状态无法读取，已停止切号")
	}
	ids := []string{}
	for _, process := range running {
		// A custom userData directory cannot be inferred from a no-argument
		// process. Refuse ambiguous closure (especially a live user's main app).
		key := "CODEX_ELECTRON_USER_DATA_PATH"
		if p.Tool.Kind == "claude" {
			key = "CLAUDE_USER_DATA_DIR"
		}
		if os.Getenv(key) != "" && !strings.Contains(strings.ToLower(process.CommandLine), "--type=") && userDataArg.FindStringSubmatch(process.CommandLine) == nil {
			return errors.New("检测到没有目录标识的桌面进程，无法确认它属于当前自定义数据目录；未关闭程序或替换凭据。请先手动退出该工具后再切换")
		}
		if process.ProcessID > 0 && originalProcess(process.CommandLine, p.AppData) {
			ids = append(ids, strconv.Itoa(process.ProcessID))
		}
	}
	if len(ids) == 0 {
		return nil
	}
	// CloseMainWindow is a graceful request. No taskkill, no force kill, no broad
	// process-name match. The app can flush its own history/database before exit.
	script := "$ErrorActionPreference='Stop'; foreach ($processId in @(" + strings.Join(ids, ",") + ")) { $process=Get-Process -Id $processId -ErrorAction SilentlyContinue; if (-not $process) { continue }; if (-not $process.CloseMainWindow()) { throw 'No safe close' }; if (-not $process.WaitForExit(12000)) { throw 'App still running' } }"
	cmd = exec.CommandContext(ctx, powershell(), "-NoProfile", "-NonInteractive", "-Command", script)
	configureProcess(cmd, false)
	if err := cmd.Run(); err != nil {
		return errors.New("原工具尚未正常退出，未替换凭据。请先结束任务并从原程序菜单退出，然后再点切换；不会强制结束进程")
	}
	return nil
}
