package console

import (
	"errors"
	"net/http"
	"os"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

func setupInstallAllowed(tool string) bool {
	switch tool {
	case "claude", "codex", "git", "claude-sdk", "deepseek-harness", "hermes":
		return true
	default:
		return false
	}
}

// Both local UI and paired direct management use the same fixed installers,
// single-job exclusion and inventory invalidation. The check is repeated after
// taking the lock, immediately before starting a process.
func (s *Server) startSetupInstall(tool, owner, requestID string, check func() error) (setupJob, int, error) {
	if !setupInstallAllowed(tool) {
		return setupJob{}, http.StatusBadRequest, errors.New("不支持的工具")
	}
	if owner != "" && (!config.ValidAccountID(requestID) || len(requestID) < 16) {
		return setupJob{}, http.StatusBadRequest, errors.New("安装请求标识无效")
	}
	cmd, err := installCommand(tool)
	if err != nil {
		return setupJob{}, http.StatusBadRequest, err
	}
	setupState.mu.Lock()
	if check != nil {
		if err := check(); err != nil {
			setupState.mu.Unlock()
			return setupJob{}, http.StatusForbidden, err
		}
	}
	receiptKey := owner + "\x00" + requestID
	old := setupState.receipts[receiptKey]
	if old == nil && setupState.job != nil && setupState.job.Owner == owner && setupState.job.RequestID == requestID {
		old = setupState.job
	}
	if owner != "" && old != nil {
		copy := *old
		setupState.mu.Unlock()
		if copy.Tool != tool {
			return setupJob{}, http.StatusConflict, errors.New("安装请求已用于另一工具")
		}
		return copy, http.StatusOK, nil
	}
	if setupState.job != nil && setupState.job.State == "running" {
		setupState.mu.Unlock()
		return setupJob{}, http.StatusConflict, errors.New("正在安装其他工具，请等它完成")
	}
	if owner != "" && len(setupState.receipts) >= 256 {
		setupState.mu.Unlock()
		return setupJob{}, http.StatusTooManyRequests, errors.New("本次运行的安装记录已满，请在电脑端重启 Bridge 后重试")
	}
	job := &setupJob{ID: config.RandomToken(6), Tool: tool, State: "running", Started: time.Now().UnixMilli(), Owner: owner, RequestID: requestID}
	setupState.job = job
	if owner != "" {
		if setupState.receipts == nil {
			setupState.receipts = map[string]*setupJob{}
		}
		setupState.receipts[receiptKey] = job
	}
	setupState.buf.Reset()
	agents.PrepareChild(cmd)
	cmd.Stdout, cmd.Stderr = jobWriter{setupState}, jobWriter{setupState}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, "CI=1", "NO_COLOR=1")
	if err := cmd.Start(); err != nil {
		job.State, job.Error, job.Finished = "failed", "无法启动安装程序："+err.Error(), time.Now().UnixMilli()
		copy := *job
		setupState.mu.Unlock()
		return copy, http.StatusInternalServerError, errors.New(copy.Error)
	}
	started := *job
	setupState.mu.Unlock()
	s.log.Printf("setup: installing %s", tool)
	go func() {
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		var err error
		select {
		case err = <-done:
		case <-time.After(15 * time.Minute):
			_ = cmd.Process.Kill()
			err = errors.New("安装超时")
		}
		setupState.mu.Lock()
		job.Finished = time.Now().UnixMilli()
		if err != nil {
			job.State, job.Error = "failed", err.Error()
		} else {
			job.State = "done"
		}
		state := job.State
		setupState.mu.Unlock()
		if tool == "claude-sdk" {
			invalidateSetupClaudeSDK()
		}
		if s.d.Local != nil {
			s.d.Local.InvalidateInventory()
		}
		s.log.Printf("setup: %s finished state=%s", tool, state)
	}()
	return started, http.StatusOK, nil
}
