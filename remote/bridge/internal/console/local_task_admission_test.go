package console

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/protocol"
)

func TestNativeAPIChangesRefuseActiveOrUnknownPhoneTasks(t *testing.T) {
	for _, family := range []string{"codex", "claude", "deepseek-harness", "hermes"} {
		t.Run(family, func(t *testing.T) {
			s, h := newTestServer(t)
			id := createLocal(t, s, h, "api", "B", "synthetic-B", "https://fixture.invalid")
			root := t.TempDir()
			t.Setenv("CODEX_HOME", root)
			t.Setenv("CLAUDE_CONFIG_DIR", root)
			t.Setenv("DSH_HOME", root)
			t.Setenv("HERMES_HOME", root)
			history := filepath.Join(root, "original-history.jsonl")
			if err := os.WriteFile(history, []byte("original A conversation\n"), 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			s.d.Local.Start = func(context.Context, launcher.Plan) (int, error) { calls++; return 1, nil }
			a := &sessionAgent{kind: family, info: protocol.SessionInfo{Tool: family, SessionKey: family + ":phone", Cwd: root}}
			s.SetManager(sessionManager{a})
			before := s.d.Store.Get()
			for _, status := range []string{"running", "waiting_approval", "unknown"} {
				a.info.Status = status
				for _, applyOnly := range []bool{false, true} {
					w := localRequest(s, h, "POST", "/api/local/switch", map[string]any{"id": id, "target": family, "confirmed": true, "applyOnly": applyOnly})
					if w.Code != http.StatusConflict {
						t.Fatalf("%s applyOnly=%v: %d %s", status, applyOnly, w.Code, w.Body)
					}
				}
				w := localRequest(s, h, "POST", "/api/local/default", map[string]any{"kind": family, "action": "restore"})
				if w.Code != http.StatusConflict {
					t.Fatalf("restore %s: %d %s", status, w.Code, w.Body)
				}
			}
			if calls != 0 || !reflect.DeepEqual(before, s.d.Store.Get()) {
				t.Fatal("rejected change modified configuration or launched a process")
			}
			data, err := os.ReadFile(history)
			if err != nil || string(data) != "original A conversation\n" {
				t.Fatal("history changed", err)
			}
		})
	}
}

func TestAdditionalNativeResumeExplainsSupportedHistoryEntry(t *testing.T) {
	s, h := newTestServer(t)
	for _, family := range []string{"deepseek-harness", "hermes"} {
		w := localRequest(s, h, "POST", "/api/local/resume", map[string]string{"target": family, "sessionKey": family + ":0123456789abcdef0123456789abcdef"})
		if w.Code != http.StatusConflict {
			t.Fatalf("%s: %d %s", family, w.Code, w.Body)
		}
	}
}
