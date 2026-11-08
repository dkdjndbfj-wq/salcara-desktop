//go:build !windows && !linux && !darwin

package hubclient

import "errors"

func startTerminalProcess(string, int, int, []string) (terminalProcess, error) {
	return nil, errors.New("当前电脑系统不支持交互终端")
}
