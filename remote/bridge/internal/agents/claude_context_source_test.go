package agents

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type claudeContextSourceFixture struct {
	*claudeContextFixture
	cfg   config.Config
	ctx   context.Context
	epoch atomic.Value
	rec   *recorder
}

func newClaudeContextSourceFixture(t *testing.T, local bool) *claudeContextSourceFixture {
	t.Helper()
	f, rec := newClaudeContextFixture(t)
	computer := config.LocalAccount{ID: "computer-a", Kind: "claude", SourceType: "api_key", BaseURL: "https://computer.invalid", Key: "synthetic-local-a", Model: "claude-local"}
	phone := config.LocalAccount{ID: "phone-b", Kind: "claude", SourceType: "api_key", BaseURL: "https://phone.invalid", Key: "synthetic-phone-b", Model: "claude-remote"}
	cfg := config.Config{LocalAccounts: []config.LocalAccount{computer, phone}, GatewayRuntimeKey: "synthetic-process", RemoteExecutionArmed: true, RemoteAPI: map[string]config.RemoteAPI{"claude": {AccountID: phone.ID, Model: phone.Model}}, GatewayAuthEpochs: map[string]string{"claude": "a1", "remote-claude": "b1"}}
	cfg.RecordAppliedAPI("claude", computer, "")
	cfg.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(cfg), Revision: "station", Station: protocol.StationPolicy{Version: 1, APIBases: []string{phone.BaseURL}}}
	f.a.settings = func() Settings { s := LocalSettingsWithGateway(cfg, 47831); s.LocalPort = 47831; return s }
	s := &claudeContextSourceFixture{claudeContextFixture: f, cfg: cfg, rec: rec}
	s.epoch.Store("r1")
	s.ctx = WithConfigSnapshot(context.Background(), cfg)
	if local {
		s.ctx = WithLocalConfigSnapshot(context.Background(), cfg)
	}
	s.ctx = WithContextLifecycleGuard(s.ctx, s.sourceGuard("r1"))
	f.p.workerSettingsSig = claudeSettingsSig(settingsForContext(s.ctx, f.a.settings), f.a.exeOverride)
	return s
}

func (f *claudeContextSourceFixture) sourceGuard(expected string) func() error {
	return func() error {
		if f.epoch.Load().(string) != expected {
			return errClaudeContextChanged
		}
		return nil
	}
}

func TestClaudeContextRemoteSnapshotReadsCompactsAndCompletesAfterCallerCancellation(t *testing.T) {
	for _, local := range []bool{false, true} {
		t.Run(map[bool]string{false: "remote-b-local-a", true: "local-a"}[local], func(t *testing.T) {
			f := newClaudeContextSourceFixture(t, local)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			guard := func() error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return f.sourceGuard("r1")()
			}
			view, err := f.a.SessionContext(ctx, f.p.id, guard)
			if err != nil || view.Snapshot == nil || !view.CanCompact {
				t.Fatalf("source-bound context: %+v %v", view, err)
			}
			op := newUUID()
			started, err := f.a.CompactContext(ctx, f.p.id, view.Revision, op, guard)
			if err != nil || started.Compaction == nil || started.Compaction.State != "running" || !started.Compaction.Blocking || !f.a.ActiveRemoteTurns() {
				t.Fatalf("source-bound compact: %+v %v", started, err)
			}
			cancel()
			f.result("source-bound-complete", true, false)
			f.rec.wait(t, "source-bound post-compact context", func(e protocol.Event) bool {
				return e.Type == "context.updated" && e.Context != nil && e.Context.Compaction != nil && e.Context.Compaction.OperationID == op && e.Context.Compaction.State == "completed" && e.Context.Snapshot != nil
			})
			if current := f.a.claudeContextView(f.p); current.Compaction.State != "completed" || current.Compaction.Blocking || f.a.ActiveRemoteTurns() {
				t.Fatal("caller cancellation invalidated admitted native work")
			}
			duplicate, err := f.a.CompactContext(f.ctx, f.p.id, view.Revision, op, f.sourceGuard("r1"))
			if err != nil || duplicate.Compaction.State != "completed" {
				t.Fatalf("receipt: %+v %v", duplicate, err)
			}
			f.a.mu.Lock()
			delete(f.a.procs, f.p.id)
			f.a.mu.Unlock()
			detached, err := f.a.SessionContext(f.ctx, f.p.id, f.sourceGuard("r1"))
			if err != nil || detached.Compaction == nil || detached.Compaction.State != "completed" || detached.Compaction.Blocking || detached.Snapshot != nil {
				t.Fatalf("detached source-bound receipt: %+v %v", detached, err)
			}
			f.mu.Lock()
			count := len(f.users)
			f.mu.Unlock()
			if count != 1 {
				t.Fatal("duplicate native compact")
			}
		})
	}
}

func TestClaudeContextSnapshotRequiresLiveAdmissionAndLifecycleGuards(t *testing.T) {
	f := newClaudeContextSourceFixture(t, false)
	if _, err := f.a.SessionContext(f.ctx, f.p.id, nil); !errors.Is(err, errClaudeContextChanged) {
		t.Fatal("frozen read accepted without live guard")
	}
	view, err := f.a.SessionContext(f.ctx, f.p.id, f.sourceGuard("r1"))
	if err != nil {
		t.Fatal(err)
	}
	unguarded := WithConfigSnapshot(context.Background(), f.cfg)
	if _, err = f.a.CompactContext(unguarded, f.p.id, view.Revision, newUUID(), f.sourceGuard("r1")); !errors.Is(err, errClaudeContextChanged) {
		t.Fatal("frozen compact accepted without lifecycle guard")
	}
	ctx, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err = f.a.CompactContext(ctx, f.p.id, view.Revision, newUUID(), f.sourceGuard("r1")); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled command admitted compact")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.users) != 0 {
		t.Fatal("unvalidated compact reached stdin")
	}
}

func TestClaudeContextRemoteReadRejectsRevocationABARestorationAndOldWorker(t *testing.T) {
	for _, change := range []string{"revoked", "aba", "old-worker"} {
		t.Run(change, func(t *testing.T) {
			f := newClaudeContextSourceFixture(t, false)
			f.onControl = func(kind string) {
				if kind != "get_context_usage" {
					return
				}
				switch change {
				case "revoked":
					f.epoch.Store("r2")
				case "aba":
					f.epoch.Store("r2-other-source")
					f.epoch.Store("r3-restored-source")
				case "old-worker":
					next := claudeSyntheticPermissionWorker(t, f.a, f.p.id)
					next.workerSettingsSig = f.p.workerSettingsSig
				}
			}
			ctx, cancel := context.WithTimeout(f.ctx, 100*time.Millisecond)
			defer cancel()
			if view, err := f.a.SessionContext(ctx, f.p.id, f.sourceGuard("r1")); err == nil || view.Snapshot != nil {
				t.Fatalf("stale read crossed source boundary: %+v %v", view, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.users) != 0 {
				t.Fatal("read became a user turn")
			}
		})
	}
}

func TestClaudeCompactRemoteLifecycleRejectsRevocationABARestorationAndOldWorker(t *testing.T) {
	for _, change := range []string{"revoked", "aba", "old-worker"} {
		t.Run(change, func(t *testing.T) {
			f := newClaudeContextSourceFixture(t, false)
			view, err := f.a.SessionContext(f.ctx, f.p.id, f.sourceGuard("r1"))
			if err != nil {
				t.Fatal(err)
			}
			op := newUUID()
			if _, err = f.a.CompactContext(f.ctx, f.p.id, view.Revision, op, f.sourceGuard("r1")); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "revoked":
				f.epoch.Store("r2")
			case "aba":
				f.epoch.Store("r2-other-source")
				f.epoch.Store("r3-restored-source")
			case "old-worker":
				next := claudeSyntheticPermissionWorker(t, f.a, f.p.id)
				next.workerSettingsSig = f.p.workerSettingsSig
				_, unlock, err := f.a.lockActiveClaudeContext(next)
				if err != nil {
					t.Fatal(err)
				}
				unlock()
			}
			f.result("stale-native-result", true, false)
			f.a.contextState.mu.Lock()
			receipt := f.a.contextState.sessions[f.p.id].operations[op].receipt
			f.a.contextState.mu.Unlock()
			if receipt.State != "unconfirmed" {
				t.Fatalf("stale native result confirmed: %+v", receipt)
			}
			if f.rec.count(func(e protocol.Event) bool {
				return e.Type == "context.updated" && e.Context != nil && e.Context.Compaction != nil && e.Context.Compaction.OperationID == op && e.Context.Compaction.State == "completed"
			}) != 0 {
				t.Fatal("stale completion emitted")
			}
		})
	}
}

func TestClaudeCompactDetachedOldAuthorizationCannotBeRevivedByNewProof(t *testing.T) {
	f := newClaudeContextSourceFixture(t, true) // Local route key can stay identical while its authorization epoch changes.
	f.version = "2.1.220"
	view, err := f.a.SessionContext(f.ctx, f.p.id, f.sourceGuard("r1"))
	if err != nil {
		t.Fatal(err)
	}
	op := newUUID()
	if _, err = f.a.CompactContext(f.ctx, f.p.id, view.Revision, op, f.sourceGuard("r1")); err != nil {
		t.Fatal(err)
	}
	f.result("finished-before-aba", true, false)
	f.a.mu.Lock()
	delete(f.a.procs, f.p.id)
	f.a.mu.Unlock()
	f.epoch.Store("r2-other")
	f.epoch.Store("r3-restored")
	fresh := WithContextLifecycleGuard(WithLocalConfigSnapshot(context.Background(), f.cfg), f.sourceGuard("r3-restored"))
	if result, err := f.a.CompactContext(fresh, f.p.id, view.Revision, op, f.sourceGuard("r3-restored")); !errors.Is(err, errClaudeContextChanged) || result.Compaction != nil {
		t.Fatal("new proof revived old operation authorization")
	}
}
