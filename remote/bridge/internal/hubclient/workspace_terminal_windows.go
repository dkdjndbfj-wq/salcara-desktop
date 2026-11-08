//go:build windows

package hubclient

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsTerminal struct {
	input, output         *os.File
	console, process, job windows.Handle
	closeOnce             sync.Once
	consoleMu             sync.Mutex
	closed                bool
}

func startTerminalProcess(cwd string, cols, rows int, env []string) (terminalProcess, error) {
	inRead, inWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		inRead.Close()
		inWrite.Close()
		return nil, err
	}
	var console windows.Handle
	if err = windows.CreatePseudoConsole(windows.Coord{X: int16(cols), Y: int16(rows)}, windows.Handle(inRead.Fd()), windows.Handle(outWrite.Fd()), 0, &console); err != nil {
		inRead.Close()
		inWrite.Close()
		outRead.Close()
		outWrite.Close()
		return nil, err
	}
	defer inRead.Close()
	defer outWrite.Close()
	t := &windowsTerminal{input: inWrite, output: outRead, console: console}
	success := false
	defer func() {
		if !success {
			t.Close()
		}
	}()
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	// PSEUDOCONSOLE takes the HPCON value, unlike attributes whose lpValue
	// points to memory. Pass the handle as a syscall word instead of inventing
	// a Go pointer from it (which also fails checkptr/go vet).
	updateAttribute := windows.NewLazySystemDLL("kernel32.dll").NewProc("UpdateProcThreadAttribute")
	if result, _, callErr := updateAttribute.Call(uintptr(unsafe.Pointer(attrs.List())), 0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, uintptr(console), unsafe.Sizeof(console), 0, 0); result == 0 {
		return nil, callErr
	}
	system, err := windows.GetSystemDirectory()
	if err != nil {
		return nil, err
	}
	shell := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
	args := []string{shell, "-NoLogo", "-NoProfile", "-NoExit"}
	if _, err = os.Stat(shell); err != nil {
		shell = filepath.Join(system, "cmd.exe")
		args = []string{shell, "/D"}
	}
	executable, err := windows.UTF16PtrFromString(shell)
	if err != nil {
		return nil, err
	}
	line, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(args))
	if err != nil {
		return nil, err
	}
	directory, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		return nil, err
	}
	sort.Slice(env, func(i, j int) bool { return strings.ToUpper(env[i]) < strings.ToUpper(env[j]) })
	block := utf16.Encode([]rune(strings.Join(env, "\x00") + "\x00\x00"))
	// Explicitly suppress the parent's standard handles. Without this flag a
	// console-hosted Bridge can leak prompts into its own console instead of
	// the PTY even though the pseudoconsole attribute is present.
	si := windows.StartupInfoEx{StartupInfo: windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESHOWWINDOW | windows.STARTF_USESTDHANDLES, ShowWindow: windows.SW_HIDE}, ProcThreadAttributeList: attrs.List()}
	pi := windows.ProcessInformation{}
	err = windows.CreateProcess(executable, line, nil, nil, false, windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT|windows.CREATE_SUSPENDED, &block[0], directory, &si.StartupInfo, &pi)
	if err != nil {
		return nil, err
	}
	t.process = pi.Process
	defer windows.CloseHandle(pi.Thread)
	t.job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(t.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		return nil, err
	}
	if err = windows.AssignProcessToJobObject(t.job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Process)
		return nil, err
	}
	if _, err = windows.ResumeThread(pi.Thread); err != nil {
		windows.TerminateJobObject(t.job, 1)
		windows.CloseHandle(pi.Process)
		return nil, err
	}
	success = true
	return t, nil
}
func (t *windowsTerminal) Read(b []byte) (int, error)  { return t.output.Read(b) }
func (t *windowsTerminal) Write(b []byte) (int, error) { return t.input.Write(b) }
func (t *windowsTerminal) Resize(cols, rows int) error {
	t.consoleMu.Lock()
	defer t.consoleMu.Unlock()
	if t.closed {
		return os.ErrClosed
	}
	return windows.ResizePseudoConsole(t.console, windows.Coord{X: int16(cols), Y: int16(rows)})
}
func (t *windowsTerminal) Wait() (int, error) {
	_, err := windows.WaitForSingleObject(t.process, windows.INFINITE)
	var code uint32
	if err == nil {
		err = windows.GetExitCodeProcess(t.process, &code)
	}
	windows.CloseHandle(t.process)
	return int(code), err
}
func (t *windowsTerminal) Close() error {
	t.closeOnce.Do(func() {
		t.consoleMu.Lock()
		t.closed = true
		t.consoleMu.Unlock()
		// Closing the job ends every child, including a long-running foreground
		// command. ConPTY output keeps draining until the console is closed.
		if t.job != 0 {
			windows.TerminateJobObject(t.job, 1)
			windows.CloseHandle(t.job)
		}
		t.input.Close()
		// ConPTY teardown may wait for buffered output; do not block command
		// dispatch while the reader drains, including startup failure cleanup.
		go func() { windows.ClosePseudoConsole(t.console); t.output.Close() }()
	})
	return nil
}
