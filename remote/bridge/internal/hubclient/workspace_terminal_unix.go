//go:build linux || darwin

package hubclient

import (
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

type unixTerminal struct {
	file      *os.File
	cmd       *exec.Cmd
	closeOnce sync.Once
}

func startTerminalProcess(cwd string, cols, rows int, env []string) (terminalProcess, error) {
	shell := "/bin/bash"
	if _, err := os.Stat(shell); err != nil {
		shell = "/bin/sh"
	}
	cmd := exec.Command(shell)
	cmd.Dir = cwd
	cmd.Env = env
	file, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	return &unixTerminal{file: file, cmd: cmd}, nil
}
func (t *unixTerminal) Read(b []byte) (int, error)  { return t.file.Read(b) }
func (t *unixTerminal) Write(b []byte) (int, error) { return t.file.Write(b) }
func (t *unixTerminal) Resize(cols, rows int) error {
	return pty.Setsize(t.file, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}
func (t *unixTerminal) Wait() (int, error) {
	err := t.cmd.Wait()
	return t.cmd.ProcessState.ExitCode(), err
}
func (t *unixTerminal) Close() error {
	t.closeOnce.Do(func() { _ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL); t.file.Close() })
	return nil
}
