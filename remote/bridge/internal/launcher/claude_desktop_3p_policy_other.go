//go:build !windows && !darwin

package launcher

import (
	"context"
	"errors"
)

func claudeManagedPolicy(context.Context) (bool, error) {
	return true, errors.New("当前平台尚未验证 Claude Desktop 管理策略，请使用原应用配置")
}
