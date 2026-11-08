package launcher

import (
	"context"
	"strings"
	"sync"
	"time"
)

// LaunchProgress contains presentation data only, never a Plan or credentials.
// A reporter belongs to one request; it cannot advance another launch.
type LaunchProgress struct {
	Type   string       `json:"type"`
	Step   string       `json:"step,omitempty"`
	Label  string       `json:"label,omitempty"`
	Detail string       `json:"detail,omitempty"`
	State  string       `json:"state,omitempty"`
	Steps  []LaunchStep `json:"steps,omitempty"`
}

type LaunchStep struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

type launchProgressKey struct{}

type safeLaunchError struct {
	cause   error
	message string
}

func (e *safeLaunchError) Error() string { return e.message }
func (e *safeLaunchError) Unwrap() error { return e.cause }

// Preserve error identity for authorization fences while excluding credentials
// from a system error that may echo command arguments or a provider response.
func RedactLaunchError(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[已隐藏]")
		}
	}
	return &safeLaunchError{cause: err, message: message}
}

func WithLaunchProgress(ctx context.Context, report func(LaunchProgress)) context.Context {
	return context.WithValue(ctx, launchProgressKey{}, report)
}

func reportLaunchProgress(ctx context.Context, event LaunchProgress) {
	if report, ok := ctx.Value(launchProgressKey{}).(func(LaunchProgress)); ok {
		report(event)
	}
}

// Advance only after the previous operation returned successfully. Repeating an
// ID updates its substep without claiming that operation has finished.
func LaunchPhase(ctx context.Context, id, label string, detail ...string) {
	event := LaunchProgress{Type: "phase", Step: id, Label: label}
	if len(detail) > 0 {
		event.Detail = detail[0]
	}
	reportLaunchProgress(ctx, event)
}

func LaunchWarning(ctx context.Context, id, detail string) {
	reportLaunchProgress(ctx, LaunchProgress{Type: "phase", Step: id, State: "warning", Detail: detail})
}

func LaunchResumePlan(ctx context.Context) {
	reportLaunchProgress(ctx, LaunchProgress{Type: "plan", Steps: []LaunchStep{{"source", "核对账号与启动配置"}, {"session", "读取并核对原会话"}, {"lock", "等待工具操作完成"}, {"tool", "检查程序与项目目录"}, {"start", "创建工具进程"}, {"confirm", "等待进程启动确认"}}})
}

func LaunchApplyPlan(ctx context.Context) {
	reportLaunchProgress(ctx, LaunchProgress{Type: "plan", Steps: []LaunchStep{{"source", "核对账号与配置"}, {"catalog", "读取可用模型"}, {"selection", "读取当前工具配置"}, {"lock", "等待工具操作完成"}, {"tool", "检查程序与项目目录"}, {"backup", "备份原工具配置"}, {"profile", "写入 API 与模型配置"}, {"authorize", "核对并保存连接授权"}, {"record", "保存配置状态"}}})
}

func LaunchPlan(ctx context.Context, target string, managed, isolated bool) {
	steps := []LaunchStep{{"source", "核对账号与启动配置"}}
	if !managed {
		steps = append(steps, LaunchStep{"catalog", "读取可用模型"}, LaunchStep{"selection", "读取当前工具配置"})
	} else {
		steps = append(steps, LaunchStep{"remote", "检查远程任务"}, LaunchStep{"service", "检查本机 API 服务"})
	}
	steps = append(steps, LaunchStep{"lock", "等待工具操作完成"}, LaunchStep{"tool", "检查程序与项目目录"})
	if !isolated {
		steps = append(steps, LaunchStep{"backup", "备份原工具配置"})
		if isDesktop(target) {
			steps = append(steps, LaunchStep{"stop", "等待原工具退出"})
		}
	}
	steps = append(steps, LaunchStep{"profile", "写入 API 与模型配置"}, LaunchStep{"authorize", "核对并保存连接授权"}, LaunchStep{"start", "创建工具进程"}, LaunchStep{"confirm", "等待进程启动确认"}, LaunchStep{"record", "保存本次启动状态"})
	reportLaunchProgress(ctx, LaunchProgress{Type: "plan", Steps: steps})
}

// A queued request must not outlive its deadline and apply a stale configuration
// when another launch finally releases the lock.
func lockLaunch(ctx context.Context, mu *sync.Mutex) error {
	LaunchPhase(ctx, "lock", "等待工具操作完成")
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mu.TryLock() {
			if err := ctx.Err(); err != nil {
				mu.Unlock()
				return err
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
