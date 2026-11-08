package hubclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const projectPreviewFixtureID = "11111111-2222-4333-8444-555555555555"

// A real local Node process and HTTP child exercise argv, readiness, pipe
// capture and process-tree termination without installing any dependencies.
const projectPreviewNodeFixture = `
const {spawn}=require('child_process');
const args=process.argv.slice(2);
const option=(names)=>{for(const name of names){const i=args.indexOf(name);if(i>=0)return args[i+1]}};
const host=option(['--host','--hostname']);
const port=Number(option(['--port']));
console.log('fixture-parent '+process.pid+' '+JSON.stringify(args));
const child=spawn(process.execPath,['-e',
 'require("http").createServer((q,r)=>r.end("fixture child; credential="+String(process.env.SALCARA_TEST_CREDENTIAL))).listen('+port+','+JSON.stringify(host)+',()=>console.log("fixture-ready"));setInterval(()=>{},1000);'
],{stdio:'inherit'});
setInterval(()=>{},1000);
`

func projectPreviewFixture(t *testing.T, framework string) (*Client, string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("Node.js unavailable")
	}
	c, _, root := workspaceFixture(t)
	t.Cleanup(c.closeWorkspaceProjectPreviews)
	manifest := `{"name":"fixture-web","scripts":{"dev":"vite --host 0.0.0.0 --port 9000","unsafe":"vite && echo unsafe"}}`
	entry := "node_modules/vite/bin/vite.js"
	if framework == "next" {
		manifest = `{"name":"fixture-web","scripts":{"dev":"next dev --hostname 0.0.0.0 --port 9000"}}`
		entry = "node_modules/next/dist/bin/next"
	}
	workspaceWrite(t, root, "website/package.json", []byte(manifest))
	workspaceWrite(t, root, "website/src/App.tsx", []byte("export default function App(){return null}"))
	file := filepath.Join(root, "website", filepath.FromSlash(entry))
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(projectPreviewNodeFixture), 0600); err != nil {
		t.Fatal(err)
	}
	return c, root
}
func projectPreviewCommand(root, action string) map[string]any {
	cmd := map[string]any{"type": "workspace.project.preview", "sessionKey": workspaceTestKey, "controlSurface": "cli", "expectedCwd": root, "action": action}
	if action == "inspect" {
		cmd["sourcePath"] = "website/src/App.tsx"
	} else {
		cmd["projectPath"] = "website"
	}
	return cmd
}
func projectPreviewInspectFixture(t *testing.T, c *Client, root string) projectPreviewInspection {
	t.Helper()
	result, err := c.Dispatch(context.Background(), projectPreviewCommand(root, "inspect"))
	if err != nil {
		t.Fatal(err)
	}
	return result.(projectPreviewInspection)
}
func projectPreviewStartFixture(t *testing.T, c *Client, root string) (map[string]any, projectPreviewState) {
	t.Helper()
	inspection := projectPreviewInspectFixture(t, c, root)
	cmd := projectPreviewCommand(root, "start")
	cmd["script"], cmd["manifestRevision"], cmd["operationId"] = "dev", inspection.ManifestRevision, projectPreviewFixtureID
	result, err := c.Dispatch(context.Background(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	reply := result.(projectPreviewResult)
	if !reply.Found || reply.Run == nil {
		t.Fatal(reply)
	}
	return cmd, *reply.Run
}
func projectPreviewWait(t *testing.T, c *Client, root string, condition func(projectPreviewState) bool) projectPreviewState {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	var last projectPreviewState
	for time.Now().Before(deadline) {
		cmd := projectPreviewCommand(root, "status")
		cmd["operationId"] = projectPreviewFixtureID
		result, err := c.Dispatch(context.Background(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		reply := result.(projectPreviewResult)
		if reply.Run != nil {
			last = *reply.Run
			if condition(last) {
				return last
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("project state did not arrive: %+v", last)
	return last
}

func TestProjectPreviewRealNodeLifecycleAndReconnect(t *testing.T) {
	for _, framework := range []string{"vite", "next"} {
		t.Run(framework, func(t *testing.T) {
			t.Setenv("SALCARA_TEST_CREDENTIAL", "must-not-inherit")
			c, root := projectPreviewFixture(t, framework)
			inspection := projectPreviewInspectFixture(t, c, root)
			if inspection.ProjectPath != "website" || !inspection.Ready || !inspection.DependenciesInstalled || inspection.Run != nil {
				t.Fatalf("inspect: %+v", inspection)
			}
			if len(c.projectPreviews) != 0 {
				t.Fatal("inspection started a process")
			}
			cmd, initial := projectPreviewStartFixture(t, c, root)
			if initial.Status != "starting" || initial.URL != "" {
				t.Fatalf("premature ready: %+v", initial)
			}
			ready := projectPreviewWait(t, c, root, func(s projectPreviewState) bool {
				return s.Status == "running" && strings.Contains(s.Logs, "fixture-ready")
			})
			if !strings.HasPrefix(ready.URL, "http://127.0.0.1:") || strings.Contains(ready.Logs, "0.0.0.0") || strings.Contains(ready.Logs, "9000") {
				t.Fatal("script overrode loopback/allocated port", ready)
			}
			client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}
			response, err := client.Get(ready.URL)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if string(body) != "fixture child; credential=undefined" {
				t.Fatal(string(body))
			}
			duplicate, err := c.Dispatch(context.Background(), cmd)
			if err != nil || duplicate.(projectPreviewResult).Run.RunID != initial.RunID || len(c.projectPreviews) != 1 {
				t.Fatal("lost-reply retry created a process", err)
			}
			inspection = projectPreviewInspectFixture(t, c, root)
			if inspection.Run == nil || inspection.Run.RunID != initial.RunID {
				t.Fatal("inspect did not recover active run")
			}
			status := projectPreviewCommand(root, "status")
			status["operationId"], status["cursor"] = projectPreviewFixtureID, ready.Cursor
			result, err := c.Dispatch(context.Background(), status)
			if err != nil || result.(projectPreviewResult).Run.Logs != "" {
				t.Fatal("log cursor replayed old output", err)
			}
			stop := projectPreviewCommand(root, "stop")
			stop["runId"] = initial.RunID
			for i := 0; i < 2; i++ {
				result, err := c.Dispatch(context.Background(), stop)
				if err != nil || result.(projectPreviewResult).Run.Status != "stopped" {
					t.Fatal("stop is not idempotent", err)
				}
			}
			projectPreviewWait(t, c, root, func(s projectPreviewState) bool { return s.ExitCode != nil })
			if response, err := client.Get(ready.URL); err == nil {
				response.Body.Close()
				t.Fatal("stop left HTTP child alive")
			}
			duplicate, err = c.Dispatch(context.Background(), cmd)
			if err != nil || duplicate.(projectPreviewResult).Run.Status != "stopped" {
				t.Fatal("duplicate start restarted stopped process", err)
			}
		})
	}
}

func TestProjectPreviewAdmissionAndMissingRecovery(t *testing.T) {
	c, root := projectPreviewFixture(t, "vite")
	inspection := projectPreviewInspectFixture(t, c, root)
	for _, s := range inspection.Scripts {
		if s.Name == "unsafe" && s.Supported {
			t.Fatal("compound shell script supported")
		}
	}
	status := projectPreviewCommand(root, "status")
	status["operationId"] = projectPreviewFixtureID
	result, err := c.Dispatch(context.Background(), status)
	if err != nil || result.(projectPreviewResult).Found {
		t.Fatal("missing operation did not return found:false", err)
	}
	cmd := projectPreviewCommand(root, "start")
	cmd["operationId"], cmd["script"], cmd["manifestRevision"] = projectPreviewFixtureID, "dev", "stale"
	if _, err = c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("stale manifest ran")
	}
	cmd["manifestRevision"], cmd["script"] = inspection.ManifestRevision, "unsafe"
	if _, err = c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("unsupported script ran")
	}
	cmd, started := projectPreviewStartFixture(t, c, root)
	cmd["script"] = "unsafe"
	if _, err = c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("operation ID changed command")
	}
	cmd["script"], cmd["operationId"] = "dev", "22222222-2222-4333-8444-555555555555"
	if _, err = c.Dispatch(context.Background(), cmd); err == nil {
		t.Fatal("second live project admitted in same scope")
	}
	status["runId"] = started.RunID
	status["operationId"] = "33333333-2222-4333-8444-555555555555"
	if _, err = c.Dispatch(context.Background(), status); err == nil {
		t.Fatal("mismatched operation and run IDs accepted")
	}
	for _, source := range []string{"../outside.tsx", "/absolute", "website/../package.json"} {
		cmd := projectPreviewCommand(root, "inspect")
		cmd["sourcePath"] = source
		if _, err = c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatal("unsafe project path accepted", source)
		}
	}
}

func TestProjectPreviewStopsOnIdleAndProjectRevocation(t *testing.T) {
	for _, scenario := range []string{"idle", "project-removed"} {
		t.Run(scenario, func(t *testing.T) {
			c, root := projectPreviewFixture(t, "vite")
			_, initial := projectPreviewStartFixture(t, c, root)
			projectPreviewWait(t, c, root, func(s projectPreviewState) bool { return s.Status == "running" })
			run := c.projectPreviews[initial.RunID]
			if scenario == "idle" {
				run.mu.Lock()
				run.touched = time.Now().Add(-projectPreviewIdleTTL - time.Minute)
				run.mu.Unlock()
			} else {
				if err := c.o.Store.Update(func(cfg *config.Config) error { cfg.Projects = nil; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-run.done:
			case <-time.After(5 * time.Second):
				t.Fatal("abandoned/revoked project survived")
			}
			run.mu.Lock()
			state := run.status
			run.mu.Unlock()
			if state != "stopped" {
				t.Fatal(state)
			}
		})
	}
}

func TestProjectPreviewFailureAndBoundedLogs(t *testing.T) {
	c, root := projectPreviewFixture(t, "vite")
	cli := filepath.Join(root, "website", "node_modules", "vite", "bin", "vite.js")
	if err := os.WriteFile(cli, []byte("console.log('fixture failure');process.exit(7)"), 0600); err != nil {
		t.Fatal(err)
	}
	projectPreviewStartFixture(t, c, root)
	failed := projectPreviewWait(t, c, root, func(s projectPreviewState) bool { return s.Status == "failed" && s.ExitCode != nil })
	if *failed.ExitCode != 7 || !strings.Contains(failed.Logs, "fixture failure") || failed.URL != "" {
		t.Fatal(failed)
	}
	run := &workspaceProjectPreview{status: "running", done: make(chan struct{})}
	run.append([]byte(strings.Repeat("x", projectPreviewLogLimit*2)))
	state := run.snapshot(0)
	if len(state.Logs) != projectPreviewLogLimit || !state.Truncated || state.Cursor != projectPreviewLogLimit*2 {
		t.Fatal("log retention is not bounded")
	}
	encoded, err := json.Marshal(state)
	if err != nil || len(encoded) > projectPreviewLogLimit+1024 {
		t.Fatal("unbounded response", err)
	}
	utf8Run := &workspaceProjectPreview{status: "running", done: make(chan struct{})}
	characters := []byte("中文")
	utf8Run.append(characters[:2])
	partial := utf8Run.snapshot(0)
	if partial.Logs != "" || partial.Cursor != 0 {
		t.Fatal("partial UTF-8 rune consumed")
	}
	utf8Run.append(characters[2:])
	complete := utf8Run.snapshot(partial.Cursor)
	if complete.Logs != "中文" || complete.Cursor != int64(len(characters)) {
		t.Fatal("UTF-8 log was corrupted")
	}
}

func TestProjectPreviewScriptGrammar(t *testing.T) {
	for _, command := range []string{"vite", "vite --mode development --host 0.0.0.0", "next dev --turbo -H localhost -p 3000"} {
		if _, _, ok := projectPreviewArgs(command); !ok {
			t.Fatal("valid script rejected", command)
		}
	}
	for _, command := range []string{"npx vite", "vite & echo unsafe", "vite --config ../outside.js", "vite --open", "next start", "next dev ../outside", "TOKEN=secret vite", "vite --config /outside.js", "vite\nnode other.js"} {
		if _, _, ok := projectPreviewArgs(command); ok {
			t.Fatal("unsupported script admitted", command)
		}
	}
}

type projectPreviewRevocableAgent struct {
	*fakeAgent
	root    string
	enabled atomic.Bool
}

func (a *projectPreviewRevocableAgent) SessionMetadata(context.Context, string) (protocol.SessionInfo, error) {
	if !a.enabled.Load() {
		return protocol.SessionInfo{}, context.Canceled
	}
	return protocol.SessionInfo{SessionKey: workspaceTestKey, Cwd: a.root, ControlSurface: "cli"}, nil
}
func TestProjectPreviewRetiresChangedExecutionSource(t *testing.T) {
	for _, scenario := range []string{"api-selection", "model", "agent-instance", "session-revoked"} {
		t.Run(scenario, func(t *testing.T) {
			c, root := projectPreviewFixture(t, "vite")
			adapter := &projectPreviewRevocableAgent{fakeAgent: &fakeAgent{id: "codex"}, root: root}
			adapter.enabled.Store(true)
			c.SetManager(&fakeManager{list: []agents.Agent{adapter}})
			if err := c.o.Store.Update(func(cfg *config.Config) error {
				cfg.LocalAccounts = []config.LocalAccount{{ID: "preview-a", Kind: "codex", BaseURL: "https://a.invalid", Key: "synthetic-a"}, {ID: "preview-b", Kind: "codex", BaseURL: "https://b.invalid", Key: "synthetic-b"}}
				cfg.RemoteAPI = map[string]config.RemoteAPI{"codex": {AccountID: "preview-a", Model: "model-a"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			_, initial := projectPreviewStartFixture(t, c, root)
			projectPreviewWait(t, c, root, func(s projectPreviewState) bool { return s.Status == "running" })
			run := c.projectPreviews[initial.RunID]
			before := c.o.Store.Get().UsageSourceRevision
			switch scenario {
			case "api-selection":
				if err := c.o.Store.Update(func(cfg *config.Config) error {
					cfg.RemoteAPI["codex"] = config.RemoteAPI{AccountID: "preview-b", Model: "model-b"}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "model":
				if err := c.o.Store.Update(func(cfg *config.Config) error {
					choice := cfg.RemoteAPI["codex"]
					choice.Model = "changed-model"
					cfg.RemoteAPI["codex"] = choice
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "agent-instance":
				replacement := &projectPreviewRevocableAgent{fakeAgent: &fakeAgent{id: "codex"}, root: root}
				replacement.enabled.Store(true)
				c.SetManager(&fakeManager{list: []agents.Agent{replacement}})
			case "session-revoked":
				adapter.enabled.Store(false)
			}
			if c.o.Store.Get().UsageSourceRevision != before {
				t.Fatal("fixture changed UsageSourceRevision instead of exercising execution identity")
			}
			select {
			case <-run.done:
			case <-time.After(5 * time.Second):
				t.Fatal("changed execution/session scope left process alive")
			}
			run.mu.Lock()
			status := run.status
			run.mu.Unlock()
			if status != "stopped" {
				t.Fatal(status)
			}
		})
	}
}
func TestProjectPreviewSurvivesSameBindingTransportReconnect(t *testing.T) {
	c, root := projectPreviewFixture(t, "vite")
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.PhoneBindingID = "fixture-binding"
		cfg.PhoneHash = strings.Repeat("a", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	c.publishPair(PairStatus{}, eventIdentity(c.o.Store.Get()))
	_, initial := projectPreviewStartFixture(t, c, root)
	projectPreviewWait(t, c, root, func(s projectPreviewState) bool { return s.Status == "running" })
	run := c.projectPreviews[initial.RunID]
	c.Kick()
	select {
	case <-run.done:
		t.Fatal("temporary upload authorization gap killed preview")
	case <-time.After(1500 * time.Millisecond):
	}
	state := run.snapshot(0)
	if state.Status != "running" || state.RunID != initial.RunID {
		t.Fatal("same-binding reconnect lost running process")
	}
}
