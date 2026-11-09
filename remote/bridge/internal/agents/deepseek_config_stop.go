package agents

import (
	"context"
	"errors"

	"salcara/bridge/internal/protocol"
)

func (a *deepSeekAgent) StopForAPIChange(ctx context.Context) error {
	a.mu.Lock()
	ids := make([]string, 0, len(a.sessions))
	for id, s := range a.sessions {
		if (s.Unconfirmed || s.RuntimePending) && s.conn == nil {
			a.mu.Unlock()
			return errors.New("DeepSeek Harness 旧运行器退出未确认；配置未更改")
		}
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.mu.Lock()
		s := a.sessions[id]
		a.mu.Unlock()
		if err := configStopLock(ctx, &s.control); err != nil {
			return err
		}
		a.mu.Lock()
		conn, busy := s.conn, s.busy
		a.mu.Unlock()
		if conn != nil {
			if err := a.stopRuntime(s, conn); err != nil {
				s.control.Unlock()
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			s.control.Unlock()
			return err
		}
		a.mu.Lock()
		unknown := s.Unconfirmed || s.RuntimePending || s.conn != nil && s.conn != conn
		a.mu.Unlock()
		if unknown {
			s.control.Unlock()
			return errors.New("DeepSeek Harness 停止尚未确认；配置未更改")
		}
		a.mu.Lock()
		if s.conn == conn {
			s.conn = nil
		}
		a.mu.Unlock()
		a.finish(id, s, nil, "idle", "")
		s.control.Unlock()
		if busy {
			a.emit(id, protocol.Event{Type: "turn", Status: "interrupted"})
		}
	}
	if a.ActiveRemoteTurns() {
		return errors.New("DeepSeek Harness 仍有未确认任务；配置未更改")
	}
	return ctx.Err()
}
