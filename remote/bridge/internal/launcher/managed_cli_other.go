//go:build !windows

package launcher

import (
	"context"
	"errors"
)

func stopManagedCLI(ctx context.Context, p Plan) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("此系统尚不能确认原生 Agent 进程树已全部停止；未替换 API 配置，请先使用受支持的电脑端")
}
