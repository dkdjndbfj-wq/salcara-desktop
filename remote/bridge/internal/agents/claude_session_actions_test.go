package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestClaudeSessionActionsRemoteSnapshotUsesGuardInsteadOfLocalFallback(t *testing.T) {
	local := config.LocalAccount{ID: "local", Kind: "api", SourceType: "api_key", BaseURL: "https://local.fixture", Key: "local-key"}
	remote := config.LocalAccount{ID: "remote", Kind: "api", SourceType: "api_key", BaseURL: "https://remote.fixture", Key: "remote-key"}
	cfg := config.Config{LocalAccounts: []config.LocalAccount{local, remote}, RemoteAPI: map[string]config.RemoteAPI{"claude": {AccountID: remote.ID}}, GatewayRuntimeKey: "runtime", RemoteExecutionArmed: true}
	cfg.RecordAppliedAPI("claude", local, "")
	cfg.RemoteStationAuthorization = &config.StationAuthorization{Identity: config.RemoteIdentity(cfg), Revision: "r1", Station: protocol.StationPolicy{Version: 1, APIBases: []string{remote.BaseURL}}}
	a := newClaudeAgent(func(protocol.Event) {}, func() Settings { return LocalSettingsWithGateway(cfg, 47831) }, "", t.TempDir())
	t.Cleanup(a.Close)
	ctx := WithConfigSnapshot(context.Background(), cfg)
	want := claudeSettingsSig(SettingsWithGateway(cfg, 47831), "")
	got, err := a.claudeActionSource(ctx, func() error { return nil })
	if err != nil || got != want || got == claudeSettingsSig(a.settings(), "") {
		t.Fatalf("remote B was compared with local A: %q %v", got, err)
	}
	if _, err = a.claudeActionSource(ctx, nil); !errors.Is(err, errClaudeActionChanged) {
		t.Fatal("unguarded frozen source accepted")
	}
	if _, err = a.claudeActionSource(ctx, func() error { return errClaudeActionChanged }); !errors.Is(err, errClaudeActionChanged) {
		t.Fatal("revoked remote source accepted")
	}
}

// All mutation tests use synthetic transcripts and an explicitly injected
// transport. They verify Bridge admission/provenance, not an installed SDK's
// functional correctness, and never use a real account or launch a model.
type claudeActionFixture struct {
	a         *claudeAgent
	p         *claudeProc
	sdk       *claudeSessionSDK
	path, cwd string
	key       atomic.Value
	calls     atomic.Int32
	rec       *recorder
}

func newClaudeActionFixture(t *testing.T) *claudeActionFixture {
	t.Helper()
	f := &claudeActionFixture{cwd: t.TempDir(), rec: &recorder{}}
	f.key.Store("synthetic-api-key")
	f.a = newClaudeAgent(f.rec.sink, func() Settings {
		return Settings{ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: f.key.Load().(string), Approval: "ask"}
	}, "", t.TempDir())
	id := "11111111-1111-4111-8111-111111111111"
	f.p = &claudeProc{id: id, cwd: f.cwd, stopCh: make(chan struct{}), done: make(chan struct{}), policy: "ask"}
	f.p.workerSettingsSig = claudeSettingsSig(f.a.settings(), "")
	f.a.procs[id] = f.p
	f.a.live[id] = protocol.SessionInfo{SessionKey: "claude:" + id, Tool: "claude", Client: "Claude Code", ControlSurface: "cli",
		Cwd: f.cwd, Title: "Original", Status: "idle", Controllable: true, LastTerminalTurnID: "terminal_fixture"}
	project := filepath.Join(f.a.hist.dir, "fixture-project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	f.path = filepath.Join(project, id+".jsonl")
	writeActionFixtureRows(t, f.path, actionFixtureRows(id, f.cwd, ""))
	f.sdk = fixtureClaudeSDK(t, os.Args[0], `export function renameSession(){}; export function forkSession(){}; export function getSessionInfo(){};`)
	f.a.actionSDKResolve = func(context.Context) (*claudeSessionSDK, error) { return f.sdk, nil }
	f.a.actionSDKRun = f.mutate
	return f
}

func fixtureClaudeSDK(t *testing.T, node, source string) *claudeSessionSDK {
	t.Helper()
	dir := t.TempDir()
	pkg := `{"name":"@anthropic-ai/claude-agent-sdk","version":"0.3.300","claudeCodeVersion":"2.1.300"}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sdk.mjs"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	sdk, err := inspectClaudeSessionSDK(node, dir)
	if err != nil {
		t.Fatal(err)
	}
	return sdk
}
func actionFixtureRows(id, cwd, parent string) []map[string]any {
	row := map[string]any{"type": "user", "sessionId": id, "uuid": newUUID(), "cwd": cwd, "entrypoint": "sdk",
		"message": map[string]any{"role": "user", "content": "Original"}}
	if parent != "" {
		row["forkedFrom"] = map[string]any{"sessionId": parent, "messageUuid": newUUID()}
	}
	return []map[string]any{row}
}
func writeActionFixtureRows(t *testing.T, path string, rows []map[string]any) {
	t.Helper()
	var data []byte
	for _, row := range rows {
		b, _ := json.Marshal(row)
		data = append(data, append(b, '\n')...)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
func appendActionFixtureTitle(path, id, title string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(map[string]any{"type": "custom-title", "sessionId": id, "customTitle": title})
}
func (f *claudeActionFixture) mutate(ctx context.Context, sdk *claudeSessionSDK, input claudeSDKActionInput, guard func() error) (claudeSDKActionOutput, bool, error) {
	f.calls.Add(1)
	if err := guard(); err != nil {
		return claudeSDKActionOutput{}, false, err
	}
	if input.Action == "rename" {
		if err := appendActionFixtureTitle(input.Path, input.ID, input.Name); err != nil {
			return claudeSDKActionOutput{}, true, err
		}
		return claudeSDKActionOutput{Phase: "done", SessionID: input.ID, Title: input.Name}, true, nil
	}
	id := "22222222-2222-4222-8222-222222222222"
	path := filepath.Join(filepath.Dir(input.Path), id+".jsonl")
	rows := actionFixtureRows(id, input.Cwd, input.ID)
	rows = append(rows, map[string]any{"type": "custom-title", "sessionId": id, "customTitle": "Original (fork)"})
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return claudeSDKActionOutput{}, true, err
	}
	for _, row := range rows {
		if err = json.NewEncoder(file).Encode(row); err != nil {
			break
		}
	}
	file.Close()
	return claudeSDKActionOutput{Phase: "done", SessionID: id, Title: "Original (fork)"}, true, err
}
func (f *claudeActionFixture) catalog(t *testing.T) protocol.SessionActions {
	t.Helper()
	view, err := f.a.SessionActions(context.Background(), f.p.id, nil)
	if err != nil {
		t.Fatal(err)
	}
	return view
}
func actionRequest(view protocol.SessionActions, action, name string) protocol.SessionActionRequest {
	return protocol.SessionActionRequest{Action: action, Name: name, ExpectedRevision: view.Revision, OperationID: "33333333-3333-4333-8333-333333333333"}
}

func TestClaudeSessionActionsCLIOnlyIsExplicitlyUnavailable(t *testing.T) {
	f := newClaudeActionFixture(t)
	missing := filepath.Join(t.TempDir(), "missing-sdk")
	f.a.actionSDKResolve = func(context.Context) (*claudeSessionSDK, error) {
		return inspectClaudeSessionSDK(os.Args[0], missing)
	}
	view := f.catalog(t)
	if len(view.Actions) != 0 || view.UnavailableReason != "未安装兼容的 Claude 会话管理组件" || view.Revision == "" {
		t.Fatalf("false support: %+v", view)
	}
	if _, err := f.a.ApplySessionAction(context.Background(), f.p.id, actionRequest(view, "rename", "New"), nil); !errors.Is(err, errClaudeActionUnsupported) {
		t.Fatalf("unsupported action: %v", err)
	}
	if f.calls.Load() != 0 {
		t.Fatal("unsupported action ran an executor")
	}
}

func TestClaudeSessionActionsRenameReceiptAndNativeTitle(t *testing.T) {
	f := newClaudeActionFixture(t)
	view := f.catalog(t)
	if strings.Join(view.Actions, ",") != "rename,fork" {
		t.Fatalf("wrong native actions: %+v", view)
	}
	req := actionRequest(view, "rename", "New name")
	result, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil)
	if err != nil || result.State != "completed" || result.Session == nil || result.Session.Title != "New name" {
		t.Fatalf("rename: %+v %v", result, err)
	}
	st, _ := os.Stat(f.path)
	info, ok := scanTranscript(f.path, st)
	if !ok || info.Title != "New name" {
		t.Fatalf("native custom-title not projected: %+v", info)
	}
	result.Session.Title = "caller mutation"
	again, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil)
	if err != nil || again.State != "completed" || again.Session.Title != "New name" || f.calls.Load() != 1 {
		t.Fatalf("receipt replayed or aliased: %+v %v", again, err)
	}
	req.Name = "different payload"
	if _, err = f.a.ApplySessionAction(context.Background(), f.p.id, req, nil); !errors.Is(err, errClaudeActionChanged) {
		t.Fatal("operation ID accepted another title")
	}
	req.Name, req.OperationID = "Another", "44444444-4444-4444-8444-444444444444"
	if _, err = f.a.ApplySessionAction(context.Background(), f.p.id, req, nil); !errors.Is(err, errClaudeActionChanged) {
		t.Fatal("stale revision accepted")
	}
	if f.rec.count(func(e protocol.Event) bool { return e.Type == "session.updated" }) != 1 {
		t.Fatal("duplicate event")
	}
}

func TestClaudeSessionActionsForkConfirmsParentAndCreatesExactLease(t *testing.T) {
	f := newClaudeActionFixture(t)
	request := actionRequest(f.catalog(t), "fork", "")
	result, err := f.a.ApplySessionAction(context.Background(), f.p.id, request, nil)
	if err != nil || result.State != "completed" || result.Session == nil || result.Session.ForkedFromSessionKey != "claude:"+f.p.id {
		t.Fatalf("fork: %+v %v", result, err)
	}
	id := strings.TrimPrefix(result.Session.SessionKey, "claude:")
	if !f.a.hist.ownedCompletion(id) {
		t.Fatal("confirmed native fork cannot be resumed")
	}
	f.a.hist.mu.Lock()
	lease := f.a.hist.ownedLeases[id]
	source, exited, worker := lease.sdkSource, lease.exited, lease.worker
	f.a.hist.mu.Unlock()
	if source != f.p.workerSettingsSig || exited || worker != nil {
		t.Fatal("fork forged an OS-exit lease")
	}
	view, err := f.a.SessionActions(context.Background(), id, nil)
	if err != nil || len(view.Actions) != 2 {
		t.Fatalf("fork catalog: %+v %v", view, err)
	}
	again, err := f.a.ApplySessionAction(context.Background(), f.p.id, request, nil)
	if err != nil || again.Session.SessionKey != result.Session.SessionKey || f.calls.Load() != 1 {
		t.Fatal("retry created another fork")
	}
	if err := appendActionFixtureTitle(lease.path, id, "external write"); err != nil {
		t.Fatal(err)
	}
	if f.a.hist.ownedCompletion(id) {
		t.Fatal("changed fork retained provenance")
	}
}

func TestClaudeSessionActionsRejectBusyAndStaleFileBeforeExecution(t *testing.T) {
	for _, kind := range []string{"busy", "changed-file", "changed-worker", "changed-source", "sdk-replaced"} {
		t.Run(kind, func(t *testing.T) {
			f := newClaudeActionFixture(t)
			req := actionRequest(f.catalog(t), "rename", "New")
			switch kind {
			case "busy":
				f.p.mu.Lock()
				f.p.turns = 1
				f.p.mu.Unlock()
			case "changed-file":
				if err := appendActionFixtureTitle(f.path, f.p.id, "other"); err != nil {
					t.Fatal(err)
				}
			case "changed-worker":
				f.a.mu.Lock()
				f.a.procs[f.p.id] = &claudeProc{id: f.p.id, cwd: f.cwd, workerSettingsSig: f.p.workerSettingsSig}
				f.a.mu.Unlock()
			case "changed-source":
				f.key.Store("another-synthetic-api-key")
			case "sdk-replaced":
				if err := os.WriteFile(filepath.Join(f.sdk.dir, "sdk.mjs"), []byte("changed"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil); err == nil {
				t.Fatal("stale or busy state was admitted")
			}
			if f.calls.Load() != 0 {
				t.Fatal("executor ran before final admission")
			}
		})
	}
}

func TestClaudeSessionActionsFinalGuardRejectsSourceAndWorkerChanges(t *testing.T) {
	for _, kind := range []string{"source", "worker"} {
		t.Run(kind, func(t *testing.T) {
			f := newClaudeActionFixture(t)
			req := actionRequest(f.catalog(t), "rename", "New")
			f.a.actionSDKRun = func(ctx context.Context, sdk *claudeSessionSDK, in claudeSDKActionInput, guard func() error) (claudeSDKActionOutput, bool, error) {
				f.calls.Add(1)
				if kind == "source" {
					f.key.Store("changed")
				} else {
					f.a.mu.Lock()
					f.a.procs[f.p.id] = &claudeProc{id: f.p.id, cwd: f.cwd, workerSettingsSig: f.p.workerSettingsSig}
					f.a.mu.Unlock()
				}
				err := guard()
				if err == nil {
					t.Error("final guard accepted stale authority")
				}
				return claudeSDKActionOutput{}, false, err
			}
			result, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil)
			if err != nil || result.State != "unconfirmed" {
				t.Fatalf("receipt: %+v %v", result, err)
			}
			title, _ := readClaudeActionTitle(f.path, f.p.id)
			if title != "" {
				t.Fatal("guard failure wrote a title")
			}
		})
	}
}

func TestClaudeSessionActionsLostAcknowledgementNeverResends(t *testing.T) {
	f := newClaudeActionFixture(t)
	req := actionRequest(f.catalog(t), "rename", "New")
	f.a.actionSDKRun = func(ctx context.Context, sdk *claudeSessionSDK, in claudeSDKActionInput, guard func() error) (claudeSDKActionOutput, bool, error) {
		_, attempted, err := f.mutate(ctx, sdk, in, guard)
		if err != nil {
			return claudeSDKActionOutput{}, attempted, err
		}
		return claudeSDKActionOutput{}, true, context.DeadlineExceeded
	}
	result, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil)
	if err != nil || result.State != "unconfirmed" {
		t.Fatalf("lost ack: %+v %v", result, err)
	}
	_ = f.catalog(t)
	again, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil)
	if err != nil || again.State != "unconfirmed" || f.calls.Load() != 1 {
		t.Fatal("lost acknowledgement was replayed")
	}
	if len(f.rec.all()) != 0 {
		t.Fatal("unconfirmed native operation published success")
	}
}

func TestClaudeSessionActionsReceiptRechecksSourceAndWorkerAfterLockWait(t *testing.T) {
	for _, kind := range []string{"source", "worker", "permission"} {
		t.Run(kind, func(t *testing.T) {
			f := newClaudeActionFixture(t)
			req := actionRequest(f.catalog(t), "rename", "New")
			if result, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil); err != nil || result.State != "completed" {
				t.Fatalf("setup receipt: %+v %v", result, err)
			}
			settings := f.a.settings
			entered := make(chan struct{})
			var reads atomic.Int32
			f.a.settings = func() Settings {
				value := settings()
				if reads.Add(1) == 2 {
					close(entered)
				}
				return value
			}
			var revoked atomic.Bool
			denied := errors.New("synthetic receipt permission revoked")
			guard := func() error {
				if revoked.Load() {
					return denied
				}
				return nil
			}
			f.a.sessionActions.mu.Lock()
			finished := make(chan error, 1)
			go func() {
				_, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, guard)
				finished <- err
			}()
			<-entered
			switch kind {
			case "source":
				f.key.Store("changed-while-waiting")
			case "worker":
				f.a.mu.Lock()
				f.a.procs[f.p.id] = &claudeProc{id: f.p.id, cwd: f.cwd, workerSettingsSig: f.p.workerSettingsSig}
				f.a.mu.Unlock()
			case "permission":
				revoked.Store(true)
			}
			f.a.sessionActions.mu.Unlock()
			if err := <-finished; err == nil {
				t.Fatal("completed receipt crossed a source/worker fence")
			}
			if f.calls.Load() != 1 {
				t.Fatal("denied receipt caused another mutation")
			}
		})
	}
}

func TestClaudeSessionActionsUnknownForkBlocksOnlyAnotherFork(t *testing.T) {
	f := newClaudeActionFixture(t)
	req := actionRequest(f.catalog(t), "fork", "")
	f.a.actionSDKRun = func(ctx context.Context, sdk *claudeSessionSDK, in claudeSDKActionInput, guard func() error) (claudeSDKActionOutput, bool, error) {
		out, attempted, err := f.mutate(ctx, sdk, in, guard)
		if in.Action == "fork" && err == nil {
			return claudeSDKActionOutput{}, true, context.DeadlineExceeded
		}
		return out, attempted, err
	}
	result, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil)
	if err != nil || result.State != "unconfirmed" {
		t.Fatalf("lost fork acknowledgement: %+v %v", result, err)
	}
	view := f.catalog(t)
	if strings.Join(view.Actions, ",") != "rename" {
		t.Fatalf("unknown fork exposed another fork or blocked rename: %+v", view)
	}
	next := actionRequest(view, "fork", "")
	next.OperationID = "44444444-4444-4444-8444-444444444444"
	if _, err := f.a.ApplySessionAction(context.Background(), f.p.id, next, nil); !errors.Is(err, errClaudeForkUnconfirmed) {
		t.Fatalf("a new operation ID duplicated an unknown fork: %v", err)
	}
	if replay, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil); err != nil || replay.State != "unconfirmed" {
		t.Fatalf("original unknown receipt unavailable: %+v %v", replay, err)
	}
	next.Action, next.Name = "rename", "Parent remains usable"
	if renamed, err := f.a.ApplySessionAction(context.Background(), f.p.id, next, nil); err != nil || renamed.State != "completed" {
		t.Fatalf("unknown fork blocked parent rename: %+v %v", renamed, err)
	}
	if view = f.catalog(t); strings.Join(view.Actions, ",") != "rename" || f.calls.Load() != 2 {
		t.Fatalf("rename cleared the unknown-fork fence: %+v", view)
	}
	var writes atomic.Int32
	f.p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) { writes.Add(1); return len(data), nil }}
	if err := f.a.Send(context.Background(), f.p.id, "parent can continue"); err != nil || writes.Load() != 1 {
		t.Fatalf("unknown fork blocked parent send: %v", err)
	}
}

func TestClaudeSessionActionsForkDeniedBeforeCommitDoesNotFence(t *testing.T) {
	f := newClaudeActionFixture(t)
	req := actionRequest(f.catalog(t), "fork", "")
	f.a.actionSDKRun = func(context.Context, *claudeSessionSDK, claudeSDKActionInput, func() error) (claudeSDKActionOutput, bool, error) {
		return claudeSDKActionOutput{}, false, errClaudeSDKUnavailable
	}
	if result, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil); err != nil || result.State != "unconfirmed" {
		t.Fatalf("pre-commit denial: %+v %v", result, err)
	}
	view := f.catalog(t)
	if strings.Join(view.Actions, ",") != "rename,fork" {
		t.Fatalf("unattempted fork blocked another fork: %+v", view)
	}
	f.a.actionSDKRun = f.mutate
	req = actionRequest(view, "fork", "")
	req.OperationID = "44444444-4444-4444-8444-444444444444"
	if result, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil); err != nil || result.State != "completed" || f.calls.Load() != 1 {
		t.Fatalf("new explicit fork after pre-commit denial: %+v %v", result, err)
	}
}

func TestClaudeSessionActionsForkDoesNotAdoptWrongParentOrStaleSource(t *testing.T) {
	for _, kind := range []string{"wrong-parent", "source-after-ack", "worker-after-ack"} {
		t.Run(kind, func(t *testing.T) {
			f := newClaudeActionFixture(t)
			req := actionRequest(f.catalog(t), "fork", "")
			f.a.actionSDKRun = func(ctx context.Context, sdk *claudeSessionSDK, in claudeSDKActionInput, guard func() error) (claudeSDKActionOutput, bool, error) {
				out, attempted, err := f.mutate(ctx, sdk, in, guard)
				if kind == "wrong-parent" {
					path := filepath.Join(filepath.Dir(in.Path), out.SessionID+".jsonl")
					writeActionFixtureRows(t, path, actionFixtureRows(out.SessionID, in.Cwd, "99999999-9999-4999-8999-999999999999"))
				} else if kind == "source-after-ack" {
					f.key.Store("changed")
				} else {
					f.a.mu.Lock()
					delete(f.a.procs, f.p.id)
					f.a.mu.Unlock()
				}
				return out, attempted, err
			}
			result, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil)
			if err != nil || result.State != "unconfirmed" {
				t.Fatalf("stale fork: %+v %v", result, err)
			}
			id := "22222222-2222-4222-8222-222222222222"
			if f.a.hist.ownedCompletion(id) {
				t.Fatal("unconfirmed fork gained execution provenance")
			}
			f.a.mu.Lock()
			_, live := f.a.live[id]
			f.a.mu.Unlock()
			if live || len(f.rec.all()) != 0 {
				t.Fatal("unconfirmed fork was published")
			}
		})
	}
}

func TestClaudeSessionActionsPermissionGuardCanReenterAgent(t *testing.T) {
	f := newClaudeActionFixture(t)
	guard := func() error {
		f.a.mu.Lock()
		f.a.mu.Unlock()
		f.a.sessionActions.mu.Lock()
		f.a.sessionActions.mu.Unlock()
		return nil
	}
	view, err := f.a.SessionActions(context.Background(), f.p.id, guard)
	if err != nil {
		t.Fatal(err)
	}
	f.a.sink = func(e protocol.Event) {
		if _, err := f.a.SessionActions(context.Background(), f.p.id, guard); err != nil {
			t.Error(err)
		}
	}
	if result, err := f.a.ApplySessionAction(context.Background(), f.p.id, actionRequest(view, "rename", "New"), guard); err != nil || result.State != "completed" {
		t.Fatalf("reentrant guard: %+v %v", result, err)
	}
}

func TestClaudeSessionActionsPendingRetryWaitsForSameReceipt(t *testing.T) {
	f := newClaudeActionFixture(t)
	req := actionRequest(f.catalog(t), "rename", "New")
	entered, release := make(chan struct{}), make(chan struct{})
	f.a.actionSDKRun = func(ctx context.Context, sdk *claudeSessionSDK, in claudeSDKActionInput, guard func() error) (claudeSDKActionOutput, bool, error) {
		close(entered)
		<-release
		return f.mutate(ctx, sdk, in, guard)
	}
	finished := make(chan protocol.SessionActionResult, 1)
	go func() {
		result, _ := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil)
		finished <- result
	}()
	<-entered
	pending, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil)
	if err != nil || pending.State != "unconfirmed" {
		t.Fatalf("pending retry: %+v %v", pending, err)
	}
	if view := f.catalog(t); len(view.Actions) != 0 || view.UnavailableReason != "会话正忙，请稍后重试" {
		t.Fatalf("pending catalog: %+v", view)
	}
	close(release)
	if result := <-finished; result.State != "completed" {
		t.Fatalf("operation did not complete: %+v", result)
	}
	final, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, nil)
	if err != nil || final.State != "completed" || f.calls.Load() != 1 {
		t.Fatal("pending retry caused a second native operation")
	}
}

func TestClaudeSessionActionsFinalPublicationGuardCannotGrantForkLease(t *testing.T) {
	f := newClaudeActionFixture(t)
	req := actionRequest(f.catalog(t), "fork", "")
	mutated := false
	postChecks := 0
	f.a.actionSDKRun = func(ctx context.Context, sdk *claudeSessionSDK, in claudeSDKActionInput, guard func() error) (claudeSDKActionOutput, bool, error) {
		out, attempted, err := f.mutate(ctx, sdk, in, guard)
		mutated = true
		return out, attempted, err
	}
	guard := func() error {
		if mutated {
			postChecks++
			if postChecks == 2 {
				f.key.Store("revoked-after-file-verification")
			}
		}
		return nil
	}
	result, err := f.a.ApplySessionAction(context.Background(), f.p.id, req, guard)
	if err != nil || result.State != "unconfirmed" || postChecks < 2 {
		t.Fatalf("late source change: %+v %v checks=%d", result, err, postChecks)
	}
	if f.a.hist.ownedCompletion("22222222-2222-4222-8222-222222222222") {
		t.Fatal("unconfirmed publication left a resumable fork lease")
	}
}

func TestClaudeSessionSDKRejectsUnpinnedManifestAndPreloadEnvironment(t *testing.T) {
	f := newClaudeActionFixture(t)
	pkg := filepath.Join(f.sdk.dir, "package.json")
	if err := os.WriteFile(pkg, []byte(`{"name":"@anthropic-ai/claude-agent-sdk","version":"0.3.301","claudeCodeVersion":"2.1.301"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectClaudeSessionSDK(os.Args[0], f.sdk.dir); err == nil {
		t.Fatal("unverified version accepted")
	}
	t.Setenv("NODE_OPTIONS", "--require=malicious-fixture")
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-secret")
	t.Setenv("CLAUDE_CONFIG_DIR", "ambient")
	env := strings.Join(claudeSDKEnvironment(map[string]string{"CLAUDE_CONFIG_DIR": "owned"}), "\n")
	if strings.Contains(env, "malicious-fixture") || strings.Contains(env, "synthetic-secret") || strings.Contains(env, "CLAUDE_CONFIG_DIR=ambient") || !strings.Contains(env, "CLAUDE_CONFIG_DIR=owned") {
		t.Fatal("helper inherited ambient execution/credential environment")
	}
}

func replaceClaudeSDKBytesPreservingStat(t *testing.T, path string) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		t.Fatalf("read fixture: %v", err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, st.Mode()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil || !sameClaudeTranscript(st, current) {
		t.Fatal("test did not preserve file identity, size and mtime")
	}
}

func TestClaudeSessionSDKIdentityBindsNodeAndRuntimeContents(t *testing.T) {
	for _, kind := range []string{"entry-bytes", "chunk-bytes", "node-replaced"} {
		t.Run(kind, func(t *testing.T) {
			node := filepath.Join(t.TempDir(), "node.exe")
			if err := os.WriteFile(node, []byte("synthetic-node-not-executed"), 0o700); err != nil {
				t.Fatal(err)
			}
			sdk := fixtureClaudeSDK(t, node, "export const fixture = 0;")
			pkg := `{"name":"@anthropic-ai/claude-agent-sdk","version":"0.3.300","claudeCodeVersion":"2.1.300","files":["sdk.mjs","core.mjs"]}`
			if err := os.WriteFile(filepath.Join(sdk.dir, "package.json"), []byte(pkg), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sdk.dir, "core.mjs"), []byte("export const chunk = 0;"), 0o600); err != nil {
				t.Fatal(err)
			}
			sdk, err := inspectClaudeSessionSDK(node, sdk.dir)
			if err != nil || !sdk.unchanged() {
				t.Fatalf("unchanged SDK unavailable: %v", err)
			}
			switch kind {
			case "entry-bytes":
				replaceClaudeSDKBytesPreservingStat(t, filepath.Join(sdk.dir, "sdk.mjs"))
			case "chunk-bytes":
				replaceClaudeSDKBytesPreservingStat(t, filepath.Join(sdk.dir, "core.mjs"))
			case "node-replaced":
				original, err := os.ReadFile(node)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(node, node+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(node, original, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(node, sdk.nodeFile.ModTime(), sdk.nodeFile.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			if sdk.unchanged() {
				t.Fatal("runtime replacement retained original authority")
			}
			current, err := inspectClaudeSessionSDK(node, sdk.dir)
			if err != nil || sameClaudeSessionSDK(sdk, current) {
				t.Fatalf("new discovery retained the catalog identity: %v", err)
			}
		})
	}
}

func TestClaudeSessionSDKRejectsUnverifiableRuntimeInventory(t *testing.T) {
	for _, name := range []string{"../outside.mjs", `outside\chunk.mjs`, "missing.mjs"} {
		t.Run(name, func(t *testing.T) {
			sdk := fixtureClaudeSDK(t, os.Args[0], "export const fixture = 0;")
			pkg, _ := json.Marshal(map[string]any{"name": "@anthropic-ai/claude-agent-sdk", "version": "0.3.300", "claudeCodeVersion": "2.1.300", "files": []string{"sdk.mjs", name}})
			if err := os.WriteFile(filepath.Join(sdk.dir, "package.json"), pkg, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := inspectClaudeSessionSDK(sdk.node, sdk.dir); err == nil {
				t.Fatal("unverifiable runtime inventory accepted")
			}
		})
	}
}

func TestClaudeSessionSDKRunnerImportsBeforeCommitAndHonorsGuard(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node unavailable; synthetic transport tests still run")
	}
	f := newClaudeActionFixture(t)
	// Deliberately fake module: verifies the runner protocol only, not native
	// SDK behavior. Any accidental invocation fails instead of mutating data.
	sdk := fixtureClaudeSDK(t, node, `export function renameSession(){throw Error("must not run")}; export function forkSession(){throw Error("must not run")}; export function getSessionInfo(){throw Error("must not run")};`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, attempted, err := runClaudeSessionSDK(ctx, sdk, claudeSDKActionInput{Mode: "probe"}, nil); err != nil || attempted || out.Phase != "done" {
		t.Fatalf("probe: %+v %v %v", out, attempted, err)
	}
	st, _ := os.Stat(f.path)
	input := claudeSDKActionInput{Mode: "apply", Action: "rename", ID: f.p.id, Name: "New", Home: f.a.home, Cwd: f.cwd, Path: f.path, Project: "fixture-project", Size: fmt.Sprint(st.Size()), Mtime: fmt.Sprint(st.ModTime().UnixNano())}
	denied := errors.New("synthetic revocation")
	if _, attempted, err := runClaudeSessionSDK(ctx, sdk, input, func() error { return denied }); attempted || !errors.Is(err, denied) {
		t.Fatalf("commit crossed revoked guard: %v %v", attempted, err)
	}
	title, _ := readClaudeActionTitle(f.path, f.p.id)
	if title != "" {
		t.Fatal("runner mutated a denied request")
	}
	if _, attempted, err := runClaudeSessionSDK(ctx, sdk, input, func() error {
		replaceClaudeSDKBytesPreservingStat(t, filepath.Join(sdk.dir, "sdk.mjs"))
		return nil
	}); attempted || !errors.Is(err, errClaudeSDKUnavailable) {
		t.Fatalf("commit crossed SDK content replacement: %v %v", attempted, err)
	}
}

func TestClaudeSessionSDKInstalledProbe(t *testing.T) {
	if os.Getenv("SALCARA_TEST_INSTALLED_CLAUDE_SDK") != "1" {
		t.Skip("optional installed-SDK probe; no installation or account use")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	InvalidateClaudeSessionSDKCache()
	if status := DetectClaudeSessionSDK(ctx); !status.Available {
		t.Fatalf("installed SDK probe: %+v", status)
	}
}
