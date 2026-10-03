package agents

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"salcara/bridge/internal/protocol"
)

var claudeAgentTrailer = regexp.MustCompile(`(?:^|\s)agentId:\s*([A-Za-z0-9_-]{1,128})(?:\s|[.)]|$)`)

func claudeResultAgentID(output string) string {
	m := claudeAgentTrailer.FindStringSubmatch(output)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// Read only an actual locally stored subagent transcript referenced by its parent's Agent
// result. Never use model-supplied output_file paths or expose this as a CLI-resumable session.
func (h *claudeHistory) childTranscriptEvents(parentPath string, info protocol.SessionInfo, refs map[string]string) []protocol.Event {
	root, err := filepath.EvalSymlinks(h.dir)
	if err != nil {
		return nil
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil
	}
	sessionID := strings.TrimPrefix(info.SessionKey, "claude:")
	if !validChildID(sessionID) {
		return nil
	}
	var taskIDs []string
	for id := range refs {
		taskIDs = append(taskIDs, id)
	}
	sort.Strings(taskIDs)
	var evs []protocol.Event
	var budget int64 = 32 << 20
	for _, taskID := range taskIDs {
		agentID := refs[taskID]
		if !validChildID(agentID) || budget <= 0 {
			continue
		}
		candidates := []string{
			filepath.Join(filepath.Dir(parentPath), sessionID, "subagents", "agent-"+agentID+".jsonl"),
			filepath.Join(filepath.Dir(parentPath), "agent-"+agentID+".jsonl"), // older Claude Code versions
		}
		for _, candidate := range candidates {
			path, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				continue
			}
			path, err = filepath.Abs(path)
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(root, path)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				continue
			}
			f, err := os.Open(path)
			if err != nil {
				continue
			}
			st, err := f.Stat()
			if err != nil || !st.Mode().IsRegular() {
				f.Close()
				continue
			}
			limit := int64(2 << 20)
			if budget < limit {
				limit = budget
			}
			budget -= min(st.Size(), limit)
			off := max(int64(0), st.Size()-limit)
			if _, err = f.Seek(off, io.SeekStart); err != nil {
				f.Close()
				continue
			}
			mapper := newClaudeMapper(info.SessionKey, info.Cwd)
			mapper.parentID = taskID
			partial := off > 0
			if partial {
				evs = append(evs, protocol.Event{SessionKey: info.SessionKey, Tool: "claude", Type: "notice", ParentID: taskID, TS: st.ModTime().UnixMilli(), Level: "info", Text: "仅显示子任务最近的记录"})
			}
			readLines(io.LimitReader(f, limit), func(line []byte) bool {
				if partial {
					partial = false
					return true
				}
				var l claudeLine
				if json.Unmarshal(line, &l) != nil {
					return true
				}
				l.IsSidechain = false // this verified file is already scoped to its real parent task
				evs = append(evs, mapper.entry(&l)...)
				if len(evs) > 4*maxHistoryEvents {
					evs = dedupeEvents(evs, 2*maxHistoryEvents)
				}
				return true
			})
			f.Close()
			break
		}
	}
	return evs
}
