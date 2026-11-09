package agents

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"salcara/bridge/internal/protocol"
)

// Only a different kernel boot on the same identified host proves that every
// process in the old tree is gone. A cache status, PID lookup, session lock or a
// second Bridge launch is insufficient. Legacy/foreign/unknown proofs stay fenced.
func (a *deepSeekAgent) restoreDeepSeekRuntime(id string, s *deepSeekSession) {
	uncertain := s.Unconfirmed || s.RuntimePending || s.Info.Status == "running" || s.Info.Status == "waiting_approval" || s.Info.Status == "unknown" || s.Info.ActiveTurnID != ""
	s.Info.ActiveTurnID = ""
	s.Info.Status, s.Info.Controllable = "idle", !uncertain
	s.Unconfirmed = uncertain
	if !uncertain {
		return
	}
	s.Info.Status = "unknown"
	if !a.boot.provesRestart(s.RuntimeBoot) {
		return
	}
	previousEvents := append([]protocol.Event(nil), s.Events...)
	previousPending := s.RuntimePending
	s.Unconfirmed, s.RuntimePending = false, false
	s.Info.Status, s.Info.Controllable = "failed", true
	s.Events = append(s.Events, protocol.Event{
		Tool: deepSeekID, SessionKey: s.Info.SessionKey, Type: "notice", ID: "deepseek-runtime-recovered", Level: "warning",
		Text: "已确认电脑重新启动，旧 DeepSeek 进程已停止。上一任务的结果仍未确认，历史已保留；请先核对结果再发送新消息，系统不会自动重发。",
	})
	if err := a.persistDeepSeekFenceLocked(id, s); err != nil {
		// An in-memory release must never outrun the durable recovery decision.
		s.Unconfirmed, s.RuntimePending = true, previousPending
		s.Info.Status, s.Info.Controllable = "unknown", false
		s.Events = previousEvents
	}
}

// Call only with positive root AND tree exit evidence. Refuse to release the
// marker of a replacement connection, and keep the fence on storage failure.
func (a *deepSeekAgent) confirmDeepSeekRuntimeStopped(s *deepSeekSession, conn *rpcConn) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.conn != nil && s.conn != conn {
		return nil
	}
	s.RuntimePending, s.Unconfirmed = false, false
	s.Info.Controllable = true
	id := strings.TrimPrefix(s.Info.SessionKey, deepSeekID+":")
	if err := a.persistDeepSeekFenceLocked(id, s); err != nil {
		s.RuntimePending, s.Unconfirmed = true, true
		s.Info.Status, s.Info.Controllable = "unknown", false
		return errors.New("DeepSeek 进程已停止，但恢复状态未能保存；仍保留隔离，请检查会话存储")
	}
	// The fast path is proof of a durable stop, not just process exit. A failed
	// checkpoint must never make a repeated stop report success.
	s.stoppedConn = conn
	return nil
}

// Sync admission/recovery checkpoints before releasing their gates. Streaming
// event cache writes retain their existing cadence; only these transitions fsync.
func (a *deepSeekAgent) persistDeepSeekFenceLocked(id string, s *deepSeekSession) error {
	if err := a.persistLocked(id, s); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(a.stateDir, id, "session.json"), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	return errors.Join(syncErr, closeErr)
}
