//go:build windows

package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if p.RuntimeDir == "" {
		p.RuntimeDir = p.ProfileDir
	}
	if !isDesktop(p.Tool.ID) {
		// exec.Cmd normally supplies NUL stdin/stdout. An interactive coding CLI
		// needs real console handles, so let ShellExecute create its own console.
		return startTerminal(ctx, p)
	}
	if p.Tool.Family == "" {
		// A plain CreateProcess of a Store app loses its package identity. Fail closed.
		if strings.Contains(strings.ToLower(p.Tool.Path), `\windowsapps\`) {
			return 0, errors.New("无法识别商店应用的包身份，请清空自定义路径后重新检测")
		}
		LaunchPhase(ctx, "start", "创建工具进程", "创建桌面应用进程")
		return startDirectContext(ctx, p, false)
	}
	return startPackaged(ctx, p)
}

func startTerminal(ctx context.Context, p Plan) (int, error) {
	return startManagedTerminal(ctx, p)
}

// Kept for receipt compatibility tests; production CLI launches always use the
// durable Job supervisor, including launches from the separate history page.
func startLegacyTerminal(ctx context.Context, p Plan) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	id := config.RandomToken(8)
	path := filepath.Join(p.RuntimeDir, ".launch-cli-"+id+".ps1")
	statusPath := filepath.Join(p.RuntimeDir, ".launch-cli-"+id+".status")
	defer os.Remove(path)
	defer os.Remove(statusPath)
	defer os.Remove(statusPath + ".tmp")
	defer os.Remove(statusPath + ".started")
	defer os.Remove(statusPath + ".started.tmp")
	if err := os.WriteFile(path, append([]byte{0xef, 0xbb, 0xbf}, []byte(windowsCLILaunchScript(p, statusPath))...), 0o600); err != nil {
		return 0, safeWindowsLaunchError("写入启动程序失败", err)
	}
	args := []string{"-NoLogo", "-NoProfile", "-NoExit", "-ExecutionPolicy", "Bypass", "-File", path}
	quoted := []string{}
	for _, arg := range args {
		quoted = append(quoted, syscall.EscapeArg(arg))
	}
	script := windowsLaunchExceptionScript + "\n$child=$null; try { $psi=New-Object System.Diagnostics.ProcessStartInfo; $psi.FileName=" + psLiteral(powershell()) + "; $psi.Arguments=" + psLiteral(strings.Join(quoted, " ")) + "; $psi.WorkingDirectory=" + psLiteral(p.Workspace) + "; $psi.UseShellExecute=$true; $psi.WindowStyle='Normal'; $child=[System.Diagnostics.Process]::Start($psi); @{state='shell';pid=$child.Id} | ConvertTo-Json -Compress } catch { if ($null -ne $child) { @{state='shell';pid=$child.Id} | ConvertTo-Json -Compress } else { New-LaunchFailure $_.Exception | ConvertTo-Json -Compress } }"
	cmd := exec.CommandContext(ctx, powershell(), "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = p.Environment
	configureProcess(cmd, false)
	LaunchPhase(ctx, "start", "创建工具进程", "打开交互终端并创建工具入口进程")
	// After submission, a lost helper response cannot prove that no target was
	// created. Claude's terminal supervisor then owns the pin's lifetime.
	var output bytes.Buffer
	cmd.Stdout = &output
	if err := cmd.Start(); err != nil {
		return 0, safeWindowsLaunchError("无法打开交互终端", err)
	}
	preserveUnconfirmed := p.ClaudeSettingsPin != ""
	wait := func() (int, error) {
		return waitWindowsLaunchReceiptWithPin(ctx, statusPath, "工具入口进程", preserveUnconfirmed)
	}
	err := cmd.Wait()
	if err != nil {
		if ctx.Err() != nil || preserveUnconfirmed {
			// Cancellation may race the helper's final stdout after the target
			// has already produced its own receipt. Keep that process configured.
			return wait()
		}
		return 0, safeWindowsLaunchError("无法打开交互终端", err)
	}
	receipt, err := parseWindowsLaunchReceipt(output.Bytes())
	if err != nil {
		if preserveUnconfirmed {
			return wait()
		}
		return 0, errors.New("终端没有返回有效启动回执")
	}
	if receipt.State == "failed" {
		return 0, windowsLaunchReceiptError("无法打开交互终端", receipt)
	}
	if receipt.State != "shell" || receipt.PID <= 0 {
		if preserveUnconfirmed {
			return wait()
		}
		return 0, errors.New("终端没有返回有效启动回执")
	}
	// A shell PID proves only that the terminal was opened. Success requires a
	// separate receipt from Process.Start for the selected executable itself.
	return wait()
}

// Error messages are intentionally absent from the cross-process contract:
// PowerShell exception text can include the entire script or credential args.
// Native codes and known exception types retain actionable OS failure causes.
const windowsLaunchExceptionScript = `$ErrorActionPreference = 'Stop'
function New-LaunchFailure($exception) {
  $base = $exception.GetBaseException()
  $value = @{state='failed'; exception=[string]$base.GetType().FullName; hresult=[int]$base.HResult}
  if ($base -is [System.ComponentModel.Win32Exception]) { $value.nativeCode = [uint32]$base.NativeErrorCode }
  return $value
}
`

func windowsLaunchReceiptScript(statusPath string) string {
	return windowsLaunchExceptionScript + "$launchStatus = " + psLiteral(statusPath) + `
function Write-LaunchReceipt($value) {
  $destination = $launchStatus
  if ($value.state -eq 'started') { $destination += '.started' }
  if ([IO.File]::Exists($destination)) { return }
  $temporary = $destination + '.tmp'
  [IO.File]::WriteAllText($temporary, ($value | ConvertTo-Json -Compress))
  [IO.File]::Move($temporary, $destination)
}
`
}

func windowsCLILaunchScript(p Plan, statusPath string) string {
	argv := make([]string, 0, len(p.Args))
	for _, arg := range p.Args {
		argv = append(argv, syscall.EscapeArg(arg))
	}
	script := windowsLaunchReceiptScript(statusPath) + `$child = $null
try {
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = ` + psLiteral(p.Tool.Path) + `
  $psi.Arguments = ` + psLiteral(strings.Join(argv, " ")) + `
  $psi.WorkingDirectory = ` + psLiteral(p.Workspace) + `
  $psi.UseShellExecute = $false
  $psi.CreateNoWindow = $false
  $psi.RedirectStandardInput = $false
  $psi.RedirectStandardOutput = $false
  $psi.RedirectStandardError = $false
  $child = [System.Diagnostics.Process]::Start($psi)
  Write-LaunchReceipt @{state='started';pid=$child.Id}
  if ($child.WaitForExit(1200) -and $child.ExitCode -ne 0) {
    Write-LaunchReceipt @{state='failed';pid=$child.Id;exitCode=[int]$child.ExitCode}
  } else {
    Write-LaunchReceipt @{state='ready';pid=$child.Id}
    $child.WaitForExit()
    if ($child.ExitCode -ne 0) { Write-Host ('Tool exited: ' + $child.ExitCode) }
  }
} catch {
  $failure = New-LaunchFailure $_.Exception
  if ($null -ne $child) { $failure.pid = $child.Id }
  Write-LaunchReceipt $failure
}
`
	if p.ClaudeSettingsPin != "" {
		// A receipt write can fail after Process.Start. Keep the API pin until
		// that child exits even when reporting/confirmation fails.
		script += `finally {
  if ($null -ne $child) { try { $child.WaitForExit() } catch {} }
  Remove-Item -LiteralPath ` + psLiteral(p.ClaudeSettingsPin) + ` -Force -ErrorAction SilentlyContinue
  try { [IO.Directory]::Delete(` + psLiteral(filepath.Dir(p.ClaudeSettingsPin)) + `, $false) } catch {}
}
`
	}
	return script
}

type windowsLaunchReceipt struct {
	State      string  `json:"state"`
	PID        int     `json:"pid,omitempty"`
	ExitCode   *int32  `json:"exitCode,omitempty"`
	NativeCode *uint32 `json:"nativeCode,omitempty"`
	HResult    *int32  `json:"hresult,omitempty"`
	Exception  string  `json:"exception,omitempty"`
}

func parseWindowsLaunchReceipt(data []byte) (windowsLaunchReceipt, error) {
	var receipt windowsLaunchReceipt
	if len(data) > 4096 {
		return receipt, errors.New("invalid launch receipt")
	}
	dec := json.NewDecoder(strings.NewReader(strings.TrimPrefix(strings.TrimSpace(string(data)), "\ufeff")))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&receipt); err != nil {
		return windowsLaunchReceipt{}, errors.New("invalid launch receipt")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return windowsLaunchReceipt{}, errors.New("invalid launch receipt")
	}
	if len(receipt.Exception) > 256 {
		return windowsLaunchReceipt{}, errors.New("invalid launch receipt")
	}
	switch receipt.State {
	case "shell", "started", "ready":
		if receipt.PID <= 0 || receipt.ExitCode != nil || receipt.NativeCode != nil || receipt.HResult != nil || receipt.Exception != "" {
			return windowsLaunchReceipt{}, errors.New("invalid launch receipt")
		}
	case "dispatched":
		if receipt.PID != 0 || receipt.ExitCode != nil || receipt.NativeCode != nil || receipt.HResult != nil || receipt.Exception != "" {
			return windowsLaunchReceipt{}, errors.New("invalid launch receipt")
		}
	case "failed":
		if receipt.ExitCode != nil && (*receipt.ExitCode == 0 || receipt.PID <= 0) {
			return windowsLaunchReceipt{}, errors.New("invalid launch receipt")
		}
	default:
		return windowsLaunchReceipt{}, errors.New("invalid launch receipt")
	}
	return receipt, nil
}

func windowsLaunchReceiptError(label string, receipt windowsLaunchReceipt) error {
	if receipt.ExitCode != nil {
		return fmt.Errorf("%s启动后立即退出（退出码 %d，0x%08X）", label, *receipt.ExitCode, uint32(*receipt.ExitCode))
	}
	if receipt.NativeCode != nil && *receipt.NativeCode != 0 {
		return fmt.Errorf("%s：%s（系统错误 %d）", label, strings.TrimSpace(syscall.Errno(*receipt.NativeCode).Error()), *receipt.NativeCode)
	}
	if receipt.HResult != nil && uint32(*receipt.HResult)&0xffff0000 == 0x80070000 {
		code := uint32(*receipt.HResult) & 0xffff
		return fmt.Errorf("%s：%s（系统错误 %d）", label, strings.TrimSpace(syscall.Errno(code).Error()), code)
	}
	reason := "系统无法执行启动操作"
	switch receipt.Exception {
	case "System.IO.FileNotFoundException", "System.Management.Automation.CommandNotFoundException":
		reason = "没有找到程序或启动命令"
	case "System.IO.DirectoryNotFoundException", "System.Management.Automation.ItemNotFoundException":
		reason = "启动目录不存在"
	case "System.UnauthorizedAccessException":
		reason = "系统拒绝访问程序或启动目录"
	case "System.ArgumentException":
		reason = "启动参数无效"
	case "System.InvalidOperationException":
		reason = "进程启动状态无效"
	}
	if receipt.HResult != nil {
		return fmt.Errorf("%s：%s（HRESULT 0x%08X）", label, reason, uint32(*receipt.HResult))
	}
	return fmt.Errorf("%s：%s", label, reason)
}

func safeWindowsLaunchError(label string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s：%w", label, err)
	}
	var code syscall.Errno
	if errors.As(err, &code) {
		value := uint32(code)
		return windowsLaunchReceiptError(label, windowsLaunchReceipt{NativeCode: &value})
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("%s（启动程序退出码 %d）", label, exit.ExitCode())
	}
	return fmt.Errorf("%s：系统无法执行启动操作", label)
}

func readWindowsLaunchReceipt(path string) (windowsLaunchReceipt, error) {
	file, err := os.Open(path)
	if err != nil {
		return windowsLaunchReceipt{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil {
		return windowsLaunchReceipt{}, err
	}
	return parseWindowsLaunchReceipt(data)
}

func waitWindowsLaunchReceipt(ctx context.Context, statusPath, label string) (int, error) {
	return waitWindowsLaunchReceiptWithPin(ctx, statusPath, label, false)
}

func waitWindowsLaunchReceiptWithPin(ctx context.Context, statusPath, label string, preserveUnconfirmed bool) (int, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	waitCtx := ctx
	finish := func() {}
	defer func() { finish() }()
	createdPID := 0
	created := func(pid int) {
		if createdPID != 0 {
			return
		}
		createdPID = pid
		// Process.Start is irreversible. Once its receipt exists, a request
		// cancellation must not turn a live process into a rollback. Finish
		// only the short early-exit check, independently of the HTTP lifetime.
		waitCtx, finish = context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		LaunchPhase(ctx, "confirm", "等待进程启动确认", "已创建"+label+"，检查是否立即退出")
	}
	keepCreated := func() (int, error) {
		LaunchWarning(ctx, "confirm", "已创建"+label+"，但未收到完整启动检查回执；配置与授权已保留")
		return createdPID, nil
	}
	keepUnconfirmed := func() (int, error) {
		LaunchWarning(ctx, "confirm", "启动请求已提交，但进程状态未确认；配置与 API 设置已保留，请检查终端后再重试")
		return 0, nil
	}
	for {
		// Read receipts before cancellation: the helper can publish a target
		// PID immediately before the request is disconnected or times out.
		receipt, err := readWindowsLaunchReceipt(statusPath)
		if err != nil || receipt.State == "failed" && receipt.ExitCode == nil {
			started, startedErr := readWindowsLaunchReceipt(statusPath + ".started")
			if startedErr == nil && started.State == "started" {
				created(started.PID)
			} else if startedErr != nil && !os.IsNotExist(startedErr) && os.IsNotExist(err) {
				if createdPID > 0 {
					return keepCreated()
				}
				if preserveUnconfirmed {
					return keepUnconfirmed()
				}
				return 0, safeWindowsLaunchError("读取进程启动回执失败", startedErr)
			}
		}
		if err == nil {
			targetCreated := receipt.State == "started" || receipt.State == "ready" || receipt.State == "failed" && receipt.PID > 0
			if targetCreated {
				if createdPID > 0 && createdPID != receipt.PID {
					return keepCreated()
				}
				created(receipt.PID)
			}
			switch receipt.State {
			case "ready":
				return receipt.PID, nil
			case "failed":
				if createdPID > 0 && receipt.ExitCode == nil {
					return keepCreated()
				}
				return 0, windowsLaunchReceiptError(label, receipt)
			case "started":
			default:
				if createdPID > 0 {
					return keepCreated()
				}
				if preserveUnconfirmed {
					return keepUnconfirmed()
				}
				return 0, errors.New("工具进程尚未返回有效启动确认")
			}
		} else if !os.IsNotExist(err) {
			if createdPID > 0 {
				return keepCreated()
			}
			if preserveUnconfirmed {
				return keepUnconfirmed()
			}
			return 0, safeWindowsLaunchError("读取进程启动回执失败", err)
		}
		if err := waitCtx.Err(); err != nil {
			if createdPID > 0 {
				return keepCreated()
			}
			if preserveUnconfirmed {
				return keepUnconfirmed()
			}
			if errors.Is(err, context.Canceled) {
				return 0, fmt.Errorf("等待进程启动确认已取消：%w", err)
			}
			return 0, fmt.Errorf("未在时限内收到工具进程的启动确认：%w", err)
		}
		select {
		case <-waitCtx.Done():
			// Check one last receipt before deciding that no target was made.
		case <-ticker.C:
		}
	}
}

func startPackaged(ctx context.Context, p Plan) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 16*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	id := config.RandomToken(8)
	dataPath := filepath.Join(p.RuntimeDir, ".launch-"+id+".json")
	scriptPath := filepath.Join(p.RuntimeDir, ".launch-"+id+".ps1")
	statusPath := filepath.Join(p.RuntimeDir, ".launch-"+id+".status")
	defer os.Remove(dataPath)
	defer os.Remove(scriptPath)
	defer os.Remove(statusPath)
	defer os.Remove(statusPath + ".tmp")
	defer os.Remove(statusPath + ".started")
	defer os.Remove(statusPath + ".started.tmp")
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
		return 0, safeWindowsLaunchError("保存应用启动配置失败", err)
	}
	// The helper command line contains paths only, never an API key.
	script := windowsPackagedLaunchScript(dataPath, statusPath)
	if err := os.WriteFile(scriptPath, append([]byte{0xef, 0xbb, 0xbf}, []byte(script)...), 0o600); err != nil {
		return 0, safeWindowsLaunchError("写入应用启动程序失败", err)
	}
	innerArgs := "-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File " + syscall.EscapeArg(scriptPath)
	outer := windowsLaunchExceptionScript + "\ntry { Invoke-CommandInDesktopPackage -PackageFamilyName " + psLiteral(p.Tool.Family) + " -AppId " + psLiteral(p.Tool.AppID) + " -PreventBreakaway -Command " + psLiteral(powershell()) + " -Args " + psLiteral(innerArgs) + " | Out-Null; @{state='dispatched'} | ConvertTo-Json -Compress } catch { New-LaunchFailure $_.Exception | ConvertTo-Json -Compress }"
	cmd := exec.CommandContext(ctx, powershell(), "-NoProfile", "-NonInteractive", "-Command", outer)
	configureProcess(cmd, false)
	LaunchPhase(ctx, "start", "创建工具进程", "在商店应用包内创建桌面进程")
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return waitWindowsLaunchReceipt(ctx, statusPath, "桌面应用进程")
		}
		return 0, safeWindowsLaunchError("商店应用包启动失败", err)
	}
	receipt, err := parseWindowsLaunchReceipt(output)
	if err != nil {
		return 0, errors.New("商店应用包没有返回有效启动回执")
	}
	if receipt.State == "failed" {
		return 0, windowsLaunchReceiptError("商店应用包启动失败", receipt)
	}
	if receipt.State != "dispatched" {
		return 0, errors.New("商店应用包没有返回有效启动回执")
	}
	return waitWindowsLaunchReceipt(ctx, statusPath, "桌面应用进程")
}

func windowsPackagedLaunchScript(dataPath, statusPath string) string {
	return windowsLaunchReceiptScript(statusPath) + `try {
  $d = Get-Content -LiteralPath ` + psLiteral(dataPath) + ` -Raw -Encoding UTF8 | ConvertFrom-Json
  $psi = New-Object System.Diagnostics.ProcessStartInfo
  $psi.FileName = $d.exe
  $psi.Arguments = $d.args
  $psi.WorkingDirectory = $d.cwd
  $psi.UseShellExecute = $false
  foreach ($key in @('OPENAI_API_KEY','OPENAI_BASE_URL','SUB2API_API_KEY','ANTHROPIC_API_KEY','ANTHROPIC_AUTH_TOKEN','ANTHROPIC_BASE_URL','ANTHROPIC_MODEL','CLAUDE_CODE_OAUTH_TOKEN','ELECTRON_RUN_AS_NODE')) { $psi.EnvironmentVariables.Remove($key) }
  foreach ($entry in $d.env.PSObject.Properties) { $psi.EnvironmentVariables[$entry.Name] = [string]$entry.Value }
  $child = [System.Diagnostics.Process]::Start($psi)
  Write-LaunchReceipt @{state='started';pid=$child.Id}
  if ($child.WaitForExit(1200) -and $child.ExitCode -ne 0) {
    Write-LaunchReceipt @{state='failed';pid=$child.Id;exitCode=[int]$child.ExitCode}
  } else { Write-LaunchReceipt @{state='ready';pid=$child.Id} }
} catch { Write-LaunchReceipt (New-LaunchFailure $_.Exception) }
`
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
