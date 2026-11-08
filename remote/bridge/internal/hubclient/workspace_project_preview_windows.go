//go:build windows

package hubclient

import (
	"os"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

type windowsProjectPreviewProcess struct {
	output       *os.File
	process, job windows.Handle
	once         sync.Once
}

func startProjectPreviewProcess(cwd, executable string, args, env []string) (projectPreviewProcess, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer write.Close()
	input, err := os.Open(os.DevNull)
	if err != nil {
		read.Close()
		return nil, err
	}
	defer input.Close()
	result := &windowsProjectPreviewProcess{output: read}
	success := false
	defer func() {
		if !success {
			result.Close()
			read.Close()
			if result.process != 0 {
				windows.TerminateProcess(result.process, 1)
				windows.CloseHandle(result.process)
			}
		}
	}()
	handles := []windows.Handle{windows.Handle(input.Fd()), windows.Handle(write.Fd())}
	for _, handle := range handles {
		if err := windows.SetHandleInformation(handle, windows.HANDLE_FLAG_INHERIT, windows.HANDLE_FLAG_INHERIT); err != nil {
			return nil, err
		}
	}
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	if err = attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		return nil, err
	}
	program, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return nil, err
	}
	line, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{executable}, args...)))
	if err != nil {
		return nil, err
	}
	directory, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		return nil, err
	}
	sort.Slice(env, func(i, j int) bool { return strings.ToUpper(env[i]) < strings.ToUpper(env[j]) })
	block := utf16.Encode([]rune(strings.Join(env, "\x00") + "\x00\x00"))
	si := windows.StartupInfoEx{StartupInfo: windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfoEx{})), Flags: windows.STARTF_USESHOWWINDOW | windows.STARTF_USESTDHANDLES, ShowWindow: windows.SW_HIDE, StdInput: handles[0], StdOutput: handles[1], StdErr: handles[1]}, ProcThreadAttributeList: attrs.List()}
	pi := windows.ProcessInformation{}
	if err = windows.CreateProcess(program, line, nil, nil, true, windows.CREATE_NO_WINDOW|windows.CREATE_SUSPENDED|windows.CREATE_UNICODE_ENVIRONMENT|windows.EXTENDED_STARTUPINFO_PRESENT, &block[0], directory, &si.StartupInfo, &pi); err != nil {
		return nil, err
	}
	result.process = pi.Process
	defer windows.CloseHandle(pi.Thread)
	result.job, err = windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(result.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return nil, err
	}
	if err = windows.AssignProcessToJobObject(result.job, pi.Process); err != nil {
		return nil, err
	}
	if _, err = windows.ResumeThread(pi.Thread); err != nil {
		return nil, err
	}
	success = true
	return result, nil
}
func (p *windowsProjectPreviewProcess) Read(data []byte) (int, error) {
	n, err := p.output.Read(data)
	if err != nil {
		p.output.Close()
	}
	return n, err
}
func (p *windowsProjectPreviewProcess) Wait() (int, error) {
	_, err := windows.WaitForSingleObject(p.process, windows.INFINITE)
	var code uint32
	if err == nil {
		err = windows.GetExitCodeProcess(p.process, &code)
	}
	windows.CloseHandle(p.process)
	return int(code), err
}
func (p *windowsProjectPreviewProcess) Close() error {
	p.once.Do(func() {
		if p.job != 0 {
			windows.TerminateJobObject(p.job, 1)
			windows.CloseHandle(p.job)
		}
	})
	return nil
}
