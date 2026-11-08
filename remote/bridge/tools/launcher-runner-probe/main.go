//go:build windows

// Diagnostic-only Windows PowerShell probe. Standard library only; it never
// launches an agent, reads an account, or calls a provider.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type probeCase struct {
	name, environment, script string
}

type probeResult struct {
	Name         string `json:"name"`
	Environment  string `json:"environment"`
	Milliseconds int64  `json:"milliseconds"`
	TimedOut     bool   `json:"timedOut"`
	Error        string `json:"error,omitempty"`
	Log          string `json:"log"`
}

func main() {
	if runtime.GOOS != "windows" {
		panic("Windows diagnostic only")
	}
	root := os.Getenv("SALCARA_PROBE_OUTPUT")
	if root == "" {
		var err error
		root, err = os.MkdirTemp("", "salcara-launcher-probe-")
		check(err)
	}
	check(os.MkdirAll(root, 0700))
	shell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	check(writeJSON(filepath.Join(root, "metadata.json"), map[string]any{
		"goVersion": runtime.Version(), "shell": shell, "output": root,
		"pathEntries":    len(filepath.SplitList(os.Getenv("PATH"))),
		"hasPathExt":     os.Getenv("PATHEXT") != "",
		"hasSystemDrive": os.Getenv("SystemDrive") != "",
	}))

	newObject := `[Console]::Error.WriteLine('probe: before New-Object')
$psi = New-Object System.Diagnostics.ProcessStartInfo
[Console]::Error.WriteLine('probe: after New-Object')
`
	readJSON := `[Console]::Error.WriteLine('probe: before Get-Content / ConvertFrom-Json')
$d = Get-Content -LiteralPath __DATA__ -Raw -Encoding UTF8 | ConvertFrom-Json
[Console]::Error.WriteLine('probe: after JSON value=' + $d.value)
`
	cases := []probeCase{
		{"language-dotnet", "fixture", `[Console]::Error.WriteLine('probe: before .NET constructor')
$psi = [System.Diagnostics.ProcessStartInfo]::new()
$psi.FileName = 'synthetic.exe'
$psi.Arguments = '-synthetic'
$psi.WorkingDirectory = __DIR__
$psi.UseShellExecute = $false
$psi.CreateNoWindow = $true
$psi.RedirectStandardInput = $false
$psi.RedirectStandardOutput = $false
$psi.RedirectStandardError = $false
[Console]::Error.WriteLine('probe: after .NET properties')
`},
		{"new-object-fixture", "fixture", newObject},
		{"json-fixture", "fixture", readJSON},
		{"new-object-system-path", "system-path", newObject},
		{"new-object-system-runtime", "system-runtime", newObject},
		{"new-object-inherited-sanitized", "inherited-sanitized", newObject},
		{"new-object-qualified", "fixture", `[Console]::Error.WriteLine('probe: before qualified New-Object')
$psi = Microsoft.PowerShell.Utility\New-Object System.Diagnostics.ProcessStartInfo
[Console]::Error.WriteLine('probe: after qualified New-Object')
`},
		{"explicit-manifests", "fixture", `[Console]::Error.WriteLine('probe: before Utility manifest import')
Import-Module -Name ($PSHOME + '\Modules\Microsoft.PowerShell.Utility\Microsoft.PowerShell.Utility.psd1') -ErrorAction Stop
[Console]::Error.WriteLine('probe: after Utility manifest import')
` + newObject + `[Console]::Error.WriteLine('probe: before Management manifest import')
Import-Module -Name ($PSHOME + '\Modules\Microsoft.PowerShell.Management\Microsoft.PowerShell.Management.psd1') -ErrorAction Stop
[Console]::Error.WriteLine('probe: after Management manifest import')
` + readJSON},
		{"explicit-utility-dll", "fixture", `$PSModuleAutoLoadingPreference = 'None'
[Console]::Error.WriteLine('probe: before Utility assembly load')
$assembly = [Reflection.Assembly]::Load('Microsoft.PowerShell.Commands.Utility, Version=3.0.0.0, Culture=neutral, PublicKeyToken=31bf3856ad364e35')
[Console]::Error.WriteLine('probe: after Utility assembly load')
[Console]::Error.WriteLine('probe: before Utility DLL import')
Import-Module -Assembly $assembly -ErrorAction Stop
[Console]::Error.WriteLine('probe: after Utility DLL import')
` + newObject},
		{"new-object-private-cache", "private-cache", newObject},
		{"resolve-cmdlet-only", "fixture", `[Console]::Error.WriteLine('probe: before GetCommand Cmdlet')
$command = $ExecutionContext.SessionState.InvokeCommand.GetCommand('New-Object', [System.Management.Automation.CommandTypes]::Cmdlet)
if ($null -eq $command) { throw 'New-Object cmdlet absent' }
[Console]::Error.WriteLine('probe: after GetCommand module=' + $command.ModuleName)
` + newObject},
		{"engine-line-trace", "fixture", `[Console]::Error.WriteLine('probe: before Set-PSDebug')
Set-PSDebug -Trace 1
[Console]::Error.WriteLine('probe: after Set-PSDebug')
` + newObject},
	}
	selected := "," + os.Getenv("SALCARA_PROBE_CASES") + ","
	results := make([]probeResult, 0, len(cases))
	for _, c := range cases {
		if selected != ",," && !strings.Contains(selected, ","+c.name+",") {
			continue
		}
		dir := filepath.Join(root, c.name)
		check(os.MkdirAll(dir, 0700))
		data := filepath.Join(dir, "synthetic.json")
		check(os.WriteFile(data, []byte(`{"value":"synthetic-only"}`), 0600))
		script := strings.ReplaceAll(strings.ReplaceAll(c.script, "__DATA__", psQuote(data)), "__DIR__", psQuote(dir))
		script = prelude + strings.ReplaceAll(receiptPreamble, "__STATUS__", psQuote(filepath.Join(dir, "unused.status"))) + script
		path := filepath.Join(dir, "probe.ps1")
		check(os.WriteFile(path, append([]byte{0xef, 0xbb, 0xbf}, []byte(script)...), 0600))
		environment := fixtureEnvironment(shell, dir)
		switch c.environment {
		case "system-path":
			environment = replaceEnv(environment, "PATH", strings.Join([]string{filepath.Join(os.Getenv("SystemRoot"), "System32"), os.Getenv("SystemRoot"), filepath.Dir(shell)}, ";"))
		case "system-runtime":
			for _, key := range []string{"SystemDrive", "OS", "COMPUTERNAME", "USERDOMAIN", "USERNAME", "PATHEXT", "NUMBER_OF_PROCESSORS", "PROCESSOR_IDENTIFIER", "PROCESSOR_LEVEL", "PROCESSOR_REVISION", "PROCESSOR_ARCHITEW6432"} {
				if value := os.Getenv(key); value != "" {
					environment = replaceEnv(environment, key, value)
				}
			}
			profile := filepath.Join(dir, "profile")
			volume := filepath.VolumeName(profile)
			environment = replaceEnv(environment, "HOMEDRIVE", volume)
			environment = replaceEnv(environment, "HOMEPATH", strings.TrimPrefix(profile, volume))
		case "inherited-sanitized":
			environment = nil
			for _, value := range os.Environ() {
				name, _, _ := strings.Cut(value, "=")
				upper := strings.ToUpper(name)
				if strings.Contains(upper, "TOKEN") || strings.Contains(upper, "SECRET") || strings.Contains(upper, "KEY") || strings.Contains(upper, "PASSWORD") || strings.HasPrefix(upper, "CLAUDE") || strings.HasPrefix(upper, "CODEX") || strings.HasPrefix(upper, "ANTHROPIC") || strings.HasPrefix(upper, "OPENAI") || strings.HasPrefix(upper, "SALCARA") {
					continue
				}
				environment = append(environment, value)
			}
		case "private-cache":
			environment = replaceEnv(environment, "PSModuleAnalysisCachePath", filepath.Join(dir, "analysis-cache"))
		}
		fmt.Println("PROBE_START " + c.name)
		logPath := filepath.Join(dir, "powershell.log")
		log, err := os.Create(logPath)
		check(err)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		cmd := exec.CommandContext(ctx, shell, "-NoLogo", "-NoProfile", "-NonInteractive", "-OutputFormat", "Text", "-ExecutionPolicy", "Bypass", "-File", path)
		cmd.Env = environment
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		start := time.Now()
		err = cmd.Run()
		result := probeResult{Name: c.name, Environment: c.environment, Milliseconds: time.Since(start).Milliseconds(), TimedOut: ctx.Err() != nil}
		if err != nil {
			result.Error = err.Error()
		}
		cancel()
		check(log.Close())
		logData, readErr := os.ReadFile(logPath)
		check(readErr)
		result.Log = string(logData)
		results = append(results, result)
		encoded, _ := json.Marshal(result)
		fmt.Println(string(encoded))
		check(writeJSON(filepath.Join(root, "results.json"), results))
	}
	fmt.Printf("Probe completed: %d cases; evidence: %s\n", len(results), root)
}

func fixtureEnvironment(shell, dir string) []string {
	env := []string{"GORACE=atexit_sleep_ms=0"}
	for _, key := range []string{"SystemRoot", "WINDIR", "TEMP", "TMP", "PATH", "COMSPEC", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "ProgramData", "PROCESSOR_ARCHITECTURE"} {
		if value := os.Getenv(key); value != "" {
			env = append(env, key+"="+value)
		}
	}
	profile := filepath.Join(dir, "profile")
	roaming := filepath.Join(profile, "AppData", "Roaming")
	local := filepath.Join(profile, "AppData", "Local")
	check(os.MkdirAll(roaming, 0700))
	check(os.MkdirAll(local, 0700))
	return append(env, "USERPROFILE="+profile, "APPDATA="+roaming, "LOCALAPPDATA="+local, "PSModulePath="+filepath.Join(filepath.Dir(shell), "Modules"))
}

func replaceEnv(env []string, name, value string) []string {
	for i, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.EqualFold(key, name) {
			env[i] = name + "=" + value
			return env
		}
	}
	return append(env, name+"="+value)
}

func psQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
func check(err error) {
	if err != nil {
		panic(err)
	}
}
func writeJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

const prelude = `$ProgressPreference = 'SilentlyContinue'
[Console]::OutputEncoding = [Text.UTF8Encoding]::new($false)
[Console]::Error.WriteLine('probe: entered PS=' + $PSVersionTable.PSVersion.ToString() + ' language=' + $ExecutionContext.SessionState.LanguageMode)
[Console]::Error.WriteLine('probe: effective PSModulePath=' + $env:PSModulePath)
`

const receiptPreamble = `$ErrorActionPreference = 'Stop'
function New-LaunchFailure($exception) {
  $base = $exception.GetBaseException()
  $value = @{state='failed'; exception=[string]$base.GetType().FullName; hresult=[int]$base.HResult}
  if ($base -is [System.ComponentModel.Win32Exception]) { $value.nativeCode = [uint32]$base.NativeErrorCode }
  return $value
}
$launchStatus = __STATUS__
function Write-LaunchReceipt($value) {
  $destination = $launchStatus
  if ($value.state -eq 'started') { $destination += '.started' }
  if ([IO.File]::Exists($destination)) { return }
  $temporary = $destination + '.tmp'
  [IO.File]::WriteAllText($temporary, ($value | ConvertTo-Json -Compress))
  [IO.File]::Move($temporary, $destination)
}
$child = $null
[Console]::Error.WriteLine('probe: receipt definitions ready')
`
