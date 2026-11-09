package agents

import (
	"context"
	"errors"
	"sync"
	"time"
)

// APIChangeStopper stops only this adapter's owned work. A successful return
// means completion was observed, not merely that an interrupt was requested.
// The caller holds task/configuration admission throughout stop and apply.
type APIChangeStopper interface{ StopForAPIChange(context.Context) error }

// A retired numeric PID is no longer an ownership capability. Keep only a
// safe-change fence when an old lifecycle discards its naturally reaped root
// without explicit subtree proof; normal send/reconnect behavior is unchanged.
// This fence cannot be cleared by a timer, registry emptying or another PID.
func (a *codexAgent) rememberRetiredCodexTreeLocked(c *rpcConn) {
	if c != nil && c.cmd != nil && c.cmd.Process != nil && c.rootProcessExitConfirmed() && !c.treeTermination.unconfirmed() && !c.treeTermination.confirmed() {
		a.apiChangeRetiredUnknown = true
	}
}

func (a *claudeAgent) rememberRetiredClaudeTreeLocked(p *claudeProc) {
	if p != nil && p.cmd != nil && p.cmd.Process != nil && p.rootProcessExitConfirmed() && !p.treeTermination.unconfirmed() && !p.treeTermination.confirmed() {
		a.apiChangeRetiredUnknown = true
	}
}

func configStopLock(ctx context.Context, mu *sync.Mutex) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mu.TryLock() {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func configStopWait(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return ctx.Err()
	}
}

func (a *codexAgent) StopForAPIChange(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	retiredUnknown := a.apiChangeRetiredUnknown
	a.mu.Unlock()
	if retiredUnknown {
		return errors.New("Codex 旧主进程已退出，但其子进程退出未确认；配置未更改，请在电脑核对")
	}
	// These independent requests have no cancellation receipt. Do not retire
	// their route while work may still be using it.
	if a.sideQuestions.activeTurns() || a.sideTools.activeTurns() {
		return errors.New("Codex 独立侧聊仍在运行，暂不能确认全部任务停止；配置未更改")
	}
	a.mu.Lock()
	if a.starting != nil {
		a.starting.cancel()
	}
	a.mu.Unlock()
	if err := configStopLock(ctx, &a.startMu); err != nil {
		return err
	}
	defer a.startMu.Unlock()
	a.mu.Lock()
	if a.apiChangeRetiredUnknown {
		a.mu.Unlock()
		return errors.New("Codex 旧进程树退出未确认；配置未更改")
	}
	c := a.conn
	if a.unconfirmedProcess != nil {
		c = a.unconfirmedProcess
	}
	if c != nil {
		a.unconfirmedProcess = c
	}
	busyWithoutOwner := c == nil && a.hasActiveTurnsLocked()
	a.mu.Unlock()
	if busyWithoutOwner {
		return errors.New("Codex 任务所有权未确认；配置未更改")
	}
	if c != nil {
		stopped := make(chan struct{})
		go func() { c.Kill(); close(stopped) }()
		if err := configStopWait(ctx, stopped); err != nil {
			return err
		}
		if err := configStopWait(ctx, c.done); err != nil {
			return err
		}
		if !c.processExitConfirmed() || !c.treeTermination.confirmed() {
			// Root disappearance alone cannot retire the original tree fence.
			if !c.treeTermination.confirmed() {
				c.treeTermination.begin()
			}
			return errors.New("Codex 进程树退出未确认；配置未更改")
		}
		a.mu.Lock()
		if a.conn == c {
			a.conn = nil
			a.markUnloadedLocked()
		}
		if a.unconfirmedProcess == c {
			a.unconfirmedProcess = nil
		}
		a.mu.Unlock()
	}
	if a.aps != nil {
		a.aps.cancelAll()
	}
	if a.ActiveRemoteTurns() {
		return errors.New("Codex 仍有未确认任务；配置未更改")
	}
	a.mu.Lock()
	retiredUnknown = a.apiChangeRetiredUnknown
	a.mu.Unlock()
	if retiredUnknown {
		return errors.New("Codex 旧进程树退出未确认；配置未更改")
	}
	return ctx.Err()
}

func (a *claudeAgent) StopForAPIChange(ctx context.Context) error {
	a.mu.Lock()
	retiredUnknown := a.apiChangeRetiredUnknown
	a.mu.Unlock()
	if retiredUnknown {
		return errors.New("Claude Code 旧主进程已退出，但其子进程退出未确认；配置未更改，请在电脑核对")
	}
	if a.sideQuestions.activeTurns() {
		return errors.New("Claude 独立侧聊仍在运行；配置未更改")
	}
	if err := configStopLock(ctx, &a.startMu); err != nil {
		return err
	}
	defer a.startMu.Unlock()
	a.mu.Lock()
	if a.apiChangeRetiredUnknown {
		a.mu.Unlock()
		return errors.New("Claude Code 旧进程树退出未确认；配置未更改")
	}
	workers := map[*claudeProc]bool{}
	for p := range a.workers {
		workers[p] = true
	}
	for _, p := range a.procs {
		workers[p] = true
	}
	a.mu.Unlock()
	for p := range workers {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Publish the startup fence before scheduling a potentially slow tree
		// terminator. A cancelled HTTP request must not release admission before
		// the stop goroutine has made the old worker unavailable to new sends.
		a.mu.Lock()
		if !p.treeTermination.confirmed() && !p.rootProcessExitConfirmed() {
			p.treeTermination.begin()
		}
		if a.workers == nil {
			a.workers = map[*claudeProc]struct{}{}
		}
		a.workers[p] = struct{}{}
		a.mu.Unlock()
		stopped := make(chan struct{})
		go func() { a.stop(p); close(stopped) }()
		if err := configStopWait(ctx, stopped); err != nil {
			return err
		}
		if err := configStopWait(ctx, p.done); err != nil {
			return err
		}
		if !p.processExitConfirmed() || p.cmd != nil && p.cmd.Process != nil && !p.treeTermination.confirmed() {
			if !p.treeTermination.confirmed() {
				p.treeTermination.begin()
			}
			a.mu.Lock()
			a.workers[p] = struct{}{}
			a.mu.Unlock()
			return errors.New("Claude Code 进程树退出未确认；配置未更改")
		}
	}
	if a.aps != nil {
		a.aps.cancelAll()
	}
	if a.ActiveRemoteTurns() {
		return errors.New("Claude 仍有未确认任务；配置未更改")
	}
	a.mu.Lock()
	retiredUnknown = a.apiChangeRetiredUnknown
	a.mu.Unlock()
	if retiredUnknown {
		return errors.New("Claude Code 旧进程树退出未确认；配置未更改")
	}
	return ctx.Err()
}

func (a *hermesAgent) StopForAPIChange(ctx context.Context) error {
	a.mu.Lock()
	ids := make([]string, 0, len(a.sessions))
	for id := range a.sessions {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		requested := false
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			a.mu.Lock()
			s := a.sessions[id]
			a.mu.Unlock()
			if err := configStopLock(ctx, &s.control); err != nil {
				return err
			}
			conn, err := a.runtime(ctx)
			if err == nil {
				err = a.resume(ctx, id, s, conn, "")
			}
			s.control.Unlock()
			if err != nil {
				return err
			}
			a.mu.Lock()
			busy, unknown := s.busy, s.uncertain || s.recovering
			pending := len(s.prompts) != 0
			turn := hermesTurnID(s.Epoch, s.Generation)
			idle := !busy && !unknown && !pending && configStopIdle(s.Info.Status, s.Info.ActiveTurnID)
			a.mu.Unlock()
			if unknown {
				return errors.New("Hermes 原生任务状态未确认；配置未更改")
			}
			if idle {
				break
			}
			if !busy {
				return errors.New("Hermes 仍有待处理审批或未知状态；配置未更改")
			}
			if !requested {
				if _, err := a.InterruptTurn(ctx, id, turn, newUUID()); err != nil {
					return err
				}
				requested = true
			}
			timer := time.NewTimer(100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	if a.ActiveRemoteTurns() {
		return errors.New("Hermes 停止尚未确认；配置未更改")
	}
	// Detach only this owned viewer after native task completion. The gateway
	// authority can be shared with other clients and is never tree-killed.
	// The native configuration guard must still refuse a surviving unowned
	// CLI/gateway; viewer exit is not proof that shared authority stopped.
	if err := configStopLock(ctx, &a.connectMu); err != nil {
		return err
	}
	defer a.connectMu.Unlock()
	a.mu.Lock()
	conn := a.conn
	a.mu.Unlock()
	if pipe, ok := conn.(*hermesPipe); ok {
		if pipe.stdin != nil {
			_ = pipe.stdin.Close()
		}
		if err := configStopWait(ctx, pipe.processWaitDone); err != nil {
			return err
		}
		if !pipe.rootProcessExitConfirmed() {
			return errors.New("Hermes 控制连接退出未确认；配置未更改")
		}
		pipe.closeOwnedPipes()
		if err := configStopWait(ctx, pipe.done); err != nil {
			return err
		}
	} else if conn != nil {
		conn.detach()
	}
	a.mu.Lock()
	if a.conn == conn {
		a.conn = nil
	}
	a.mu.Unlock()
	return ctx.Err()
}

func configStopIdle(status, turn string) bool {
	if turn != "" {
		return false
	}
	switch status {
	case "idle", "completed", "failed", "interrupted", "stopped":
		return true
	}
	return false
}
