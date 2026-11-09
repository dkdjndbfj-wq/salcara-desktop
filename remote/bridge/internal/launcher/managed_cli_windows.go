//go:build windows

package launcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"salcara/bridge/internal/atomicfile"
	"salcara/bridge/internal/config"
)

const managedCLIProtocol = "salcara-owned-cli.v1"

// The receipt contains no API, prompt, command line, or session history. ID is
// an unguessable capability for this one Job; PID is diagnostic, not authority.
type managedCLIReceipt struct {
	Protocol  string `json:"protocol"`
	ID        string `json:"id"`
	Host      string `json:"host"`
	Boot      string `json:"boot"`
	Owner     string `json:"owner"`
	Family    string `json:"family"`
	Target    string `json:"target"`
	Shared    bool   `json:"shared"`
	State     string `json:"state"`
	Cancelled bool   `json:"cancelled"`
	Zero      bool   `json:"zero"`
	PID       uint32 `json:"pid"`
	Created   string `json:"created"`
}

var managedCapability = regexp.MustCompile(`^[a-f0-9]{64}$`)

func managedCLINames(id string) (job, mutex string) {
	return `Global\SalcaraCLIJob-v1-` + id, `Global\SalcaraCLIGate-v1-` + id
}

func managedCLIIdentity() (host, boot, owner string, err error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", "", "", err
	}
	owner = u.User.Sid.String()
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", "", "", err
	}
	defer k.Close()
	machine, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return "", "", "", err
	}
	machine = strings.ToLower(strings.ReplaceAll(strings.Trim(machine, "{}"), "-", ""))
	if len(machine) != 32 || strings.Trim(machine, "0") == "" || strings.Trim(machine, "f") == "" {
		return "", "", "", errors.New("invalid machine identity")
	}
	if _, err = hex.DecodeString(machine); err != nil {
		return "", "", "", err
	}
	var info struct {
		ID       windows.GUID
		Firmware uint32
		Pad      uint32
		Flags    uint64
	}
	var length uint32
	if err = windows.NtQuerySystemInformation(windows.SystemBootEnvironmentInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), &length); err != nil {
		return "", "", "", err
	}
	if length < 20 || length > uint32(unsafe.Sizeof(info)) || info.ID == (windows.GUID{}) {
		return "", "", "", errors.New("invalid boot identity")
	}
	h := sha256.Sum256([]byte("salcara-cli-host-v1\x00" + machine + "\x00" + owner))
	b := sha256.Sum256([]byte("salcara-cli-boot-v1\x00" + info.ID.String()))
	return hex.EncodeToString(h[:]), hex.EncodeToString(b[:]), owner, nil
}

func managedCLISecurity() (*windows.SECURITY_DESCRIPTOR, string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, "", err
	}
	sddl := "D:P(A;OICI;FA;;;" + u.User.Sid.String() + ")(A;OICI;FA;;;SY)"
	sd, err := windows.SecurityDescriptorFromString(sddl)
	return sd, sddl, err
}

func managedCLIPrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := managedCLINoReparse(dir); err != nil {
		return err
	}
	sd, _, err := managedCLISecurity()
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}

func managedCLINoReparse(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	attr, err := windows.GetFileAttributes(p)
	if err != nil {
		return err
	}
	if attr&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("ownership path is a reparse point")
	}
	return nil
}

func readManagedCLI(path string) (r managedCLIReceipt, err error) {
	if err = managedCLINoReparse(path); err != nil {
		return r, err
	}
	f, err := os.Open(path)
	if err != nil {
		return r, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 8193))
	if err != nil || len(b) > 8192 {
		return r, errors.New("invalid ownership receipt")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || d.Decode(new(any)) != io.EOF {
		return r, errors.New("invalid ownership receipt")
	}
	if r.Protocol != managedCLIProtocol || !managedCapability.MatchString(r.ID) || !managedCapability.MatchString(r.Host) || !managedCapability.MatchString(r.Boot) || !ValidTarget(r.Target) || config.ToolFamily(r.Target) != r.Family || !strings.HasPrefix(r.Owner, "S-1-") || r.Created != "" && len(r.Created) > 24 {
		return r, errors.New("invalid ownership receipt")
	}
	if filepath.Base(path) != r.ID+".json" {
		return r, errors.New("ownership capability mismatch")
	}
	switch r.State {
	case "pending", "running", "stopped":
	default:
		return r, errors.New("invalid ownership state")
	}
	if r.State == "stopped" && !r.Zero || r.State != "stopped" && r.Zero {
		return r, errors.New("invalid ownership proof")
	}
	return r, nil
}

func writeManagedCLI(path string, r managedCLIReceipt) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, b, 0600)
}

// Windows mutex ownership is bound to an OS thread, not a goroutine.
func lockManagedCLI(ctx context.Context, id string) (func(), error) {
	_, name := managedCLINames(id)
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	sd, _, err := managedCLISecurity()
	if err != nil {
		return nil, err
	}
	sa := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	h, err := windows.CreateMutex(&sa, false, n)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return nil, err
	}
	runtime.LockOSThread()
	for {
		if err := ctx.Err(); err != nil {
			windows.CloseHandle(h)
			runtime.UnlockOSThread()
			return nil, err
		}
		status, err := windows.WaitForSingleObject(h, 50)
		if err != nil {
			windows.CloseHandle(h)
			runtime.UnlockOSThread()
			return nil, err
		}
		if status == windows.WAIT_OBJECT_0 || status == windows.WAIT_ABANDONED {
			return func() { windows.ReleaseMutex(h); windows.CloseHandle(h); runtime.UnlockOSThread() }, nil
		}
	}
}

func startManagedTerminal(ctx context.Context, p Plan) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !ValidTarget(p.Tool.ID) || isDesktop(p.Tool.ID) || p.RuntimeDir == "" {
		return 0, errors.New("工具启动缺少受管运行目录")
	}
	dir := filepath.Join(p.RuntimeDir, "managed-cli-v1")
	if err := managedCLIPrivateDir(dir); err != nil {
		return 0, safeWindowsLaunchError("无法保护工具运行回执", err)
	}
	host, boot, owner, err := managedCLIIdentity()
	if err != nil {
		return 0, errors.New("无法验证当前电脑的运行身份，未启动工具")
	}
	id := config.RandomToken(32)
	r := managedCLIReceipt{Protocol: managedCLIProtocol, ID: id, Host: host, Boot: boot, Owner: owner, Family: config.ToolFamily(p.Tool.ID), Target: p.Tool.ID, Shared: p.Shared, State: "pending"}
	path := filepath.Join(dir, id+".json")
	// Persist intent before any helper can start. Never remove this fence on an
	// uncertain helper response or a cancelled HTTP request.
	if err := writeManagedCLI(path, r); err != nil {
		return 0, safeWindowsLaunchError("无法保存工具启动意图", err)
	}
	status := filepath.Join(dir, id+".status")
	scriptPath := filepath.Join(dir, id+".ps1")
	if err := os.WriteFile(scriptPath, append([]byte{0xef, 0xbb, 0xbf}, []byte(managedCLIScript(p, path, status, id))...), 0600); err != nil {
		return 0, safeWindowsLaunchError("写入受管启动程序失败", err)
	}
	defer os.Remove(scriptPath)
	defer os.Remove(status)
	defer os.Remove(status + ".started")
	args := []string{"-NoLogo", "-NoProfile", "-NoExit", "-ExecutionPolicy", "Bypass", "-File", scriptPath}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = syscall.EscapeArg(a)
	}
	outer := windowsLaunchExceptionScript + "\ntry { $psi=New-Object System.Diagnostics.ProcessStartInfo; $psi.FileName=" + psLiteral(powershell()) + "; $psi.Arguments=" + psLiteral(strings.Join(quoted, " ")) + "; $psi.WorkingDirectory=" + psLiteral(p.Workspace) + "; $psi.UseShellExecute=$true; $psi.WindowStyle='Normal'; $p=[System.Diagnostics.Process]::Start($psi); @{state='shell';pid=$p.Id}|ConvertTo-Json -Compress } catch { New-LaunchFailure $_.Exception|ConvertTo-Json -Compress }"
	launchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(launchCtx, powershell(), "-NoProfile", "-NonInteractive", "-Command", outer)
	cmd.Env = p.Environment
	configureProcess(cmd, false)
	LaunchPhase(ctx, "start", "创建工具进程", "打开受管交互终端")
	_, _ = cmd.Output()
	// A failed/lost helper response can race actual process creation. Read the
	// child's receipt; an unresolved intent remains a durable stop fence.
	pid, err := waitWindowsLaunchReceiptWithPin(launchCtx, status, "受管工具入口进程", true)
	if err != nil {
		return pid, err
	}
	final, finalErr := readWindowsLaunchReceipt(status)
	if finalErr != nil || final.State != "ready" || final.PID != pid {
		// A started receipt proves creation, but not completion of the startup
		// check. Preserve config and ownership while surfacing uncertainty.
		return 0, nil
	}
	return pid, nil
}

var openManagedJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("OpenJobObjectW")

func openManagedCLIJob(id string) (windows.Handle, error) {
	name, _ := managedCLINames(id)
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	h, _, callErr := openManagedJob.Call(0x0004|0x0008, 0, uintptr(unsafe.Pointer(n)))
	if h == 0 {
		return 0, callErr
	}
	return windows.Handle(h), nil
}

func managedCLIActive(h windows.Handle) (uint32, error) {
	var accounting struct {
		Times                             [4]int64
		Faults, Total, Active, Terminated uint32
	}
	var returned uint32
	err := windows.QueryInformationJobObject(h, windows.JobObjectBasicAccountingInformation, uintptr(unsafe.Pointer(&accounting)), uint32(unsafe.Sizeof(accounting)), &returned)
	if err != nil || returned != uint32(unsafe.Sizeof(accounting)) {
		return 0, errors.New("无法核实工具进程树状态")
	}
	return accounting.Active, nil
}

func stopManagedCLIReceipt(ctx context.Context, path, host, boot, owner string) error {
	r, err := readManagedCLI(path)
	if err != nil {
		return errors.New("工具运行回执无法读取，未替换 API 配置")
	}
	unlock, err := lockManagedCLI(ctx, r.ID)
	if err != nil {
		return fmt.Errorf("等待工具启动/停止锁失败：%w", err)
	}
	defer unlock()
	r, err = readManagedCLI(path)
	if err != nil {
		return errors.New("工具运行回执已改变，未替换 API 配置")
	}
	if r.Host != host || r.Owner != owner {
		return errors.New("工具运行回执不属于当前电脑或用户，未替换 API 配置")
	}
	if r.Boot != boot || r.State == "stopped" && r.Zero || r.State == "pending" {
		// Under this mutex, pending proves the supervisor has not created a
		// target. The tombstone prevents a delayed helper from ever resuming it.
		r.Cancelled, r.State, r.Zero = true, "stopped", true
		return writeManagedCLI(path, r)
	}
	r.Cancelled = true
	if err := writeManagedCLI(path, r); err != nil {
		return errors.New("无法撤销旧工具的启动许可，未替换 API 配置")
	}
	h, err := openManagedCLIJob(r.ID)
	if err != nil {
		return errors.New("旧工具的进程树归属无法确认，未替换 API 配置；请检查终端或重启电脑后再试")
	}
	defer windows.CloseHandle(h)
	if err := windows.TerminateJobObject(h, 1); err != nil {
		return errors.New("无法停止所选 Agent 的进程树，未替换 API 配置")
	}
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	for {
		count, err := managedCLIActive(h)
		if err != nil {
			return err
		}
		if count == 0 {
			r.State, r.Zero = "stopped", true
			return writeManagedCLI(path, r)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("工具尚未确认完全停止，未替换 API 配置：%w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func managedCLIFamilyReceipts(p Plan) ([]string, error) {
	if p.RuntimeDir == "" {
		return nil, errors.New("没有受管运行目录")
	}
	root := filepath.Dir(p.RuntimeDir)
	var paths []string
	for _, target := range toolOrder {
		if isDesktop(target) || config.ToolFamily(target) != config.ToolFamily(p.Tool.ID) {
			continue
		}
		dir := filepath.Join(root, target, "managed-cli-v1")
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := managedCLINoReparse(dir); err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".json") {
				r, err := readManagedCLI(filepath.Join(dir, entry.Name()))
				if err != nil {
					return nil, err
				}
				if r.Target != target || r.Family != config.ToolFamily(p.Tool.ID) {
					return nil, errors.New("ownership family mismatch")
				}
				paths = append(paths, filepath.Join(dir, entry.Name()))
			}
		}
	}
	return paths, nil
}

func stopManagedCLI(ctx context.Context, p Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	host, boot, owner, err := managedCLIIdentity()
	if err != nil {
		return errors.New("无法验证当前电脑运行身份，未替换 API 配置")
	}
	paths, err := managedCLIFamilyReceipts(p)
	if err != nil {
		return errors.New("无法确认旧工具运行记录，未替换 API 配置")
	}
	for _, path := range paths {
		r, err := readManagedCLI(path)
		if err != nil {
			return errors.New("无法读取旧工具归属，未替换 API 配置")
		}
		if !r.Shared {
			continue
		}
		if err := stopManagedCLIReceipt(ctx, path, host, boot, owner); err != nil {
			return err
		}
	}
	return rejectUnownedCLI(ctx, p, paths, host, boot, owner)
}

// This inventory never kills an unowned process. Generic node/python names are
// insufficient; only a known CLI image or package entry can cause a refusal.
type managedCLIProcess struct {
	ProcessID                         uint32
	Name, ExecutablePath, CommandLine string
}

func rejectUnownedCLI(ctx context.Context, p Plan, receipts []string, host, boot, owner string) error {
	probe := `$ErrorActionPreference='Stop'; $p=@(Get-CimInstance Win32_Process | Where-Object { $_.Name -match '^(codex|claude|hermes|dsh|node|python|pythonw)(\.exe)?$' -or [string]::Equals($_.Name,` + psLiteral(filepath.Base(p.Tool.Path)) + `,[StringComparison]::OrdinalIgnoreCase) -or ($_.ExecutablePath -and [string]::Equals($_.ExecutablePath,` + psLiteral(p.Tool.Path) + `,[StringComparison]::OrdinalIgnoreCase)) } | Select-Object ProcessId,Name,ExecutablePath,CommandLine); ConvertTo-Json -InputObject $p -Compress`
	cmd := exec.CommandContext(ctx, powershell(), "-NoProfile", "-NonInteractive", "-Command", probe)
	configureProcess(cmd, false)
	data, err := cmd.Output()
	if err != nil {
		return errors.New("无法检查旧 CLI 的运行归属，未替换 API 配置")
	}
	var entries []managedCLIProcess
	if json.Unmarshal(data, &entries) != nil {
		return errors.New("无法读取旧 CLI 的运行状态，未替换 API 配置")
	}
	for _, e := range entries {
		image := strings.ToLower(e.Name)
		family := config.ToolFamily(p.Tool.ID)
		if isDesktop(p.Tool.ID) && strings.EqualFold(e.ExecutablePath, p.Tool.Path) {
			continue
		}
		if (image == family+".exe" || family == "deepseek-harness" && image == "dsh.exe" || strings.EqualFold(e.Name, filepath.Base(p.Tool.Path))) && (e.ExecutablePath == "" || e.CommandLine == "") {
			return errors.New("同系列工具的运行信息不可读取，无法确认归属；未替换 API 配置")
		}
		if managedCLIProcessMatches(e, p) {
			private, err := managedCLIPrivateProcess(e.ProcessID, receipts, host, boot, owner)
			if err != nil {
				return err
			}
			if private {
				continue
			}
			return errors.New("检测到没有受管停止凭据的同系列 CLI，未替换 API；请先在该终端退出 Agent 后再试")
		}
	}
	return ctx.Err()
}

func managedCLIProcessMatches(e managedCLIProcess, p Plan) bool {
	image, command := strings.ToLower(e.Name), strings.ToLower(strings.ReplaceAll(e.CommandLine, "/", `\`))
	family := config.ToolFamily(p.Tool.ID)
	generic := image == "node.exe" || image == "python.exe" || image == "pythonw.exe"
	match := image == family+".exe" || !isDesktop(p.Tool.ID) && !generic && strings.EqualFold(e.ExecutablePath, p.Tool.Path)
	if generic && len(p.Args) > 0 && filepath.IsAbs(p.Args[0]) {
		entry := strings.ToLower(strings.ReplaceAll(filepath.Clean(p.Args[0]), "/", `\`))
		match = match || strings.Contains(command, entry)
	}
	if family == "deepseek-harness" {
		match = match || image == "dsh.exe" || strings.Contains(command, `\@deepseek-ai\dsh\`)
	}
	if family == "codex" {
		match = match || strings.Contains(command, `\@openai\codex\`)
	}
	if family == "claude" {
		match = match || strings.Contains(command, `\@anthropic-ai\claude-code\`)
	}
	if family == "hermes" {
		match = match || (image == "python.exe" || image == "pythonw.exe") && (strings.Contains(command, `\hermes_cli\`) || strings.Contains(command, "-m hermes_cli"))
	}
	return match
}

var isManagedProcessInJob = windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")

func managedCLIPrivateProcess(pid uint32, paths []string, host, boot, owner string) (bool, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return true, nil
	} // this observed process has exited
	if err != nil {
		return false, errors.New("无法核实独立工具的运行归属，未替换 API 配置")
	}
	defer windows.CloseHandle(process)
	for _, path := range paths {
		r, err := readManagedCLI(path)
		if err != nil {
			return false, err
		}
		if r.Shared || r.State != "running" || r.Host != host || r.Boot != boot || r.Owner != owner {
			continue
		}
		job, err := openManagedCLIJob(r.ID)
		if err != nil {
			continue
		} // no positive ownership proof; never exempt it
		var member int32
		ok, _, callErr := isManagedProcessInJob.Call(uintptr(process), uintptr(job), uintptr(unsafe.Pointer(&member)))
		windows.CloseHandle(job)
		if ok == 0 {
			return false, fmt.Errorf("无法核实工具进程树归属：%w", callErr)
		}
		if member != 0 {
			return true, nil
		}
	}
	return false, nil
}
