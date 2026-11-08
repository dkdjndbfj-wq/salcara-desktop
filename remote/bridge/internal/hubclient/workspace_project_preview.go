package hubclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

const projectPreviewLogLimit = 64 << 10
const projectPreviewIdleTTL = 10 * time.Minute
const projectPreviewReceiptTTL = 30 * time.Minute

type projectPreviewProcess interface {
	io.Reader
	io.Closer
	Wait() (int, error)
}

type projectPreviewScript struct {
	Name      string  `json:"name"`
	Command   string  `json:"command"`
	Framework *string `json:"framework"`
	Supported bool    `json:"supported"`
	Reason    string  `json:"reason,omitempty"`
}
type projectPreviewInspection struct {
	ProjectPath           string                 `json:"projectPath"`
	ManifestRevision      string                 `json:"manifestRevision"`
	PackageName           string                 `json:"packageName"`
	Scripts               []projectPreviewScript `json:"scripts"`
	DependenciesInstalled bool                   `json:"dependenciesInstalled"`
	Ready                 bool                   `json:"ready"`
	Reason                string                 `json:"reason,omitempty"`
	Run                   *projectPreviewState   `json:"run,omitempty"`
}
type projectPreviewState struct {
	RunID       string `json:"runId"`
	OperationID string `json:"operationId"`
	ProjectPath string `json:"projectPath"`
	Script      string `json:"script"`
	Status      string `json:"status"`
	URL         string `json:"url,omitempty"`
	Logs        string `json:"logs"`
	Cursor      int64  `json:"cursor"`
	Truncated   bool   `json:"truncated"`
	ExitCode    *int   `json:"exitCode,omitempty"`
	Error       string `json:"error,omitempty"`
}
type projectPreviewResult struct {
	Found bool                 `json:"found"`
	Run   *projectPreviewState `json:"run,omitempty"`
}
type projectPreviewManifest struct {
	Name    string            `json:"name"`
	Scripts map[string]string `json:"scripts"`
}
type workspaceProjectPreview struct {
	mu                                                   sync.Mutex
	scope                                                workspaceFileScope
	rootInfo, projectInfo                                os.FileInfo
	sourceRevision, manifestRevision                     string
	executionFingerprint                                 string
	agent                                                agents.Agent
	agentEpoch                                           uint64
	id, operation, project, script, status, url, failure string
	process                                              projectPreviewProcess
	buffer                                               []byte
	end                                                  int64
	exitCode                                             *int
	touched, created                                     time.Time
	done                                                 chan struct{}
	stopOnce                                             sync.Once
}

func projectPreviewError(code, message string) error {
	return workspaceAdmissionError{code: code, text: message}
}

// This private fingerprint fences selections as well as vault credentials.
// UsageSourceRevision alone intentionally excludes model and executor changes.
func projectPreviewExecutionFingerprint(cfg config.Config, scope workspaceFileScope, agent agents.Agent) string {
	family, _, _ := strings.Cut(scope.key, ":")
	family = config.NormalizeAgentFamily(family)
	base := ""
	var descriptor any
	if agent != nil {
		desc := agents.Describe(agent)
		descriptor, family = desc, desc.APIFamily
		base = featureCatalogScope(context.Background(), cfg, agent, "", scope.root, scope.key)
	}
	binding := cfg.ToolAPIApplied[config.ToolFamily(family)]
	data, _ := json.Marshal(map[string]any{
		"features": base, "descriptor": descriptor, "usage": cfg.UsageSourceRevision,
		"remote": cfg.RemoteAPI[family], "selection": cfg.ToolAPISelections[family],
		"model": cfg.ToolModels[family], "protocol": cfg.ToolProtocols[family], "path": cfg.LocalToolPaths[family],
		"applied": binding, "launchLease": binding.LaunchLease, "clientEpoch": binding.ClientEpoch, "serviceEpoch": binding.ServiceEpoch,
		"localAuthorization": cfg.GatewayAuthorizationRevision(family), "remoteAuthorization": cfg.GatewayAuthorizationRevision("remote-" + family),
		"activeCodex": cfg.ActiveCodexAccount, "activeClaude": cfg.ActiveClaudeAccount,
	})
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (c *Client) projectPreviewExecutionValid(p *workspaceProjectPreview, cfg config.Config) bool {
	if c.projectPreviewAgentEpoch.Load() != p.agentEpoch {
		return false
	}
	if p.agent != nil {
		manager := c.manager()
		if manager == nil || manager.Get(p.agent.ID()) != p.agent {
			return false
		}
	}
	return p.executionFingerprint == projectPreviewExecutionFingerprint(cfg, p.scope, p.agent)
}

// A deliberately small argv grammar supports installed framework CLIs without
// invoking npm, a shell, pre/post hooks, package downloads or arbitrary executables.
// The chosen command remains reviewable in inspect; unsupported scripts stay visible.
func projectPreviewArgs(command string) (string, []string, bool) {
	if len(command) > 4096 || strings.ContainsAny(command, "\r\n\x00;&|<>`$\\") || strings.IndexFunc(command, func(r rune) bool { return unicode.IsControl(r) && r != '\t' }) >= 0 {
		return "", nil, false
	}
	var words []string
	var word strings.Builder
	var quote rune
	for _, ch := range command {
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				word.WriteRune(ch)
			}
		} else if ch == '\'' || ch == '"' {
			quote = ch
		} else if ch == ' ' || ch == '\t' {
			if word.Len() > 0 {
				words = append(words, word.String())
				word.Reset()
			}
		} else {
			word.WriteRune(ch)
		}
	}
	if quote != 0 {
		return "", nil, false
	}
	if word.Len() > 0 {
		words = append(words, word.String())
	}
	if len(words) == 0 {
		return "", nil, false
	}
	framework := words[0]
	args := words[1:]
	if framework == "next" {
		if len(args) == 0 || args[0] != "dev" {
			return "", nil, false
		}
		args = args[1:]
	} else if framework != "vite" {
		return "", nil, false
	}
	var safe []string
	for i := 0; i < len(args); i++ {
		arg, value, assigned := strings.Cut(args[i], "=")
		switch {
		case arg == "--host" && framework == "vite", arg == "--hostname" && framework == "next", arg == "-H" && framework == "next", arg == "--port", arg == "-p" && framework == "next":
			if !assigned && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			// Never retain a script's network binding or port.
		case arg == "--strictPort" && framework == "vite":
		case (arg == "--turbo" || arg == "--turbopack" || arg == "--webpack") && framework == "next" && !assigned:
			safe = append(safe, arg)
		case (arg == "--mode" || arg == "--base" || arg == "--config") && framework == "vite":
			if !assigned {
				i++
				if i >= len(args) {
					return "", nil, false
				}
				value = args[i]
			}
			if value == "" || strings.HasPrefix(value, "-") || len(value) > 1024 {
				return "", nil, false
			}
			if arg == "--config" && (path.IsAbs(value) || strings.Contains(value, ":") || strings.HasPrefix(path.Clean(value), "..")) {
				return "", nil, false
			}
			safe = append(safe, arg, value)
		default:
			return "", nil, false
		}
	}
	return framework, safe, true
}

func projectPreviewOpen(scope workspaceFileScope, relative string, file bool) (*os.File, error) {
	root, err := os.OpenRoot(scope.root)
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer root.Close()
	rootHandle, err := root.Open(".")
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer rootHandle.Close()
	return workspaceOpen(root, rootHandle, scope.root, relative, file)
}

func projectPreviewReadManifest(scope workspaceFileScope, project string) (projectPreviewManifest, string, error) {
	var manifest projectPreviewManifest
	file, err := projectPreviewOpen(scope, path.Join(project, "package.json"), true)
	if err != nil {
		return manifest, "", projectPreviewError("project_manifest_missing", "未找到可读取的 package.json")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 256*1024+1))
	if err != nil || len(data) > 256*1024 || json.Unmarshal(data, &manifest) != nil || len(manifest.Scripts) > 100 || len(manifest.Name) > 512 {
		return manifest, "", projectPreviewError("project_manifest_invalid", "package.json 无效或超过读取限制")
	}
	hash := sha256.Sum256(data)
	return manifest, hex.EncodeToString(hash[:]), nil
}

func projectPreviewFind(scope workspaceFileScope, source string) (string, error) {
	file, err := projectPreviewOpen(scope, source, false)
	if err != nil {
		file, err = projectPreviewOpen(scope, source, true)
		if err != nil {
			return "", errWorkspaceFiles
		}
	}
	info, err := file.Stat()
	file.Close()
	if err != nil {
		return "", errWorkspaceFiles
	}
	dir := source
	if !info.IsDir() {
		dir = path.Dir(source)
		if dir == "." {
			dir = ""
		}
	}
	for {
		if _, _, err := projectPreviewReadManifest(scope, dir); err == nil {
			return dir, nil
		}
		// An invalid nearest manifest must not silently select a parent package.
		if _, err := os.Lstat(filepath.Join(scope.root, filepath.FromSlash(dir), "package.json")); err == nil {
			return "", projectPreviewError("project_manifest_invalid", "最近的 package.json 无法读取或格式无效")
		}
		if dir == "" {
			return "", projectPreviewError("project_manifest_missing", "当前文件所在项目没有 package.json")
		}
		dir = path.Dir(dir)
		if dir == "." {
			dir = ""
		}
	}
}

func projectPreviewCLI(scope workspaceFileScope, project, framework string) string {
	root, err := os.OpenRoot(scope.root)
	if err != nil {
		return ""
	}
	defer root.Close()
	entry := "vite/bin/vite.js"
	if framework == "next" {
		entry = "next/dist/bin/next"
	}
	dir := project
	for {
		relative := filepath.Join(filepath.FromSlash(dir), "node_modules", filepath.FromSlash(entry))
		// Root resolves local package-manager links without probing a symlink
		// target outside the authorized tree (including a network share).
		file, err := workspaceRootReadFile(root, relative)
		if err == nil {
			info, statErr := file.Stat()
			file.Close()
			if statErr == nil && info.Mode().IsRegular() {
				return filepath.Join(scope.root, relative)
			}
		}
		if dir == "" {
			return ""
		}
		dir = path.Dir(dir)
		if dir == "." {
			dir = ""
		}
	}
}

func projectPreviewInspect(scope workspaceFileScope, project string) (projectPreviewInspection, error) {
	manifest, revision, err := projectPreviewReadManifest(scope, project)
	display := func(value string) string {
		return strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, value)
	}
	result := projectPreviewInspection{ProjectPath: project, ManifestRevision: revision, PackageName: display(manifest.Name), Scripts: []projectPreviewScript{}}
	if err != nil {
		return result, err
	}
	_, nodeErr := exec.LookPath("node")
	for name, command := range manifest.Scripts {
		framework, _, supported := projectPreviewArgs(command)
		if name == "" || len(name) > 128 || len(command) > 4096 || display(name) != name {
			continue
		}
		s := projectPreviewScript{Name: name, Command: display(command), Supported: supported}
		if framework != "" {
			s.Framework = &framework
		}
		if manifest.Scripts["pre"+name] != "" || manifest.Scripts["post"+name] != "" {
			s.Supported = false
			s.Reason = "此脚本包含前后置任务，请在电脑运行"
		}
		if !s.Supported && s.Reason == "" {
			s.Reason = "仅支持简单的 vite 或 next dev 脚本"
		}
		if s.Supported && projectPreviewCLI(scope, project, framework) != "" {
			result.DependenciesInstalled = true
		}
		result.Scripts = append(result.Scripts, s)
	}
	sort.Slice(result.Scripts, func(i, j int) bool { return result.Scripts[i].Name < result.Scripts[j].Name })
	result.Ready = nodeErr == nil && result.DependenciesInstalled
	if nodeErr != nil {
		result.Reason = "电脑未找到 Node.js"
	} else if !result.DependenciesInstalled {
		result.Reason = "未找到可运行且已安装依赖的 Vite / Next 开发脚本，请先在电脑配置"
	}
	return result, nil
}

func (c *Client) dispatchWorkspaceProjectPreview(ctx context.Context, cmd map[string]any) (any, error) {
	action := str(cmd, "action")
	allowed := map[string]bool{"type": true, "action": true, "sessionKey": true, "controlSurface": true, "expectedCwd": true}
	switch action {
	case "inspect":
		allowed["sourcePath"] = true
	case "start":
		for _, k := range []string{"projectPath", "script", "manifestRevision", "operationId"} {
			allowed[k] = true
		}
	case "status", "stop":
		for _, k := range []string{"projectPath", "runId", "operationId"} {
			allowed[k] = true
		}
		if action == "status" {
			allowed["cursor"] = true
		}
	default:
		return nil, projectPreviewError("project_preview_invalid", "项目预览操作无效")
	}
	for k, v := range cmd {
		if !allowed[k] {
			return nil, errWorkspaceFiles
		}
		if k != "cursor" {
			if s, ok := v.(string); !ok || len(s) > 4096 || !utf8.ValidString(s) {
				return nil, errWorkspaceFiles
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	scope, err := c.previewScope(ctx, cmd)
	if err != nil {
		return nil, err
	}
	project := str(cmd, "projectPath")
	if action == "inspect" {
		source, ok := cmd["sourcePath"].(string)
		if !ok {
			return nil, errWorkspaceFiles
		}
		if _, err := workspaceRelativePath(source); err != nil {
			return nil, err
		}
		project, err = projectPreviewFind(scope, source)
	} else {
		if _, ok := cmd["projectPath"].(string); !ok {
			return nil, errWorkspaceFiles
		}
		_, err = workspaceRelativePath(project)
	}
	if err != nil {
		return nil, err
	}
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	if err := c.checkUpdateCommand(cmd); err != nil {
		return nil, err
	}
	if err := c.validateCommandGuard(ctx, str(cmd, "type")); err != nil {
		return nil, err
	}
	cfg := c.o.Store.Get()
	root, rootErr := workspaceConfiguredRoot(cfg, scope.root)
	if rootErr != nil || root != scope.root || config.RemoteIdentity(cfg) != scope.identity || cfg.DeviceID != scope.device || cfg.PhoneBindingID != scope.binding || cfg.PhoneHash != scope.phone || ctx.Err() != nil {
		return nil, errWorkspaceChanged
	}
	projectHandle, err := projectPreviewOpen(scope, project, false)
	if err != nil {
		return nil, err
	}
	defer projectHandle.Close()
	projectInfo, err := projectHandle.Stat()
	rootInfo, rootErr := os.Lstat(root)
	if err != nil || rootErr != nil || !projectInfo.IsDir() {
		return nil, errWorkspaceFiles
	}
	c.projectPreviewMu.Lock()
	defer c.projectPreviewMu.Unlock()
	if c.projectPreviews == nil {
		c.projectPreviews = map[string]*workspaceProjectPreview{}
	}
	for id, run := range c.projectPreviews {
		run.mu.Lock()
		expired := time.Since(run.touched) > projectPreviewReceiptTTL
		run.mu.Unlock()
		if expired || run.scope.identity != scope.identity || run.scope.device != scope.device || run.scope.phone != scope.phone || run.scope.binding != scope.binding || run.sourceRevision != cfg.UsageSourceRevision || !c.projectPreviewExecutionValid(run, cfg) {
			run.stop("预览授权已变化或已过期")
			delete(c.projectPreviews, id)
		}
	}
	if action == "inspect" {
		result, err := projectPreviewInspect(scope, project)
		if err != nil {
			return nil, err
		}
		var latest *workspaceProjectPreview
		for _, run := range c.projectPreviews {
			if run.scope == scope && run.project == project && (latest == nil || run.created.After(latest.created)) {
				latest = run
			}
		}
		if latest != nil {
			state := latest.snapshot(0)
			result.Run = &state
		}
		return result, nil
	}
	operation, runID := str(cmd, "operationId"), str(cmd, "runId")
	if operation != "" && !terminalIdentifier(operation) || runID != "" && !terminalIdentifier(runID) || operation == "" && runID == "" {
		return nil, errWorkspaceFiles
	}
	cursor := int64(0)
	if value, present := cmd["cursor"]; present {
		var ok bool
		cursor, ok = terminalInteger(value, 0, 1<<52)
		if !ok {
			return nil, errWorkspaceFiles
		}
	}
	var found *workspaceProjectPreview
	for _, run := range c.projectPreviews {
		if operation != "" && run.operation == operation || runID != "" && run.id == runID {
			if run.scope != scope || run.project != project || operation != "" && run.operation != operation || runID != "" && run.id != runID || !os.SameFile(run.rootInfo, rootInfo) || !os.SameFile(run.projectInfo, projectInfo) {
				return nil, errWorkspaceChanged
			}
			found = run
			break
		}
	}
	if found != nil {
		if action == "start" && (found.script != str(cmd, "script") || found.manifestRevision != str(cmd, "manifestRevision")) {
			return nil, projectPreviewError("project_operation_conflict", "同一启动记录不能用于不同脚本或项目版本")
		}
		if action == "stop" {
			found.stop("")
		}
		state := found.snapshot(cursor)
		return projectPreviewResult{Found: true, Run: &state}, nil
	}
	if action != "start" {
		return projectPreviewResult{Found: false}, nil
	}
	if len(c.projectPreviews) >= 64 {
		return nil, projectPreviewError("project_preview_capacity", "运行记录已达限制，请稍后重试")
	}
	active := 0
	for _, run := range c.projectPreviews {
		run.mu.Lock()
		live := run.status == "starting" || run.status == "running"
		run.mu.Unlock()
		if live {
			active++
			if run.scope == scope {
				return nil, projectPreviewError("project_preview_busy", "此会话已有运行项目，请先停止")
			}
		}
	}
	if active >= 4 {
		return nil, projectPreviewError("project_preview_capacity", "最多同时运行四个项目，请先停止一个")
	}
	inspection, err := projectPreviewInspect(scope, project)
	if err != nil {
		return nil, err
	}
	if inspection.ManifestRevision != str(cmd, "manifestRevision") {
		return nil, projectPreviewError("project_manifest_changed", "项目脚本已变化，请刷新后重新确认")
	}
	script := str(cmd, "script")
	var command string
	for _, s := range inspection.Scripts {
		if s.Name == script && s.Supported {
			command = s.Command
		}
	}
	framework, args, supported := projectPreviewArgs(command)
	cli := projectPreviewCLI(scope, project, framework)
	node, nodeErr := exec.LookPath("node")
	if !supported || cli == "" || nodeErr != nil {
		return nil, projectPreviewError("project_runner_unavailable", "该脚本不能运行，请在电脑确认 Node.js、脚本和本地依赖")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, projectPreviewError("project_port_unavailable", "无法分配本地预览端口")
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	listener.Close()
	if framework == "vite" {
		args = append(args, "--host", "127.0.0.1", "--port", port, "--strictPort")
	} else {
		args = append([]string{"dev"}, args...)
		args = append(args, "--hostname", "127.0.0.1", "--port", port)
	}
	args = append([]string{cli}, args...)
	bytes := make([]byte, 24)
	if _, err := rand.Read(bytes); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var agent agents.Agent
	if manager := c.manager(); manager != nil {
		agent, _, _ = agentFor(manager, scope.key)
	}
	if scope.surface == "cli" && agent == nil {
		return nil, errWorkspaceChanged
	}
	run := &workspaceProjectPreview{scope: scope, rootInfo: rootInfo, projectInfo: projectInfo, sourceRevision: cfg.UsageSourceRevision, manifestRevision: inspection.ManifestRevision,
		agent: agent, agentEpoch: c.projectPreviewAgentEpoch.Load(), executionFingerprint: projectPreviewExecutionFingerprint(cfg, scope, agent),
		id: base64.RawURLEncoding.EncodeToString(bytes), operation: operation, project: project, script: script, status: "starting", url: "http://127.0.0.1:" + port,
		touched: time.Now(), created: time.Now(), done: make(chan struct{})}
	// Register even failed process starts, preserving this operation's result.
	c.projectPreviews[run.id] = run
	env := append(terminalEnvironment(), "CI=1", "NO_COLOR=1", "NEXT_TELEMETRY_DISABLED=1")
	run.process, err = startProjectPreviewProcess(filepath.Join(root, filepath.FromSlash(project)), node, args, env)
	if err != nil {
		run.status = "failed"
		run.failure = "无法启动项目进程，请在电脑检查运行环境"
	} else {
		go run.collect()
		go c.watchProjectPreview(run)
	}
	state := run.snapshot(0)
	return projectPreviewResult{Found: true, Run: &state}, nil
}

func (p *workspaceProjectPreview) snapshot(cursor int64) projectPreviewState {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.touched = time.Now()
	start := p.end - int64(len(p.buffer))
	end := p.end
	// A pipe read can end halfway through a UTF-8 rune. Leave those bytes for
	// the next cursor response instead of corrupting streamed non-ASCII logs.
	if (p.status == "starting" || p.status == "running") && len(p.buffer) > 0 {
		i := len(p.buffer) - 1
		for i > 0 && i >= len(p.buffer)-utf8.UTFMax && !utf8.RuneStart(p.buffer[i]) {
			i--
		}
		if !utf8.FullRune(p.buffer[i:]) {
			end = start + int64(i)
		}
	}
	truncated := cursor < start
	if cursor < start {
		cursor = start
	}
	if cursor > end {
		cursor = end
		truncated = true
	}
	state := projectPreviewState{RunID: p.id, OperationID: p.operation, ProjectPath: p.project, Script: p.script, Status: p.status,
		Logs: strings.ToValidUTF8(string(p.buffer[cursor-start:end-start]), "�"), Cursor: end, Truncated: truncated, ExitCode: p.exitCode, Error: p.failure}
	if p.status == "running" {
		state.URL = p.url
	}
	return state
}
func (p *workspaceProjectPreview) append(data []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.end += int64(len(data))
	if len(data) >= projectPreviewLogLimit {
		p.buffer = append(p.buffer[:0], data[len(data)-projectPreviewLogLimit:]...)
	} else {
		p.buffer = append(p.buffer, data...)
		if len(p.buffer) > projectPreviewLogLimit {
			p.buffer = append([]byte(nil), p.buffer[len(p.buffer)-projectPreviewLogLimit:]...)
		}
	}
}
func (p *workspaceProjectPreview) collect() {
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		buf := make([]byte, 8192)
		for {
			n, err := p.process.Read(buf)
			if n > 0 {
				p.append(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	code, err := p.process.Wait()
	p.process.Close()
	<-readDone
	p.mu.Lock()
	p.exitCode = &code
	if p.status != "stopped" && p.status != "failed" {
		if err != nil || code != 0 || p.status == "starting" {
			p.status = "failed"
			p.failure = "项目进程已退出，请查看运行日志"
		} else {
			p.status = "exited"
		}
	}
	p.mu.Unlock()
}
func (p *workspaceProjectPreview) stop(reason string) {
	p.stopOnce.Do(func() {
		p.mu.Lock()
		if p.status == "starting" || p.status == "running" {
			p.status = "stopped"
			p.failure = reason
		}
		close(p.done)
		process := p.process
		p.mu.Unlock()
		if process != nil {
			process.Close()
		}
	})
}
func (c *Client) projectPreviewScopeValid(p *workspaceProjectPreview) bool {
	cfg := c.o.Store.Get()
	root, err := workspaceConfiguredRoot(cfg, p.scope.root)
	info, statErr := os.Lstat(root)
	// A transport reconnect can temporarily clear uploadAuthorization. The
	// unchanged durable binding keeps this process alive until the idle TTL;
	// confirmed revocation changes the saved phone/binding/source generation.
	if err != nil || statErr != nil || !os.SameFile(info, p.rootInfo) || config.RemoteIdentity(cfg) != p.scope.identity || cfg.DeviceID != p.scope.device || cfg.PhoneBindingID != p.scope.binding || cfg.PhoneHash != p.scope.phone || cfg.UsageSourceRevision != p.sourceRevision || !c.projectPreviewExecutionValid(p, cfg) {
		return false
	}
	f, err := projectPreviewOpen(p.scope, p.project, false)
	if err != nil {
		return false
	}
	projectInfo, err := f.Stat()
	f.Close()
	if err != nil || !os.SameFile(projectInfo, p.projectInfo) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cwd, err := c.workspaceSessionCwd(ctx, p.scope.key, p.scope.surface)
	return err == nil && workspaceSamePath(cwd, p.scope.root)
}
func (c *Client) watchProjectPreview(p *workspaceProjectPreview) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 750 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for {
		select {
		case <-p.done:
			return
		case <-ticker.C:
		}
		p.mu.Lock()
		state, idle := p.status, time.Since(p.touched) > projectPreviewIdleTTL
		p.mu.Unlock()
		if state != "starting" && state != "running" {
			return
		}
		if idle || !c.projectPreviewScopeValid(p) {
			p.stop("预览已过期或项目授权已变化")
			return
		}
		if state == "starting" {
			if time.Since(p.created) > 90*time.Second {
				p.mu.Lock()
				if p.status == "starting" {
					p.status = "failed"
					p.failure = "项目启动超时，请查看日志"
				}
				p.mu.Unlock()
				p.stop("")
				return
			}
			response, err := client.Get(p.url)
			if err == nil {
				response.Body.Close()
				if response.StatusCode < 500 {
					p.mu.Lock()
					if p.status == "starting" {
						p.status = "running"
					}
					p.mu.Unlock()
				}
			}
		}
	}
}
func (c *Client) closeWorkspaceProjectPreviews() {
	c.projectPreviewMu.Lock()
	defer c.projectPreviewMu.Unlock()
	for id, run := range c.projectPreviews {
		run.stop("")
		delete(c.projectPreviews, id)
	}
}
