package agents

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type resumeAdmissionTransport struct {
	base *permissionTransport
	hook func(string)
}

func (w *resumeAdmissionTransport) Close() error { return nil }
func (w *resumeAdmissionTransport) Write(b []byte) (int, error) {
	var req struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(b, &req); err != nil {
		return 0, err
	}
	if w.hook != nil {
		w.hook(req.Method)
	}
	return w.base.Write(b)
}

func TestCodexInitialResumeRejectsWrongIdentityAndUnsafeReply(t *testing.T) {
	for _, scenario := range []string{"wrong-id", "empty-id", "active", "unknown", "active-turn"} {
		t.Run(scenario, func(t *testing.T) {
			a, p := permissionFixture(t)
			target := a.threads["permission-thread"]
			target.loaded = false
			p.corrupt = func(reply map[string]any) {
				thread := p.base.thread
				switch scenario {
				case "wrong-id":
					thread.ID = "wrong-thread"
				case "empty-id":
					thread.ID = ""
				case "active":
					thread.Status.Type = "active"
				case "unknown":
					thread.Status.Type = "future-state"
				case "active-turn":
					thread.Turns = []cxTurn{{ID: "unexpected-turn", Status: "inProgress"}}
				}
				reply["thread"] = thread
			}
			if err := a.Send(context.Background(), "permission-thread", "not sent"); err == nil {
				t.Fatal("invalid initial resume accepted")
			}
			for _, req := range p.requests {
				if req["method"] == "turn/start" {
					t.Fatal("input sent after invalid resume")
				}
			}
			if target.loaded || a.threads["wrong-thread"] != nil || a.threads[""] != nil {
				t.Fatal("wrong identity mutated thread registry")
			}
		})
	}
}

func TestCodexInitialResumeFencesReadProviderAndResumeEpoch(t *testing.T) {
	for _, method := range []string{"thread/read", "config/read", "thread/resume"} {
		for _, change := range []string{"runtime", "turn", "status"} {
			t.Run(method+"/"+change, func(t *testing.T) {
				a, p := permissionFixture(t)
				target := a.threads["permission-thread"]
				target.loaded = false
				a.conn.stdin = &resumeAdmissionTransport{base: p, hook: func(observed string) {
					if observed != method {
						return
					}
					a.mu.Lock()
					defer a.mu.Unlock()
					switch change {
					case "runtime":
						a.conn = &rpcConn{pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
					case "turn":
						target.turnVersion++
						target.turnID = "new-turn"
						target.info.ActiveTurnID = "new-turn"
						target.info.Status = "running"
					case "status":
						target.statusUnconfirmed = true
						target.info.Status = "waiting_approval"
					}
				}}
				err := a.Send(context.Background(), "permission-thread", "not sent")
				if err == nil {
					t.Fatal("changed runtime/session admitted input")
				}
				for _, req := range p.requests {
					if req["method"] == "turn/start" {
						t.Fatal("input sent after changed runtime/session")
					}
				}
				if change == "turn" && (target.turnID != "new-turn" || target.info.Status != "running") {
					t.Fatal("late snapshot overwrote newer turn")
				}
				if change == "status" && (!target.statusUnconfirmed || target.info.Status != "waiting_approval") {
					t.Fatal("late snapshot overwrote newer status")
				}
			})
		}
	}
}

func TestCodexInheritedDangerousSandboxNeedsConsentWithoutChangingHostMode(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial-tuple", true: "full-tuple"}[confirmed], func(t *testing.T) {
			a, p := permissionFixture(t)
			a.threads["permission-thread"].loaded = false
			p.corrupt = func(reply map[string]any) {
				reply["sandbox"] = map[string]any{"type": "dangerFullAccess"}
				if confirmed {
					reply["approvalPolicy"] = "never"
					reply["approvalsReviewer"] = "user"
				}
			}
			denied := errors.New("synthetic full-access consent required")
			guard := func() error {
				if a.SessionRequiresFullAccessConsent("permission-thread") {
					return denied
				}
				return nil
			}
			if err := a.SendWithOptions(context.Background(), "permission-thread", "unsent", TurnOptions{ValidatePermission: guard}); !errors.Is(err, denied) {
				t.Fatal(err)
			}
			if !a.SessionRequiresFullAccessConsent("permission-thread") {
				t.Fatal("dangerous inherited sandbox hidden")
			}
			if !confirmed && a.KnownSessionPermission("permission-thread") != "" {
				t.Fatal("unknown tuple falsely labeled")
			}
			for _, req := range p.requests {
				if req["method"] == "turn/start" {
					t.Fatal("input sent before consent")
				}
				if req["method"] == "thread/resume" {
					params := req["params"].(map[string]any)
					if _, exists := params["sandbox"]; exists {
						t.Fatal("follow-host silently rewrote permissions")
					}
				}
			}
			p.corrupt = nil
			if err := a.SendWithOptions(context.Background(), "permission-thread", "safe recovery", TurnOptions{PermissionMode: "ask"}); err != nil {
				t.Fatal(err)
			}
			if a.SessionRequiresFullAccessConsent("permission-thread") || a.KnownSessionPermission("permission-thread") != "ask" {
				t.Fatal("explicit safe mode failed to retire inherited privilege")
			}
		})
	}
}
