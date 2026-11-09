package hubclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/atomicfile"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

var errProjectless = errors.New("projectless_unavailable: 独立会话的工作目录无法确认，请检查电脑存储后重试")
var errProjectlessFields = errors.New("projectless_invalid: 不关联文件夹需要明确的独立会话请求，不能同时指定项目目录")

// These records are not global project grants. Each directory belongs to one
// immutable start operation and, after acceptance, exactly one native session.
// Content is durable user work: never expire or recursively delete it as cache.
type projectlessRecord struct {
	DirectoryID string                 `json:"directoryId"`
	OperationID string                 `json:"operationId"`
	ComputerID  string                 `json:"computerId"`
	Owner       string                 `json:"owner"`
	Source      string                 `json:"source"`
	Tool        string                 `json:"tool"`
	Mode        protocol.WorkspaceMode `json:"mode"`
	Command     string                 `json:"command"`
	State       string                 `json:"state"`
	SessionKey  string                 `json:"sessionKey,omitempty"`
}

func validateProjectlessFields(cmd map[string]any) error {
	value, present := cmd["projectless"]
	if !present {
		return nil
	}
	cwd, hasCwd := cmd["cwd"].(string)
	if value != true || str(cmd, "type") != "session.start" || !hasCwd || cwd != "" || str(cmd, "controlSurface") != "cli" || !stationSwitchOperationID(str(cmd, "operationId")) || str(cmd, "workspaceMode") != "code" && str(cmd, "workspaceMode") != "work" {
		return errProjectlessFields
	}
	if _, exists := cmd["entrypoint"]; exists {
		return errProjectlessFields
	}
	return nil
}

func projectlessOwner(cfg config.Config, source string) string {
	switch source {
	case "direct":
		return durableHash([]string{source, cfg.ComputerID, cfg.DeviceID, cfg.DirectPhoneID, cfg.DirectPhoneHash})
	case "local":
		return durableHash([]string{source, cfg.ComputerID, cfg.DeviceID})
	default:
		return durableHash([]string{"hub", cfg.ComputerID, cfg.DeviceID, config.RemoteIdentity(cfg), cfg.PhoneBindingID, cfg.PhoneHash})
	}
}

func projectlessSource(ctx context.Context) string {
	if IsDirectExecution(ctx) {
		return "direct"
	}
	if isLocalExecution(ctx) {
		return "local"
	}
	return "hub"
}

func (c *Client) projectlessOwner(ctx context.Context, cfg config.Config, source string) string {
	base := projectlessOwner(cfg, source)
	if source == "hub" {
		return durableHash([]string{base, c.eventScopeFromContext(ctx, cfg)})
	}
	return base
}

func (c *Client) projectlessOwnerMatches(ctx context.Context, cfg config.Config, record projectlessRecord) bool {
	// The authenticated loopback desktop is the owner of the physical machine;
	// pairing boundaries constrain phones, not the user's own local console.
	return isLocalExecution(ctx) && !IsDirectExecution(ctx) && record.ComputerID == cfg.ComputerID || record.Owner == c.projectlessOwner(ctx, cfg, projectlessSource(ctx))
}

func (c *Client) projectlessGuard(ctx context.Context) error {
	if c.o.Store == nil || ctx.Err() != nil {
		return errProjectless
	}
	if IsDirectExecution(ctx) {
		return validateDirectBinding(ctx, c.o.Store.Get())
	}
	if isLocalExecution(ctx) {
		return nil
	}
	if _, guarded := commandGuardFromContext(ctx); !guarded {
		if _, sourced := executionSourceFromContext(ctx); !sourced {
			return errProjectless
		}
	}
	if err := c.validateCommandGuard(ctx, "session.start"); err != nil {
		return err
	}
	if env, ok := executionSourceFromContext(ctx); ok {
		return c.validateExecutionSourceFor(env, c.o.Store.Get())
	}
	return nil
}

func (c *Client) projectlessPaths() (base, manifest string, err error) {
	if c.o.Store == nil || runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		return "", "", errProjectless
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(c.o.Store.Path()))
	if err != nil || !filepath.IsAbs(parent) || !workspaceLocalRoot(parent) {
		return "", "", errProjectless
	}
	return filepath.Join(parent, "session-workspaces"), filepath.Join(parent, "projectless-sessions.json"), nil
}

// projectlessMu protects the manifest cache, not worker execution. An adapter
// may synchronously emit an event while Start is still on the stack.
func (c *Client) loadProjectlessLocked() error {
	if c.projectlessLoaded {
		return c.projectlessErr
	}
	c.projectlessLoaded = true
	c.projectlessRecords = make(map[string]projectlessRecord)
	_, file, err := c.projectlessPaths()
	if err != nil {
		c.projectlessErr = err
		return err
	}
	f, err := os.Open(file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		c.projectlessErr = errProjectless
		return c.projectlessErr
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4*1024*1024+1))
	var records map[string]projectlessRecord
	if err != nil || len(data) > 4*1024*1024 || json.Unmarshal(data, &records) != nil || len(records) > 4096 {
		c.projectlessErr = errProjectless
		return c.projectlessErr
	}
	for key, record := range records {
		if len(key) != 64 || strings.Trim(key, "0123456789abcdef") != "" || len(record.Owner) != 64 || len(record.Command) != 64 || record.Source != "hub" && record.Source != "direct" && record.Source != "local" || record.Tool == "" || record.Mode != "code" && record.Mode != "work" || record.State != "prepared" && record.State != "starting" && record.State != "bound" || record.State == "bound" && !strings.HasPrefix(record.SessionKey, record.Tool+":") {
			c.projectlessErr = errProjectless
			return c.projectlessErr
		}
	}
	c.projectlessRecords = records
	return nil
}

func (c *Client) saveProjectlessLocked(key string, record projectlessRecord) error {
	_, file, err := c.projectlessPaths()
	if err != nil {
		return err
	}
	next := make(map[string]projectlessRecord, len(c.projectlessRecords)+1)
	for k, v := range c.projectlessRecords {
		next[k] = v
	}
	next[key] = record
	if len(next) > 4096 {
		return errors.New("projectless_limit: 独立会话记录已满，请在电脑整理后重试")
	}
	data, err := json.Marshal(next)
	if err != nil || len(data) > 4*1024*1024 {
		return errProjectless
	}
	if atomicfile.WriteFile(file, data, 0600) != nil {
		return errProjectless
	}
	c.projectlessRecords = next
	return nil
}

func verifiedProjectlessDirectory(base, key string, create bool, identity *string) (string, error) {
	if create {
		if err := os.Mkdir(base, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", errProjectless
		}
	}
	st, err := os.Lstat(base)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeType != os.ModeDir {
		return "", errProjectless
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return "", errProjectless
	}
	defer root.Close()
	handle, err := root.Open(".")
	if err != nil {
		return "", errProjectless
	}
	defer handle.Close()
	if workspaceVerifyHandle(handle, handle, base, "", false) != nil {
		return "", errProjectless
	}
	if create {
		if err := root.Mkdir(key, 0700); err != nil {
			return "", errProjectless
		}
	}
	dir, err := workspaceOpen(root, handle, base, key, false)
	if err != nil {
		return "", errProjectless
	}
	defer dir.Close()
	actualID, err := projectlessDirectoryIdentity(dir)
	if err != nil || identity == nil || !create && *identity != actualID {
		return "", errProjectless
	}
	current, err := os.Lstat(filepath.Join(base, key))
	opened, openErr := dir.Stat()
	if err != nil || openErr != nil || !os.SameFile(current, opened) || workspaceVerifyHandle(handle, dir, base, key, false) != nil {
		return "", errProjectless
	}
	if create {
		*identity = actualID
	}
	return filepath.Join(base, key), nil
}

// Called after all input/account/model/image validations, under taskConfigMu.
// Persisting starting precedes invoking the adapter; a crash cannot cause an
// unknown operation to launch again, even if its transport receipt is lost.
func (c *Client) prepareProjectless(ctx context.Context, cmd map[string]any) (cwd, key, accepted string, err error) {
	if err = c.projectlessGuard(ctx); err != nil {
		return
	}
	cfg := c.o.Store.Get()
	source := projectlessSource(ctx)
	owner := c.projectlessOwner(ctx, cfg, source)
	key = durableHash([]string{owner, str(cmd, "tool"), str(cmd, "workspaceMode"), str(cmd, "operationId")})
	base, _, e := c.projectlessPaths()
	if e != nil {
		err = e
		return
	}
	c.projectlessMu.Lock()
	defer c.projectlessMu.Unlock()
	if err = c.loadProjectlessLocked(); err != nil {
		return
	}
	record, exists := c.projectlessRecords[key]
	if exists {
		if record.Owner != owner || record.Command != durableHash(cmd) {
			err = errProjectlessFields
			return
		}
		if record.State == "starting" {
			err = newTaskUncertainError{}
			return
		}
		if cwd, err = verifiedProjectlessDirectory(base, key, false, &record.DirectoryID); err != nil {
			return
		}
		if record.State == "bound" {
			accepted = record.SessionKey
		}
	} else {
		if len(c.projectlessRecords) >= 4096 {
			err = errors.New("projectless_limit: 独立会话记录已满，请在电脑整理后重试")
			return
		}
		record = projectlessRecord{Owner: owner, Source: source, Tool: str(cmd, "tool"), Mode: protocol.WorkspaceMode(str(cmd, "workspaceMode")), Command: durableHash(cmd), State: "prepared", OperationID: str(cmd, "operationId"), ComputerID: cfg.ComputerID}
		// An orphaned directory is never adopted or overwritten.
		if _, e = os.Lstat(filepath.Join(base, key)); !errors.Is(e, os.ErrNotExist) {
			err = errProjectless
			return
		}
		if cwd, err = verifiedProjectlessDirectory(base, key, true, &record.DirectoryID); err != nil {
			return
		}
		if err = c.saveProjectlessLocked(key, record); err != nil {
			return
		}
	}
	if err = c.projectlessGuard(ctx); err != nil {
		return
	}
	if c.projectlessOwner(ctx, c.o.Store.Get(), source) != owner {
		err = errWorkspaceChanged
		return
	}
	return
}

func (c *Client) markProjectlessStarting(ctx context.Context, key string) error {
	if err := c.projectlessGuard(ctx); err != nil {
		return err
	}
	c.projectlessMu.Lock()
	defer c.projectlessMu.Unlock()
	r, ok := c.projectlessRecords[key]
	if !ok || r.State != "prepared" || r.Owner != c.projectlessOwner(ctx, c.o.Store.Get(), r.Source) {
		return errProjectless
	}
	base, _, err := c.projectlessPaths()
	if err != nil {
		return err
	}
	if _, err = verifiedProjectlessDirectory(base, key, false, &r.DirectoryID); err != nil {
		return err
	}
	r.State = "starting"
	return c.saveProjectlessLocked(key, r)
}

func (c *Client) bindProjectless(key, sessionKey string) error {
	c.projectlessMu.Lock()
	defer c.projectlessMu.Unlock()
	r, ok := c.projectlessRecords[key]
	if ok && r.State == "bound" && r.SessionKey == sessionKey {
		return nil
	}
	if !ok || r.State != "starting" || !strings.HasPrefix(sessionKey, r.Tool+":") {
		return errProjectless
	}
	for other, record := range c.projectlessRecords {
		if other != key && record.SessionKey == sessionKey {
			return errProjectless
		}
	}
	r.State = "bound"
	r.SessionKey = sessionKey
	return c.saveProjectlessLocked(key, r)
}

func (c *Client) projectlessStillOwned(ctx context.Context, key string) bool {
	if c.projectlessGuard(ctx) != nil {
		return false
	}
	c.projectlessMu.Lock()
	defer c.projectlessMu.Unlock()
	r, ok := c.projectlessRecords[key]
	return ok && r.Owner == c.projectlessOwner(ctx, c.o.Store.Get(), r.Source)
}

func (c *Client) isManagedCwd(cwd string) bool {
	base, _, err := c.projectlessPaths()
	return err == nil && IsWithin(base, cwd, runtime.GOOS == "windows")
}

func (c *Client) managedRecord(sessionKey, cwd string) (projectlessRecord, string, bool) {
	base, _, err := c.projectlessPaths()
	if err != nil || !workspaceExpectedCwdMatches(filepath.Join(base, filepath.Base(cwd)), cwd) {
		return projectlessRecord{}, "", false
	}
	c.projectlessMu.Lock()
	defer c.projectlessMu.Unlock()
	if c.loadProjectlessLocked() != nil {
		return projectlessRecord{}, "", false
	}
	key := filepath.Base(cwd)
	r, ok := c.projectlessRecords[key]
	return r, key, ok && r.State == "bound" && r.SessionKey == sessionKey
}

func (c *Client) validateManagedSession(ctx context.Context, cfg config.Config, key string) error {
	if key == "" || c.o.Store == nil || runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		return nil
	}
	c.projectlessMu.Lock()
	if err := c.loadProjectlessLocked(); err != nil {
		c.projectlessMu.Unlock()
		return err
	}
	var record projectlessRecord
	registered := len(c.projectlessRecords) != 0
	for _, r := range c.projectlessRecords {
		if r.SessionKey == key {
			record = r
			break
		}
	}
	c.projectlessMu.Unlock()
	if record.SessionKey == "" {
		// Ordinary installations retain their existing exact-metadata read
		// counts and never start unrelated adapters just to check this feature.
		if !registered {
			base, _, err := c.projectlessPaths()
			if err != nil {
				return nil
			}
			if _, err = os.Lstat(base); errors.Is(err, os.ErrNotExist) {
				return nil
			}
		}
		// Native sessions can be created outside this API. Merely reporting an
		// internal managed path must not adopt another operation's directory.
		if cwd, err := c.workspaceSessionCwd(ctx, key, "cli"); err == nil && c.isManagedCwd(cwd) {
			// A crash may have left the original native creation unbound.
			// Corroborate its original operation before admitting any command.
			tool, _, _ := strings.Cut(key, ":")
			c.reconcileProjectless(ctx, protocol.SessionInfo{SessionKey: key, Tool: tool, Cwd: cwd, ControlSurface: "cli"}, nil)
			if recovered, _, ok := c.managedRecord(key, cwd); !ok || !c.projectlessOwnerMatches(ctx, cfg, recovered) {
				return errProjectless
			}
			_, err = c.workspaceSessionRoot(ctx, cfg, key, "cli", cwd)
			return err
		}
		return nil
	}
	if !c.projectlessOwnerMatches(ctx, cfg, record) {
		return errProjectless
	}
	cwd, err := c.workspaceSessionCwd(ctx, key, "cli")
	if err != nil {
		return err
	}
	_, err = c.workspaceSessionRoot(ctx, cfg, key, "cli", cwd)
	return err
}

// Only session-bound operations use this extension; project-level operations
// deliberately keep workspaceConfiguredRoot and can never adopt managed cwd.
func (c *Client) workspaceSessionRoot(ctx context.Context, cfg config.Config, key, surface, cwd string) (string, error) {
	if !c.isManagedCwd(cwd) {
		return workspaceConfiguredRoot(cfg, cwd)
	}
	r, dir, ok := c.managedRecord(key, cwd)
	if !ok || surface != "cli" || !c.projectlessOwnerMatches(ctx, cfg, r) {
		return "", errWorkspaceFiles
	}
	if err := c.projectlessGuard(ctx); err != nil {
		return "", err
	}
	base, _, err := c.projectlessPaths()
	if err != nil {
		return "", err
	}
	return verifiedProjectlessDirectory(base, dir, false, &r.DirectoryID)
}

// Internal lifecycle watchers have no incoming phone request. They may retain
// an already admitted exact session root only while its stored owner survives.
func (c *Client) workspaceRetainedRoot(cfg config.Config, key, surface, cwd string) (string, error) {
	if !c.isManagedCwd(cwd) {
		return workspaceConfiguredRoot(cfg, cwd)
	}
	r, dir, ok := c.managedRecord(key, cwd)
	if !ok || surface != "cli" || r.Owner != c.projectlessOwner(context.Background(), cfg, r.Source) {
		return "", errWorkspaceFiles
	}
	base, _, err := c.projectlessPaths()
	if err != nil {
		return "", err
	}
	return verifiedProjectlessDirectory(base, dir, false, &r.DirectoryID)
}

func (c *Client) projectProjectlessSession(info protocol.SessionInfo) protocol.SessionInfo {
	info.Projectless = false
	if r, _, ok := c.managedRecord(info.SessionKey, info.Cwd); ok {
		info.Projectless = true
		info.WorkspaceMode = r.Mode
	}
	return info
}

// ProjectSessionEvent copies metadata; it neither changes ownership nor grants
// access. Both Hub and direct-event boundaries use it after adapter validation.
func (c *Client) ProjectSessionEvent(ev protocol.Event) protocol.Event {
	if ev.Session != nil {
		info := c.projectProjectlessSession(*ev.Session)
		ev.Session = &info
	}
	return ev
}

// Recover the narrow crash window after native creation but before binding the
// returned key. The exact managed cwd and persisted filesystem identity are
// still required. A user-message operation marker is corroborating evidence,
// never a phone authorization, and no task is dispatched while reconciling.
func (c *Client) reconcileProjectless(ctx context.Context, info protocol.SessionInfo, knownEvents []protocol.Event) {
	if !c.isManagedCwd(info.Cwd) || c.projectlessGuard(ctx) != nil {
		return
	}
	base, _, err := c.projectlessPaths()
	if err != nil || !workspaceExpectedCwdMatches(filepath.Join(base, filepath.Base(info.Cwd)), info.Cwd) {
		return
	}
	key := filepath.Base(info.Cwd)
	c.projectlessMu.Lock()
	if c.loadProjectlessLocked() != nil {
		c.projectlessMu.Unlock()
		return
	}
	record, ok := c.projectlessRecords[key]
	c.projectlessMu.Unlock()
	if !ok || record.State != "starting" || record.Owner != c.projectlessOwner(ctx, c.o.Store.Get(), projectlessSource(ctx)) || !stationSwitchOperationID(record.OperationID) {
		return
	}
	m := c.manager()
	if m == nil {
		return
	}
	a, id, err := agentFor(m, info.SessionKey)
	if err != nil || a.ID() != record.Tool || !agents.SessionBelongsTo(a, info.SessionKey, info) {
		return
	}
	if _, err = verifiedProjectlessDirectory(base, key, false, &record.DirectoryID); err != nil {
		return
	}
	if knownEvents == nil {
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var opened protocol.SessionInfo
		if pager, ok := a.(agents.MessageHistoryPager); ok {
			opened, knownEvents, _, err = pager.OpenMessagesPage(readCtx, id, "", 10)
		} else if pager, ok := a.(agents.HistoryPager); ok {
			opened, knownEvents, _, err = pager.OpenPage(readCtx, id, "", maxOpenEvents)
		} else {
			opened, knownEvents, err = a.Open(readCtx, id)
		}
		if err != nil || readCtx.Err() != nil || len(knownEvents) > maxOpenEvents || opened.SessionKey != info.SessionKey || !workspaceExpectedCwdMatches(opened.Cwd, info.Cwd) {
			return
		}
	}
	for _, event := range knownEvents {
		if event.Type == "message" && event.Role == "user" && event.ClientID == record.OperationID && event.ParentID == "" && event.MessageOrigin == nil && (event.SessionKey == "" || event.SessionKey == info.SessionKey) && (event.Tool == "" || event.Tool == record.Tool) {
			if c.projectlessGuard(ctx) != nil || record.Owner != c.projectlessOwner(ctx, c.o.Store.Get(), record.Source) {
				return
			}
			if _, err = verifiedProjectlessDirectory(base, key, false, &record.DirectoryID); err == nil {
				_ = c.bindProjectless(key, info.SessionKey)
			}
			return
		}
	}
}

func (c *Client) projectProjectlessResult(ctx context.Context, result map[string]any) map[string]any {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if info, ok := result["session"].(protocol.SessionInfo); ok {
		events, _ := result["events"].([]protocol.Event)
		c.reconcileProjectless(ctx, info, events)
		result["session"] = c.projectProjectlessSession(info)
	}
	if rows, ok := result["sessions"].([]protocol.SessionInfo); ok {
		copy := append([]protocol.SessionInfo(nil), rows...)
		for i := range copy {
			c.reconcileProjectless(ctx, copy[i], nil)
			copy[i] = c.projectProjectlessSession(copy[i])
		}
		result["sessions"] = copy
	}
	if events, ok := result["events"].([]protocol.Event); ok {
		copy := append([]protocol.Event(nil), events...)
		for i := range copy {
			copy[i] = c.ProjectSessionEvent(copy[i])
		}
		result["events"] = copy
	}
	return result
}

func (c *Client) knownProjectlessReceipt(ctx context.Context, cmd map[string]any) (map[string]any, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if c.projectlessGuard(ctx) != nil {
		return nil, false
	}
	cfg := c.o.Store.Get()
	owner := c.projectlessOwner(ctx, cfg, projectlessSource(ctx))
	key := durableHash([]string{owner, str(cmd, "tool"), str(cmd, "workspaceMode"), str(cmd, "operationId")})
	read := func() (projectlessRecord, bool) {
		c.projectlessMu.Lock()
		defer c.projectlessMu.Unlock()
		if c.loadProjectlessLocked() != nil {
			return projectlessRecord{}, false
		}
		r, ok := c.projectlessRecords[key]
		return r, ok && r.Owner == owner && r.Command == durableHash(cmd)
	}
	record, ok := read()
	if !ok {
		return nil, false
	}
	base, _, err := c.projectlessPaths()
	if err != nil {
		return nil, false
	}
	cwd, err := verifiedProjectlessDirectory(base, key, false, &record.DirectoryID)
	if err != nil {
		return nil, false
	}
	if record.State == "starting" {
		m := c.manager()
		if m == nil {
			return nil, false
		}
		page, err := dispatchSessionsPage(ctx, m, map[string]any{"type": "sessions.list", "tool": record.Tool, "limit": maxSessions})
		if err != nil {
			return nil, false
		}
		rows, _ := page["sessions"].([]protocol.SessionInfo)
		for _, info := range rows {
			if workspaceExpectedCwdMatches(info.Cwd, cwd) {
				c.reconcileProjectless(ctx, info, nil)
			}
		}
		record, ok = read()
	}
	if !ok || record.State != "bound" || c.projectlessGuard(ctx) != nil || owner != c.projectlessOwner(ctx, c.o.Store.Get(), projectlessSource(ctx)) {
		return nil, false
	}
	return map[string]any{"sessionKey": record.SessionKey}, true
}
