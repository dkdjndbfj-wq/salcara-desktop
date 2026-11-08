package hubclient

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

// workspaceLease is a Bridge-local view of a session's last authoritative
// session.updated event. It is only a write-conflict guard; it is not an
// ownership or permission grant.
type workspaceLease struct {
	cwd    string
	active bool
}

// Only retained during a serialized native session.start call. This lets an
// authoritative event emitted before Start returns win over its later receipt.
type workspaceStartObservation struct {
	sessions map[string]struct{}
}

type workspaceAdmissionError struct {
	code string
	text string
}

func (e workspaceAdmissionError) Error() string              { return e.text }
func (e workspaceAdmissionError) ExecutionErrorCode() string { return e.code }

var errWorkspaceBusy = workspaceAdmissionError{code: "workspace_busy", text: "工作目录正在被其他任务使用，请等待任务完成或选择其他目录"}

func workspaceAdmissionUnavailable(err error) error {
	// The adapter error may contain a local path or provider details. Keep it in
	// the Bridge log, never in the phone-visible reply.
	return workspaceAdmissionError{code: "workspace_admission_unavailable", text: "无法确认工作目录状态，请稍后重试"}
}

func workspaceGuardedAgent(a agents.Agent) bool {
	if a == nil {
		return false
	}
	// Codex and Claude are the Bridge-owned coding workers. Extension agents
	// may define their own workspace semantics and are intentionally left to
	// their adapter contract.
	return a.ID() == "codex" || a.ID() == "claude"
}

// ObserveLocalEvent updates only Bridge-local workspace admission. In
// particular it must never queue, persist or upload a Direct session's event.
func (c *Client) ObserveLocalEvent(ev protocol.Event) {
	c.observeWorkspaceEvent(ev)
}

func (c *Client) observeWorkspaceEvent(ev protocol.Event) {
	if ev.Type != "session.updated" || ev.Session == nil || ev.Session.SessionKey == "" {
		return
	}
	c.workspaceMu.Lock()
	defer c.workspaceMu.Unlock()
	if observer := c.workspaceStartObserver; observer != nil {
		observer.sessions[ev.Session.SessionKey] = struct{}{}
	}
	c.rememberWorkspaceSessionLocked(ev.Session.SessionKey, ev.Session.Cwd, workspaceActiveStatus(ev.Session.Status))
}

// beginWorkspaceStart runs while taskConfigMu serializes admission. The
// returned function is idempotent and handles both successful/partial native
// creation and a failure before an ID exists. No terminal tombstones survive
// this one call, so the workspace registry stays bounded by active sessions.
func (c *Client) beginWorkspaceStart() func(string, string) {
	observer := &workspaceStartObservation{sessions: make(map[string]struct{})}
	c.workspaceMu.Lock()
	c.workspaceStartObserver = observer
	c.workspaceMu.Unlock()
	return func(sessionKey, cwd string) {
		c.workspaceMu.Lock()
		defer c.workspaceMu.Unlock()
		if c.workspaceStartObserver != observer {
			return
		}
		c.workspaceStartObserver = nil
		if _, observed := observer.sessions[sessionKey]; sessionKey != "" && !observed {
			c.rememberWorkspaceSessionLocked(sessionKey, cwd, true)
		}
	}
}

func (c *Client) rememberWorkspaceSession(sessionKey, cwd string, active bool) {
	if sessionKey == "" {
		return
	}
	c.workspaceMu.Lock()
	defer c.workspaceMu.Unlock()
	c.rememberWorkspaceSessionLocked(sessionKey, cwd, active)
}

func (c *Client) rememberWorkspaceSessionLocked(sessionKey, cwd string, active bool) {
	if c.workspaceSessions == nil {
		c.workspaceSessions = make(map[string]workspaceLease)
	}
	if !active || strings.TrimSpace(cwd) == "" {
		delete(c.workspaceSessions, sessionKey)
		return
	}
	c.workspaceSessions[sessionKey] = workspaceLease{cwd: normalizeWorkspacePath(cwd), active: true}
}

func normalizeWorkspacePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

// workspacePathsOverlap treats a project and one of its subdirectories as
// conflicting. Agents can write files outside their exact cwd through tools,
// so exact-string comparison would leave a common overlap hole.
func workspacePathsOverlap(left, right string) bool {
	left, right = normalizeWorkspacePath(left), normalizeWorkspacePath(right)
	if left == "" || right == "" {
		return false
	}
	if left == right {
		return true
	}
	for _, pair := range [][2]string{{left, right}, {right, left}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

func workspaceActiveStatus(status string) bool {
	return status == "running" || status == "waiting_approval"
}

// checkWorkspaceAdmission is called while taskConfigMu is held. That lock
// makes the check and the following session.start one admission transaction;
// two starts cannot both observe an idle directory and then race to write it.
func (c *Client) checkWorkspaceAdmission(ctx context.Context, m agents.Manager, cwd, exclude string) error {
	if strings.TrimSpace(cwd) == "" || m == nil {
		return nil
	}
	want := normalizeWorkspacePath(cwd)
	// Temporary tool workers are deliberately absent from the conversation
	// directory and global event stream. Their private reservations still take
	// part in the same write-conflict admission as ordinary tasks.
	for _, a := range m.Agents() {
		if private, ok := a.(interface{ SideQuestionToolWorkspaces() []protocol.SessionInfo }); ok {
			for _, session := range private.SideQuestionToolWorkspaces() {
				if session.SessionKey != exclude && workspaceActiveStatus(session.Status) && workspacePathsOverlap(want, session.Cwd) {
					return errWorkspaceBusy
				}
			}
		}
	}
	c.workspaceMu.Lock()
	for key, lease := range c.workspaceSessions {
		if key != exclude && lease.active && workspacePathsOverlap(want, lease.cwd) {
			c.workspaceMu.Unlock()
			return errWorkspaceBusy
		}
	}
	scanned := c.workspaceScanned
	c.workspaceMu.Unlock()
	if scanned {
		return nil
	}

	// The first admission after a Bridge/manager restart reconciles tasks that
	// were already running before the local event registry was rebuilt. Only
	// adapters that advertise session listing are queried; an extension that is
	// explicitly read-only cannot own a writable running session here.
	for _, a := range m.Agents() {
		if a == nil || !agents.Describe(a).Capabilities.List {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		sessions, err := a.Sessions(ctx)
		// Codex session listing needs its executable. Its absence is expected
		// on a Claude-only computer; an installed but failing adapter still
		// blocks admission. Live reservations were checked above regardless.
		if a.ID() == "codex" && errors.Is(err, agents.ErrCodexNotInstalled) {
			continue
		}
		if err != nil {
			return workspaceAdmissionUnavailable(err)
		}
		for _, session := range sessions {
			if session.SessionKey == exclude || !workspaceActiveStatus(session.Status) || !workspacePathsOverlap(want, session.Cwd) {
				continue
			}
			return errWorkspaceBusy
		}
	}
	c.workspaceMu.Lock()
	c.workspaceScanned = true
	c.workspaceMu.Unlock()
	return nil
}

func (c *Client) checkSessionWorkspace(ctx context.Context, m agents.Manager, a agents.Agent, id, sessionKey string) error {
	reader, ok := a.(agents.SessionMetadataReader)
	if !ok {
		// Legacy extension adapters do not expose exact cwd metadata. Their
		// descriptor is conservative and they remain compatible with older
		// callers; built-in Codex and Claude both implement this reader.
		return nil
	}
	info, err := reader.SessionMetadata(ctx, id)
	if err != nil {
		return workspaceAdmissionUnavailable(err)
	}
	if info.SessionKey != sessionKey || strings.TrimSpace(info.Cwd) == "" {
		return workspaceAdmissionUnavailable(nil)
	}
	return c.checkWorkspaceAdmission(ctx, m, info.Cwd, sessionKey)
}
