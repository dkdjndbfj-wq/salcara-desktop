package hubclient

import (
	"context"
	"errors"
	"time"

	"salcara/bridge/internal/desktopcompanion"
)

// PrepareUpdate never stops a task. It shares the task-admission lock with
// phone/local sends so a new task cannot slip between the idle check and quit.
// Production agents expose in-memory activity; this must not start a CLI or
// issue a paid model request just to check whether an update is safe.
func (c *Client) PrepareUpdate(ctx context.Context) error {
	if !c.taskConfigMu.TryLock() {
		return errors.New("正在处理请求，请稍后更新")
	}
	defer c.taskConfigMu.Unlock()
	c.expireUpdateLocked()
	if c.updatePrepared {
		return nil
	}
	m := c.manager()
	if m == nil {
		return errors.New("程序尚未准备好，请稍后更新")
	}
	for _, a := range m.Agents() {
		active, ok := a.(interface{ ActiveRemoteTurns() bool })
		if !ok || active.ActiveRemoteTurns() {
			return errors.New("任务尚未结束，请完成后再更新")
		}
	}
	if c.o.Desktop != nil {
		status, err := c.o.Desktop.NativeStatus(ctx)
		if err != nil && !errors.Is(err, desktopcompanion.ErrActivationRequired) {
			return errors.New("无法确认桌面任务状态，请稍后更新")
		}
		if err == nil && desktopcompanion.ValidateNativeConnection(status, nowMS()) {
			sessions, err := c.o.Desktop.NativeList(ctx)
			if err != nil {
				return errors.New("无法确认桌面任务状态，请稍后更新")
			}
			for _, session := range sessions {
				if session.Status == "running" || session.Status == "waiting_approval" {
					return errors.New("桌面任务尚未结束，请完成后再更新")
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.updatePrepared = true
	c.updateUntil = time.Now().Add(30 * time.Second)
	return nil
}

func (c *Client) expireUpdateLocked() {
	if c.updatePrepared && !c.updateCommitted && time.Now().After(c.updateUntil) {
		c.updatePrepared = false
	}
}

// CancelUpdate restores admission only before shutdown has committed.
func (c *Client) CancelUpdate() bool {
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	if c.updateCommitted {
		return false
	}
	c.updatePrepared = false
	return true
}

func (c *Client) CommitUpdate() error {
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	c.expireUpdateLocked()
	if !c.updatePrepared {
		return errors.New("更新准备已取消或过期，请重试")
	}
	c.updateCommitted = true
	return nil
}
