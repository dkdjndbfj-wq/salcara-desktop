package console

import (
	"context"
	"sync"
	"time"

	"salcara/bridge/internal/agents"
)

var setupClaudeSDK = struct {
	sync.Mutex
	generation uint64
	pending    bool
	at         time.Time
	item       setupItem
}{}

func invalidateSetupClaudeSDK() {
	setupClaudeSDK.Lock()
	setupClaudeSDK.generation++
	setupClaudeSDK.pending, setupClaudeSDK.at = false, time.Time{}
	setupClaudeSDK.Unlock()
	agents.InvalidateClaudeSessionSDKCache()
}

// SDK discovery may import Node code. Keep it off the settings rendering path
// and discard an old discovery if an installation/refresh superseded it.
func setupClaudeSDKSnapshot() (setupItem, bool) {
	setupClaudeSDK.Lock()
	defer setupClaudeSDK.Unlock()
	if setupClaudeSDK.item.ID == "" {
		setupClaudeSDK.item = setupItem{ID: "claude-sdk", Name: "Claude 会话管理", Role: "原生改名与分支", Note: "检测中…"}
	}
	if !setupClaudeSDK.pending && time.Since(setupClaudeSDK.at) > 30*time.Second {
		setupClaudeSDK.pending = true
		generation := setupClaudeSDK.generation
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			status := agents.DetectClaudeSessionSDK(ctx)
			_, installErr := agents.ClaudeSessionSDKInstallCommand()
			item := setupItem{ID: "claude-sdk", Name: "Claude 会话管理", Role: "原生改名与分支", Installed: status.Available,
				Path: status.Path, Version: status.Version, Installable: installErr == nil}
			if !status.Available && installErr != nil {
				item.Note = "Node.js 未就绪"
				item.Download = "https://nodejs.org/"
			}
			setupClaudeSDK.Lock()
			if setupClaudeSDK.generation == generation {
				setupClaudeSDK.item, setupClaudeSDK.at, setupClaudeSDK.pending = item, time.Now(), false
			}
			setupClaudeSDK.Unlock()
		}()
	}
	return setupClaudeSDK.item, setupClaudeSDK.pending
}
