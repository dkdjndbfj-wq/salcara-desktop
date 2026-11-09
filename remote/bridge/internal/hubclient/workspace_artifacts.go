package hubclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const artifactMaxBytes = 20 << 20
const artifactChunkBytes = 64 << 10
const artifactPageSize = 50

type artifactDescriptor struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	Revision  string `json:"revision"`
	UpdatedAt int64  `json:"updatedAt"`
}
type artifactChunk struct {
	Artifact   artifactDescriptor `json:"artifact"`
	Encoding   string             `json:"encoding"`
	Content    string             `json:"content"`
	Offset     int                `json:"offset"`
	NextOffset int                `json:"nextOffset"`
}
type artifactPage struct {
	Artifacts  []artifactDescriptor `json:"artifacts"`
	NextCursor string               `json:"nextCursor,omitempty"`
}
type artifactCursor struct {
	Scope    string `json:"scope"`
	History  string `json:"history"`
	Offset   int    `json:"offset"`
	Revision string `json:"revision"`
}

func artifactType(rel string) (kind, mime string) {
	switch strings.ToLower(path.Ext(rel)) {
	case ".png":
		return "image", "image/png"
	case ".jpg", ".jpeg":
		return "image", "image/jpeg"
	case ".webp":
		return "image", "image/webp"
	case ".gif":
		return "image", "image/gif"
	case ".pdf":
		return "pdf", "application/pdf"
	case ".docx":
		return "document", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".xlsx":
		return "spreadsheet", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".pptx":
		return "presentation", "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".html", ".htm":
		return "html", "text/html"
	case ".svg":
		return "svg", "image/svg+xml"
	case ".md":
		return "text", "text/markdown"
	case ".txt":
		return "text", "text/plain"
	case ".json":
		return "text", "application/json"
	}
	return "", ""
}
func artifactEditable(rel string) bool {
	kind, _ := artifactType(rel)
	return kind == "text" || kind == "html" || kind == "svg"
}
func artifactDescribe(rel string, data []byte, modified int64) artifactDescriptor {
	kind, mime := artifactType(rel)
	return artifactDescriptor{ID: "artifact_" + workspaceFileRevision([]byte(rel)), Path: rel, Name: path.Base(rel), Kind: kind, Mime: mime, Size: int64(len(data)), Revision: workspaceFileRevision(data), UpdatedAt: modified}
}

// Content is never selected by an absolute phone path. This is the same opened
// handle boundary as workspace files, with a larger bounded binary reader.
func artifactRead(ctx context.Context, root *os.Root, handle *os.File, scope workspaceFileScope, rel string) (artifactDescriptor, []byte, error) {
	var empty artifactDescriptor
	if kind, _ := artifactType(rel); kind == "" {
		return empty, nil, errWorkspaceFiles
	}
	f, err := workspaceOpen(root, handle, scope.root, rel, true)
	if err != nil {
		return empty, nil, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || before.Size() > artifactMaxBytes {
		return empty, nil, errors.New("artifact_size: 文件最大为 20 MB")
	}
	data, err := io.ReadAll(io.LimitReader(f, artifactMaxBytes+1))
	if err != nil || len(data) > artifactMaxBytes || int64(len(data)) != before.Size() || ctx.Err() != nil {
		return empty, nil, errWorkspaceChanged
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || workspaceVerifyHandle(handle, f, scope.root, rel, true) != nil {
		return empty, nil, errWorkspaceChanged
	}
	if artifactEditable(rel) && !utf8.Valid(data) {
		return empty, nil, errors.New("artifact_encoding: 文本文件不是 UTF-8")
	}
	return artifactDescribe(rel, data, after.ModTime().UnixMilli()), data, nil
}
func artifactTextRead(ctx context.Context, root *os.Root, handle *os.File, scope workspaceFileScope, rel string) (workspaceFileText, error) {
	if !artifactEditable(rel) {
		return workspaceFileText{}, errWorkspaceFiles
	}
	desc, data, err := artifactRead(ctx, root, handle, scope, rel)
	if err != nil {
		return workspaceFileText{}, err
	}
	if len(data) > workspaceFileBytes {
		return workspaceFileText{}, errors.New("artifact_edit_size: 可编辑文本最大为 128 KB")
	}
	return workspaceFileText{Path: rel, Text: string(data), Size: desc.Size, ModifiedAt: desc.UpdatedAt, Revision: desc.Revision}, nil
}

func artifactInteger(cmd map[string]any, name string, fallback, maximum int) (int, error) {
	raw, exists := cmd[name]
	if !exists {
		return fallback, nil
	}
	var value int
	switch n := raw.(type) {
	case int:
		value = n
	case float64:
		if n != float64(int(n)) {
			return 0, errWorkspaceFiles
		}
		value = int(n)
	default:
		return 0, errWorkspaceFiles
	}
	if value < 0 || value > maximum {
		return 0, errWorkspaceFiles
	}
	return value, nil
}

func (c *Client) dispatchWorkspaceArtifacts(ctx context.Context, cmd map[string]any) (any, error) {
	key, surface, expected, action := str(cmd, "sessionKey"), str(cmd, "controlSurface"), str(cmd, "expectedCwd"), str(cmd, "action")
	if c.o.Store == nil || str(cmd, "type") != "workspace.artifacts" || key == "" || len(key) > 256 || (surface != "cli" && surface != "desktop") || expected == "" || len(expected) > 4096 || !utf8.ValidString(expected) {
		return nil, errWorkspaceFiles
	}
	allowed := map[string]bool{"type": true, "action": true, "sessionKey": true, "controlSurface": true, "expectedCwd": true}
	switch action {
	case "list":
		allowed["cursor"] = true
	case "read":
		for _, name := range []string{"path", "offset", "length", "expectedRevision"} {
			allowed[name] = true
		}
	case "save":
		for _, name := range []string{"path", "text", "expectedRevision"} {
			allowed[name] = true
		}
	default:
		return nil, errWorkspaceFiles
	}
	for name, value := range cmd {
		if !allowed[name] {
			return nil, errWorkspaceFiles
		}
		if name != "offset" && name != "length" {
			if _, ok := value.(string); !ok {
				return nil, errWorkspaceFiles
			}
		}
	}
	if surface == "cli" {
		m := c.manager()
		if m == nil {
			return nil, errWorkspaceFiles
		}
		a, _, err := agentFor(m, key)
		if err != nil {
			return nil, err
		}
		if err = requireAgentCapability(a, "read"); err != nil {
			return nil, err
		}
	}
	rel, err := workspaceRelativePath(str(cmd, "path"))
	if err != nil || action != "list" && rel == "" {
		return nil, errWorkspaceFiles
	}
	if action == "save" {
		if _, present := cmd["text"]; !present {
			return nil, errWorkspaceFiles
		}
		if !artifactEditable(rel) {
			return nil, errors.New("artifact_read_only: 此文件只能查看或下载")
		}
		result, err := c.dispatchWorkspaceTextWrite(ctx, map[string]any{"type": "workspace.files.write", "sessionKey": key, "controlSurface": surface, "expectedCwd": expected, "path": rel, "text": str(cmd, "text"), "expectedRevision": str(cmd, "expectedRevision")}, artifactTextRead)
		if err != nil {
			return nil, err
		}
		saved := result.(workspaceFileText)
		// The verified digest is the save receipt. Do not echo text: Go's JSON
		// HTML escaping can expand an otherwise valid document past relay limits.
		return map[string]any{"artifact": artifactDescribe(rel, []byte(saved.Text), saved.ModifiedAt)}, nil
	}
	if !c.workspaceFilesMu.TryLock() {
		return nil, errors.New("workspace_files_busy: 请稍后重试")
	}
	defer c.workspaceFilesMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c.taskConfigMu.Lock()
	err = c.validateCommandGuard(ctx, "workspace.artifacts")
	cfg := c.o.Store.Get()
	c.taskConfigMu.Unlock()
	if err != nil {
		return nil, err
	}
	cwd, err := c.workspaceSessionCwd(ctx, key, surface)
	if err != nil {
		return nil, err
	}
	if !workspaceExpectedCwdMatches(expected, cwd) {
		return nil, errWorkspaceChanged
	}
	rootPath, err := c.workspaceSessionRoot(ctx, cfg, key, surface, cwd)
	if err != nil {
		return nil, err
	}
	scope := workspaceFileScope{key, surface, rootPath, config.RemoteIdentity(cfg), cfg.DeviceID, cfg.PhoneBindingID, cfg.PhoneHash}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer root.Close()
	handle, err := root.Open(".")
	if err != nil {
		return nil, errWorkspaceFiles
	}
	defer handle.Close()
	if workspaceVerifyHandle(handle, handle, rootPath, "", false) != nil {
		return nil, errWorkspaceFiles
	}
	var result any
	if action == "list" {
		result, err = c.artifactList(ctx, root, handle, scope, str(cmd, "cursor"))
	} else {
		offset, offsetErr := artifactInteger(cmd, "offset", 0, artifactMaxBytes)
		length, lengthErr := artifactInteger(cmd, "length", artifactChunkBytes, artifactChunkBytes)
		if offsetErr != nil || lengthErr != nil || length < 1 {
			return nil, errWorkspaceFiles
		}
		revision := str(cmd, "expectedRevision")
		if offset > 0 && revision == "" {
			return nil, errWorkspaceChanged
		}
		var desc artifactDescriptor
		var data []byte
		desc, data, err = artifactRead(ctx, root, handle, scope, rel)
		if err == nil {
			if revision != "" && revision != desc.Revision || offset > len(data) {
				return nil, errWorkspaceFileConflict
			}
			end := min(offset+length, len(data))
			result = artifactChunk{desc, "base64", base64.StdEncoding.EncodeToString(data[offset:end]), offset, end}
		}
	}
	if err != nil {
		return nil, err
	}
	// Revalidate after history and disk IO; a detached phone or switched project
	// must never receive buffered bytes from the old owner.
	currentCwd, err := c.workspaceSessionCwd(ctx, key, surface)
	if err != nil || !workspaceExpectedCwdMatches(expected, currentCwd) || !workspaceSamePath(cwd, currentCwd) {
		return nil, errWorkspaceChanged
	}
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	if err = c.validateCommandGuard(ctx, "workspace.artifacts"); err != nil {
		return nil, err
	}
	current := c.o.Store.Get()
	currentRoot, err := c.workspaceSessionRoot(ctx, current, key, surface, currentCwd)
	if err != nil || !workspaceSamePath(currentRoot, scope.root) || config.RemoteIdentity(current) != scope.identity || current.DeviceID != scope.device || current.PhoneBindingID != scope.binding || current.PhoneHash != scope.phone || ctx.Err() != nil {
		return nil, errWorkspaceChanged
	}
	currentInfo, err := os.Lstat(currentRoot)
	opened, openErr := handle.Stat()
	if err != nil || openErr != nil || !os.SameFile(currentInfo, opened) || workspaceVerifyHandle(handle, handle, scope.root, "", false) != nil {
		return nil, errWorkspaceChanged
	}
	return result, nil
}

var artifactMarkdownLink = regexp.MustCompile(`!?\[[^\]\r\n]*\]\(\s*(?:<([^>\r\n]+)>|([^\s)]+))(?:\s+"[^"\r\n]*")?\s*\)`)

// Only explicit file links and native file-change paths count as artifacts.
// A folder mention or a tool's arbitrary stdout never triggers a disk scan.
func artifactEventPaths(events []protocol.Event, scope workspaceFileScope) []string {
	paths := []string{}
	seen := map[string]bool{}
	add := func(raw string) {
		if len(raw) > 8192 {
			return
		}
		raw = strings.TrimSpace(raw)
		if strings.Contains(raw, "://") {
			return
		}
		if decoded, err := url.PathUnescape(raw); err == nil {
			raw = decoded
		} else {
			return
		}
		if filepath.IsAbs(raw) {
			rel, err := filepath.Rel(scope.root, raw)
			if err != nil {
				return
			}
			raw = filepath.ToSlash(rel)
		}
		if strings.HasPrefix(raw, "./") {
			raw = strings.TrimPrefix(raw, "./")
		}
		rel, err := workspaceRelativePath(raw)
		if err != nil || rel == "" || seen[rel] {
			return
		}
		if kind, _ := artifactType(rel); kind == "" {
			return
		}
		seen[rel] = true
		paths = append(paths, rel)
	}
	for _, event := range events {
		if event.SessionKey != "" && event.SessionKey != scope.key || event.ParentID != "" {
			continue
		}
		if event.Type == "message" && (event.Role == "assistant" || event.Role == "user") {
			for _, match := range artifactMarkdownLink.FindAllStringSubmatch(event.Text, -1) {
				if match[1] != "" {
					add(match[1])
				} else {
					add(match[2])
				}
			}
		}
		if event.Type == "tool" && event.Kind == "file_change" {
			for _, line := range strings.Split(event.Detail, "\n") {
				add(line)
			}
			for _, line := range strings.Split(event.Diff, "\n") {
				for _, prefix := range []string{"+++ b/", "--- a/", "*** Add File: ", "*** Update File: "} {
					if strings.HasPrefix(line, prefix) {
						add(strings.TrimPrefix(line, prefix))
					}
				}
			}
		}
	}
	return paths
}

func (c *Client) artifactList(ctx context.Context, root *os.Root, handle *os.File, scope workspaceFileScope, cursor string) (artifactPage, error) {
	result := artifactPage{Artifacts: []artifactDescriptor{}}
	scopeID := workspaceFileRevision([]byte(strings.Join([]string{scope.key, scope.surface, scope.root, scope.identity, scope.device, scope.binding, scope.phone}, "\x00")))
	position := artifactCursor{Scope: scopeID}
	if cursor != "" {
		if len(cursor) > 8192 {
			return result, errWorkspaceChanged
		}
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &position) != nil || position.Scope != scopeID || position.Offset < 0 || position.Offset > 4096 || len(position.History) > 4096 {
			return result, errWorkspaceChanged
		}
	}
	var events []protocol.Event
	var next string
	if scope.surface == "desktop" {
		raw, err := c.dispatchDesktop(ctx, map[string]any{"type": "desktop.session.open", "sessionKey": scope.key, "controlSurface": "desktop", "cursor": position.History, "limit": 10})
		if err != nil {
			return result, err
		}
		page, ok := raw.(map[string]any)
		if !ok {
			return result, errWorkspaceChanged
		}
		events, _ = page["events"].([]protocol.Event)
		next, _ = page["nextCursor"].(string)
	} else {
		page, err := dispatchHistoryPageWithAttachments(ctx, c.manager(), map[string]any{"type": "session.open", "sessionKey": scope.key, "cursor": position.History, "limit": 40}, c.attachmentsFor(ctx))
		if err != nil {
			return result, err
		}
		events, _ = page["events"].([]protocol.Event)
		next, _ = page["nextCursor"].(string)
	}
	paths := artifactEventPaths(events, scope)
	if len(paths) > 4096 {
		return result, errors.New("artifact_list_size: 这一页的文件链接过多")
	}
	encoded, _ := json.Marshal(paths)
	revision := workspaceFileRevision(encoded)
	if position.Offset > len(paths) || position.Offset > 0 && position.Revision != revision {
		return result, errWorkspaceChanged
	}
	index := position.Offset
	for ; index < len(paths) && len(result.Artifacts) < artifactPageSize; index++ {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		desc, _, err := artifactRead(ctx, root, handle, scope, paths[index])
		if err == nil {
			result.Artifacts = append(result.Artifacts, desc)
		}
	}
	if index < len(paths) {
		position.Offset = index
		position.Revision = revision
	} else if next != "" {
		position.History = next
		position.Offset = 0
		position.Revision = ""
	} else {
		return result, nil
	}
	encoded, _ = json.Marshal(position)
	result.NextCursor = base64.RawURLEncoding.EncodeToString(encoded)
	return result, nil
}
