package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/protocol"
)

const (
	workspaceFileBytes = 128 << 10
	workspaceListPage  = 50
	workspaceListScan  = 4096
)

var errWorkspaceFiles = errors.New("workspace_files_unavailable: 仅可访问电脑已授权项目的文本文件")
var errWorkspaceChanged = errors.New("workspace_files_changed: 项目或目录已变化，请刷新后重试")

type workspaceFileEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
	Size int64  `json:"size,omitempty"`
}

type workspaceFilesPage struct {
	Path       string               `json:"path"`
	Entries    []workspaceFileEntry `json:"entries"`
	NextCursor string               `json:"nextCursor,omitempty"`
	Truncated  bool                 `json:"truncated,omitempty"`
}

type workspaceFileText struct {
	Path       string `json:"path"`
	Text       string `json:"text"`
	Size       int64  `json:"size"`
	ModifiedAt int64  `json:"modifiedAt"`
	Revision   string `json:"revision"`
}

type workspaceFileScope struct {
	key, surface, root, identity, device, binding, phone string
}

type workspaceDirectoryCursor struct {
	Version  int    `json:"v"`
	Offset   int    `json:"o"`
	Revision string `json:"r"`
}

func workspaceDecodeCursor(encoded, revision string) (workspaceDirectoryCursor, error) {
	var result workspaceDirectoryCursor
	if len(encoded) > 512 {
		return result, errWorkspaceChanged
	}
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(raw) > 256 {
		return result, errWorkspaceChanged
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	start, err := dec.Token()
	if err != nil || start != json.Delim('{') {
		return result, errWorkspaceChanged
	}
	seen := map[string]bool{}
	for dec.More() {
		token, err := dec.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return result, errWorkspaceChanged
		}
		seen[name] = true
		switch name {
		case "v":
			err = dec.Decode(&result.Version)
		case "o":
			err = dec.Decode(&result.Offset)
		case "r":
			err = dec.Decode(&result.Revision)
		default:
			return result, errWorkspaceChanged
		}
		if err != nil {
			return result, errWorkspaceChanged
		}
	}
	end, err := dec.Token()
	if err != nil || end != json.Delim('}') || len(seen) != 3 || result.Version != 1 || result.Offset < 1 || result.Offset >= workspaceListScan || result.Revision != revision {
		return result, errWorkspaceChanged
	}
	if _, err := dec.Token(); err != io.EOF {
		return result, errWorkspaceChanged
	}
	return result, nil
}

// This is an independent read-only surface, not an executor or a native-to-CLI
// fallback. A session cwd alone never grants filesystem access: it must exactly
// match a project explicitly saved on this computer.
func (c *Client) dispatchWorkspaceFiles(ctx context.Context, cmd map[string]any) (any, error) {
	if !c.workspaceFilesMu.TryLock() {
		return nil, errors.New("workspace_files_busy: 请稍后重试")
	}
	defer c.workspaceFilesMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	key, ok := cmd["sessionKey"].(string)
	surface, surfaceOK := cmd["controlSurface"].(string)
	expectedCwd, expectedOK := cmd["expectedCwd"].(string)
	if !ok || len(key) > 256 || key == "" || !surfaceOK || (surface != "cli" && surface != "desktop") || !expectedOK || len(expectedCwd) == 0 || len(expectedCwd) > 4096 || !utf8.ValidString(expectedCwd) || c.o.Store == nil {
		return nil, errWorkspaceFiles
	}
	for name, value := range cmd {
		if name != "type" && name != "sessionKey" && name != "controlSurface" && name != "path" && name != "cursor" && name != "expectedCwd" {
			return nil, errWorkspaceFiles
		}
		if _, isString := value.(string); !isString {
			return nil, errWorkspaceFiles
		}
	}
	rel, err := workspaceRelativePath(str(cmd, "path"))
	if err != nil || (str(cmd, "type") == "workspace.files.read" && (rel == "" || str(cmd, "cursor") != "")) {
		return nil, errWorkspaceFiles
	}
	c.taskConfigMu.Lock()
	err = c.validateCommandGuard(ctx, str(cmd, "type"))
	cfg := c.o.Store.Get()
	c.taskConfigMu.Unlock()
	if err != nil {
		return nil, err
	}
	cwd, err := c.workspaceSessionCwd(ctx, key, surface)
	if err != nil {
		return nil, err
	}
	// expectedCwd is only a stale-metadata assertion. Never stat, resolve or
	// select a root from this phone-supplied string. Only adapter cwd below can
	// be authorized against desktop-saved projects and opened by the Bridge.
	if !workspaceExpectedCwdMatches(expectedCwd, cwd) {
		return nil, errWorkspaceChanged
	}
	rootPath, err := workspaceConfiguredRoot(cfg, cwd)
	if err != nil {
		return nil, err
	}
	scope := workspaceFileScope{key, surface, rootPath, config.RemoteIdentity(cfg), cfg.DeviceID, cfg.PhoneBindingID, cfg.PhoneHash}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer root.Close()
	rootHandle, err := root.Open(".")
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer rootHandle.Close()
	if err = workspaceVerifyHandle(rootHandle, rootHandle, rootPath, "", false); err != nil {
		return nil, errWorkspaceFiles
	}
	var result any
	if str(cmd, "type") == "workspace.files.list" {
		result, err = workspaceList(ctx, root, rootHandle, scope, rel, str(cmd, "cursor"))
	} else if str(cmd, "type") == "workspace.files.read" {
		result, err = workspaceRead(ctx, root, rootHandle, scope, rel)
	} else {
		return nil, errWorkspaceFiles
	}
	if err != nil {
		return nil, err
	}
	// Never release a buffered result after a detach, station move, project
	// removal, lease expiration or session cwd change that happened during IO.
	currentCwd, err := c.workspaceSessionCwd(ctx, key, surface)
	if err != nil || !workspaceSamePath(currentCwd, cwd) || !workspaceExpectedCwdMatches(expectedCwd, currentCwd) {
		return nil, errWorkspaceChanged
	}
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	if err = c.validateCommandGuard(ctx, str(cmd, "type")); err != nil {
		return nil, err
	}
	current := c.o.Store.Get()
	currentRoot, err := workspaceConfiguredRoot(current, currentCwd)
	if err != nil || !workspaceSamePath(currentRoot, scope.root) || config.RemoteIdentity(current) != scope.identity || current.DeviceID != scope.device || current.PhoneBindingID != scope.binding || current.PhoneHash != scope.phone || ctx.Err() != nil {
		return nil, errWorkspaceChanged
	}
	currentRootInfo, err := os.Lstat(currentRoot)
	openedRootInfo, openedErr := rootHandle.Stat()
	if err != nil || openedErr != nil || !os.SameFile(currentRootInfo, openedRootInfo) || workspaceVerifyHandle(rootHandle, rootHandle, scope.root, "", false) != nil {
		return nil, errWorkspaceChanged
	}
	return result, nil
}

func workspaceExpectedCwdMatches(expected, actual string) bool {
	if !filepath.IsAbs(expected) || !filepath.IsAbs(actual) {
		return false
	}
	for _, ch := range expected {
		if unicode.IsControl(ch) || unicode.Is(unicode.Cf, ch) {
			return false
		}
	}
	expected, actual = filepath.Clean(expected), filepath.Clean(actual)
	if runtime.GOOS == "windows" && len(expected) >= 2 && len(actual) >= 2 && expected[1] == ':' && actual[1] == ':' {
		// The drive letter is insensitive, but per-directory NTFS case-sensitive
		// projects forbid guessing that A and a identify the same root.
		expected = strings.ToLower(expected[:1]) + expected[1:]
		actual = strings.ToLower(actual[:1]) + actual[1:]
	}
	return expected == actual
}

func (c *Client) workspaceSessionCwd(ctx context.Context, key, surface string) (string, error) {
	var sessions []protocol.SessionInfo
	if surface == "desktop" {
		if c.o.Desktop == nil {
			return "", desktopcompanion.ErrActivationRequired
		}
		st, err := c.o.Desktop.NativeStatus(ctx)
		if err != nil || !desktopcompanion.ValidateNativeConnection(st, nowMS()) || !st.Capabilities.List || !st.Capabilities.Read || !containsKey(st.SessionKeys, key) {
			return "", desktopcompanion.ErrDesktopScope
		}
		sessions, err = c.o.Desktop.NativeList(ctx)
		if err != nil {
			return "", desktopcompanion.ErrDesktopUnavailable
		}
	} else {
		m := c.manager()
		if m == nil {
			return "", errWorkspaceFiles
		}
		a, id, err := agentFor(m, key)
		if err != nil {
			return "", errWorkspaceFiles
		}
		if reader, exact := a.(agents.SessionMetadataReader); exact {
			info, metadataErr := reader.SessionMetadata(ctx, id)
			if metadataErr != nil {
				return "", errWorkspaceFiles
			}
			sessions = []protocol.SessionInfo{info}
		} else {
			// Extension adapters without the optional interface keep their
			// bounded legacy directory. Built-ins never read complete history
			// or downgrade after an exact metadata lookup fails.
			sessions, err = a.Sessions(ctx)
		}
		if err != nil {
			return "", errWorkspaceFiles
		}
	}
	for _, info := range sessions {
		if info.SessionKey != key {
			continue
		}
		if surface == "desktop" && (info.ControlSurface != "desktop" || !info.Controllable || info.ControlExpiresAt <= nowMS()) || surface == "cli" && info.ControlSurface != "" && info.ControlSurface != "cli" {
			return "", errWorkspaceFiles
		}
		if !filepath.IsAbs(info.Cwd) || len(info.Cwd) > 4096 {
			return "", errWorkspaceFiles
		}
		return filepath.Clean(info.Cwd), nil
	}
	return "", errWorkspaceFiles
}

func workspaceSamePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func workspaceConfiguredRoot(cfg config.Config, cwd string) (string, error) {
	if !filepath.IsAbs(cwd) || !workspaceLocalRoot(cwd) {
		return "", errWorkspaceFiles
	}
	st, err := os.Lstat(cwd)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeType != os.ModeDir {
		return "", errWorkspaceFiles
	}
	// Some Windows directories enable case-sensitive names. String EqualFold
	// is only a candidate match, never proof that two names grant the same root.
	allowed := false
	for _, project := range cfg.Projects {
		if !filepath.IsAbs(project.Path) || !workspaceSamePath(project.Path, cwd) {
			continue
		}
		configuredInfo, err := os.Lstat(project.Path)
		if err == nil && configuredInfo.IsDir() && configuredInfo.Mode()&os.ModeType == os.ModeDir && os.SameFile(configuredInfo, st) {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", errWorkspaceFiles
	}
	real, err := filepath.EvalSymlinks(cwd)
	if err != nil || !workspaceLocalRoot(real) || workspaceBroadRoot(real) {
		return "", errWorkspaceFiles
	}
	return real, nil
}

func workspaceBroadRoot(root string) bool {
	if workspaceSamePath(root, filepath.VolumeName(root)+string(filepath.Separator)) {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && IsWithin(root, home, runtime.GOOS == "windows") {
		return true
	}
	for _, env := range []string{"SystemRoot", "ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
		if system := os.Getenv(env); system != "" && IsWithin(system, root, runtime.GOOS == "windows") {
			return true
		}
	}
	for _, part := range strings.Split(filepath.ToSlash(root), "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	if runtime.GOOS == "linux" {
		for _, system := range []string{"/dev", "/proc", "/sys", "/etc", "/usr", "/bin", "/sbin", "/run", "/var"} {
			if IsWithin(system, root, false) {
				return true
			}
		}
	}
	return false
}

func workspaceRelativePath(input string) (string, error) {
	if input == "" {
		return "", nil
	}
	if !utf8.ValidString(input) || len(input) > 1024 || strings.ContainsAny(input, "\\:*?\"<>|\x00") || strings.HasPrefix(input, "/") {
		return "", errWorkspaceFiles
	}
	for _, part := range strings.Split(input, "/") {
		if part == "" || part == "." || part == ".." || len(part) > 255 || strings.TrimSpace(part) != part || strings.HasSuffix(part, ".") || !workspaceVisibleName(part) {
			return "", errWorkspaceFiles
		}
		for _, ch := range part {
			if unicode.IsControl(ch) || unicode.Is(unicode.Cf, ch) {
				return "", errWorkspaceFiles
			}
		}
	}
	return input, nil
}

func workspaceVisibleName(name string) bool {
	lower := strings.ToLower(name)
	if strings.HasPrefix(lower, ".") {
		return false
	}
	device := strings.SplitN(lower, ".", 2)[0]
	if device == "con" || device == "prn" || device == "aux" || device == "nul" || device == "conin$" || device == "conout$" || len(device) == 4 && (strings.HasPrefix(device, "com") || strings.HasPrefix(device, "lpt")) && device[3] >= '1' && device[3] <= '9' {
		return false
	}
	for _, sensitive := range []string{"secret", "credential", "token", "password", "private-key", "apikey", "api-key", "api_key"} {
		if strings.Contains(lower, sensitive) {
			return false
		}
	}
	base := strings.TrimSuffix(lower, path.Ext(lower))
	for _, sensitive := range []string{"auth", "config", "settings", "appsettings", "env", "identity", "service-account", "service_account", "kubeconfig"} {
		if base == sensitive || strings.HasPrefix(base, sensitive+".") || strings.HasPrefix(base, sensitive+"_") || strings.HasPrefix(base, sensitive+"-") {
			return false
		}
	}
	for _, denied := range []string{"auth", "authentication", "authorization", "config", "settings", "id_rsa", "id_ed25519", "known_hosts", "authorized_keys", "key", "keys", "cert", "certificate", "node_modules", "vendor", "dist", "build", "target", "out", "cache", "logs"} {
		if base == denied {
			return false
		}
	}
	return true
}

func workspaceTextName(name string) bool {
	if !workspaceVisibleName(name) {
		return false
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".txt", ".md", ".mdx", ".rst", ".go", ".rs", ".c", ".h", ".cpp", ".hpp", ".cs", ".java", ".kt", ".swift", ".py", ".rb", ".php", ".js", ".jsx", ".ts", ".tsx", ".vue", ".svelte", ".html", ".css", ".scss", ".less", ".json", ".yaml", ".yml", ".toml", ".xml", ".sql", ".sh", ".ps1", ".bat", ".cmd", ".gradle":
		return true
	}
	switch strings.ToLower(name) {
	case "dockerfile", "makefile", "license", "readme":
		return true
	}
	return false
}

// Every component is checked, not just the leaf. os.Root anchors traversal;
// final-handle verification separately rejects in-root aliases into secrets.
func workspaceOpen(root *os.Root, rootHandle *os.File, rootPath, rel string, file bool) (*os.File, error) {
	name := "."
	if rel != "" {
		name = filepath.FromSlash(rel)
	}
	prefix := ""
	if rel != "" {
		for _, part := range strings.Split(rel, "/") {
			prefix = filepath.Join(prefix, part)
			st, err := root.Lstat(prefix)
			if err != nil || (!st.IsDir() && !st.Mode().IsRegular()) || (prefix == name && file == st.IsDir()) {
				return nil, errWorkspaceFiles
			}
		}
	}
	f, err := workspaceRootReadFile(root, name)
	if err != nil {
		return nil, errWorkspaceFiles
	}
	if err = workspaceVerifyHandle(rootHandle, f, rootPath, rel, file); err != nil {
		f.Close()
		return nil, errWorkspaceFiles
	}
	return f, nil
}

func workspaceDirectoryRevision(scope workspaceFileScope, rel string, st os.FileInfo) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%d", scope.root, scope.key, scope.surface, rel, st.ModTime().UnixNano(), st.Size())))
	return hex.EncodeToString(h[:])
}

func workspaceList(ctx context.Context, root *os.Root, rootHandle *os.File, scope workspaceFileScope, rel, cursor string) (workspaceFilesPage, error) {
	page := workspaceFilesPage{Path: rel, Entries: []workspaceFileEntry{}}
	dir, err := workspaceOpen(root, rootHandle, scope.root, rel, false)
	if err != nil {
		return page, err
	}
	defer dir.Close()
	before, err := dir.Stat()
	if err != nil || !before.IsDir() {
		return page, errWorkspaceFiles
	}
	revision := workspaceDirectoryRevision(scope, rel, before)
	position := workspaceDirectoryCursor{Version: 1, Revision: revision}
	if cursor != "" {
		position, err = workspaceDecodeCursor(cursor, revision)
		if err != nil {
			return page, errWorkspaceChanged
		}
	}
	for i := 0; i < position.Offset; i++ {
		if ctx.Err() != nil {
			return page, errWorkspaceChanged
		}
		if _, err = dir.ReadDir(1); err != nil {
			return page, errWorkspaceChanged
		}
	}
	eof := false
	for position.Offset < workspaceListScan && len(page.Entries) < workspaceListPage {
		if ctx.Err() != nil {
			return page, errWorkspaceChanged
		}
		entries, err := dir.ReadDir(1)
		if err == io.EOF {
			eof = true
			break
		}
		if err != nil || len(entries) != 1 {
			return page, errWorkspaceFiles
		}
		position.Offset++
		e := entries[0]
		child := path.Join(rel, e.Name())
		if _, err = workspaceRelativePath(child); err != nil || e.Type()&(os.ModeSymlink|os.ModeIrregular) != 0 {
			continue
		}
		isDir := e.IsDir()
		if !isDir && !workspaceTextName(e.Name()) {
			continue
		}
		f, err := workspaceOpen(root, rootHandle, scope.root, child, !isDir)
		if err != nil {
			continue
		}
		st, err := f.Stat()
		f.Close()
		if err != nil || isDir != st.IsDir() || (!isDir && (!st.Mode().IsRegular() || st.Size() > workspaceFileBytes)) {
			continue
		}
		kind := "file"
		if isDir {
			kind = "directory"
		}
		page.Entries = append(page.Entries, workspaceFileEntry{Name: e.Name(), Path: child, Kind: kind, Size: st.Size()})
	}
	if !eof {
		if _, err = dir.ReadDir(1); err == io.EOF {
			eof = true
		} else if err != nil {
			return page, errWorkspaceFiles
		}
	}
	after, err := dir.Stat()
	if err != nil || revision != workspaceDirectoryRevision(scope, rel, after) || workspaceVerifyHandle(rootHandle, dir, scope.root, rel, false) != nil {
		return page, errWorkspaceChanged
	}
	if !eof && position.Offset < workspaceListScan {
		raw, _ := json.Marshal(position)
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	page.Truncated = !eof && position.Offset >= workspaceListScan
	return page, nil
}

func workspaceRead(ctx context.Context, root *os.Root, rootHandle *os.File, scope workspaceFileScope, rel string) (workspaceFileText, error) {
	result := workspaceFileText{Path: rel}
	if !workspaceTextName(path.Base(rel)) {
		return result, errWorkspaceFiles
	}
	f, err := workspaceOpen(root, rootHandle, scope.root, rel, true)
	if err != nil {
		return result, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > workspaceFileBytes {
		return result, errWorkspaceFiles
	}
	data, err := io.ReadAll(io.LimitReader(f, workspaceFileBytes+1))
	if err != nil || len(data) > workspaceFileBytes || !utf8.Valid(data) || int64(len(data)) != before.Size() || ctx.Err() != nil {
		return result, errWorkspaceFiles
	}
	for _, b := range data {
		if b < 32 && b != '\n' && b != '\r' && b != '\t' || b == 127 {
			return result, errWorkspaceFiles
		}
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || workspaceVerifyHandle(rootHandle, f, scope.root, rel, true) != nil {
		return result, errWorkspaceChanged
	}
	result.Text, result.Size, result.ModifiedAt = string(data), after.Size(), after.ModTime().UnixMilli()
	result.Revision = workspaceFileRevision(data)
	return result, nil
}
