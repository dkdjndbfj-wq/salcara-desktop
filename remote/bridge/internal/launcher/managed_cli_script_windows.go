//go:build windows

package launcher

import (
	"strings"
	"syscall"
)

// The interactive PowerShell is only a supervisor. It is deliberately outside
// the Job so commands typed into its later -NoExit prompt are never stopped by
// an API change. The non-inheritable Job handle stays here until ALL descendants
// exit, not merely until the initial tool process exits.
func managedCLIScript(p Plan, intent, status, id string) string {
	job, gate := managedCLINames(id)
	_, sddl, _ := managedCLISecurity()
	argv := []string{syscall.EscapeArg(p.Tool.Path)}
	for _, arg := range p.Args {
		argv = append(argv, syscall.EscapeArg(arg))
	}
	return windowsLaunchReceiptScript(status) + `
Add-Type -TypeDefinition @'
using System;
using System.Text;
using System.Runtime.InteropServices;
using System.ComponentModel;
public static class SalcaraOwnedCLI {
  [StructLayout(LayoutKind.Sequential)] public struct SA { public int length; public IntPtr descriptor; public int inherit; }
  [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)] public struct SI {
    public int cb; public string reserved, desktop, title; public int x,y,xs,ys,xc,yc,fill,flags; public short show,reserved2; public IntPtr reservedPtr,input,output,error;
  }
  [StructLayout(LayoutKind.Sequential)] public struct PI { public IntPtr process,thread; public uint pid,tid; }
  [StructLayout(LayoutKind.Sequential)] public struct BASICLIMIT { public long processTime,jobTime; public uint flags; public UIntPtr min,max; public uint active; public UIntPtr affinity; public uint priority,scheduling; }
  [StructLayout(LayoutKind.Sequential)] public struct IOCOUNTERS { public ulong r,w,o,rb,wb,ob; }
  [StructLayout(LayoutKind.Sequential)] public struct LIMITS { public BASICLIMIT basic; public IOCOUNTERS io; public UIntPtr processMemory,jobMemory,peakProcess,peakJob; }
  [StructLayout(LayoutKind.Sequential)] public struct ACCOUNTING { public long a,b,c,d; public uint faults,total,active,terminated; }
  [DllImport("advapi32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern bool ConvertStringSecurityDescriptorToSecurityDescriptor(string s,uint rev,out IntPtr sd,out uint size);
  [DllImport("kernel32.dll")] static extern IntPtr LocalFree(IntPtr p);
  [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern IntPtr CreateJobObject(ref SA sa,string name);
  [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern IntPtr CreateMutex(ref SA sa,bool owner,string name);
  [DllImport("kernel32.dll",SetLastError=true)] public static extern bool ReleaseMutex(IntPtr h);
  [DllImport("kernel32.dll",SetLastError=true)] public static extern uint WaitForSingleObject(IntPtr h,uint ms);
  [DllImport("kernel32.dll",SetLastError=true)] public static extern bool CloseHandle(IntPtr h);
  [DllImport("kernel32.dll",SetLastError=true)] static extern bool SetInformationJobObject(IntPtr h,int info,ref LIMITS limits,uint size);
  [DllImport("kernel32.dll",SetLastError=true)] static extern bool QueryInformationJobObject(IntPtr h,int info,out ACCOUNTING a,uint size,out uint ret);
  [DllImport("kernel32.dll",SetLastError=true)] public static extern bool AssignProcessToJobObject(IntPtr job,IntPtr process);
  [DllImport("kernel32.dll",SetLastError=true)] public static extern bool TerminateJobObject(IntPtr job,uint code);
  [DllImport("kernel32.dll",SetLastError=true)] public static extern bool TerminateProcess(IntPtr process,uint code);
  [DllImport("kernel32.dll",SetLastError=true)] public static extern uint ResumeThread(IntPtr thread);
  [DllImport("kernel32.dll",SetLastError=true)] static extern bool GetProcessTimes(IntPtr p,out long c,out long e,out long k,out long u);
  [DllImport("kernel32.dll",SetLastError=true)] public static extern bool GetExitCodeProcess(IntPtr p,out uint code);
  [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] public static extern bool MoveFileEx(string from,string to,uint flags);
  [DllImport("kernel32.dll",CharSet=CharSet.Unicode,SetLastError=true)] static extern bool CreateProcess(string exe,StringBuilder line,IntPtr pa,IntPtr ta,bool inherit,uint flags,IntPtr env,string dir,ref SI si,out PI pi);
  public static IntPtr NewObject(string name,string sddl,bool mutex) {
    IntPtr sd; uint size;
    if(!ConvertStringSecurityDescriptorToSecurityDescriptor(sddl,1,out sd,out size)) throw new Win32Exception();
    try { SA sa=new SA(); sa.length=Marshal.SizeOf(typeof(SA)); sa.descriptor=sd;
      IntPtr h=mutex?CreateMutex(ref sa,false,name):CreateJobObject(ref sa,name);
      int error=Marshal.GetLastWin32Error();
      if(h==IntPtr.Zero) throw new Win32Exception(error);
      if(!mutex && error==183) { CloseHandle(h); throw new Win32Exception(183); }
      if(!mutex) { LIMITS limits=new LIMITS(); limits.basic.flags=0x2000;
        if(!SetInformationJobObject(h,9,ref limits,(uint)Marshal.SizeOf(typeof(LIMITS)))) { int code=Marshal.GetLastWin32Error(); CloseHandle(h); throw new Win32Exception(code); }
      }
      return h;
    } finally { LocalFree(sd); }
  }
  public static PI Suspended(string exe,string line,string dir) {
    SI si=new SI(); si.cb=Marshal.SizeOf(typeof(SI)); PI pi;
    if(!CreateProcess(exe,new StringBuilder(line),IntPtr.Zero,IntPtr.Zero,false,0x4|0x400,IntPtr.Zero,dir,ref si,out pi)) throw new Win32Exception();
    return pi;
  }
  public static string Created(IntPtr p) { long c,e,k,u; if(!GetProcessTimes(p,out c,out e,out k,out u)) throw new Win32Exception(); return c.ToString(System.Globalization.CultureInfo.InvariantCulture); }
  public static uint Active(IntPtr h) {
    ACCOUNTING a; uint returned;
    if(!QueryInformationJobObject(h,1,out a,(uint)Marshal.SizeOf(typeof(ACCOUNTING)),out returned) || returned!=(uint)Marshal.SizeOf(typeof(ACCOUNTING))) throw new Win32Exception();
    return a.active;
  }
}
'@
$intent = ` + psLiteral(intent) + `
$capability = ` + psLiteral(id) + `
$gateName = ` + psLiteral(gate) + `
$jobName = ` + psLiteral(job) + `
$security = ` + psLiteral(sddl) + `
$mutex = [IntPtr]::Zero
$job = [IntPtr]::Zero
$process = [IntPtr]::Zero
$thread = [IntPtr]::Zero
$locked = $false
$assigned = $false
$created = $false
function Enter-CLIGate {
  $result = [SalcaraOwnedCLI]::WaitForSingleObject($mutex, 15000)
  if ($result -ne 0 -and $result -ne 128) { throw [InvalidOperationException]::new('Gate unavailable') }
  $script:locked = $true
}
function Leave-CLIGate {
  if ($script:locked) { [SalcaraOwnedCLI]::ReleaseMutex($mutex) | Out-Null; $script:locked=$false }
}
function Read-CLIIntent {
  $r = [IO.File]::ReadAllText($intent) | ConvertFrom-Json
  if ($r.protocol -ne 'salcara-owned-cli.v1' -or $r.id -cne $capability) { throw [InvalidOperationException]::new('Intent changed') }
  return $r
}
function Save-CLIIntent($r) {
  $tmp = $intent + '.' + [Guid]::NewGuid().ToString('N') + '.tmp'
  try {
    $data = [Text.Encoding]::UTF8.GetBytes(($r | ConvertTo-Json -Compress))
    $file = [IO.FileStream]::new($tmp,[IO.FileMode]::CreateNew,[IO.FileAccess]::Write,[IO.FileShare]::None)
    try { $file.Write($data,0,$data.Length); $file.Flush($true) } finally { $file.Dispose() }
    if (-not [SalcaraOwnedCLI]::MoveFileEx($tmp,$intent,9)) { throw [ComponentModel.Win32Exception]::new() }
  } finally { if ([IO.File]::Exists($tmp)) { [IO.File]::Delete($tmp) } }
}
try {
  $mutex = [SalcaraOwnedCLI]::NewObject($gateName,$security,$true)
  Enter-CLIGate
  $r = Read-CLIIntent
  if ($r.cancelled -or $r.state -ne 'pending') { throw [InvalidOperationException]::new('Launch revoked') }
  $job = [SalcaraOwnedCLI]::NewObject($jobName,$security,$false)
  # Arm the durable fence before CreateProcess, including the suspended window.
  $r.state='running'; $r.zero=$false; Save-CLIIntent $r
  $pi = [SalcaraOwnedCLI]::Suspended(` + psLiteral(p.Tool.Path) + `,` + psLiteral(strings.Join(argv, " ")) + `,` + psLiteral(p.Workspace) + `)
  $created=$true; $process=$pi.process; $thread=$pi.thread
  if (-not [SalcaraOwnedCLI]::AssignProcessToJobObject($job,$process)) { throw [ComponentModel.Win32Exception]::new() }
  $assigned=$true
  $r.pid=$pi.pid; $r.created=[SalcaraOwnedCLI]::Created($process); Save-CLIIntent $r
  if ([SalcaraOwnedCLI]::ResumeThread($thread) -eq [uint32]::MaxValue) { throw [ComponentModel.Win32Exception]::new() }
  [SalcaraOwnedCLI]::CloseHandle($thread) | Out-Null; $thread=[IntPtr]::Zero
  Write-LaunchReceipt @{state='started';pid=$pi.pid}
  Leave-CLIGate
  $wait = [SalcaraOwnedCLI]::WaitForSingleObject($process,1200)
  if ($wait -eq 0) {
    [uint32]$code=0
    if (-not [SalcaraOwnedCLI]::GetExitCodeProcess($process,[ref]$code)) { throw [ComponentModel.Win32Exception]::new() }
    if ($code -ne 0) {
      # A nonzero root can leave working descendants. A rollback-capable
      # failure receipt is legal only after the entire owned Job is empty.
      if ([SalcaraOwnedCLI]::Active($job) -ne 0) {
        if (-not [SalcaraOwnedCLI]::TerminateJobObject($job,1)) { throw [ComponentModel.Win32Exception]::new() }
        $until=[DateTime]::UtcNow.AddSeconds(8)
        while ([SalcaraOwnedCLI]::Active($job) -ne 0 -and [DateTime]::UtcNow -lt $until) { [Threading.Thread]::Sleep(30) }
        if ([SalcaraOwnedCLI]::Active($job) -ne 0) { throw [InvalidOperationException]::new('Tree exit unconfirmed') }
      }
      Write-LaunchReceipt @{state='failed';pid=$pi.pid;exitCode=[int32]$code}
    }
    else { Write-LaunchReceipt @{state='ready';pid=$pi.pid} }
  } elseif ($wait -eq 258) { Write-LaunchReceipt @{state='ready';pid=$pi.pid} }
  else { throw [ComponentModel.Win32Exception]::new() }
  # The initial executable may spawn a worker and exit. Keep ownership until
  # the kernel confirms the entire tree has ended.
  while ([SalcaraOwnedCLI]::Active($job) -ne 0) { [Threading.Thread]::Sleep(100) }
  Enter-CLIGate
  $r=Read-CLIIntent; $r.state='stopped'; $r.zero=$true; Save-CLIIntent $r
  Leave-CLIGate
} catch {
  # Do not expose exception text (which may contain the script or arguments).
  $failure=New-LaunchFailure $_.Exception
  if ($created) { $failure.pid=$pi.pid }
  try { Write-LaunchReceipt $failure } catch {}
} finally {
  # Any exception after CreateProcess still terminates the owned tree. An
  # unassigned child was never resumed and cannot have descendants.
  $safe=$false
  if ($created -and -not $assigned) {
    [SalcaraOwnedCLI]::TerminateProcess($process,1) | Out-Null
    $safe=([SalcaraOwnedCLI]::WaitForSingleObject($process,10000) -eq 0)
  } elseif ($job -ne [IntPtr]::Zero) {
    try {
      if ([SalcaraOwnedCLI]::Active($job) -ne 0) { [SalcaraOwnedCLI]::TerminateJobObject($job,1) | Out-Null }
      $until=[DateTime]::UtcNow.AddSeconds(10)
      while ([SalcaraOwnedCLI]::Active($job) -ne 0 -and [DateTime]::UtcNow -lt $until) { [Threading.Thread]::Sleep(30) }
      $safe=([SalcaraOwnedCLI]::Active($job) -eq 0)
    } catch {}
  } elseif (-not $created) { $safe=$true }
  if ($safe -and $mutex -ne [IntPtr]::Zero) {
    try { if (-not $locked) { Enter-CLIGate }; $r=Read-CLIIntent; $r.state='stopped'; $r.zero=$true; Save-CLIIntent $r } catch {}
  }
  Leave-CLIGate
  if ($thread -ne [IntPtr]::Zero) { [SalcaraOwnedCLI]::CloseHandle($thread) | Out-Null }
  if ($process -ne [IntPtr]::Zero) { [SalcaraOwnedCLI]::CloseHandle($process) | Out-Null }
  if ($job -ne [IntPtr]::Zero) { [SalcaraOwnedCLI]::CloseHandle($job) | Out-Null }
  if ($mutex -ne [IntPtr]::Zero) { [SalcaraOwnedCLI]::CloseHandle($mutex) | Out-Null }
  if ($safe -and ` + psLiteral(p.ClaudeSettingsPin) + ` -ne '') {
    Remove-Item -LiteralPath ` + psLiteral(p.ClaudeSettingsPin) + ` -Force -ErrorAction SilentlyContinue
    try { [IO.Directory]::Delete([IO.Path]::GetDirectoryName(` + psLiteral(p.ClaudeSettingsPin) + `),$false) } catch {}
  }
}
`
}
