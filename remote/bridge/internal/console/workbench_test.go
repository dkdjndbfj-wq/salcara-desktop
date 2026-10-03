package console

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
	"salcara/bridge/internal/launcher"
	"salcara/bridge/internal/protocol"
)

func workbenchConfig() (config.Config, []launcher.Tool) {
	a := config.LocalAccount{ID: "cx", Kind: "codex", Key: "cx-key", BaseURL: "https://api.test", Model: "model"}
	b := config.LocalAccount{ID: "cl", Kind: "claude", Key: "cl-key", BaseURL: "https://api.test", Model: "model"}
	c := config.Config{RelayRoot: "https://hub.test", AccountKey: "hub-key", LocalAccounts: []config.LocalAccount{a, b}, ActiveCodexAccount: a.ID, ActiveClaudeAccount: b.ID}
	return c, []launcher.Tool{{ID: "codex-desktop", Kind: "codex", Available: true}, {ID: "codex", Kind: "codex", Available: true}, {ID: "claude-desktop", Kind: "claude", Available: true}, {ID: "claude", Kind: "claude", Available: true}}
}

func TestRemoteLampFollowsHubAndNeedsNoAPISelection(t *testing.T) {
	c, tools := workbenchConfig()
	if b := toolBindings(c, tools, hubclient.StateConnected)["codex"]; !b.Pending || b.Remote.State != "connected" {
		t.Fatal("remote tasks use the tool's own login; an unapplied API must not block them")
	}
	for _, tc := range []struct{ hub, want string }{{hubclient.StateConnecting, "connecting"}, {hubclient.StateInvalidKey, "error"}} {
		if b := toolBindings(c, tools, tc.hub)["codex"]; b.Remote.State != tc.want {
			t.Fatal("incorrect Hub state")
		}
	}
	c.AccountKey = ""
	if b := toolBindings(c, tools, hubclient.StateConnected)["codex"]; b.Remote.State != "off" {
		t.Fatal("no remote login showed connected")
	}
	c.AccountKey = "hub-key"
	if b := toolBindings(c, tools, hubclient.StateConnected)["claude-desktop"]; b.CLIRemote == nil || b.CLIRemote.State != "connected" {
		t.Fatal("Claude Code sessions should be reachable from the phone")
	}
	tools[3].Available = false
	if b := toolBindings(c, tools, hubclient.StateConnected)["claude-desktop"]; b.CLIRemote == nil || b.CLIRemote.Supported {
		t.Fatal("missing CLI showed remote support")
	}
}

func TestWorkbenchDesktopLampStaysUnavailableWithStalePayloadAndDraft(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node.js is required for the embedded workbench rendering test")
	}
	source, err := webFS.ReadFile("web/app.js")
	if err != nil {
		t.Fatal(err)
	}
	// Execute the actual rendering functions without booting a browser or touching
	// any local credential, desktop process, or session. An old backend payload is
	// intentionally marked connected to verify the desktop UI also fails closed.
	script := `
const fs = require('fs'), vm = require('vm'), assert = require('assert');
const source = fs.readFileSync(0, 'utf8');
function section(start, end) {
  const a = source.indexOf(start), b = source.indexOf(end, a);
  assert(a >= 0 && b > a, 'workbench function boundary missing');
  return source.slice(a, b);
}
const tools = ['codex-desktop', 'codex', 'claude-desktop', 'claude'].map(id => ({ id, kind: id.split('-')[0], name: id, available: true }));
const remote = { state: 'connected', label: 'CLI connected', detail: 'CLI only', supported: true };
const bindings = Object.fromEntries(tools.map(t => [t.id, { accountId: 'a', appliedId: 'a', model: 'model', protocol: t.kind === 'codex' ? 'responses' : 'anthropic', pending: false, remote, cliRemote: remote }]));
const elements = new Map();
const restartButton = { disabled: false };
const card = { dataset: { available: 'true' }, querySelector: () => restartButton };
const context = { S: { local: { tools, bindings, accounts: [{ id: 'a', name: 'saved API', baseUrl: 'https://api.test', keyMasked: '***', models: [] }] } },
  esc: value => String(value ?? ''), icon: () => '', ico: () => '', document: { querySelector: () => card, getElementById: id => elements.get(id) } };
vm.createContext(context);
vm.runInContext(section('function remoteLampHTML(', 'function renderAgentCards(') + section('function toolForm(', 'async function openRestore('), context);
for (const tool of tools) {
  const desktop = tool.id.endsWith('-desktop');
  const html = context.agentCard(tool);
  const main = html.match(new RegExp('<div id="lamp-' + tool.id + '">([\\s\\S]*?)<\\/div>'))?.[1];
  assert(main, 'main lamp missing');
  assert(main.includes('lamp-connected'), tool.id + ' incorrect initial lamp');
  if (desktop) assert(html.includes('CLI 恢复会话'), 'desktop CLI restore missing');
  assert(!html.includes('desktopProbe'), 'experimental desktop plugin controls must not be shown');
  if (tool.id === 'claude-desktop') {
    const button = html.match(/<button[^>]*data-act="toolRestart"[^>]*>([\s\S]*?)<\/button>/)?.[0];
    assert(button?.includes('disabled') && button.includes('自动应用暂不可用'), 'Claude Desktop auto apply was enabled');
    assert(html.includes('Developer → Configure Third-Party Inference') && html.includes('聊天库的可见性'), 'manual desktop compatibility warning missing');
    assert(!html.includes('已应用 saved API') && !html.includes('应用并重启'), 'old applied payload claimed supported desktop API application');
    assert(html.includes('data-act="toolRestore" data-target="claude"'), 'desktop restore must use the Code card');
  }
  for (const dirty of [false, true]) {
    elements.set('toolAPI-' + tool.id, { value: 'a' });
    elements.set('toolModel-' + tool.id, { value: dirty ? 'changed-model' : 'model' });
    elements.set('toolProtocol-' + tool.id, { value: dirty ? (bindings[tool.id].protocol === 'responses' ? 'chat' : 'responses') : bindings[tool.id].protocol });
    elements.set('lamp-' + tool.id, { innerHTML: '' });
    elements.set('agentApplied-' + tool.id, { innerHTML: '' });
    context.toolDraftStatus(tool.id);
    const lamp = elements.get('lamp-' + tool.id).innerHTML;
    assert(lamp.includes(desktop ? 'lamp-connected' : dirty ? 'lamp-pending' : 'lamp-connected'), tool.id + ' draft changed capability');
    assert(elements.get('agentApplied-' + tool.id).innerHTML.includes(tool.id === 'claude-desktop' ? '自动应用暂不可用' : dirty ? '设置待应用' : '已应用'), 'API capability / pending indicator was lost');
    assert(restartButton.disabled === (tool.id === 'claude-desktop'), tool.id + ' draft changed automatic API support');
  }
}
assert(source.includes("$$('.remote-lamp[data-remote-capability=\"cli\"]')"), 'connection loss must not alter the unsupported desktop lamp');
// Exercise the guard and restore modal with fake callbacks only. No HTTP or UI
// interaction is performed, even when a disabled button handler is invoked.
vm.runInContext('globalThis.testActions = ({' + section('  toolRestart: async (b) => {', '  toolReadModels: async (b) => {') + '}); globalThis.testRestart = testActions.toolRestart;' + section('async function openRestore(', 'function matchesSession('), context);
(async () => {
  let requests = 0;
  context.api = async () => { requests++; throw new Error('must not send an automatic desktop request'); };
  await assert.rejects(context.testRestart({ dataset: { target: 'claude-desktop' } }), /自动应用暂不可用/);
  assert(requests === 0, 'blocked desktop handler sent a request');
  context.busy = async (button, action) => action();
  context.toast = () => {};
  const modal = { innerHTML: '' }, restoreList = { innerHTML: '' };
  context.$ = selector => selector === '#modalRoot' ? modal : selector === '#restoreList' ? restoreList : null;
  context.TOOL_LABEL = { claude: 'Claude Code', 'claude-desktop': 'Claude Desktop' };
  context.renderRestoreList = () => {};
  bindings['claude-desktop'].pending = true;
  bindings['claude-desktop'].appliedId = '';
  context.api = async path => { assert(path === '/api/sessions?tool=claude', 'wrong restore backend'); return { sessions: [] }; };
  await context.openRestore('claude-desktop');
  assert(context.S.restoreTarget === 'claude', 'restore used the disabled desktop binding');
  assert(modal.innerHTML.includes('使用 Claude Code 卡片已应用的 API') && !modal.innerHTML.includes('callout warn'), 'an unapplied desktop binding blocked an applied Code API');
  console.log('desktop and CLI rendering / draft / blocked action / Code restore assertions passed');
})().catch(error => { console.error(error); process.exitCode = 1; });
`
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, node, "-e", script)
	cmd.Stdin = strings.NewReader(string(source))
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("workbench rendering assertions failed: %v\n%s", err, output)
	}
}

func TestDeviceOnlyLampDoesNotRequireOrDescribeModelKeyLogin(t *testing.T) {
	c, tools := workbenchConfig()
	c.AccountKey = ""
	c.RemoteDeviceOnly = true
	c.RecordAppliedAPI("codex", c.LocalAccounts[0], "openai")
	if b := toolBindings(c, tools, hubclient.StateConnected)["codex"]; b.Remote.State != "connected" {
		t.Fatal("device-only connection required a model login key")
	}
	b := toolBindings(c, tools, hubclient.StateInvalidKey)["codex"]
	if b.Remote.State != "error" || b.Remote.Label != "设备验证失败" || strings.Contains(b.Remote.Detail, "登录 Key") {
		t.Fatal("device authorization incorrectly described as API key login")
	}
}

func TestToolBindPersistsWithoutMutatingOriginalCredentials(t *testing.T) {
	s, h := newTestServer(t)
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	path := filepath.Join(root, "auth.json")
	if err := os.WriteFile(path, []byte("original-auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	cx := createLocal(t, s, h, "codex", "codex", "private-cx", "https://api.test")
	cl := createLocal(t, s, h, "claude", "claude", "private-cl", "https://api.test")
	for _, tc := range []struct {
		target, id string
		code       int
	}{{"codex-desktop", cx, 200}, {"claude", cx, 200}, {"claude-desktop", cl, 200}, {"codex", "missing", 400}, {"shell", cl, 400}, {"codex", "", 200}} {
		w := localRequest(s, h, "POST", "/api/local/bind", map[string]string{"target": tc.target, "id": tc.id})
		if w.Code != tc.code {
			t.Fatalf("binding response %d", w.Code)
		}
		if tc.target == "claude-desktop" && (!strings.Contains(w.Body.String(), "自动应用暂不可用") || strings.Contains(w.Body.String(), "重启应用后")) {
			t.Fatal("saving a Claude Desktop draft claimed automatic API support")
		}
	}
	data, _ := os.ReadFile(path)
	if string(data) != "original-auth" || len(s.d.Store.Get().ToolAPIApplied) != 0 {
		t.Fatal("binding mutated tool auth")
	}
	reopened, err := config.Open(s.d.Store.Path())
	if err != nil || reopened.Get().ToolAPISelections["codex-desktop"] != cx {
		t.Fatal("binding not persisted")
	}
	if _, ok := reopened.Get().SelectedToolAccount("codex"); ok {
		t.Fatal("deselection lost after restart")
	}
	if w := localRequest(s, h, "POST", "/api/local/accounts/delete", map[string]string{"id": cx}); w.Code != 200 || s.d.Store.Get().ToolAPISelections["codex-desktop"] != "" {
		t.Fatal("removed API still selected")
	}
}

type sessionAgent struct {
	info protocol.SessionInfo
	kind string
}

func (a *sessionAgent) ID() string {
	if a.kind != "" {
		return a.kind
	}
	return "codex"
}
func (a *sessionAgent) Name() string { return a.ID() }
func (a *sessionAgent) Detect(context.Context) protocol.Tool {
	return protocol.Tool{ID: a.ID(), Available: true}
}
func (a *sessionAgent) Sessions(context.Context) ([]protocol.SessionInfo, error) {
	return []protocol.SessionInfo{a.info}, nil
}
func (a *sessionAgent) Open(context.Context, string) (protocol.SessionInfo, []protocol.Event, error) {
	return a.info, nil, nil
}
func (a *sessionAgent) Start(context.Context, string, string, string, string) (string, error) {
	panic("must not create a session")
}
func (a *sessionAgent) Send(context.Context, string, string) error { panic("must not send a turn") }
func (a *sessionAgent) Interrupt(context.Context, string) error    { return nil }
func (a *sessionAgent) Respond(string, string, string) bool        { return false }
func (a *sessionAgent) Models(context.Context) []string            { return nil }
func (a *sessionAgent) Close()                                     {}

type sessionManager struct{ a *sessionAgent }

func (m sessionManager) Agents() []agents.Agent { return []agents.Agent{m.a} }
func (m sessionManager) Get(k string) agents.Agent {
	if k == m.a.ID() {
		return m.a
	}
	return nil
}

func TestClaudeDesktopRestoreUsesActuallyAppliedCodeAPI(t *testing.T) {
	s, h := newTestServer(t)
	root, cwd := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	t.Setenv("CLAUDE_USER_DATA_DIR", t.TempDir())
	settingsPath := filepath.Join(root, "settings.json")
	originalSettings := `{"theme":"dark","env":{"ANTHROPIC_AUTH_TOKEN":"original-fixture"}}`
	if err := os.WriteFile(settingsPath, []byte(originalSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "claude.exe")
	if err := os.WriteFile(exe, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
		return []launcher.Tool{{ID: "claude", Kind: "claude", Available: true, Path: exe}}
	}
	calls := 0
	s.d.Local.Start = func(_ context.Context, p launcher.Plan) (int, error) {
		calls++
		if p.Tool.ID != "claude" || p.Workspace != cwd || p.ProfileDir != root || strings.Join(p.Args, " ") != "--resume 11111111-2222-3333-4444-555555555555 --model code-applied-model" {
			t.Fatal("desktop restore did not open the exact Code session with its applied model")
		}
		for _, want := range []string{"ANTHROPIC_AUTH_TOKEN=code-key", "ANTHROPIC_BASE_URL=https://code.test", "ANTHROPIC_MODEL=code-applied-model"} {
			found := false
			for _, env := range p.Environment {
				if env == want {
					found = true
				}
				if strings.Contains(env, "desktop-key") || strings.Contains(env, "desktop-only-model") {
					t.Fatal("disabled desktop API supplied Code credentials")
				}
			}
			if !found {
				t.Fatalf("applied Code environment missing %s", strings.Split(want, "=")[0])
			}
		}
		return 101, nil
	}
	codeID := createLocal(t, s, h, "claude", "Code API", "code-key", "https://code.test")
	desktopID := createLocal(t, s, h, "claude", "Desktop draft", "desktop-key", "https://desktop.test")
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ToolAPISelections = map[string]string{"claude": codeID, "claude-desktop": desktopID}
		c.ToolModels = map[string]string{"claude": "code-applied-model", "claude-desktop": "desktop-only-model"}
		draft, _ := c.SelectedToolAPI("claude-desktop")
		// An older Bridge may have recorded a desktop application. It still must
		// neither substitute for a Code application nor provide Code credentials.
		c.RecordAppliedAPI("claude-desktop", draft, "")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	key := "claude:11111111-2222-3333-4444-555555555555"
	a := &sessionAgent{kind: "claude", info: protocol.SessionInfo{SessionKey: key, Tool: "claude", Status: "idle", Cwd: cwd}}
	s.SetManager(sessionManager{a})
	request := func(target string) int {
		return localRequest(s, h, "POST", "/api/local/resume", map[string]string{"target": target, "sessionKey": key}).Code
	}
	if request("claude-desktop") != 409 || calls != 0 {
		t.Fatal("desktop application substituted for missing Code application")
	}
	if err := s.d.Store.Update(func(c *config.Config) error {
		applied, _ := c.SelectedToolAPI("claude")
		c.RecordAppliedAPI("claude", applied, "")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"claude-desktop", "claude"} {
		if request(target) != 200 {
			t.Fatalf("%s could not restore the applied Code API", target)
		}
	}
	if calls != 2 {
		t.Fatal("unexpected Code launch count")
	}
	if err := s.d.Store.Update(func(c *config.Config) error {
		c.ToolModels["claude"] = "pending-code-model"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if request("claude-desktop") != 409 || calls != 2 {
		t.Fatal("an unapplied Code draft was launched through the desktop card")
	}
	settings, err := os.ReadFile(settingsPath)
	if err != nil || string(settings) != originalSettings {
		t.Fatal("CLI restore modified original tool settings")
	}
	bindings := toolBindings(s.d.Store.Get(), []launcher.Tool{{ID: "claude-desktop", Kind: "claude", Available: true}, {ID: "claude", Kind: "claude", Available: true}}, hubclient.StateConnected)
	if bindings["claude-desktop"].Remote.Supported {
		t.Fatal("CLI restore enabled desktop control")
	}
}
func (m sessionManager) Watch(context.Context)      {}
func (m sessionManager) LocalHandler() http.Handler { return http.NotFoundHandler() }
func (m sessionManager) Close()                     {}

func TestResumeAPIRequiresActualIdleSessionAndAppliedSelection(t *testing.T) {
	s, h := newTestServer(t)
	root, cwd := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", root)
	exe := filepath.Join(t.TempDir(), "codex.exe")
	if err := os.WriteFile(exe, []byte("fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	s.d.Local.FindTools = func(context.Context, map[string]string) []launcher.Tool {
		return []launcher.Tool{{ID: "codex", Kind: "codex", Available: true, Path: exe}}
	}
	calls := 0
	s.d.Local.Start = func(_ context.Context, p launcher.Plan) (int, error) {
		calls++
		if p.Workspace != cwd || p.ProfileDir != root || strings.Join(p.Args, " ") != "resume 11111111-2222-3333-4444-555555555555 --model coding-model" {
			t.Fatal("wrong original session or directory")
		}
		return 101, nil
	}
	id := createLocal(t, s, h, "codex", "one", "private-key", "https://api.test")
	key := "codex:11111111-2222-3333-4444-555555555555"
	a := &sessionAgent{info: protocol.SessionInfo{SessionKey: key, Tool: "codex", Status: "idle", Cwd: cwd}}
	s.SetManager(sessionManager{a})
	request := func(key string) int {
		return localRequest(s, h, "POST", "/api/local/resume", map[string]string{"target": "codex", "sessionKey": key}).Code
	}
	if request(key) != 409 {
		t.Fatal("resume before apply allowed")
	}
	if err := s.d.Store.Update(func(c *config.Config) error {
		x, _ := c.LocalAccount(id)
		c.RecordAppliedAPI("codex", x, "openai")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if request("codex:missing") != 404 || calls != 0 {
		t.Fatal("unknown session created a new one")
	}
	a.info.Status = "running"
	if request(key) != 409 || calls != 0 {
		t.Fatal("busy session duplicated")
	}
	a.info.Status, a.info.Cwd = "idle", ""
	if request(key) != 409 || calls != 0 {
		t.Fatal("unknown cwd used default directory")
	}
	a.info.Cwd = cwd
	if request(key) != 200 || calls != 1 {
		t.Fatal("valid exact resume failed")
	}
}
