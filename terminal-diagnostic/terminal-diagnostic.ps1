param(
  [Parameter(Mandatory=$true)][string]$RepositoryRoot,
  [Parameter(Mandatory=$true)][string]$OutputDirectory,
  [ValidateSet('baseline','normal-std-handles','no-psreadline','detached','normal-std-handles-detached')]
  [string[]]$Variants=@('baseline','normal-std-handles','no-psreadline','detached','normal-std-handles-detached')
)
$ErrorActionPreference='Stop'
$PSNativeCommandUseErrorActionPreference=$false
$bridgeRoot=Join-Path $RepositoryRoot 'remote/bridge'
$terminalSourcePath=Join-Path $bridgeRoot 'internal/hubclient/workspace_terminal_windows.go'
$commonSourcePath=Join-Path $bridgeRoot 'internal/hubclient/workspace_terminal.go'
$diagnosticSourcePath=Join-Path $PSScriptRoot 'probe/terminal_diagnostic_test.go'
$terminalSource=[IO.File]::ReadAllText($terminalSourcePath)
$commonSource=[IO.File]::ReadAllText($commonSourcePath)
$environmentStart=$commonSource.IndexOf('func terminalEnvironment() []string {')
if($environmentStart -lt 0){throw 'Production terminalEnvironment function not found'}
$helperHeader=@'
package hubclient
import ("os"; "strings")

'@
$helpers=$helperHeader+[Environment]::NewLine+$commonSource.Substring($environmentStart)
$goMod=@'
module salcara-terminal-diagnostic
go 1.26.0
require golang.org/x/sys v0.47.0

'@
New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
$outputRoot=(Resolve-Path -LiteralPath $OutputDirectory).Path
[pscustomobject]@{ProductionSHA256=(Get-FileHash -LiteralPath $terminalSourcePath).Hash;EnvironmentSHA256=(Get-FileHash -LiteralPath $commonSourcePath).Hash;OS=[Environment]::OSVersion.VersionString} | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $outputRoot 'source.json')
$results=@()
foreach($variant in $Variants){
  $caseRoot=Join-Path $outputRoot $variant
  New-Item -ItemType Directory -Force -Path $caseRoot | Out-Null
  $caseSource=$terminalSource
  if($variant.StartsWith('normal-std-handles')){
    $needle=' | windows.STARTF_USESTDHANDLES'
    if(-not $caseSource.Contains($needle)){throw 'Standard handle diagnostic patch no longer matches production source'}
    $caseSource=$caseSource.Replace($needle,'')
  }
  if($variant -eq 'no-psreadline'){
    $needle='args := []string{shell, "-NoLogo", "-NoProfile", "-NoExit"}'
    if(-not $caseSource.Contains($needle)){throw 'PSReadLine diagnostic patch no longer matches production source'}
    $caseSource=$caseSource.Replace($needle,'args := []string{shell, "-NoLogo", "-NoProfile", "-NoExit", "-Command", "Remove-Module PSReadLine -ErrorAction SilentlyContinue"}')
  }
  [IO.File]::WriteAllText((Join-Path $caseRoot 'workspace_terminal_windows.go'),$caseSource)
  [IO.File]::WriteAllText((Join-Path $caseRoot 'terminal_environment.go'),$helpers)
  Copy-Item -LiteralPath $diagnosticSourcePath -Destination (Join-Path $caseRoot 'terminal_diagnostic_test.go')
  Copy-Item -LiteralPath (Join-Path $bridgeRoot 'go.sum') -Destination (Join-Path $caseRoot 'go.sum')
  [IO.File]::WriteAllText((Join-Path $caseRoot 'go.mod'),$goMod)
  $env:SALCARA_TERMINAL_DIAG_VARIANT=$variant
  Push-Location -LiteralPath $caseRoot
  try {
    & go test -tags terminaldiagnostic -mod=mod -count=1 -v -run '^TestTerminalDiagnostic$' -timeout=120s . 2>&1 | Tee-Object -FilePath (Join-Path $outputRoot ($variant+'.log'))
    $caseExit=$LASTEXITCODE
    $results += [pscustomobject]@{Variant=$variant;ExitCode=$caseExit}
  }finally{Pop-Location}
}
$results | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $outputRoot 'summary.json')
$results | Format-Table -AutoSize
# This manual diagnostic collects observations, including unsupported encodings.
# Normal PTY tests remain the release gate, with their existing assertions intact.
Write-Output ('Diagnostic observations saved to '+$outputRoot)
exit 0
