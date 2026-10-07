package agents

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"

	"salcara/bridge/internal/protocol"
)

const (
	maxTailBytes       = 8 << 20
	maxTailEvents      = 16000
	maxTailMappedBytes = 32 << 20
)

// Iterate without bytes.Split's per-row allocation. Even millions of empty
// rows must stay within the input budget, and cancellation is checked per row.
func walkTailLines(ctx context.Context, data []byte, start int64, visit func([]byte, int64) error) error {
	for len(data) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, rest, found := bytes.Cut(data, []byte{'\n'})
		if err := visit(line, start); err != nil {
			return err
		}
		start += int64(len(line) + 1)
		if !found {
			break
		}
		data = rest
	}
	return nil
}

type transcriptTailCursor struct {
	Version   int    `json:"v"`
	Scope     string `json:"s"`
	Start     int64  `json:"b"`
	End       int64  `json:"e"`
	Signature string `json:"f"`
	Inside    string `json:"i,omitempty"`
}

// ReadAt never follows a caller-supplied path. Callers open and verify their
// own namespace first. Cursors bind the scope and byte boundary; newer appends
// do not shift older pages, while truncation/rotation requires a fresh read.
func transcriptBoundary(f *os.File, end int64) (string, error) {
	var sample []byte
	for _, span := range [][2]int64{{0, min(end, 256)}, {max(0, end-256), end}} {
		buf := make([]byte, span[1]-span[0])
		if len(buf) > 0 {
			if _, err := f.ReadAt(buf, span[0]); err != nil {
				return "", err
			}
		}
		sample = append(sample, buf...)
	}
	hash := sha256.Sum256(sample)
	return hex.EncodeToString(hash[:]), nil
}

func readTailRange(ctx context.Context, f *os.File, start, end int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if start < 0 || end < start || end-start > maxTailBytes {
		return nil, errors.New("这一段历史过大，请在电脑查看")
	}
	buf := make([]byte, end-start)
	if len(buf) != 0 {
		if _, err := f.ReadAt(buf, start); err != nil {
			return nil, err
		}
	}
	return buf, ctx.Err()
}

func rootPrompt(line []byte) bool {
	var value claudeLine
	if json.Unmarshal(line, &value) != nil || value.Type != "user" || value.IsMeta || value.IsCompactSummary || value.IsSidechain || value.ParentTU != nil {
		return false
	}
	var message claudeMessage
	if json.Unmarshal(value.Message, &message) != nil {
		return false
	}
	blocks, text := parseBlocks(message.Content)
	for _, block := range blocks {
		if block.Type == "text" {
			text += block.Text
		}
	}
	return realUserText(text)
}

func readClaudeTail(ctx context.Context, f *os.File, st os.FileInfo, scope string, info protocol.SessionInfo, cursor string, limit int, expectedSession string, desktop bool) ([]protocol.Event, string, error) {
	limit = pageLimit(limit, 10)
	position := transcriptTailCursor{Version: 1, Scope: scope, End: st.Size()}
	fixedRange := false
	if cursor != "" {
		if readCursor(cursor, &position) != nil || position.Version != 1 || position.Scope != scope || position.Start < 0 || position.End < position.Start || position.End > st.Size() {
			return nil, "", errors.New("历史读取位置已变化，请刷新")
		}
		actual, err := transcriptBoundary(f, position.End)
		if err != nil || actual != position.Signature {
			return nil, "", errors.New("历史文件已变化，请刷新")
		}
		fixedRange = position.Inside != ""
	}
	var data []byte
	var err error
	if fixedRange {
		data, err = readTailRange(ctx, f, position.Start, position.End)
		if err != nil {
			return nil, "", err
		}
	} else {
		// Grow backwards until a complete turn boundary and enough prompts are
		// present. Mapping starts at a real user prompt, so split assistant block
		// IDs and tool-use/result context remain stable across page sizes.
		for window := int64(64 << 10); ; window = min(window*2, maxTailBytes) {
			start := max(0, position.End-window)
			data, err = readTailRange(ctx, f, start, position.End)
			if err != nil {
				return nil, "", err
			}
			if start > 0 {
				cut := bytes.IndexByte(data, '\n')
				if cut >= 0 {
					start += int64(cut + 1)
					data = data[cut+1:]
				} else {
					data = nil
				}
			}
			firstPrompt, prompts := int64(-1), 0
			err = walkTailLines(ctx, data, 0, func(line []byte, offset int64) error {
				if rootPrompt(line) {
					if firstPrompt < 0 {
						firstPrompt = offset
					}
					prompts++
				}
				return nil
			})
			if err != nil {
				return nil, "", err
			}
			if start == 0 || prompts >= limit+1 {
				if start > 0 && firstPrompt >= 0 {
					start += firstPrompt
					data = data[firstPrompt:]
				}
				position.Start = start
				break
			}
			if window == maxTailBytes {
				return nil, "", errors.New("当前回合历史过大，请在电脑查看")
			}
		}
	}
	mapper := newClaudeMapper(info.SessionKey, info.Cwd)
	all := []protocol.Event{}
	mappedBytes := 0
	err = walkTailLines(ctx, data, position.Start, func(line []byte, offset int64) error {
		var value claudeLine
		if len(line) > 0 && json.Unmarshal(line, &value) == nil {
			for _, id := range []string{value.SessionID, value.SessionIDT} {
				if expectedSession != "" && id != "" && !strings.EqualFold(id, expectedSession) {
					return errors.New("转录与会话编号不匹配")
				}
			}
			if desktop && value.UUID == "" {
				hash := sha256.Sum256(line)
				value.UUID = "desktop-byte-" + strconv.FormatInt(offset, 10) + "-" + hex.EncodeToString(hash[:])
			}
			if desktop && value.Timestamp == "" {
				value.Timestamp = "1970-01-01T00:00:00Z"
			}
			mapped := mapper.entry(&value)
			encoded, marshalErr := json.Marshal(mapped)
			if marshalErr != nil {
				return marshalErr
			}
			mappedBytes += len(encoded)
			if mappedBytes > maxTailMappedBytes || len(all)+len(mapped) > maxTailEvents {
				return errors.New("这一段历史内容过大，请在电脑查看")
			}
			all = append(all, mapped...)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS < all[j].TS })
	page, inside, err := paginateMessageHistory(scope, all, position.Inside, limit)
	if err != nil {
		return nil, "", err
	}
	if inside == "" && position.Start == 0 {
		return page, "", nil
	}
	position.Inside = inside
	if inside == "" {
		position.End, position.Start = position.Start, 0
	}
	position.Signature, err = transcriptBoundary(f, position.End)
	if err != nil {
		return nil, "", err
	}
	return page, cursorValue(position), nil
}

func (h *claudeHistory) openTailPage(ctx context.Context, id, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	path, err := h.path(id)
	if err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	info, ok := h.summary(path, st)
	if !ok {
		return info, nil, "", errors.New("会话摘要不可用")
	}
	events, next, err := readClaudeTail(ctx, f, st, info.SessionKey, info, cursor, limit, id, false)
	if err == nil {
		refs := map[string]string{}
		for _, event := range events {
			if event.Kind == "subagent" && event.ID != "" {
				if child := claudeResultAgentID(event.Output); child != "" && len(refs) < 64 {
					refs[event.ID] = child
				}
			}
		}
		events = append(events, h.childTranscriptEvents(path, info, refs)...)
		sort.SliceStable(events, func(i, j int) bool { return events[i].TS < events[j].TS })
	}
	return info, events, next, err
}
