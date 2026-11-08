package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type receiptReconnectAgent struct {
	*fakeAgent
	entered, release chan struct{}
	calls            atomic.Int32
	fail             bool
}

func (a *receiptReconnectAgent) finish() error {
	a.calls.Add(1)
	close(a.entered)
	<-a.release
	if a.fail {
		return errors.New("fixture-adapter-rejection")
	}
	return nil
}
func (a *receiptReconnectAgent) StartWithOptions(context.Context, string, string, string, agents.TurnOptions) (string, error) {
	if err := a.finish(); err != nil {
		return "", err
	}
	return "private-session-created", nil
}
func (a *receiptReconnectAgent) SendWithOptions(context.Context, string, string, agents.TurnOptions) error {
	return a.finish()
}

func TestExecutedCommandRetainsUncertaintyAcrossAuthorizationGap(t *testing.T) {
	for _, typ := range []string{"session.start", "session.send"} {
		for _, scenario := range []string{"kick", "kick-after-adapter-error", "revoked", "replaced-binding", "stable-success", "stable-error", "before-admission"} {
			t.Run(typ+"/"+scenario, func(t *testing.T) {
				hub, server := newFakeHub(t)
				cwd := t.TempDir()
				c, store := newTestClient(t, server.URL, hub.key, []protocol.Project{{Path: cwd}})
				if err := store.Update(func(cfg *config.Config) error {
					cfg.PhoneBindingID = strings.Repeat("a", 64)
					cfg.PhoneHash = strings.Repeat("b", 64)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				cfg := store.Get()
				c.publishPair(PairStatus{}, eventIdentity(cfg))
				a := &receiptReconnectAgent{fakeAgent: &fakeAgent{id: "codex"}, entered: make(chan struct{}), release: make(chan struct{}), fail: scenario == "kick-after-adapter-error" || scenario == "stable-error"}
				c.SetManager(&fakeManager{list: []agents.Agent{a}})
				var once sync.Once
				release := func() { once.Do(func() { close(a.release) }) }
				defer release()
				cmd := map[string]any{"type": typ, "controlSurface": "cli", "operationId": projectPreviewFixtureID}
				if typ == "session.start" {
					cmd["tool"], cmd["cwd"], cmd["prompt"] = "codex", cwd, "fixture prompt"
				} else {
					cmd["sessionKey"], cmd["text"] = workspaceTestKey, "fixture prompt"
				}
				env := protocol.CommandEnvelope{DeviceID: cfg.DeviceID, CommandID: "executed-reconnect-fixture", BindingID: cfg.PhoneBindingID, PhoneHash: cfg.PhoneHash, Command: cmd}
				if scenario == "before-admission" {
					c.Kick()
				}
				done := make(chan struct{})
				go func() { defer close(done); c.executeFor(env, cfg) }()
				if scenario != "before-admission" {
					select {
					case <-a.entered:
					case rep := <-hub.replies:
						t.Fatalf("adapter not reached: %+v", rep)
					case <-time.After(3 * time.Second):
						t.Fatal("adapter not reached")
					}
					switch scenario {
					case "kick", "kick-after-adapter-error":
						c.Kick()
					case "revoked":
						if err := store.Update(func(next *config.Config) error { next.PhoneHash = ""; return nil }); err != nil {
							t.Fatal(err)
						}
					case "replaced-binding":
						if err := store.Update(func(next *config.Config) error {
							next.PhoneBindingID = strings.Repeat("c", 64)
							next.PhoneHash = strings.Repeat("d", 64)
							return nil
						}); err != nil {
							t.Fatal(err)
						}
						c.publishPair(PairStatus{}, eventIdentity(store.Get()))
					}
					release()
				}
				var reply protocol.Reply
				select {
				case reply = <-hub.replies:
				case <-time.After(3 * time.Second):
					t.Fatal("no execution reply")
				}
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("execution did not finish")
				}
				switch scenario {
				case "stable-success":
					if !reply.OK || reply.Code != "" || reply.Result == nil {
						t.Fatalf("success changed: %+v", reply)
					}
				case "stable-error":
					if reply.OK || reply.Code != "" || reply.Error != "fixture-adapter-rejection" {
						t.Fatalf("definite error was changed: %+v", reply)
					}
				case "before-admission":
					if reply.OK || reply.Code != "" || a.calls.Load() != 0 {
						t.Fatalf("pre-dispatch denial became uncertain: %+v", reply)
					}
				default:
					if reply.OK || reply.Code != "command_delivery_uncertain" || reply.Result != nil {
						t.Fatalf("executed command lost pending semantics: %+v", reply)
					}
					raw, _ := json.Marshal(reply)
					if strings.Contains(string(raw), "private-session-created") || strings.Contains(string(raw), "fixture-adapter-rejection") {
						t.Fatal("authorization gap leaked private result")
					}
				}
				if scenario != "before-admission" && a.calls.Load() != 1 {
					t.Fatal("command was replayed")
				}
			})
		}
	}
}
