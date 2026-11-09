package hubclient

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type projectlessAgent struct {
	*aggregationFixtureAgent
	fail        bool
	beforeStart func()
	history     []protocol.Event
}

func (a *projectlessAgent) Start(ctx context.Context, cwd, prompt, model, approval string) (string, error) {
	if a.beforeStart != nil {
		a.beforeStart()
	}
	if a.fail {
		return "", errors.New("synthetic uncertain native exit")
	}
	a.started = append(a.started, cwd)
	key := fmt.Sprintf("%s:independent-%d", a.ID(), len(a.started))
	a.sessions = append(a.sessions, protocol.SessionInfo{SessionKey: key, Tool: a.ID(), Cwd: cwd, ControlSurface: "cli", Status: "idle"})
	return strings.TrimPrefix(key, a.ID()+":"), nil
}
func (a *projectlessAgent) SessionMetadata(ctx context.Context, id string) (protocol.SessionInfo, error) {
	for _, s := range a.sessions {
		if s.SessionKey == a.ID()+":"+id {
			return s, nil
		}
	}
	return protocol.SessionInfo{}, errors.New("missing session")
}
func (a *projectlessAgent) Open(ctx context.Context, id string) (protocol.SessionInfo, []protocol.Event, error) {
	info, e := a.SessionMetadata(ctx, id)
	return info, a.history, e
}
func projectlessFixture(t *testing.T) (*Client, *projectlessAgent, context.Context) {
	t.Helper()
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("verified filesystem handles unavailable")
	}
	c, base, _ := aggregationFixture(t, "hermes")
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	a := &projectlessAgent{aggregationFixtureAgent: base}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	return c, a, WithLocalExecution(context.Background())
}
func projectlessCommand() map[string]any {
	return map[string]any{"type": "session.start", "controlSurface": "cli", "tool": "hermes", "cwd": "", "projectless": true, "prompt": "write a note", "workspaceMode": "work", "operationId": "e6da529b-aa90-4a36-a949-a121984da31f"}
}

func TestProjectlessStartsWithoutAnyProjectAndPersists(t *testing.T) {
	c, a, ctx := projectlessFixture(t)
	cmd := projectlessCommand()
	result, err := c.Dispatch(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	key := result.(map[string]any)["sessionKey"].(string)
	cwd := a.started[0]
	if len(c.o.Store.Get().Projects) != 0 || !c.isManagedCwd(cwd) {
		t.Fatal("managed workspace leaked as a project", cwd)
	}
	if err = os.WriteFile(filepath.Join(cwd, "notes.md"), []byte("persistent work"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"sessions.list", "session.open"} {
		command := map[string]any{"type": typ, "tool": "hermes", "controlSurface": "cli"}
		if typ == "session.open" {
			command["sessionKey"] = key
		}
		result, err = c.Dispatch(ctx, command)
		if err != nil {
			t.Fatal(err)
		}
		page := result.(map[string]any)
		info := protocol.SessionInfo{}
		if typ == "session.open" {
			info = page["session"].(protocol.SessionInfo)
		} else {
			info = page["sessions"].([]protocol.SessionInfo)[0]
		}
		if !info.Projectless || info.Cwd != cwd || info.WorkspaceMode != "work" {
			t.Fatal("lost managed metadata", info)
		}
	}
	event := c.ProjectSessionEvent(protocol.Event{Type: "session.updated", Session: &a.sessions[0]})
	if !event.Session.Projectless || a.sessions[0].Projectless {
		t.Fatal("event projection lost flag or mutated adapter")
	}
	result, err = c.Dispatch(ctx, map[string]any{"type": "projects.list"})
	if err != nil || len(result.(map[string]any)["projects"].([]protocol.Project)) != 0 {
		t.Fatal("managed path leaked in picker", result, err)
	}
	restarted := New(Options{Store: c.o.Store})
	restarted.SetManager(&fakeManager{list: []agents.Agent{a}})
	result, err = restarted.Dispatch(ctx, cmd)
	if err != nil || result.(map[string]any)["sessionKey"] != key || len(a.started) != 1 {
		t.Fatal("restart duplicated accepted operation", result, err, a.started)
	}
	read := map[string]any{"type": "workspace.files.read", "controlSurface": "cli", "sessionKey": key, "expectedCwd": cwd, "path": "notes.md"}
	result, err = restarted.Dispatch(ctx, read)
	if err != nil || result.(workspaceFileText).Text != "persistent work" {
		t.Fatal(result, err)
	}
	if _, err = restarted.Dispatch(ctx, map[string]any{"type": "session.send", "controlSurface": "cli", "sessionKey": key, "text": "continue"}); err != nil {
		t.Fatal("continued session failed", err)
	}
}

func TestProjectlessFieldsFailClosed(t *testing.T) {
	for _, change := range []map[string]any{{"projectless": false}, {"projectless": nil}, {"projectless": "true"}, {"cwd": "C:/other"}, {"cwd": nil}, {"controlSurface": "desktop"}, {"operationId": "../a"}, {"workspaceMode": ""}, {"type": "session.send"}, {"entrypoint": "claude-desktop"}} {
		c, a, ctx := projectlessFixture(t)
		cmd := projectlessCommand()
		for k, v := range change {
			cmd[k] = v
		}
		if _, err := c.Dispatch(ctx, cmd); err == nil {
			t.Fatal("accepted invalid command", change)
		}
		if len(a.started) > 0 {
			t.Fatal("invalid command reached adapter")
		}
	}
	c, a, ctx := projectlessFixture(t)
	cmd := projectlessCommand()
	delete(cmd, "projectless")
	if _, err := c.Dispatch(ctx, cmd); err == nil || len(a.started) > 0 {
		t.Fatal("ordinary empty cwd became projectless", err)
	}
}

func TestProjectlessCannotAuthorizeAnotherSessionOrProject(t *testing.T) {
	c, a, ctx := projectlessFixture(t)
	cmd := projectlessCommand()
	if _, err := c.Dispatch(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	cwd, key := a.started[0], a.sessions[0].SessionKey
	plain := projectlessCommand()
	delete(plain, "projectless")
	plain["cwd"] = cwd
	plain["operationId"] = config.NewUUID()
	if _, err := c.Dispatch(ctx, plain); err == nil {
		t.Fatal("managed cwd became reusable global grant")
	}
	if _, err := CheckCwd(cwd, c.o.Store.Get().Projects); err == nil {
		t.Fatal("managed cwd in global allowlist")
	}
	if err := os.WriteFile(filepath.Join(cwd, "note.md"), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	a.sessions = append(a.sessions, protocol.SessionInfo{SessionKey: "hermes:other", Cwd: cwd, ControlSurface: "cli"})
	if _, err := c.Dispatch(ctx, map[string]any{"type": "session.send", "controlSurface": "cli", "sessionKey": "hermes:other", "text": "must not adopt directory"}); err == nil {
		t.Fatal("unrecorded native session adopted managed cwd")
	}
	for _, session := range []string{"hermes:other", key} {
		command := map[string]any{"type": "workspace.files.read", "controlSurface": "cli", "sessionKey": session, "expectedCwd": cwd, "path": "note.md"}
		if session == key {
			if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.ComputerID = "another-computer"; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := c.Dispatch(ctx, command); err == nil {
			t.Fatal("another session or computer read managed files", session)
		}
	}
}

func TestProjectlessOperationFingerprintAndUncertainty(t *testing.T) {
	c, a, ctx := projectlessFixture(t)
	cmd := projectlessCommand()
	if _, err := c.Dispatch(ctx, cmd); err != nil {
		t.Fatal(err)
	}
	cmd["prompt"] = "different content"
	if _, err := c.Dispatch(ctx, cmd); err == nil || len(a.started) != 1 {
		t.Fatal("same UUID changed payload", err)
	}
	c2, a2, ctx2 := projectlessFixture(t)
	a2.fail = true
	cmd = projectlessCommand()
	if _, err := c2.Dispatch(ctx2, cmd); err == nil {
		t.Fatal("synthetic failure ignored")
	}
	a2.fail = false
	if _, err := c2.Dispatch(ctx2, cmd); err == nil || len(a2.started) > 0 {
		t.Fatal("unconfirmed native start repeated", err)
	}
}

func TestProjectlessIdentityIsolationAndCancellation(t *testing.T) {
	c, a, ctx := projectlessFixture(t)
	for _, mode := range []string{"code", "work"} {
		cmd := projectlessCommand()
		cmd["workspaceMode"] = mode
		if _, err := c.Dispatch(ctx, cmd); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.started) != 2 || a.started[0] == a.started[1] {
		t.Fatal("mode collision", a.started)
	}
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.DeviceID = "another-device"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Dispatch(ctx, projectlessCommand()); err != nil {
		t.Fatal(err)
	}
	if a.started[1] == a.started[2] {
		t.Fatal("computer identity collision")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	cmd := projectlessCommand()
	cmd["operationId"] = config.NewUUID()
	if _, err := c.Dispatch(cancelled, cmd); err == nil || len(a.started) != 3 {
		t.Fatal("cancelled start executed", err)
	}
}

func TestProjectlessDirectPairingRecoveryAndRevocation(t *testing.T) {
	c, a, _ := projectlessFixture(t)
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.DirectRelayEnabled = true
		cfg.DirectPhoneID = "phone-a"
		cfg.DirectPhoneHash = "phone-a-secret"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	valid := true
	ctx := WithDirectExecutionValidator(context.Background(), "lease-a", func() error {
		if !valid {
			return errors.New("revoked")
		}
		return nil
	})
	cmd := projectlessCommand()
	result, err := c.Dispatch(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	key := result.(map[string]any)["sessionKey"]
	if replay, err := c.Dispatch(ctx, cmd); err != nil || replay.(map[string]any)["sessionKey"] != key || len(a.started) != 1 {
		t.Fatal("direct replay duplicated", replay, err)
	}
	if _, err := c.Dispatch(WithLocalExecution(context.Background()), map[string]any{"type": "session.send", "sessionKey": key, "controlSurface": "cli", "text": "continue from the computer"}); err != nil {
		t.Fatal("local owner lost its own conversation", err)
	}
	valid = false
	cmd["operationId"] = config.NewUUID()
	if _, err := c.Dispatch(ctx, cmd); err == nil || len(a.started) != 1 {
		t.Fatal("revoked direct lease executed", err)
	}
}

func TestProjectlessDoesNotAdoptOrReplaceExistingDirectory(t *testing.T) {
	c, _, ctx := projectlessFixture(t)
	cmd := projectlessCommand()
	cfg := c.o.Store.Get()
	key := durableHash([]string{projectlessOwner(cfg, "local"), "hermes", "work", str(cmd, "operationId")})
	base, _, err := c.projectlessPaths()
	if err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(base, key)
	if err = os.MkdirAll(existing, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(existing, "keep.md")
	if err = os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Dispatch(ctx, cmd); err == nil {
		t.Fatal("orphaned folder adopted")
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatal("existing work overwritten")
	}
}

func TestProjectlessRejectsReplacedDirectoryAfterRestart(t *testing.T) {
	c, a, ctx := projectlessFixture(t)
	if _, err := c.Dispatch(ctx, projectlessCommand()); err != nil {
		t.Fatal(err)
	}
	cwd, key := a.started[0], a.sessions[0].SessionKey
	// All implementation handles are closed here, so Windows can exercise the
	// same persisted-identity attack as Linux without an OS-lock-based skip.
	if err := os.Rename(cwd, cwd+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "note.md"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := New(Options{Store: c.o.Store})
	restarted.SetManager(&fakeManager{list: []agents.Agent{a}})
	for _, cmd := range []map[string]any{projectlessCommand(), {"type": "session.send", "controlSurface": "cli", "sessionKey": key, "text": "must reject"}, {"type": "workspace.files.read", "controlSurface": "cli", "sessionKey": key, "expectedCwd": cwd, "path": "note.md"}} {
		if _, err := restarted.Dispatch(ctx, cmd); err == nil {
			t.Fatal("replacement directory accepted", cmd["type"])
		}
	}
	if len(a.started) != 1 || len(a.sent) != 0 {
		t.Fatal("replacement executed")
	}
}

func TestProjectlessHubGenerationIsolatedAndRevokedBeforeStart(t *testing.T) {
	c, a, _ := projectlessFixture(t)
	cfg := c.o.Store.Get()
	if err := c.acceptEventScope(cfg, "generation-one", true); err != nil {
		t.Fatal(err)
	}
	guard := commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID, bindingID: cfg.PhoneBindingID, phoneHash: cfg.PhoneHash, eventScope: "generation-one"}
	ctx := withCommandGuard(context.Background(), guard)
	if _, err := c.Dispatch(ctx, projectlessCommand()); err != nil {
		t.Fatal(err)
	}
	first := a.started[0]
	if err := c.acceptEventScope(cfg, "generation-two", true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Dispatch(ctx, projectlessCommand()); err == nil {
		t.Fatal("old Hub generation executed")
	}
	guard.eventScope = "generation-two"
	ctx = withCommandGuard(context.Background(), guard)
	if _, err := c.Dispatch(ctx, projectlessCommand()); err != nil {
		t.Fatal(err)
	}
	if len(a.started) != 2 || a.started[1] == first {
		t.Fatal("Hub generation reused workspace")
	}
}

func TestProjectlessWriteOnlyExactOwnedSession(t *testing.T) {
	c, a, ctx := projectlessFixture(t)
	if _, err := c.Dispatch(ctx, projectlessCommand()); err != nil {
		t.Fatal(err)
	}
	cwd, key := a.started[0], a.sessions[0].SessionKey
	file := filepath.Join(cwd, "note.md")
	if err := os.WriteFile(file, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	write := map[string]any{"type": "workspace.files.write", "controlSurface": "cli", "sessionKey": key, "expectedCwd": cwd, "path": "note.md", "text": "after", "expectedRevision": workspaceFileRevision([]byte("before"))}
	if _, err := c.Dispatch(ctx, write); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "after" {
		t.Fatal("owned write failed", err)
	}
	write["text"] = "overwrite"
	if _, err := c.Dispatch(ctx, write); err == nil {
		t.Fatal("stale revision overwritten")
	}
	if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.ComputerID = "revoked-owner"; return nil }); err != nil {
		t.Fatal(err)
	}
	write["expectedRevision"] = workspaceFileRevision([]byte("after"))
	if _, err := c.Dispatch(ctx, write); err == nil {
		t.Fatal("revoked owner wrote")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "after" {
		t.Fatal("revoked write changed content")
	}
}

func TestProjectlessRecoversNativeCreationBeforeBindingWithoutReplay(t *testing.T) {
	c, a, ctx := projectlessFixture(t)
	cmd := projectlessCommand()
	cwd, recordKey, _, err := c.prepareProjectless(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.markProjectlessStarting(ctx, recordKey); err != nil {
		t.Fatal(err)
	}
	// Native accepted the first turn, then the Bridge died before saving its key.
	id, err := a.Start(ctx, cwd, "original", "", "ask")
	if err != nil {
		t.Fatal(err)
	}
	key := "hermes:" + id
	restarted := New(Options{Store: c.o.Store})
	restarted.SetManager(&fakeManager{list: []agents.Agent{a}})
	list := map[string]any{"type": "sessions.list", "tool": "hermes", "controlSurface": "cli"}
	result, err := restarted.Dispatch(ctx, list)
	if err != nil || result.(map[string]any)["sessions"].([]protocol.SessionInfo)[0].Projectless {
		t.Fatal("guessed association without original turn", result, err)
	}
	a.history = []protocol.Event{{Type: "message", Role: "user", ClientID: str(cmd, "operationId"), SessionKey: key, Text: "original"}}
	result, err = restarted.Dispatch(ctx, list)
	if err != nil || !result.(map[string]any)["sessions"].([]protocol.SessionInfo)[0].Projectless {
		t.Fatal("original operation did not recover", result, err)
	}
	result, err = restarted.Dispatch(ctx, cmd)
	if err != nil || result.(map[string]any)["sessionKey"] != key || len(a.started) != 1 {
		t.Fatal("recovery replayed native task", result, err)
	}
}

func TestProjectlessRepairsOnlyExactCrashedDurableReceipt(t *testing.T) {
	for _, route := range []string{"direct", "hub"} {
		for _, change := range []string{"unchanged", "prompt", "source", "missing-marker"} {
			t.Run(route+"/"+change, func(t *testing.T) {
				c, a, _ := projectlessFixture(t)
				var ctx context.Context
				if route == "direct" {
					if err := c.o.Store.Update(func(cfg *config.Config) error {
						cfg.DirectRelayEnabled = true
						cfg.DirectPhoneID = "phone-a"
						cfg.DirectPhoneHash = "phone-secret"
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					ctx = WithDirectExecutionValidator(context.Background(), "live-lease", func() error { return nil })
				} else {
					cfg := c.o.Store.Get()
					ctx = withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID})
				}
				cmd := projectlessCommand()
				cfg := executionConfig(ctx, c.o.Store.Get())
				finish, replayed, _, err := c.beginDurableExecution(ctx, cfg, cmd)
				if err != nil || replayed || finish == nil {
					t.Fatal("no durable claim", err)
				}
				cwd, recordKey, _, err := c.prepareProjectless(ctx, cmd)
				if err != nil {
					t.Fatal(err)
				}
				if err = c.markProjectlessStarting(ctx, recordKey); err != nil {
					t.Fatal(err)
				}
				id, err := a.Start(ctx, cwd, "original", "", "ask")
				if err != nil {
					t.Fatal(err)
				}
				key := "hermes:" + id
				if change != "missing-marker" {
					a.history = []protocol.Event{{Type: "message", Role: "user", ClientID: str(cmd, "operationId"), SessionKey: key, Text: "original"}}
				}
				// Discard finish and the Client, exactly as a process kill would.
				restarted := New(Options{Store: c.o.Store})
				restarted.SetManager(&fakeManager{list: []agents.Agent{a}})
				if change == "prompt" {
					cmd["prompt"] = "changed prompt"
				}
				if change == "source" {
					if err = c.o.Store.Update(func(cfg *config.Config) error {
						cfg.ToolModels = map[string]string{"hermes": "changed-model-source"}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				}
				result, err := restarted.Dispatch(ctx, cmd)
				if change == "unchanged" {
					if err != nil || result.(map[string]any)["sessionKey"] != key {
						t.Fatal("exact receipt not recovered", result, err)
					}
					if replay, err := restarted.Dispatch(ctx, cmd); err != nil || replay.(map[string]any)["sessionKey"] != key {
						t.Fatal("repaired receipt not durable", replay, err)
					}
				} else if err == nil {
					t.Fatal("changed or unproved claim was recovered", result)
				}
				if len(a.started) != 1 {
					t.Fatal("recovery dispatched another native task", a.started)
				}
			})
		}
	}
}
