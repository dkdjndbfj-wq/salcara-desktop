package hubclient

import (
	"context"
	"errors"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

type safeAPIWorker struct {
	*busyAPIWorker
	stops         int
	stopError     error
	requestedOnly bool
}

func (a *safeAPIWorker) StopForAPIChange(context.Context) error {
	a.stops++
	if a.stopError != nil {
		return a.stopError
	}
	if !a.requestedOnly {
		a.busy = false
	}
	return nil
}

func TestSafeAgentChangeFamilyStopRetainsAdmission(t *testing.T) {
	for _, mode := range []string{"success", "requested", "failed"} {
		t.Run(mode, func(t *testing.T) {
			c := statusFixture(t, config.Config{})
			a := &safeAPIWorker{busyAPIWorker: &busyAPIWorker{fakeAgent: &fakeAgent{id: "codex"}, busy: true}, requestedOnly: mode == "requested"}
			other := &safeAPIWorker{busyAPIWorker: &busyAPIWorker{fakeAgent: &fakeAgent{id: "hermes"}, busy: true}}
			if mode == "failed" {
				a.stopError = errors.New("synthetic stop failure")
			}
			c.SetManager(&fakeManager{list: []agents.Agent{a, other}})
			release, err := c.BeginLocalAgentStopChange(context.Background(), "codex-desktop")
			if other.stops != 0 || !other.busy || a.stops != 1 {
				t.Fatal("wrong family stopped")
			}
			if mode != "success" {
				if err == nil {
					release()
					t.Fatal("unconfirmed stop admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.taskConfigMu.TryLock() {
				c.taskConfigMu.Unlock()
				t.Fatal("phone sends were not fenced")
			}
			release()
			release()
		})
	}
}
