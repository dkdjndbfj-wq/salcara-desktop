//go:build !windows

package hubclient

import "os/exec"

func hidePreviewProcess(cmd *exec.Cmd) {}
