//go:build linux || darwin

package hubclient

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
)

type unixProjectPreviewProcess struct {
	output *os.File
	cmd    *exec.Cmd
	once   sync.Once
}

func startProjectPreviewProcess(cwd, executable string, args, env []string) (projectPreviewProcess, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer write.Close()
	cmd := exec.Command(executable, args...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdout = write
	cmd.Stderr = write
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		read.Close()
		return nil, err
	}
	return &unixProjectPreviewProcess{output: read, cmd: cmd}, nil
}
func (p *unixProjectPreviewProcess) Read(data []byte) (int, error) {
	n, err := p.output.Read(data)
	if err != nil {
		p.output.Close()
	}
	return n, err
}
func (p *unixProjectPreviewProcess) Wait() (int, error) {
	err := p.cmd.Wait()
	return p.cmd.ProcessState.ExitCode(), err
}
func (p *unixProjectPreviewProcess) Close() error {
	p.once.Do(func() { _ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL) })
	return nil
}
