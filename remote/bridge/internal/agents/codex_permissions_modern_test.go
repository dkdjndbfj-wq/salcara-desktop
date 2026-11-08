package agents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// Models the installed app-server: loaded-thread resume ignores legacy
// overrides, settings/update returns {}, and a later resume reports the tuple.
type modernPermissionTransport struct {
	base                      *resumeSafetyTransport
	mode, failure             string
	updates, inputs           int
	policy, reviewer, sandbox string
	created                   string
}

func (p *modernPermissionTransport) Close() error { return nil }
func (p *modernPermissionTransport) Write(data []byte) (int, error) {
	var req struct {
		ID     int64          `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return 0, err
	}
	response := map[string]any{"id": req.ID}
	switch req.Method {
	case "thread/settings/update":
		p.updates++
		if req.Params["sandbox"] != nil || req.Params["modelProvider"] != nil {
			p.base.t.Fatal("settings update used legacy or unrelated routing fields")
		}
		if p.failure == "unsupported" {
			response["error"] = map[string]any{"code": -32601, "message": "unsupported"}
			break
		}
		if p.failure != "ignored" {
			p.policy, _ = req.Params["approvalPolicy"].(string)
			p.reviewer, _ = req.Params["approvalsReviewer"].(string)
			p.sandbox, _ = req.Params["sandboxPolicy"].(map[string]any)["type"].(string)
		}
		response["result"] = map[string]any{}
		if p.created != "" && p.failure != "no-notification" {
			id := p.created
			if p.failure == "wrong-identity" {
				id = "wrong-thread"
			}
			note, _ := json.Marshal(map[string]any{"method": "thread/settings/updated", "params": map[string]any{"threadId": id,
				"threadSettings": map[string]any{"approvalPolicy": p.policy, "approvalsReviewer": p.reviewer, "sandboxPolicy": map[string]any{"type": p.sandbox}}}})
			p.base.conn.handleLine(note)
		}
	case "thread/start", "thread/resume":
		thread := p.base.thread
		if req.Method == "thread/start" {
			thread.ID = "modern-created"
			p.created = thread.ID
		} else {
			thread.ID, _ = req.Params["threadId"].(string)
			if thread.ID == p.created {
				response["error"] = map[string]any{"code": -32600, "message": "no rollout found for thread id"}
				break
			}
		}
		if p.failure == "wrong-identity" && p.updates > 0 {
			thread.ID = "wrong-thread"
		}
		response["result"] = map[string]any{"thread": thread, "approvalPolicy": p.policy, "approvalsReviewer": p.reviewer, "sandbox": map[string]any{"type": p.sandbox}}
	case "turn/start":
		p.inputs++
		if p.updates != 1 {
			p.base.t.Fatal("input preceded explicit permission acknowledgement")
		}
		return p.base.Write(data)
	default:
		return p.base.Write(data)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return 0, err
	}
	p.base.conn.handleLine(encoded)
	return len(data), nil
}

func TestCodexModernPermissionUpdateConfirmsBeforeInput(t *testing.T) {
	for _, start := range []bool{false, true} {
		for _, mode := range []string{"ask", "auto_review", "full_access"} {
			t.Run(map[bool]string{false: "resume", true: "new"}[start]+"/"+mode, func(t *testing.T) {
				a, legacy := permissionFixture(t)
				p := &modernPermissionTransport{base: legacy.base, mode: mode, policy: "never", reviewer: "user", sandbox: "readOnly"}
				a.conn.stdin = p
				var err error
				if start {
					_, err = a.StartWithOptions(context.Background(), t.TempDir(), "fixture", "ask", TurnOptions{PermissionMode: mode})
				} else {
					err = a.SendWithOptions(context.Background(), "permission-thread", "fixture", TurnOptions{PermissionMode: mode})
				}
				if err != nil || p.updates != 1 || p.inputs != 1 {
					t.Fatalf("permission roundtrip failed: updates=%d inputs=%d err=%v", p.updates, p.inputs, err)
				}
			})
		}
	}
}

func TestCodexModernPermissionUpdateFailureNeverSends(t *testing.T) {
	for _, failure := range []string{"unsupported", "ignored", "wrong-identity"} {
		t.Run(failure, func(t *testing.T) {
			a, legacy := permissionFixture(t)
			p := &modernPermissionTransport{base: legacy.base, failure: failure, policy: "never", reviewer: "user", sandbox: "readOnly"}
			a.conn.stdin = p
			err := a.SendWithOptions(context.Background(), "permission-thread", "must not send", TurnOptions{PermissionMode: "ask"})
			if !errors.Is(err, errCodexPermissionUnconfirmed) || p.inputs != 0 {
				t.Fatalf("unconfirmed mode admitted input: %v", err)
			}
		})
	}
}

func TestCodexModernNewPermissionRequiresMatchingNotification(t *testing.T) {
	for _, failure := range []string{"unsupported", "ignored", "wrong-identity", "no-notification"} {
		t.Run(failure, func(t *testing.T) {
			a, legacy := permissionFixture(t)
			p := &modernPermissionTransport{base: legacy.base, failure: failure, policy: "never", reviewer: "user", sandbox: "readOnly"}
			a.conn.stdin = p
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err := a.StartWithOptions(ctx, t.TempDir(), "must not send", "ask", TurnOptions{PermissionMode: "ask"})
			if !errors.Is(err, errCodexPermissionUnconfirmed) || p.inputs != 0 {
				t.Fatalf("new thread sent input without an effective settings notification: %v", err)
			}
			if len(a.conn.permissionObservers) != 0 {
				t.Fatal("permission observation leaked after failed confirmation")
			}
		})
	}
}
