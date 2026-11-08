package hubclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

type localNativeIdleReader struct {
	routeDesktop
	idleErr error
	checks  int
}

func (d *localNativeIdleReader) NativeStationIdle(context.Context) error {
	d.checks++
	return d.idleErr
}

func TestLocalAgentChangeRequiresStrictNativeIdleProof(t *testing.T) {
	c := statusFixture(t, config.Config{})
	c.SetManager(&fakeManager{list: []agents.Agent{&busyAPIWorker{fakeAgent: &fakeAgent{id: "codex"}}}})
	d := &localNativeIdleReader{routeDesktop: routeDesktop{st: liveDesktop("codex:11111111-2222-4333-8444-555555555555")}, idleErr: errors.New("native state unknown")}
	c.o.Desktop = d
	if release, err := c.BeginLocalAgentChange(context.Background(), "codex"); err == nil {
		release()
		t.Fatal("display-only idle list was treated as proof that all native work stopped")
	}
	if d.checks != 1 {
		t.Fatal("native owner's strict check was not called")
	}
	d.idleErr = nil
	release, err := c.BeginLocalAgentChange(context.Background(), "codex")
	if err != nil {
		t.Fatal(err)
	}
	release()
	c.o.Desktop = &d.routeDesktop
	if release, err = c.BeginLocalAgentChange(context.Background(), "codex"); err == nil {
		release()
		t.Fatal("older native adapter without idle proof was allowed to change API")
	}
}

func TestLocalAgentChangeSharesPhoneAdmissionAndScopesBusyChecks(t *testing.T) {
	for _, family := range []string{"codex", "claude", "deepseek-harness", "hermes"} {
		t.Run(family, func(t *testing.T) {
			c := statusFixture(t, config.Config{})
			worker := &busyAPIWorker{fakeAgent: &fakeAgent{id: family}, busy: true}
			other := "hermes"
			if family == other {
				other = "codex"
			}
			c.SetManager(&fakeManager{list: []agents.Agent{worker, &busyAPIWorker{fakeAgent: &fakeAgent{id: other}, busy: true}}})
			if release, err := c.BeginLocalAgentChange(context.Background(), family); err == nil {
				release()
				t.Fatal("active phone task admitted native configuration change")
			}
			worker.busy = false
			release, err := c.BeginLocalAgentChange(context.Background(), family)
			if err != nil {
				t.Fatal("another family's work blocked this idle tool", err)
			}
			defer release()
			if c.taskConfigMu.TryLock() {
				c.taskConfigMu.Unlock()
				t.Fatal("native change did not hold phone admission")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			release()
			release() // callers can safely defer the lease as well
			if next, err := c.BeginLocalAgentChange(ctx, family); err == nil {
				next()
				t.Fatal("cancelled native change acquired admission")
			}
			ctx, cancel = context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			next, err := c.BeginLocalAgentChange(ctx, family)
			if err != nil {
				t.Fatal("released admission did not recover", err)
			}
			next()
		})
	}
}
