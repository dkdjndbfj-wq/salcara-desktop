package hubclient

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

type cancellationAPIWorker struct {
	*fakeAgent
	checks  int
	onCheck func(int)
}

func (a *cancellationAPIWorker) ActiveRemoteTurns() bool {
	a.checks++
	if a.onCheck != nil {
		a.onCheck(a.checks)
	}
	return false
}

func TestCancelledDispatchDoesNotChangeAPI(t *testing.T) {
	for _, scenario := range []string{"already_cancelled", "deadline_expired", "cancelled_while_queued", "cancelled_during_final_activity_check"} {
		t.Run(scenario, func(t *testing.T) {
			account := config.LocalAccount{ID: "vault", Name: "备用", Kind: "api", Key: "fake-not-real", BaseURL: "https://fixture.invalid/v1", Models: []string{"m1"}}
			c := statusFixture(t, config.Config{DeviceID: "pc", DeviceSecret: "fixture-secret", LocalAccounts: []config.LocalAccount{account}})
			worker := &cancellationAPIWorker{fakeAgent: &fakeAgent{id: "codex"}}
			c.SetManager(&fakeManager{list: []agents.Agent{worker}})
			cmd := map[string]any{"type": "agents.api.set", "agent": "codex", "accountId": APIHandle(c.o.Store.Get(), account.ID), "sessionKey": "codex:original"}
			before := c.o.Store.Get()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wantErr := error(context.Canceled)
			switch scenario {
			case "already_cancelled":
				cancel()
			case "deadline_expired":
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer deadlineCancel()
				wantErr = context.DeadlineExceeded
			case "cancelled_during_final_activity_check":
				worker.onCheck = func(checks int) {
					if checks == 2 {
						cancel()
					}
				}
			}
			var dispatchErr error
			if scenario == "cancelled_while_queued" {
				c.taskConfigMu.Lock()
				var unlock sync.Once
				release := func() { unlock.Do(c.taskConfigMu.Unlock) }
				defer release()
				started, done := make(chan struct{}), make(chan error, 1)
				go func() {
					close(started)
					_, err := c.Dispatch(ctx, cmd)
					done <- err
				}()
				<-started
				select {
				case err := <-done:
					t.Fatalf("dispatch completed while admission was locked: %v", err)
				case <-time.After(25 * time.Millisecond):
				}
				cancel()
				release()
				select {
				case dispatchErr = <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("cancelled command remained stuck after admission unlocked")
				}
			} else {
				_, dispatchErr = c.Dispatch(ctx, cmd)
			}
			if !errors.Is(dispatchErr, wantErr) {
				t.Errorf("cancelled API change returned %v, want %v", dispatchErr, wantErr)
			}
			if !reflect.DeepEqual(before, c.o.Store.Get()) {
				t.Error("cancelled command changed the selected API or desktop configuration")
			}
			if len(worker.started)+len(worker.sent)+len(worker.stopped)+len(worker.opened) != 0 {
				t.Error("cancelled command affected a conversation")
			}
		})
	}
}
