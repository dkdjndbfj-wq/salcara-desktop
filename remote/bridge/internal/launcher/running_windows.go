//go:build windows

package launcher

import (
	"context"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var runningKernel = windows.NewLazySystemDLL("kernel32.dll")
var packageFamilyProc = runningKernel.NewProc("GetPackageFamilyName")
var appUserModelProc = runningKernel.NewProc("GetApplicationUserModelId")

// Read only the current user's executable locations and package identities.
// Never read command lines (which can contain keys), inject into a tool, or
// launch/stop a process during discovery.
func runningDesktopTools(ctx context.Context) map[string]Tool {
	out := map[string]Tool{}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return out
	}
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return out
	}
	defer windows.CloseHandle(snapshot)
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil && ctx.Err() == nil; err = windows.Process32Next(snapshot, &entry) {
		name := strings.ToLower(windows.UTF16ToString(entry.ExeFile[:]))
		if name != "codex.exe" && name != "chatgpt.exe" && name != "claude.exe" {
			continue
		}
		process, e := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, entry.ProcessID)
		if e != nil {
			continue
		}
		tool, kind := toolFromRunningProcess(process, user.User.Sid, name)
		windows.CloseHandle(process)
		if kind != "" && out[kind].Path == "" {
			out[kind] = tool
		}
	}
	return out
}

func processPackageString(process windows.Handle, proc *windows.LazyProc) string {
	if proc.Find() != nil {
		return ""
	}
	buf := make([]uint16, 512)
	length := uint32(len(buf))
	code, _, _ := proc.Call(uintptr(process), uintptr(unsafe.Pointer(&length)), uintptr(unsafe.Pointer(&buf[0])))
	if code != 0 || length > uint32(len(buf)) {
		return ""
	}
	return windows.UTF16ToString(buf)
}

func toolFromRunningProcess(process windows.Handle, sid *windows.SID, name string) (Tool, string) {
	if sid == nil {
		return Tool{}, ""
	}
	var token windows.Token
	if windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token) != nil {
		return Tool{}, ""
	}
	owner, err := token.GetTokenUser()
	token.Close()
	if err != nil || !sid.Equals(owner.User.Sid) {
		return Tool{}, ""
	}
	buf := make([]uint16, 32768)
	length := uint32(len(buf))
	if windows.QueryFullProcessImageName(process, 0, &buf[0], &length) != nil || length == 0 || length > uint32(len(buf)) {
		return Tool{}, ""
	}
	exe := windows.UTF16ToString(buf[:length])
	tool, kind := toolFromRunningPath(exe, name)
	if kind == "" {
		return Tool{}, ""
	}
	// Both appmodel APIs accept a process handle, not a token handle. The
	// token above is used only to restrict discovery to the current user.
	tool.Family = processPackageString(process, packageFamilyProc)
	if modelID := processPackageString(process, appUserModelProc); tool.Family != "" && strings.HasPrefix(modelID, tool.Family+"!") {
		tool.AppID = strings.TrimPrefix(modelID, tool.Family+"!")
	}
	if tool.Family != "" && tool.AppID == "" {
		return Tool{}, ""
	}
	return tool, kind
}

func toolFromRunningPath(exe, name string) (Tool, string) {
	name = strings.ToLower(name)
	if name != "codex.exe" && name != "chatgpt.exe" && name != "claude.exe" {
		return Tool{}, ""
	}
	if !filepath.IsAbs(exe) || !fileExists(exe) {
		return Tool{}, ""
	}
	// The CLI and Electron app can have the same executable name. Only an
	// adjacent Electron bundle is a desktop installation.
	if !fileExists(filepath.Join(filepath.Dir(exe), "resources", "app.asar")) {
		return Tool{}, ""
	}
	kind := "codex"
	if name == "claude.exe" {
		kind = "claude"
	}
	if name == "chatgpt.exe" {
		p := strings.ToLower(exe)
		if !strings.Contains(p, "openai.codex") && !strings.Contains(p, `\codex\`) {
			return Tool{}, ""
		}
	}
	return Tool{Path: exe}, kind
}
