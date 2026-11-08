package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"salcara/bridge/internal/protocol"
)

const (
	sessionMetadataHeadBytes = 64 << 10
	sessionMetadataTailBytes = 256 << 10
)

// SessionMetadata reads bounded metadata spans of exactly one transcript.
// It does not map message content, populate history caches, start a process or
// broaden the trusted transcript namespace. A live-only session is accepted
// solely when no transcript exists; unreadable/invalid transcripts fail closed.
func (a *claudeAgent) SessionMetadata(ctx context.Context, id string) (protocol.SessionInfo, error) {
	if err := ctx.Err(); err != nil {
		return protocol.SessionInfo{}, err
	}
	if !validChildID(id) {
		return protocol.SessionInfo{}, errSessionMetadata
	}
	a.mu.Lock()
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return protocol.SessionInfo{}, errSessionMetadata
	}
	transcript, err := a.hist.path(id)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return protocol.SessionInfo{}, errSessionMetadata
		}
		a.mu.Lock()
		info, exists := a.live[id]
		current := !a.closed
		a.mu.Unlock()
		if err = ctx.Err(); err != nil {
			return protocol.SessionInfo{}, err
		}
		if !current || !exists || info.SessionKey != "claude:"+id || !sessionMetadataCwd(info.Cwd) {
			return protocol.SessionInfo{}, errSessionMetadata
		}
		return protocol.SessionInfo{SessionKey: info.SessionKey, Tool: "claude", Client: info.Client, ControlSurface: "cli",
			Cwd: filepath.Clean(info.Cwd), UpdatedAt: info.UpdatedAt, Status: info.Status, Controllable: false}, nil
	}
	if err = ctx.Err(); err != nil {
		return protocol.SessionInfo{}, err
	}
	before, err := os.Stat(transcript)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 0 || !a.hist.safeTranscriptPath(transcript) {
		return protocol.SessionInfo{}, errSessionMetadata
	}
	f, err := os.Open(transcript)
	if err != nil {
		return protocol.SessionInfo{}, errSessionMetadata
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) || !opened.Mode().IsRegular() {
		return protocol.SessionInfo{}, errSessionMetadata
	}
	info, err := readClaudeSessionMetadata(ctx, f, opened, id)
	if err != nil {
		return protocol.SessionInfo{}, err
	}
	final, err := f.Stat()
	pathFinal, pathErr := os.Stat(transcript)
	if err != nil || pathErr != nil || !os.SameFile(opened, final) || !os.SameFile(final, pathFinal) || opened.Size() != final.Size() || !opened.ModTime().Equal(final.ModTime()) || !a.hist.safeTranscriptPath(transcript) {
		return protocol.SessionInfo{}, errSessionMetadata
	}
	a.mu.Lock()
	current := !a.closed
	a.mu.Unlock()
	if err = ctx.Err(); err != nil {
		return protocol.SessionInfo{}, err
	}
	if !current {
		return protocol.SessionInfo{}, errSessionMetadata
	}
	return info, nil
}

func readClaudeSessionMetadata(ctx context.Context, f *os.File, stat os.FileInfo, id string) (protocol.SessionInfo, error) {
	info := protocol.SessionInfo{SessionKey: "claude:" + id, Tool: "claude", Client: "Claude Code", ControlSurface: "cli",
		UpdatedAt: stat.ModTime().UnixMilli(), Status: "idle", Controllable: false}
	entrypoint := ""
	visit := func(data []byte, skipFirst bool) error {
		for len(data) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			line, rest, found := bytes.Cut(data, []byte{'\n'})
			data = rest
			if skipFirst {
				skipFirst = false
				continue
			}
			var row struct {
				SessionID  string  `json:"sessionId"`
				StreamID   string  `json:"session_id"`
				Cwd        string  `json:"cwd"`
				Entrypoint string  `json:"entrypoint"`
				Sidechain  bool    `json:"isSidechain"`
				ParentTool *string `json:"parent_tool_use_id"`
			}
			if json.Unmarshal(line, &row) != nil || row.Sidechain || row.ParentTool != nil || row.Cwd == "" && row.Entrypoint == "" {
				if !found {
					break
				}
				continue
			}
			if row.SessionID != "" && row.SessionID != id || row.StreamID != "" && row.StreamID != id {
				return errSessionMetadata
			}
			if row.Cwd != "" {
				if !sessionMetadataCwd(row.Cwd) {
					return errSessionMetadata
				}
				info.Cwd = filepath.Clean(row.Cwd)
			}
			if row.Entrypoint != "" {
				entrypoint = row.Entrypoint
			}
			if !found {
				break
			}
		}
		return nil
	}
	readSpan := func(start, size int64) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data := make([]byte, size)
		if size > 0 {
			if n, err := f.ReadAt(data, start); err != nil && err != io.EOF || int64(n) != size {
				return nil, errSessionMetadata
			}
		}
		return data, ctx.Err()
	}
	head, err := readSpan(0, min(stat.Size(), sessionMetadataHeadBytes))
	if err != nil {
		return protocol.SessionInfo{}, err
	}
	if err = visit(head, false); err != nil {
		return protocol.SessionInfo{}, err
	}
	if stat.Size() > sessionMetadataHeadBytes {
		start := max(0, stat.Size()-sessionMetadataTailBytes)
		tail, err := readSpan(start, stat.Size()-start)
		if err != nil {
			return protocol.SessionInfo{}, err
		}
		if err = visit(tail, start > 0); err != nil {
			return protocol.SessionInfo{}, err
		}
	}
	if info.Cwd == "" {
		return protocol.SessionInfo{}, errSessionMetadata
	}
	info.Client = claudeClient(entrypoint)
	return info, nil
}
