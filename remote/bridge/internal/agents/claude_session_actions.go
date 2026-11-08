package agents

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const claudeSessionSDKVersion = "0.3.300"
const claudeSessionSDKCodeVersion = "2.1.300"
const maxClaudeActionTranscript = 64 << 20
const maxClaudeSessionActionReceipts = 64

var errClaudeActionChanged = errors.New("session_action_stale: Claude 会话、文件或 API 来源已变化，请刷新")
var errClaudeActionBusy = errors.New("session_action_busy: Claude 正在处理任务或另一个会话操作")
var errClaudeActionUnsupported = errors.New("session_action_unsupported: 当前 Claude 未确认支持该原生会话操作")
var errClaudeForkUnconfirmed = errors.New("session_action_unconfirmed: 上次分支结果尚未确认，暂不能再次创建分支")
var errClaudeSDKUnavailable = errors.New("claude_sdk_unavailable: 需要正式安装 Claude Agent SDK 0.3.300")
var errClaudeSDKResult = errors.New("session_action_unconfirmed: 未确认 Claude 原生操作结果，不会自动重试")

// ClaudeSessionSDKStatus is for the local setup page. Paths never enter the
// remote session-action catalog.
type ClaudeSessionSDKStatus struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Path      string `json:"path,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type claudeSessionSDK struct {
	node, dir, fingerprint string
	nodeFile               os.FileInfo
	files                  map[string]os.FileInfo
}

var claudeSDKCache struct {
	sync.Mutex
	sdk        *claudeSessionSDK
	err        error
	at         time.Time
	generation uint64
}

// InvalidateClaudeSessionSDKCache is called after an explicitly requested setup
// install. Discovery never installs or upgrades anything.
func InvalidateClaudeSessionSDKCache() {
	claudeSDKCache.Lock()
	claudeSDKCache.at = time.Time{}
	claudeSDKCache.generation++
	claudeSDKCache.Unlock()
}

func DetectClaudeSessionSDK(ctx context.Context) ClaudeSessionSDKStatus {
	sdk, err := resolveClaudeSessionSDK(ctx)
	if err != nil {
		return ClaudeSessionSDKStatus{Reason: "未找到可用的官方 Claude Agent SDK 0.3.300；仅安装 Claude CLI 不提供此功能"}
	}
	return ClaudeSessionSDKStatus{Available: true, Version: claudeSessionSDKVersion, Path: sdk.dir}
}

func claudeSDKNodeNPM() (string, string, error) {
	node, ok := findExecutable("node", "")
	if !ok {
		return "", "", errClaudeSDKUnavailable
	}
	node, err := filepath.Abs(node)
	if err == nil {
		node, err = filepath.EvalSymlinks(node)
	}
	if err != nil || runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(node), ".exe") {
		return "", "", errClaudeSDKUnavailable
	}
	candidates := []string{filepath.Join(filepath.Dir(node), "node_modules", "npm", "bin", "npm-cli.js")}
	if npm, ok := findExecutable("npm", ""); ok {
		if resolved, err := filepath.EvalSymlinks(npm); err == nil && filepath.Base(resolved) == "npm-cli.js" {
			candidates = append(candidates, resolved)
		}
		candidates = append(candidates, filepath.Join(filepath.Dir(npm), "node_modules", "npm", "bin", "npm-cli.js"))
	}
	for _, candidate := range candidates {
		absolute, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			continue
		}
		if st, err := os.Stat(resolved); err == nil && st.Mode().IsRegular() {
			return node, resolved, nil
		}
	}
	return "", "", errClaudeSDKUnavailable
}

func ClaudeSessionSDKInstallCommand() (*exec.Cmd, error) {
	node, npm, err := claudeSDKNodeNPM()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(node, npm, "install", "--global", "--ignore-scripts", "--no-audit", "--no-fund", "--no-update-notifier", "@anthropic-ai/claude-agent-sdk@"+claudeSessionSDKVersion)
	cmd.Dir = filepath.Dir(node)
	cmd.Env = claudeSDKEnvironment(nil)
	prepareCmd(cmd)
	return cmd, nil
}

// Remove ambient model credentials and Node preloads. These processes only
// import the installed SDK and invoke its standalone filesystem helpers.
func claudeSDKEnvironment(set map[string]string) []string {
	env := make([]string, 0, len(os.Environ())+len(set))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		upper := strings.ToUpper(key)
		if upper == "NODE_OPTIONS" || upper == "NODE_PATH" || upper == "ELECTRON_RUN_AS_NODE" ||
			strings.HasPrefix(upper, "ANTHROPIC_") || strings.HasPrefix(upper, "CLAUDE_") ||
			strings.HasPrefix(upper, "OPENAI_") || strings.HasPrefix(upper, "CODEX_") {
			continue
		}
		replaced := false
		for replacement := range set {
			replaced = replaced || strings.EqualFold(key, replacement)
		}
		if !replaced {
			env = append(env, entry)
		}
	}
	for key, value := range set {
		env = append(env, key+"="+value)
	}
	return env
}

func (s *claudeSessionSDK) unchanged() bool {
	if s == nil {
		return false
	}
	current, err := inspectClaudeSessionSDK(s.node, s.dir)
	return err == nil && sameClaudeSessionSDK(s, current)
}

func sameClaudeSessionSDK(a, b *claudeSessionSDK) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.node != b.node || a.dir != b.dir || a.fingerprint != b.fingerprint ||
		!sameClaudeTranscript(a.nodeFile, b.nodeFile) || len(a.files) != len(b.files) {
		return false
	}
	for name, file := range a.files {
		if !sameClaudeTranscript(file, b.files[name]) {
			return false
		}
	}
	return true
}

func resolveClaudeSessionSDK(ctx context.Context) (*claudeSessionSDK, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	claudeSDKCache.Lock()
	generation := claudeSDKCache.generation
	if time.Since(claudeSDKCache.at) < 10*time.Second {
		sdk, err := claudeSDKCache.sdk, claudeSDKCache.err
		claudeSDKCache.Unlock()
		if sdk == nil || sdk.unchanged() {
			return sdk, err
		}
	} else {
		claudeSDKCache.Unlock()
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	node, npm, err := claudeSDKNodeNPM()
	var sdk *claudeSessionSDK
	if err == nil {
		cmd := exec.CommandContext(ctx, node, npm, "root", "--global", "--ignore-scripts", "--no-audit", "--no-fund", "--no-update-notifier")
		cmd.Dir, cmd.Env = filepath.Dir(node), claudeSDKEnvironment(nil)
		prepareCmd(cmd)
		var output claudeSDKLimitedBuffer
		cmd.Stdout, cmd.Stderr = &output, io.Discard
		err = cmd.Run()
		root := strings.TrimSpace(output.String())
		if err == nil && filepath.IsAbs(root) && !strings.ContainsAny(root, "\r\n") {
			sdk, err = inspectClaudeSessionSDK(node, filepath.Join(root, "@anthropic-ai", "claude-agent-sdk"))
			if err == nil {
				_, _, err = runClaudeSessionSDK(ctx, sdk, claudeSDKActionInput{Mode: "probe"}, nil)
			}
		} else {
			err = errClaudeSDKUnavailable
		}
	}
	if err != nil {
		sdk, err = nil, errClaudeSDKUnavailable
	}
	claudeSDKCache.Lock()
	if claudeSDKCache.generation == generation {
		claudeSDKCache.sdk, claudeSDKCache.err, claudeSDKCache.at = sdk, err, time.Now()
	}
	claudeSDKCache.Unlock()
	return sdk, err
}

func inspectClaudeSessionSDK(node, dir string) (*claudeSessionSDK, error) {
	node, err := filepath.Abs(node)
	if err == nil {
		node, err = filepath.EvalSymlinks(node)
	}
	if err != nil || runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(node), ".exe") {
		return nil, errClaudeSDKUnavailable
	}
	nodeStat, err := os.Stat(node)
	if err != nil || !nodeStat.Mode().IsRegular() {
		return nil, errClaudeSDKUnavailable
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil || !filepath.IsAbs(dir) {
		return nil, errClaudeSDKUnavailable
	}
	data, pkgStat, err := readClaudeSDKFile(filepath.Join(dir, "package.json"), 64<<10)
	var pkg struct {
		Name, Version     string
		ClaudeCodeVersion string `json:"claudeCodeVersion"`
		Files             []string
	}
	if err != nil || json.Unmarshal(data, &pkg) != nil || pkg.Name != "@anthropic-ai/claude-agent-sdk" ||
		pkg.Version != claudeSessionSDKVersion || pkg.ClaudeCodeVersion != claudeSessionSDKCodeVersion || len(pkg.Files) > 128 {
		return nil, errClaudeSDKUnavailable
	}
	// The pinned package has several bundled runtime chunks. Bind their bytes,
	// not just sdk.mjs metadata, so same-size/mtime replacements invalidate CAS.
	names := map[string]bool{"sdk.mjs": true}
	for _, name := range pkg.Files {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\:") || filepath.Base(name) != name {
			return nil, errClaudeSDKUnavailable
		}
		if ext := strings.ToLower(filepath.Ext(name)); ext == ".mjs" || ext == ".js" {
			names[name] = true
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	digest := sha256.New()
	fmt.Fprintf(digest, "%q\x00%d\x00%d\x00", node, nodeStat.Size(), nodeStat.ModTime().UnixNano())
	pkgDigest := sha256.Sum256(data)
	digest.Write(pkgDigest[:])
	files := map[string]os.FileInfo{"package.json": pkgStat}
	remaining := int64(32 << 20)
	for _, name := range ordered {
		data, st, err := readClaudeSDKFile(filepath.Join(dir, name), min(16<<20, remaining))
		if err != nil {
			return nil, errClaudeSDKUnavailable
		}
		remaining -= int64(len(data))
		part := sha256.Sum256(data)
		fmt.Fprintf(digest, "%q\x00%d\x00", name, len(data))
		digest.Write(part[:])
		files[name] = st
	}
	if current, err := os.Stat(node); err != nil || !sameClaudeTranscript(nodeStat, current) {
		return nil, errClaudeSDKUnavailable
	}
	return &claudeSessionSDK{node: node, dir: dir, fingerprint: hex.EncodeToString(digest.Sum(nil)), nodeFile: nodeStat, files: files}, nil
}

func readClaudeSDKFile(path string, limit int64) ([]byte, os.FileInfo, error) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Size() < 0 || st.Size() > limit {
		return nil, nil, errClaudeSDKUnavailable
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, errClaudeSDKUnavailable
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameClaudeTranscript(st, opened) {
		return nil, nil, errClaudeSDKUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	current, statErr := os.Lstat(path)
	if err != nil || statErr != nil || int64(len(data)) != st.Size() || !sameClaudeTranscript(st, current) {
		return nil, nil, errClaudeSDKUnavailable
	}
	return data, st, nil
}

type claudeSDKLimitedBuffer struct{ bytes.Buffer }

func (b *claudeSDKLimitedBuffer) Write(data []byte) (int, error) {
	if b.Len()+len(data) > 64<<10 {
		return 0, errClaudeSDKResult
	}
	return b.Buffer.Write(data)
}

type claudeSDKActionInput struct {
	Mode    string `json:"mode"`
	SDKDir  string `json:"sdkDir"`
	Action  string `json:"action,omitempty"`
	ID      string `json:"id,omitempty"`
	Name    string `json:"name,omitempty"`
	Home    string `json:"home,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Path    string `json:"path,omitempty"`
	Project string `json:"project,omitempty"`
	Size    string `json:"size,omitempty"`
	Mtime   string `json:"mtime,omitempty"`
}

type claudeSDKActionOutput struct {
	Phase     string `json:"phase"`
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
}

type claudeSessionSDKRunner func(context.Context, *claudeSessionSDK, claudeSDKActionInput, func() error) (claudeSDKActionOutput, bool, error)

// This script has no query(), subprocess launch, prompt or hand-written
// transcript mutation. All writes are official renameSession/forkSession calls.
// The ready/commit exchange places the source/CAS guard after module import.
const claudeSessionSDKScript = `
import fs from "node:fs/promises";
import path from "node:path";
import {pathToFileURL} from "node:url";
import readline from "node:readline";
const reader = readline.createInterface({input: process.stdin, crlfDelay: Infinity});
const lines = reader[Symbol.asyncIterator]();
const emit = value => process.stdout.write(JSON.stringify(value) + "\n");
const next = async () => {
  const line = await lines.next();
  if (line.done || line.value.length > 65536) throw new Error("input");
  return JSON.parse(line.value);
};
const samePath = (a,b) => process.platform === "win32" ? a.toLowerCase() === b.toLowerCase() : a === b;
try {
  const req = await next();
  const pkg = JSON.parse(await fs.readFile(path.join(req.sdkDir, "package.json"), "utf8"));
  if (pkg.name !== "@anthropic-ai/claude-agent-sdk" || pkg.version !== "0.3.300" || pkg.claudeCodeVersion !== "2.1.300") throw new Error("version");
  const sdk = await import(pathToFileURL(path.join(req.sdkDir, "sdk.mjs")).href);
  if (typeof sdk.renameSession !== "function" || typeof sdk.forkSession !== "function" || typeof sdk.getSessionInfo !== "function") throw new Error("exports");
  if (req.mode === "probe") {
    emit({phase:"done"});
  } else {
    if (!["rename","fork"].includes(req.action) || !req.cwd || !req.home || !req.project) throw new Error("action");
    const expected = path.resolve(req.path);
    const target = path.join(req.home, "projects", req.project, req.id + ".jsonl");
    const check = async () => {
      if (!samePath(await fs.realpath(target), expected)) throw new Error("path");
      const st = await fs.stat(target, {bigint:true});
      if (!st.isFile() || st.size.toString() !== req.size || st.mtimeNs.toString() !== req.mtime) throw new Error("changed");
    };
    await check();
    emit({phase:"ready"});
    if ((await next()).commit !== true) throw new Error("cancel");
    await check();
    let id = req.id;
    if (req.action === "rename") {
      await sdk.renameSession(id, req.name, {dir:req.cwd});
    } else {
      const result = await sdk.forkSession(id, {dir:req.cwd});
      id = result.sessionId;
      if (typeof id !== "string" || id === req.id) throw new Error("fork");
      await check();
    }
    const info = await sdk.getSessionInfo(id, {dir:req.cwd});
    if (!info || info.sessionId !== id || req.action === "rename" && info.customTitle !== req.name) throw new Error("confirmation");
    emit({phase:"done", sessionId:id, title:info.customTitle || info.summary || ""});
  }
} catch {
  emit({phase:"error"});
  process.exitCode = 1;
} finally {
  reader.close();
  process.stdin.destroy();
}
`

func runClaudeSessionSDK(ctx context.Context, sdk *claudeSessionSDK, input claudeSDKActionInput, guard func() error) (claudeSDKActionOutput, bool, error) {
	var result claudeSDKActionOutput
	if !sdk.unchanged() {
		return result, false, errClaudeSDKUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	input.SDKDir = sdk.dir
	cmd := exec.CommandContext(ctx, sdk.node, "--input-type=module", "--eval", claudeSessionSDKScript)
	cmd.Dir = sdk.dir
	env := map[string]string{"DISABLE_TELEMETRY": "1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1"}
	if input.Mode != "probe" {
		env["CLAUDE_CONFIG_DIR"], env["CLAUDE_CODE_PROJECT_DIR_NAME"] = input.Home, input.Project
	}
	cmd.Env, cmd.Stderr = claudeSDKEnvironment(env), io.Discard
	cmd.WaitDelay = time.Second
	prepareCmd(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		return result, false, errClaudeSDKUnavailable
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return result, false, errClaudeSDKUnavailable
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return result, false, errClaudeSDKUnavailable
	}
	waited := false
	defer func() {
		in.Close()
		if !waited {
			cancel()
			_ = cmd.Wait()
		}
	}()
	encoded, _ := json.Marshal(input)
	if _, err = in.Write(append(encoded, '\n')); err != nil {
		return result, false, errClaudeSDKResult
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 64<<10)
	read := func() error {
		if !scanner.Scan() || json.Unmarshal(scanner.Bytes(), &result) != nil {
			return errClaudeSDKResult
		}
		return nil
	}
	if err = read(); err != nil {
		return result, false, err
	}
	attempted := false
	if input.Mode != "probe" {
		if result.Phase != "ready" || guard == nil {
			return result, false, errClaudeSDKResult
		}
		if err = guard(); err != nil {
			return result, false, err
		}
		if !sdk.unchanged() {
			return result, false, errClaudeSDKUnavailable
		}
		attempted = true // Even a partial commit write must never be replayed.
		if _, err = io.WriteString(in, "{\"commit\":true}\n"); err != nil {
			return result, true, errClaudeSDKResult
		}
		if err = read(); err != nil {
			return result, true, err
		}
	}
	in.Close()
	err = cmd.Wait()
	waited = true
	if err != nil || result.Phase != "done" {
		return result, attempted, errClaudeSDKResult
	}
	return result, attempted, nil
}

type claudeSessionActionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*claudeSessionActionState
}
type claudeSessionActionState struct {
	revision   string
	snapshot   claudeActionSnapshot
	sdk        *claudeSessionSDK
	operations map[string]*claudeSessionActionOperation
	order      []string
	pending    *claudeSessionActionOperation
}
type claudeSessionActionOperation struct {
	request   protocol.SessionActionRequest
	source    string
	worker    *claudeProc
	lease     *claudeOwnedLease
	result    protocol.SessionActionResult
	attempted bool
	finished  bool
}

// A missing acknowledgement cannot prove a fork did not create a transcript.
// Keep this fence even after the helper exits and across catalog revisions.
func (s *claudeSessionActionState) unconfirmedFork() bool {
	for _, op := range s.operations {
		if op.request.Action == "fork" && op.attempted && op.result.State != "completed" {
			return true
		}
	}
	return false
}

type claudeActionSnapshot struct {
	id, source, path, project, home string
	worker                          *claudeProc
	lease                           *claudeOwnedLease
	file                            os.FileInfo
	info                            protocol.SessionInfo
}

func (a *claudeAgent) claudeActionSource(ctx context.Context, validate func() error) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return "", err
		}
	}
	requested := settingsForContext(ctx, a.settings)
	if err := validateClaudeAPI(requested); err != nil {
		return "", err
	}
	source := claudeSettingsSig(requested, a.exeOverride)
	if _, frozen := ctx.Value(configSnapshotKey{}).(config.Config); frozen {
		// Both phone and local callers bind their immutable snapshot through the
		// dispatcher's source guard. The local fallback may intentionally use a
		// different account than the phone's selected source.
		if validate == nil {
			return "", errClaudeActionChanged
		}
	} else if source != claudeSettingsSig(a.settings(), a.exeOverride) {
		return "", errClaudeActionChanged
	}
	return source, ctx.Err()
}

func (a *claudeAgent) validateClaudeActionReceipt(ctx context.Context, id string, op *claudeSessionActionOperation, validate func() error) error {
	// Run the source callback after waiting for the receipt lock, and without
	// agent/registry locks: it may itself inspect this agent.
	source, err := a.claudeActionSource(ctx, validate)
	if err != nil {
		return err
	}
	if source != op.source {
		return errClaudeActionChanged
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.desktop[id] || a.live[id].SessionKey != "claude:"+id || a.procs[id] != op.worker {
		return errClaudeActionChanged
	}
	if op.worker != nil {
		op.worker.mu.Lock()
		current := !op.worker.stopped && !op.worker.transportRetired() && op.worker.workerSettingsSig == source
		op.worker.mu.Unlock()
		if !current {
			return errClaudeActionChanged
		}
	} else {
		a.hist.mu.Lock()
		current := op.lease != nil && a.hist.ownedLeases[id] == op.lease
		a.hist.mu.Unlock()
		if !current {
			return errClaudeActionChanged
		}
	}
	return ctx.Err()
}

func (a *claudeAgent) captureClaudeAction(ctx context.Context, id string, validate func() error) (claudeActionSnapshot, string, error) {
	snap := claudeActionSnapshot{id: id}
	if !claudeActionUUID(id) {
		return snap, "", errClaudeActionChanged
	}
	source, err := a.claudeActionSource(ctx, validate)
	if err != nil {
		return snap, "", err
	}
	snap.source = source
	a.mu.Lock()
	p, info, closed, desktop := a.procs[id], a.live[id], a.closed, a.desktop[id]
	snap.worker, snap.info = p, info
	reason := ""
	if closed || info.SessionKey != "claude:"+id || desktop {
		reason = "runtime_not_owned"
	} else if p != nil {
		p.mu.Lock()
		if p.workerSettingsSig != source || p.stopped || p.transportRetired() {
			reason = "runtime_changed"
		} else if p.turns != 0 || len(p.turnIDs) != 0 || p.contextCompacting || p.activeSubtasks.Load() || info.ActiveTurnID != "" {
			reason = "runtime_busy"
		}
		p.mu.Unlock()
	}
	a.mu.Unlock()
	if reason != "" {
		return snap, reason, nil
	}
	if a.aps.countFor("claude:"+id) != 0 {
		return snap, "runtime_busy", nil
	}
	a.hist.mu.Lock()
	snap.lease = a.hist.ownedLeases[id]
	leaseSource := ""
	if snap.lease != nil {
		leaseSource = snap.lease.sdkSource
		if snap.lease.worker != nil {
			leaseSource = snap.lease.worker.workerSettingsSig
		}
	}
	a.hist.mu.Unlock()
	if p == nil && (leaseSource != source || !a.hist.ownedCompletion(id)) {
		return snap, "runtime_not_owned", nil
	}
	matches, err := filepath.Glob(filepath.Join(a.hist.dir, "*", id+".jsonl"))
	if err != nil || len(matches) != 1 || !a.hist.safeTranscriptPath(matches[0]) {
		return snap, "transcript_unavailable", nil
	}
	snap.path, err = filepath.EvalSymlinks(matches[0])
	if err == nil {
		snap.path, err = filepath.Abs(snap.path)
	}
	if err != nil {
		return snap, "transcript_unavailable", nil
	}
	snap.file, err = os.Stat(snap.path)
	if err != nil || !snap.file.Mode().IsRegular() || snap.file.Size() <= 0 || snap.file.Size() > maxClaudeActionTranscript {
		return snap, "transcript_unavailable", nil
	}
	snap.project = filepath.Base(filepath.Dir(snap.path))
	root, err := filepath.EvalSymlinks(a.hist.dir)
	if err != nil || !sameClaudeActionPath(filepath.Dir(filepath.Dir(snap.path)), root) || snap.project == "." || snap.project == ".." {
		return snap, "transcript_unavailable", nil
	}
	snap.home = filepath.Dir(root)
	metadata, err := a.SessionMetadata(ctx, id)
	if err != nil || !sameClaudeActionPath(metadata.Cwd, info.Cwd) || strings.Contains(strings.ToLower(metadata.Client), "desktop") {
		return snap, "transcript_unavailable", nil
	}
	if title, titleErr := readClaudeActionTitle(snap.path, id); titleErr != nil {
		return snap, "transcript_unavailable", nil
	} else if title != "" {
		snap.info.Title = titleText(title)
	}
	if current, err := os.Stat(snap.path); err != nil || !sameClaudeTranscript(snap.file, current) {
		return snap, "", errClaudeActionChanged
	}
	return snap, "", nil
}

func sameClaudeActionPath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func claudeActionUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}
func sameClaudeActionSnapshot(a, b claudeActionSnapshot) bool {
	return a.worker == b.worker && a.lease == b.lease && a.source == b.source && a.path == b.path &&
		a.info.ActiveTurnID == b.info.ActiveTurnID && a.info.LastTerminalTurnID == b.info.LastTerminalTurnID &&
		a.info.Title == b.info.Title && sameClaudeTranscript(a.file, b.file)
}
func cloneClaudeActionResult(result protocol.SessionActionResult) protocol.SessionActionResult {
	if result.Session != nil {
		copy := *result.Session
		result.Session = &copy
	}
	return result
}

func claudeSessionActionUnavailableReason(reason string) string {
	switch reason {
	case "":
		return ""
	case "runtime_not_owned", "runtime_changed":
		return "当前运行连接不可用"
	case "runtime_busy":
		return "会话正忙，请稍后重试"
	case "transcript_unavailable":
		return "会话记录暂不可用"
	case "claude_sdk_unavailable":
		return "未安装兼容的 Claude 会话管理组件"
	case "session_action_limit":
		return "会话管理操作记录已达上限"
	default:
		return "当前会话暂不支持此操作"
	}
}

func (a *claudeAgent) SessionActions(ctx context.Context, id string, validate func() error) (protocol.SessionActions, error) {
	snap, reason, err := a.captureClaudeAction(ctx, id, validate)
	view := protocol.SessionActions{SessionKey: "claude:" + id, Title: snap.info.Title, Actions: []string{}, UnavailableReason: claudeSessionActionUnavailableReason(reason)}
	if err != nil {
		return view, err
	}
	if reason != "" {
		return view, nil
	}
	resolve := a.actionSDKResolve
	if resolve == nil {
		resolve = resolveClaudeSessionSDK
	}
	sdk, sdkErr := resolve(ctx)
	if err := ctx.Err(); err != nil {
		return view, err
	}
	current, currentReason, err := a.captureClaudeAction(ctx, id, validate)
	if err != nil || currentReason != "" || !sameClaudeActionSnapshot(snap, current) {
		return view, errClaudeActionChanged
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.procs[id] != snap.worker {
		return view, errClaudeActionChanged
	}
	a.sessionActions.mu.Lock()
	defer a.sessionActions.mu.Unlock()
	if a.sessionActions.sessions == nil {
		a.sessionActions.sessions = make(map[string]*claudeSessionActionState)
	}
	s := a.sessionActions.sessions[id]
	if s == nil {
		if len(a.sessionActions.sessions) >= maxClaudeOwnedLeases {
			view.UnavailableReason = claudeSessionActionUnavailableReason("session_action_limit")
			return view, nil
		}
		s = &claudeSessionActionState{operations: make(map[string]*claudeSessionActionOperation)}
		a.sessionActions.sessions[id] = s
	}
	if s.pending != nil {
		view.Revision, view.UnavailableReason = s.revision, claudeSessionActionUnavailableReason("runtime_busy")
		return view, nil
	}
	changedSDK := !sameClaudeSessionSDK(s.sdk, sdk)
	if s.revision == "" || !sameClaudeActionSnapshot(s.snapshot, snap) || changedSDK {
		s.revision = newID("actions_")
	}
	s.snapshot, s.sdk = snap, sdk
	view.Revision = s.revision
	if sdkErr != nil || sdk == nil {
		view.UnavailableReason = claudeSessionActionUnavailableReason("claude_sdk_unavailable")
		return view, nil
	}
	view.Actions, view.UnavailableReason = []string{"rename"}, ""
	if !s.unconfirmedFork() {
		view.Actions = append(view.Actions, "fork")
	}
	return view, nil
}

func (a *claudeAgent) ApplySessionAction(ctx context.Context, id string, request protocol.SessionActionRequest, validate func() error) (protocol.SessionActionResult, error) {
	if !ValidSessionAction(request) {
		return protocol.SessionActionResult{}, errors.New("session_action_invalid: 会话操作参数无效")
	}
	if request.Action != "rename" && request.Action != "fork" {
		return protocol.SessionActionResult{}, errClaudeActionUnsupported
	}
	source, err := a.claudeActionSource(ctx, validate)
	if err != nil {
		return protocol.SessionActionResult{}, err
	}
	a.sessionActions.mu.Lock()
	s := a.sessionActions.sessions[id]
	if s != nil {
		if op := s.operations[request.OperationID]; op != nil {
			valid := op.request == request && op.source == source
			result := cloneClaudeActionResult(op.result)
			a.sessionActions.mu.Unlock()
			if !valid {
				return protocol.SessionActionResult{}, errClaudeActionChanged
			}
			if err := a.validateClaudeActionReceipt(ctx, id, op, validate); err != nil {
				return protocol.SessionActionResult{}, err
			}
			return result, nil
		}
	}
	if s == nil || s.revision != request.ExpectedRevision || s.snapshot.source != source {
		a.sessionActions.mu.Unlock()
		return protocol.SessionActionResult{}, errClaudeActionChanged
	}
	if s.sdk == nil {
		a.sessionActions.mu.Unlock()
		return protocol.SessionActionResult{}, errClaudeActionUnsupported
	}
	if request.Action == "fork" && s.unconfirmedFork() {
		a.sessionActions.mu.Unlock()
		return protocol.SessionActionResult{}, errClaudeForkUnconfirmed
	}
	snapshot, sdk := s.snapshot, s.sdk
	a.sessionActions.mu.Unlock()
	if !a.startMu.TryLock() {
		return protocol.SessionActionResult{}, errClaudeActionBusy
	}
	lockedStart := true
	defer func() {
		if lockedStart {
			a.startMu.Unlock()
		}
	}()
	p := snapshot.worker
	lockedTurn := false
	if p != nil {
		if !p.turnMu.TryLock() {
			return protocol.SessionActionResult{}, errClaudeActionBusy
		}
		lockedTurn = true
		defer func() {
			if lockedTurn {
				p.turnMu.Unlock()
			}
		}()
	}
	current, reason, err := a.captureClaudeAction(ctx, id, validate)
	if err != nil {
		return protocol.SessionActionResult{}, err
	}
	if reason == "runtime_busy" {
		return protocol.SessionActionResult{}, errClaudeActionBusy
	}
	if reason != "" || !sameClaudeActionSnapshot(snapshot, current) || !sdk.unchanged() {
		return protocol.SessionActionResult{}, errClaudeActionChanged
	}
	a.mu.Lock()
	if a.closed || a.procs[id] != snapshot.worker {
		a.mu.Unlock()
		return protocol.SessionActionResult{}, errClaudeActionChanged
	}
	a.sessionActions.mu.Lock()
	if a.sessionActions.sessions[id] != s || s.revision != request.ExpectedRevision || s.pending != nil {
		a.sessionActions.mu.Unlock()
		a.mu.Unlock()
		return protocol.SessionActionResult{}, errClaudeActionChanged
	}
	if request.Action == "fork" && s.unconfirmedFork() {
		a.sessionActions.mu.Unlock()
		a.mu.Unlock()
		return protocol.SessionActionResult{}, errClaudeForkUnconfirmed
	}
	// Never evict receipts and later replay an old operation ID. Refreshing a
	// catalog does not reset this bounded process-local deduplication history.
	if len(s.operations) >= maxClaudeSessionActionReceipts {
		a.sessionActions.mu.Unlock()
		a.mu.Unlock()
		return protocol.SessionActionResult{}, errors.New("session_action_limit: 此会话已达到原生管理操作记录上限")
	}
	op := &claudeSessionActionOperation{request: request, source: source, worker: snapshot.worker, lease: snapshot.lease, result: protocol.SessionActionResult{
		SessionKey: "claude:" + id, OperationID: request.OperationID, Action: request.Action, State: "unconfirmed", Error: "Claude 原生操作正在确认"}}
	s.operations[request.OperationID], s.pending = op, op
	s.order = append(s.order, request.OperationID)
	a.sessionActions.mu.Unlock()
	a.mu.Unlock()
	guard := func() error {
		now, reason, err := a.captureClaudeAction(ctx, id, validate)
		if err != nil {
			return err
		}
		if reason != "" || !sameClaudeActionSnapshot(snapshot, now) {
			return errClaudeActionChanged
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.sessionActions.mu.Lock()
		defer a.sessionActions.mu.Unlock()
		if a.closed || a.procs[id] != snapshot.worker || a.sessionActions.sessions[id] != s ||
			s.pending != op || s.revision != request.ExpectedRevision {
			return errClaudeActionChanged
		}
		return ctx.Err()
	}
	input := claudeSDKActionInput{Mode: "apply", Action: request.Action, ID: id, Name: request.Name,
		Home: snapshot.home, Cwd: snapshot.info.Cwd, Path: snapshot.path, Project: snapshot.project,
		Size: fmt.Sprint(snapshot.file.Size()), Mtime: fmt.Sprint(snapshot.file.ModTime().UnixNano())}
	run := a.actionSDKRun
	if run == nil {
		run = runClaudeSessionSDK
	}
	output, attempted, runErr := run(ctx, sdk, input, guard)
	var confirmed *claudeActionConfirmation
	if runErr == nil && attempted {
		confirmed, runErr = a.confirmClaudeSessionAction(ctx, snapshot, request, output, validate)
	}
	if runErr == nil {
		finalSource, guardErr := a.claudeActionSource(ctx, validate)
		if guardErr != nil || finalSource != snapshot.source {
			runErr = errClaudeActionChanged
		}
	}
	a.mu.Lock()
	active := !a.closed && a.procs[id] == snapshot.worker && ctx.Err() == nil
	if p != nil {
		p.mu.Lock()
		active = active && !p.stopped && !p.transportRetired() && p.turns == 0 && !p.contextCompacting && !p.activeSubtasks.Load()
		p.mu.Unlock()
	}
	a.sessionActions.mu.Lock()
	if runErr == nil && confirmed != nil && active && s.pending == op {
		// Ownership grants and the published completion are one decision. A
		// stale response must never leave behind a resumable orphan fork.
		a.hist.mu.Lock()
		if request.Action == "fork" {
			if a.hist.ownedLeases == nil {
				a.hist.ownedLeases = make(map[string]*claudeOwnedLease)
			}
			if a.hist.ownedLeases[output.SessionID] != nil || len(a.hist.ownedLeases) >= maxClaudeOwnedLeases || a.live[output.SessionID].SessionKey != "" {
				runErr = errClaudeActionChanged
			} else {
				a.hist.ownedLeases[output.SessionID] = &claudeOwnedLease{sdkSource: snapshot.source, path: confirmed.path, file: confirmed.file}
			}
		} else if snapshot.worker == nil {
			if a.hist.ownedLeases[id] != snapshot.lease || snapshot.lease == nil {
				runErr = errClaudeActionChanged
			} else {
				snapshot.lease.file = confirmed.file
			}
		}
		delete(a.hist.cache, confirmed.path)
		a.hist.mu.Unlock()
	}
	if runErr == nil && confirmed != nil && active && s.pending == op {
		info := confirmed.info
		if request.Action == "fork" {
			newID := strings.TrimPrefix(info.SessionKey, "claude:")
			a.live[newID] = info
			a.policies[newID] = "ask"
		} else {
			a.live[id] = info
		}
		op.result.State, op.result.Error, op.result.Session = "completed", "", &info
	} else {
		op.result.Error = "未确认 Claude 原生操作结果；此操作标识不会重发"
	}
	op.attempted, op.finished, s.pending = attempted, true, nil
	s.revision = newID("actions_")
	result := cloneClaudeActionResult(op.result)
	a.sessionActions.mu.Unlock()
	a.mu.Unlock()
	if lockedTurn {
		p.turnMu.Unlock()
		lockedTurn = false
	}
	a.startMu.Unlock()
	lockedStart = false
	if result.State == "completed" {
		a.emit(protocol.Event{SessionKey: result.Session.SessionKey, Type: "session.updated", Session: ptrInfo(*result.Session)})
	}
	// Even a canceled caller can retrieve the durable-in-this-process receipt.
	// No side effect is retried merely because its confirmation was lost.
	return result, nil
}

type claudeActionConfirmation struct {
	info protocol.SessionInfo
	path string
	file os.FileInfo
}

func (a *claudeAgent) confirmClaudeSessionAction(ctx context.Context, snap claudeActionSnapshot, request protocol.SessionActionRequest, output claudeSDKActionOutput, validate func() error) (*claudeActionConfirmation, error) {
	source, err := a.claudeActionSource(ctx, validate)
	if err != nil || source != snap.source {
		return nil, errClaudeActionChanged
	}
	a.mu.Lock()
	current := !a.closed && a.procs[snap.id] == snap.worker
	if snap.worker != nil {
		snap.worker.mu.Lock()
		current = current && !snap.worker.stopped && !snap.worker.transportRetired() &&
			snap.worker.turns == 0 && !snap.worker.activeSubtasks.Load() && !snap.worker.contextCompacting
		snap.worker.mu.Unlock()
	}
	a.mu.Unlock()
	if !current || !claudeActionUUID(output.SessionID) {
		return nil, errClaudeActionChanged
	}
	info := snap.info
	path := snap.path
	var confirmedFile os.FileInfo
	if request.Action == "rename" {
		if output.SessionID != snap.id || output.Title != request.Name {
			return nil, errClaudeSDKResult
		}
		st, err := os.Stat(path)
		if err != nil || !os.SameFile(snap.file, st) || st.Size() <= snap.file.Size() || !a.hist.safeTranscriptPath(path) {
			return nil, errClaudeSDKResult
		}
		title, err := readClaudeActionTitle(path, snap.id)
		if err != nil || title != request.Name {
			return nil, errClaudeSDKResult
		}
		info.Title, info.UpdatedAt = titleText(title), st.ModTime().UnixMilli()
		confirmedFile = st
	} else {
		if output.SessionID == snap.id {
			return nil, errClaudeSDKResult
		}
		if current, err := os.Stat(snap.path); err != nil || !sameClaudeTranscript(snap.file, current) {
			return nil, errClaudeActionChanged
		}
		path = filepath.Join(filepath.Dir(snap.path), output.SessionID+".jsonl")
		st, err := os.Stat(path)
		if err != nil || !a.hist.safeTranscriptPath(path) || st.Size() <= 0 || st.Size() > maxClaudeActionTranscript {
			return nil, errClaudeSDKResult
		}
		if err := verifyClaudeSDKFork(path, snap.id, output.SessionID); err != nil {
			return nil, err
		}
		final, err := os.Stat(path)
		if err != nil || !sameClaudeTranscript(st, final) {
			return nil, errClaudeActionChanged
		}
		native, ok := a.hist.summary(path, final)
		if !ok || !sameClaudeActionPath(native.Cwd, snap.info.Cwd) || native.Title != titleText(output.Title) {
			return nil, errClaudeSDKResult
		}
		info = native
		info.ControlSurface, info.Controllable, info.Status, info.ApprovalMode = "cli", true, "idle", "ask"
		info.ForkedFromSessionKey = "claude:" + snap.id
		confirmedFile = final
	}
	return &claudeActionConfirmation{info: info, path: path, file: confirmedFile}, nil
}

func verifyClaudeSDKFork(path, parent, id string) error {
	f, err := os.Open(path)
	if err != nil {
		return errClaudeSDKResult
	}
	defer f.Close()
	found, valid := false, true
	err = readLines(io.LimitReader(f, maxClaudeActionTranscript+1), func(line []byte) bool {
		var row struct {
			Type, SessionID string
			ForkedFrom      *struct {
				SessionID string `json:"sessionId"`
			} `json:"forkedFrom"`
		}
		if json.Unmarshal(line, &row) != nil || row.SessionID != id {
			valid = false
			return false
		}
		if row.Type == "user" || row.Type == "assistant" {
			if row.ForkedFrom == nil || row.ForkedFrom.SessionID != parent {
				valid = false
				return false
			}
			found = true
		}
		return true
	})
	if err != nil || !valid || !found {
		return errClaudeSDKResult
	}
	return nil
}

var _ SessionActionProvider = (*claudeAgent)(nil)
