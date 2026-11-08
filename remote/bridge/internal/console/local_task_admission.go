package console

import (
	"context"
	"errors"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

// Native configuration writes and exact-session launches must share admission
// with phone sends. Holding this lease through launch prevents a phone turn
// from starting between the idle check and replacing its gateway credential.
func (s *Server) beginLocalToolChange(ctx context.Context, target string) (func(), error) {
	if s.d.Hub != nil {
		return s.d.Hub.BeginLocalAgentChange(ctx, target)
	}
	if m := s.manager(); m != nil {
		for _, a := range m.Agents() {
			if agents.Describe(a).APIFamily != config.ToolFamily(target) {
				continue
			}
			if active, ok := a.(interface{ ActiveRemoteTurns() bool }); ok {
				if active.ActiveRemoteTurns() {
					return nil, errors.New("任务尚未结束或停止结果未确认，请核对后再切换 API 或打开原工具")
				}
				continue
			}
			rows, err := a.Sessions(ctx)
			if err != nil {
				return nil, errors.New("无法确认任务状态，请刷新后重试")
			}
			for _, row := range rows {
				if !nativeSessionIdle(row.Status, row.ActiveTurnID) {
					return nil, errors.New("任务尚未结束或状态未确认，请核对后再操作")
				}
			}
		}
	}
	return func() {}, ctx.Err()
}

func nativeSessionIdle(status, activeTurn string) bool {
	if activeTurn != "" {
		return false
	}
	switch status {
	case "idle", "completed", "failed", "interrupted", "stopped":
		return true
	}
	return false
}
