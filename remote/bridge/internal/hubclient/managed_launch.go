package hubclient

import (
	"context"
	"errors"
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"sync"
)

// BeginLocalToolChange holds the same admission lock as remote sends without
// changing any remote API or pretending that a pooled client is one source.
func (c *Client) BeginLocalToolChange(ctx context.Context) (func(), error) {
	return c.beginLocalToolChange(ctx, "", false)
}

// BeginLocalAgentChange fences a native API change against the corresponding
// family's tasks on every surface, including Direct and follow-computer tasks.
func (c *Client) BeginLocalAgentChange(ctx context.Context, target string) (func(), error) {
	family := config.ToolFamily(target)
	if !config.ValidAgentFamily(family) {
		return nil, errors.New("无效的 Agent")
	}
	return c.beginLocalToolChange(ctx, family, false)
}

// BeginLocalAgentStopChange retains the send/approval admission lock until the
// caller finishes applying/opening. Adapter calls must be direct: Dispatch
// would recursively acquire the same mutex.
func (c *Client) BeginLocalAgentStopChange(ctx context.Context, target string) (func(), error) {
	family := config.ToolFamily(target)
	if !config.ValidAgentFamily(family) {
		return nil, errors.New("无效的 Agent")
	}
	return c.beginLocalToolChange(ctx, family, true)
}

func (c *Client) beginLocalToolChange(ctx context.Context, family string, stop bool) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.CheckUpdateAdmission(); err != nil {
		return nil, err
	}
	if !c.taskConfigMu.TryLock() {
		return nil, errors.New("正在处理远程请求，请稍后打开工具")
	}
	ok := false
	defer func() {
		if !ok {
			c.taskConfigMu.Unlock()
		}
	}()
	c.expireUpdateLocked()
	if err := c.CheckUpdateAdmission(); err != nil {
		return nil, err
	}
	if c.updatePrepared || c.updateCommitted {
		return nil, errors.New("正在准备更新，请稍后打开工具")
	}
	m := c.manager()
	if m == nil {
		return nil, errors.New("远程运行器尚未就绪")
	}
	for _, a := range m.Agents() {
		if family != "" && agents.Describe(a).APIFamily != family {
			continue
		}
		if stop {
			if stopper, ok := a.(agents.APIChangeStopper); ok {
				if err := stopper.StopForAPIChange(ctx); err != nil {
					return nil, err
				}
			}
		}
		active, known := a.(interface{ ActiveRemoteTurns() bool })
		if !known || active.ActiveRemoteTurns() {
			return nil, errors.New("远程任务尚未结束，请完成后再打开工具")
		}
	}
	if c.o.Desktop != nil && (family == "" || family == "codex") {
		// NativeList normalizes some unknown statuses for display. Require the
		// native owner's strict, complete idle check before replacing its API.
		if err := c.checkNativeStationIdle(ctx); err != nil {
			return nil, err
		}
	}
	if ctx.Err() != nil {
		return nil, errors.New("打开工具检查已取消")
	}
	ok = true
	var once sync.Once
	return func() { once.Do(c.taskConfigMu.Unlock) }, nil
}
