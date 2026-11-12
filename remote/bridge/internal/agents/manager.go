package agents

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"salcara/bridge/internal/protocol"
)

const maxWorkspaceModeEntries = 20_000
const maxWorkspaceModeFileBytes = 2 << 20

// Options lets tests (or the core) point the agents at specific executables and folders.
// Zero values mean "discover automatically".
type Options struct {
	// BundledAgents enables the official DeepSeek Harness and Hermes adapters.
	// Explicit embedding/test registries can instead supply their own ExtraAgents.
	BundledAgents bool
	CodexPath     string // codex executable
	ClaudePath    string // claude executable
	ClaudeHome    string // ~/.claude (projects/ inside)
	StateDir      string // where the bridge remembers which Codex threads it started; "" = user config dir
	// ExtraAgents are independently constructed adapters. They share the
	// manager's normalized transport but are never routed through Codex or
	// Claude. An adapter without an explicit DescriptorProvider is exposed as
	// unknown/read-only until it declares its capabilities.
	ExtraAgents []Agent

	ClaudeWatchInterval time.Duration // default 2s
	CodexWatchInterval  time.Duration // default 10s
}

type manager struct {
	sink   Sink
	codex  *codexAgent
	claude *claudeAgent
	// all is the aggregation registry. Keep the built-ins first for stable
	// legacy ordering, then append validated extension adapters.
	all []Agent
	mux *http.ServeMux

	mu                sync.Mutex
	watchOn           bool
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	closed            atomic.Bool
	eventMu           sync.RWMutex // sink must not synchronously re-enter Close
	opts              Options
	workspaceModeMu   sync.RWMutex
	workspaceModes    map[string]protocol.WorkspaceMode
	workspaceModeFile string
}

// NewManager creates the Codex and Claude Code agents.
func NewManager(sink Sink, settings func() Settings) Manager {
	return NewManagerWithOptions(sink, settings, Options{BundledAgents: true})
}

// NewManagerWithOptions is NewManager with explicit executable paths / folders.
func NewManagerWithOptions(sink Sink, settings func() Settings, opts Options) Manager {
	if sink == nil {
		sink = func(protocol.Event) {}
	}
	if opts.StateDir == "" {
		if d, err := os.UserConfigDir(); err == nil {
			opts.StateDir = filepath.Join(d, "salcara-bridge")
		}
	}
	if opts.ClaudeWatchInterval <= 0 {
		opts.ClaudeWatchInterval = 2 * time.Second
	}
	if opts.CodexWatchInterval <= 0 {
		opts.CodexWatchInterval = 10 * time.Second
	}
	stateFile := ""
	if opts.StateDir != "" {
		stateFile = filepath.Join(opts.StateDir, "codex-threads.json")
	}
	all := []Agent{}
	seen := map[string]bool{}
	reserved := map[string]bool{"codex": true, "claude": true, "codex-desktop": true, "claude-desktop": true}
	add := func(a Agent) {
		if a == nil || a.ID() == "" || seen[a.ID()] || (len(all) >= 2 && reserved[a.ID()]) {
			return
		}
		d, valid := CanonicalDescriptor(a)
		// This registry owns only the CLI Agent contract. Native/read-only
		// providers have independent lease/scope-aware routes, not a fallback
		// through Agent.Start or Agent.Send.
		if !valid || d.ControlSurface != "cli" {
			return
		}
		seen[a.ID()] = true
		all = append(all, a)
	}
	m := &manager{sink: sink, opts: opts, mux: http.NewServeMux(), workspaceModes: map[string]protocol.WorkspaceMode{}}
	if opts.StateDir != "" {
		m.workspaceModeFile = filepath.Join(opts.StateDir, "session-workspace-modes.json")
	}
	m.codex = newCodexAgent(func(e protocol.Event) { m.publishAgentEvent(m.codex, "codex", e) }, settings, opts.CodexPath, stateFile)
	m.claude = newClaudeAgent(func(e protocol.Event) { m.publishAgentEvent(m.claude, "claude", e) }, settings, opts.ClaudePath, opts.ClaudeHome)
	if opts.StateDir != "" {
		m.codex.sideQuestions.persistentDir = filepath.Join(opts.StateDir, "side-conversations", "codex")
		m.claude.sideQuestions.persistentDir = filepath.Join(opts.StateDir, "side-conversations", "claude")
	}
	add(m.codex)
	add(m.claude)
	for _, a := range opts.ExtraAgents {
		add(a)
	}
	if opts.BundledAgents {
		add(newDeepSeekAgent(settings, opts.StateDir))
		add(newHermesAgent(settings, opts.StateDir))
	}
	m.all = all
	m.loadWorkspaceModes()
	for _, a := range m.all[2:] {
		if binder, ok := a.(EventSinkBinder); ok {
			adapter, id := a, a.ID()
			binder.BindEventSink(func(e protocol.Event) { m.publishAgentEvent(adapter, id, e) })
		}
	}
	m.mux.HandleFunc(PermissionPath, m.claude.permissionHandler)
	m.mux.HandleFunc(ClaudeElicitationPath, m.claude.elicitationHandler)
	return m
}

func (m *manager) Agents() []Agent { return append([]Agent(nil), m.all...) }

func (m *manager) Get(tool string) Agent {
	for _, a := range m.all {
		if a.ID() == tool {
			return a
		}
	}
	return nil
}

// RememberWorkspaceMode persists the phone's explicit Code/Work label. The
// label is deliberately stored separately from project authorization and is
// never used to admit a cwd.
func (m *manager) RememberWorkspaceMode(sessionKey string, mode protocol.WorkspaceMode) error {
	if !validWorkspaceMode(mode) || !SessionBelongsTo(m.Get(sessionPrefix(sessionKey)), sessionKey, protocol.SessionInfo{SessionKey: sessionKey}) {
		return errors.New("无效的会话工作区模式")
	}
	m.workspaceModeMu.Lock()
	m.workspaceModes[sessionKey] = mode
	if len(m.workspaceModes) > maxWorkspaceModeEntries {
		keys := make([]string, 0, len(m.workspaceModes))
		for key := range m.workspaceModes {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys[:len(keys)-maxWorkspaceModeEntries] {
			delete(m.workspaceModes, key)
		}
	}
	data, err := json.Marshal(m.workspaceModes)
	if err == nil && m.workspaceModeFile != "" {
		if err = os.MkdirAll(filepath.Dir(m.workspaceModeFile), 0o700); err == nil {
			tmp := m.workspaceModeFile + ".tmp"
			if len(data) > maxWorkspaceModeFileBytes {
				err = errors.New("会话工作区记录超过上限")
			} else if err = os.WriteFile(tmp, data, 0o600); err == nil {
				err = os.Rename(tmp, m.workspaceModeFile)
				if err != nil {
					_ = os.Remove(tmp)
				}
			}
		}
	}
	m.workspaceModeMu.Unlock()
	return err
}

func sessionPrefix(key string) string {
	if i := strings.IndexByte(key, ':'); i > 0 {
		return key[:i]
	}
	return ""
}

func validWorkspaceMode(mode protocol.WorkspaceMode) bool {
	return mode == protocol.WorkspaceModeCode || mode == protocol.WorkspaceModeWork
}

func (m *manager) applyWorkspaceMode(info *protocol.SessionInfo) {
	if info == nil {
		return
	}
	m.workspaceModeMu.RLock()
	mode := m.workspaceModes[info.SessionKey]
	m.workspaceModeMu.RUnlock()
	if validWorkspaceMode(mode) {
		info.WorkspaceMode = mode
		return
	}
	if !validWorkspaceMode(info.WorkspaceMode) {
		info.WorkspaceMode = ""
	}
}

// ApplyWorkspaceMode adds the persisted phone directory classification to an
// adapter-owned snapshot without changing session identity or permissions.
func (m *manager) ApplyWorkspaceMode(info *protocol.SessionInfo) { m.applyWorkspaceMode(info) }

func ApplyRememberedWorkspaceMode(m Manager, info *protocol.SessionInfo) {
	if projector, ok := m.(interface{ ApplyWorkspaceMode(*protocol.SessionInfo) }); ok {
		projector.ApplyWorkspaceMode(info)
	}
}

func (m *manager) loadWorkspaceModes() {
	if m.workspaceModeFile == "" {
		return
	}
	b, err := os.ReadFile(m.workspaceModeFile)
	if err != nil || len(b) > maxWorkspaceModeFileBytes {
		return
	}
	var raw map[string]protocol.WorkspaceMode
	if json.Unmarshal(b, &raw) != nil || len(raw) > maxWorkspaceModeEntries {
		return
	}
	for key, mode := range raw {
		if validWorkspaceMode(mode) && SessionBelongsTo(m.Get(sessionPrefix(key)), key, protocol.SessionInfo{SessionKey: key}) {
			m.workspaceModes[key] = mode
		}
	}
}

func (m *manager) publishAgentEvent(a Agent, registeredID string, e protocol.Event) {
	// Close waits for an admitted fan-out to finish before retiring the sink.
	// The core sink only queues/broadcasts; it must not synchronously call Close.
	m.eventMu.RLock()
	defer m.eventMu.RUnlock()
	if m.closed.Load() || a == nil || a.ID() != registeredID || (e.Tool != "" && e.Tool != registeredID) {
		return
	}
	d, valid := CanonicalDescriptor(a)
	if !valid || d.ControlSurface != "cli" {
		return
	}
	if e.Type == "features.changed" {
		if !d.Capabilities.Features || e.SessionKey != "" || e.Session != nil {
			return
		}
		// A runtime-wide invalidation is not a timeline event. Project only the
		// registered agent identity, never arbitrary adapter payload or a session.
		m.sink(protocol.Event{Type: "features.changed", Tool: registeredID, TS: e.TS})
		return
	}
	key := e.SessionKey
	if key == "" && e.Session != nil {
		key = e.Session.SessionKey
	}
	if !SessionBelongsTo(a, key, protocol.SessionInfo{SessionKey: key}) || e.Session != nil && !SessionBelongsTo(a, key, *e.Session) {
		return
	}
	if e.Type == "session.updated" {
		if !d.Capabilities.List && !d.Capabilities.Read {
			return
		}
	} else if !d.Capabilities.Read {
		return
	}
	if e.Type == "approval.request" && ((e.Kind == "question" && !d.Capabilities.Questions) || (e.Kind != "question" && !d.Capabilities.Approval)) {
		return
	}
	// Copy mutable fields before the asynchronous upload; preserve ImageData's
	// in-process handoff, which is intentionally excluded from JSON.
	copy := cloneProtocolEvent(e)
	copy.Tool, copy.SessionKey = registeredID, key
	copy = protocol.NormalizeTimelineEvent(copy)
	if copy.Session != nil {
		*copy.Session = ProjectSessionInfo(*copy.Session, registeredID, d.Capabilities)
		m.applyWorkspaceMode(copy.Session)
	}
	ProjectEventMedia(&copy, d.Capabilities)
	m.sink(copy)
}

func cloneProtocolEvent(e protocol.Event) protocol.Event {
	copy := e
	if e.Session != nil {
		value := *e.Session
		copy.Session = &value
	}
	if e.MessageOrigin != nil {
		value := *e.MessageOrigin
		copy.MessageOrigin = &value
	}
	if e.Browser != nil {
		value := *e.Browser
		copy.Browser = &value
	}
	if e.Computer != nil {
		value := *e.Computer
		copy.Computer = &value
	}
	if e.ExitCode != nil {
		value := *e.ExitCode
		copy.ExitCode = &value
	}
	if e.Usage != nil {
		value := *e.Usage
		copy.Usage = &value
	}
	copy.Attachments = append([]protocol.AttachmentRef(nil), e.Attachments...)
	copy.ImageData = append([]string(nil), e.ImageData...)
	copy.ChildSessionKeys = append([]string(nil), e.ChildSessionKeys...)
	copy.Questions = cloneQuestions(e.Questions)
	return copy
}

func (m *manager) Watch(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watchOn {
		return
	}
	if m.closed.Load() {
		return
	}
	m.watchOn = true
	ctx, m.cancel = context.WithCancel(ctx)
	m.wg.Add(2)
	go func() {
		defer m.wg.Done()
		m.claude.watch(ctx, m.opts.ClaudeWatchInterval)
	}()
	go func() {
		defer m.wg.Done()
		m.codex.watch(ctx, m.opts.CodexWatchInterval)
	}()
	for _, a := range m.all[2:] {
		_, bound := a.(EventSinkBinder)
		d, valid := CanonicalDescriptor(a)
		if watcher, ok := a.(Watcher); ok && bound && valid && (d.Capabilities.List || d.Capabilities.Read) {
			m.wg.Add(1)
			go func(w Watcher) {
				defer m.wg.Done()
				w.Watch(ctx)
			}(watcher)
		}
	}
}

func (m *manager) LocalHandler() http.Handler { return m.mux }

// Respond is a convenience for the core: routes an approval answer to whichever agent raised it.
func (m *manager) Respond(approvalID, decision, message string) bool {
	for _, a := range m.Agents() {
		if a.Respond(approvalID, decision, message) {
			return true
		}
	}
	return false
}

func (m *manager) Close() {
	m.eventMu.Lock()
	if !m.closed.CompareAndSwap(false, true) {
		m.eventMu.Unlock()
		return
	}
	m.eventMu.Unlock()
	m.mu.Lock()
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	for _, a := range m.all {
		if binder, ok := a.(EventSinkBinder); ok {
			binder.BindEventSink(nil)
		}
		a.Close()
	}
	m.wg.Wait()
}
