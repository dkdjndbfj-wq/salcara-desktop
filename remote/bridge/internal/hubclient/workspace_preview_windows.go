//go:build windows

package hubclient

import (
	"os/exec"
	"syscall"
)

func hidePreviewProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
