param(
  [ValidateSet('Parent', 'Worker')][string]$Mode = 'Parent',
  [string]$InstallerPath,
  [string]$PayloadPath,
  [string]$FixtureRoot,
  [string]$ExpectedSid
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Assert-Condition([bool]$Condition, [string]$Message) {
  if (-not $Condition) { throw $Message }
}

function Start-Installer([string]$File, [string]$Extra = '') {
  $log = Join-Path $FixtureRoot ('installer-' + [Guid]::NewGuid().ToString('N') + '.log')
  $arguments = '/VERYSILENT /SUPPRESSMSGBOXES /NORESTART /NOCLOSEAPPLICATIONS /NORESTARTAPPLICATIONS /SP- /TASKS="desktopicon" /LOG="' + $log + '" ' + $Extra
  $process = Start-Process -FilePath $File -ArgumentList $arguments -WindowStyle Hidden -PassThru -Wait
  return $process.ExitCode
}

function Remove-FixtureTree([string]$Directory) {
  $item = Get-Item -LiteralPath $Directory -Force -ErrorAction SilentlyContinue
  if (-not $item) { return }
  Assert-Condition (($item.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0) 'Refusing linked fixture cleanup'
  foreach ($child in Get-ChildItem -LiteralPath $Directory -Force) {
    Assert-Condition (($child.Attributes -band [IO.FileAttributes]::ReparsePoint) -eq 0) 'Refusing linked fixture child cleanup'
    if ($child.PSIsContainer) { Remove-FixtureTree $child.FullName }
    else { Remove-Item -LiteralPath $child.FullName -Force }
  }
  Remove-Item -LiteralPath $Directory -Force
}

if ($Mode -eq 'Worker') {
  $identity = [Security.Principal.WindowsIdentity]::GetCurrent()
  Assert-Condition ($ExpectedSid -match '^S-1-5-21-(\d+-){3}\d+$' -and $identity.User.Value -eq $ExpectedSid) 'The smoke worker must use the disposable account'
  $FixtureRoot = [IO.Path]::GetFullPath($FixtureRoot)
  $identityFile = Join-Path $FixtureRoot '.salcara-installer-smoke'
  Assert-Condition (([IO.File]::ReadAllText($identityFile)).Trim() -eq $ExpectedSid) 'Invalid isolated fixture identity'
  $profile = [Environment]::GetFolderPath('UserProfile')
  $roaming = [Environment]::GetFolderPath('ApplicationData')
  $local = [Environment]::GetFolderPath('LocalApplicationData')
  Assert-Condition ([IO.Path]::GetFileName($profile) -like 'SalcaraCI_*') 'The worker profile must be disposable'
  foreach ($folder in @($roaming, $local)) {
    Assert-Condition ($folder.StartsWith($profile + '\', [StringComparison]::OrdinalIgnoreCase)) 'Known folders escaped the disposable profile'
  }
  # CreateProcessWithLogonW can inherit the parent environment. Replace only
  # this worker's process environment with its own token's known folders.
  $env:USERPROFILE = $profile
  $env:APPDATA = $roaming
  $env:LOCALAPPDATA = $local
  $env:TEMP = Join-Path $local 'Temp'
  $env:TMP = $env:TEMP
  New-Item -ItemType Directory -Path $env:TEMP -Force | Out-Null

  try {
    $expected = Get-Content -LiteralPath (Join-Path $FixtureRoot 'expected.json') -Raw | ConvertFrom-Json
    $setup = Join-Path $FixtureRoot 'setup.exe'
    $root = Join-Path $local 'Programs\Salcara Desktop'
    $app = Join-Path $root 'app'
    $exe = Join-Path $app 'Salcara Bridge.exe'
    $uninstall = Join-Path $root 'unins000.exe'
    $uninstallKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\top.salcara.desktop.peruser_is1'
    $runKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
    $configFiles = @()
    foreach ($name in @('SalcaraBridge', 'Salcara Bridge', 'salcara-bridge-desktop')) {
      $directory = Join-Path $roaming $name
      New-Item -ItemType Directory -Path $directory -Force | Out-Null
      $file = Join-Path $directory 'smoke-config.json'
      [IO.File]::WriteAllText($file, '{"fixture":"retain-userdata"}')
      $configFiles += $file
    }
    Assert-Condition ((Start-Installer $setup) -eq 0) 'First installation failed'
    Assert-Condition (Test-Path -LiteralPath $uninstall -PathType Leaf) 'The uninstall program must be outside the payload'
    Assert-Condition (Test-Path -LiteralPath (Join-Path $root '.salcara-installer') -PathType Leaf) 'Installer ownership marker missing'
    foreach ($file in $expected.files) {
      $installed = Join-Path $app $file.name
      Assert-Condition ((Get-FileHash -LiteralPath $installed -Algorithm SHA256).Hash.ToLowerInvariant() -eq $file.sha256) ('Installed payload mismatch: ' + $file.name)
    }
    Assert-Condition ((Get-ItemProperty -LiteralPath $uninstallKey).DisplayVersion -eq $expected.version) 'HKCU uninstall version mismatch'
    $shell = New-Object -ComObject WScript.Shell
    foreach ($shortcut in @(
      (Join-Path ([Environment]::GetFolderPath('Programs')) 'Salcara Desktop.lnk'),
      (Join-Path ([Environment]::GetFolderPath('DesktopDirectory')) 'Salcara Desktop.lnk'))) {
      Assert-Condition (Test-Path -LiteralPath $shortcut -PathType Leaf) 'Expected current-user shortcut missing'
      Assert-Condition ($shell.CreateShortcut($shortcut).TargetPath -eq $exe) 'Shortcut target must be the stable app path'
    }
    Assert-Condition ((Start-Installer $setup) -eq 0) 'Same-version reinstall failed'

    $otherRoot = Join-Path $FixtureRoot 'rejected-other-install-root'
    Assert-Condition ((Start-Installer $setup ('/DIR="' + $otherRoot + '"')) -ne 0) 'A user-specified installation root was accepted'
    Assert-Condition (-not (Test-Path -LiteralPath $otherRoot)) 'Rejected installation wrote another root'
    $lock = [IO.File]::Open((Join-Path $app 'resources\app.asar'), [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::None)
    try { Assert-Condition ((Start-Installer $setup) -ne 0) 'A locked application file was overwritten' }
    finally { $lock.Dispose() }

    $marker = Join-Path $app '.salcara-install.json'
    $originalMarker = [IO.File]::ReadAllText($marker)
    $parts = $expected.version.Split('.')
    $newer = $parts[0] + '.' + $parts[1] + '.' + ([int]$parts[2] + 1)
    [IO.File]::WriteAllText($marker, '{"product":"salcara-desktop","version":"' + $newer + '"}' + "`n")
    Assert-Condition ((Start-Installer $setup) -ne 0) 'An older installer downgraded an auto-updated payload'
    Assert-Condition (([IO.File]::ReadAllText($marker)) -match ('"version":"' + [regex]::Escape($newer) + '"')) 'Rejected downgrade changed the marker'
    [IO.File]::WriteAllText($marker, $originalMarker)

    $external = Join-Path $local 'salcara-smoke-external'
    New-Item -ItemType Directory -Path $external -Force | Out-Null
    $externalFile = Join-Path $external 'retain.txt'
    [IO.File]::WriteAllText($externalFile, 'outside-the-payload')
    $junction = Join-Path $app 'linked-directory'
    New-Item -ItemType Junction -Path $junction -Target $external | Out-Null
    Assert-Condition ((Start-Installer $setup) -ne 0) 'Installation traversed a reparse directory'
    Assert-Condition ((Start-Installer $uninstall) -ne 0) 'Uninstall traversed a reparse directory'
    Assert-Condition (([IO.File]::ReadAllText($externalFile)) -eq 'outside-the-payload') 'An external file changed'
    [IO.Directory]::Delete($junction)

    $setupRecovery = Join-Path $root '.salcara-setup-recovery'
    New-Item -ItemType Directory -Path $setupRecovery -Force | Out-Null
    [IO.File]::WriteAllText((Join-Path $setupRecovery '.salcara-install.json'), $originalMarker)
    [IO.File]::WriteAllText((Join-Path $setupRecovery 'retain.txt'), 'interrupted-installer-copy')
    Assert-Condition ((Start-Installer $setup) -ne 0) 'An interrupted installer recovery copy was overwritten'
    [IO.File]::WriteAllText((Join-Path $app 'auto-added-file.txt'), 'introduced-by-update')
    [IO.File]::WriteAllText((Join-Path $root 'retain-parent-file.txt'), 'not-application-payload')
    $backup = Join-Path $root 'app.old-123'
    New-Item -ItemType Directory -Path $backup -Force | Out-Null
    [IO.File]::WriteAllText((Join-Path $backup 'retain.txt'), 'recovery-copy')
    New-Item -Path $runKey -Force | Out-Null
    $ownedRun = '"' + $exe + '" --background'
    New-ItemProperty -LiteralPath $runKey -Name 'SalcaraBridge' -Value $ownedRun -PropertyType String -Force | Out-Null
    Assert-Condition ((Start-Installer $uninstall) -eq 0) 'Uninstall failed'
    Assert-Condition (-not (Test-Path -LiteralPath $app)) 'Uninstall retained an update-added payload file'
    Assert-Condition (-not (Test-Path -LiteralPath $uninstallKey)) 'Uninstall retained its registration'
    Assert-Condition (-not (Get-ItemProperty -LiteralPath $runKey).PSObject.Properties['SalcaraBridge']) 'Owned autostart value was retained'
    Assert-Condition (([IO.File]::ReadAllText((Join-Path $root 'retain-parent-file.txt'))) -eq 'not-application-payload') 'Uninstall removed a parent file'
    Assert-Condition (([IO.File]::ReadAllText((Join-Path $backup 'retain.txt'))) -eq 'recovery-copy') 'Uninstall removed a recovery copy'
    Assert-Condition (([IO.File]::ReadAllText((Join-Path $setupRecovery 'retain.txt'))) -eq 'interrupted-installer-copy') 'Uninstall removed an installer recovery copy'
    foreach ($file in $configFiles) { Assert-Condition (([IO.File]::ReadAllText($file)) -eq '{"fixture":"retain-userdata"}') 'Uninstall changed user configuration' }

    # A parent containing unrelated files is no longer owned after uninstall.
    # Clear only the deliberately created sentinels before a fresh install.
    Remove-Item -LiteralPath (Join-Path $root 'retain-parent-file.txt') -Force
    Remove-FixtureTree $backup
    Remove-FixtureTree $setupRecovery
    Assert-Condition ((Start-Installer $setup) -eq 0) 'Fresh install after uninstall failed'
    $foreignRun = '"C:\Different Installation\Salcara Bridge.exe" --background'
    New-ItemProperty -LiteralPath $runKey -Name 'SalcaraBridge' -Value $foreignRun -PropertyType String -Force | Out-Null
    Assert-Condition ((Start-Installer $uninstall) -eq 0) 'Second uninstall failed'
    Assert-Condition ((Get-ItemProperty -LiteralPath $runKey).SalcaraBridge -eq $foreignRun) 'Uninstall removed another installation autostart value'
    $running = @(Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -and $_.ExecutablePath.StartsWith($root + '\', [StringComparison]::OrdinalIgnoreCase) })
    Assert-Condition ($running.Count -eq 0) 'Silent smoke launched an application'
    [IO.File]::WriteAllText((Join-Path $FixtureRoot 'result.json'), (@{ passed = $true; version = $expected.version; profile = $profile; appLaunched = $false } | ConvertTo-Json -Compress))
  } catch {
    [IO.File]::WriteAllText((Join-Path $FixtureRoot 'result.json'), (@{ passed = $false; error = $_.Exception.Message } | ConvertTo-Json -Compress))
    exit 1
  }
  exit 0
}

# Native installation is restricted to a disposable GitHub Actions Windows
# runner. No real developer profile, desktop app, or Agent is used by this test.
Assert-Condition ($env:GITHUB_ACTIONS -eq 'true' -and $env:RUNNER_OS -eq 'Windows') 'Installer smoke only runs on a GitHub Actions Windows runner'
$principal = [Security.Principal.WindowsPrincipal]::new([Security.Principal.WindowsIdentity]::GetCurrent())
Assert-Condition ($principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) 'The runner must create an isolated local test account'
$desktopRoot = Split-Path -Parent $PSScriptRoot
$version = (Get-Content -LiteralPath (Join-Path $desktopRoot 'package.json') -Raw | ConvertFrom-Json).version
if (-not $InstallerPath) { $InstallerPath = Join-Path $desktopRoot "out\release\Salcara-Desktop-$version-win32-x64-setup.exe" }
if (-not $PayloadPath) { $PayloadPath = Join-Path $desktopRoot 'out\Salcara Bridge-win32-x64' }
$InstallerPath = (Get-Item -LiteralPath $InstallerPath).FullName
$PayloadPath = (Get-Item -LiteralPath $PayloadPath).FullName
$runnerTemp = (Get-Item -LiteralPath $env:RUNNER_TEMP).FullName
$FixtureRoot = Join-Path $runnerTemp ('salcara-install-smoke-' + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $FixtureRoot | Out-Null
$taskUser = 'SalcaraCI_' + [Guid]::NewGuid().ToString('N').Substring(0, 8)
$account = $null
try {
  $bytes = New-Object byte[] 32
  $random = [Security.Cryptography.RandomNumberGenerator]::Create()
  try { $random.GetBytes($bytes) } finally { $random.Dispose() }
  $securePassword = ConvertTo-SecureString ('sC!9' + [Convert]::ToBase64String($bytes)) -AsPlainText -Force
  $account = New-LocalUser -Name $taskUser -Password $securePassword -AccountNeverExpires -PasswordNeverExpires -Description 'Disposable Salcara installer CI test'
  $usersGroup = Get-LocalGroup -SID ([Security.Principal.SecurityIdentifier]::new('S-1-5-32-545'))
  Add-LocalGroupMember -Group $usersGroup -Member $account
  $acl = Get-Acl -LiteralPath $FixtureRoot
  $rule = [Security.AccessControl.FileSystemAccessRule]::new($account.SID, 'Modify', 'ContainerInherit,ObjectInherit', 'None', 'Allow')
  $acl.AddAccessRule($rule)
  Set-Acl -LiteralPath $FixtureRoot -AclObject $acl
  [IO.File]::WriteAllText((Join-Path $FixtureRoot '.salcara-installer-smoke'), $account.SID.Value)
  Copy-Item -LiteralPath $InstallerPath -Destination (Join-Path $FixtureRoot 'setup.exe')
  Copy-Item -LiteralPath $PSCommandPath -Destination (Join-Path $FixtureRoot 'worker.ps1')
  $names = @('Salcara Bridge.exe', 'resources/app.asar', 'resources/SalcaraBridge.exe', 'resources/SalcaraProbeNode.exe',
    'resources/desktop-companion/src/stdio.mjs', 'resources/NODE-LICENSE.txt', 'resources/GO-THIRD-PARTY-NOTICES.txt', '.salcara-install.json')
  $files = @($names | ForEach-Object { @{ name = $_; sha256 = (Get-FileHash -LiteralPath (Join-Path $PayloadPath $_) -Algorithm SHA256).Hash.ToLowerInvariant() } })
  [IO.File]::WriteAllText((Join-Path $FixtureRoot 'expected.json'), (@{ version = $version; files = $files } | ConvertTo-Json -Depth 5))
  $credential = [Management.Automation.PSCredential]::new($env:COMPUTERNAME + '\' + $taskUser, $securePassword)
  $powershell = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'
  $arguments = '-NoProfile -NonInteractive -ExecutionPolicy Bypass -File "' + (Join-Path $FixtureRoot 'worker.ps1') + '" -Mode Worker -FixtureRoot "' + $FixtureRoot + '" -ExpectedSid "' + $account.SID.Value + '"'
  $worker = Start-Process -FilePath $powershell -ArgumentList $arguments -Credential $credential -LoadUserProfile -WindowStyle Hidden -PassThru -Wait
  $resultFile = Join-Path $FixtureRoot 'result.json'
  Assert-Condition (Test-Path -LiteralPath $resultFile -PathType Leaf) 'The isolated worker did not produce a result'
  $result = Get-Content -LiteralPath $resultFile -Raw | ConvertFrom-Json
  if ($worker.ExitCode -ne 0 -or -not $result.passed) { throw ('Installer smoke failed: ' + $result.error) }
  Write-Output ($result | ConvertTo-Json -Compress)
} finally {
  if ($account) {
    $profile = Get-CimInstance Win32_UserProfile -Filter ("SID='" + $account.SID.Value + "'")
    if ($profile) {
      $profilePath = [IO.Path]::GetFullPath($profile.LocalPath)
      $usersRoot = [IO.Path]::GetFullPath((Join-Path $env:SystemDrive 'Users'))
      Assert-Condition (-not $profile.Special -and [IO.Path]::GetDirectoryName($profilePath) -eq $usersRoot -and [IO.Path]::GetFileName($profilePath).StartsWith($taskUser, [StringComparison]::OrdinalIgnoreCase)) 'Refusing to remove an unexpected user profile'
      Assert-Condition (-not $profile.Loaded) 'The disposable worker profile is still loaded'
      Remove-CimInstance -InputObject $profile
    }
    Remove-LocalUser -SID $account.SID
  }
  Assert-Condition ([IO.Path]::GetDirectoryName([IO.Path]::GetFullPath($FixtureRoot)) -eq [IO.Path]::GetFullPath($runnerTemp)) 'Refusing fixture cleanup outside RUNNER_TEMP'
  Remove-FixtureTree $FixtureRoot
}
