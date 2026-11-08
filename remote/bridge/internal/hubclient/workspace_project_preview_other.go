//go:build !windows && !linux && !darwin

package hubclient

import "errors"

func startProjectPreviewProcess(string, string, []string, []string) (projectPreviewProcess, error) {
	return nil, errors.New("当前系统不支持项目运行预览")
}
