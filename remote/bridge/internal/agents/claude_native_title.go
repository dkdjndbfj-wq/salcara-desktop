package agents

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
)

var errClaudeTitleUnconfirmed = errors.New("Claude native title exceeds the bounded metadata scan")

const claudeNativeTitleScanBytes = 64 << 20
const claudeNativeTitleLineBytes = 16 << 20

func claudeNativeCustomTitle(line []byte, id string) string {
	if !bytes.Contains(line, []byte(`"custom-title"`)) {
		return ""
	}
	var row struct {
		Type, SessionID, CustomTitle string
	}
	if json.Unmarshal(line, &row) != nil || row.Type != "custom-title" || row.SessionID != id {
		return ""
	}
	return strings.TrimSpace(row.CustomTitle)
}

func readClaudeActionTitle(path, id string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	return readClaudeNativeTitle(f, st, id)
}

// Native custom-title rows are append-only metadata. A backwards scan can stop
// at the newest matching row without decoding the conversation. Fragment lists
// keep a long JSONL row linear in IO/copy cost, with fixed total and row bounds.
// SDK 0.3.300 getSessionInfo alone cannot substitute for this: it falls back to
// a native custom-title.json sidecar that standalone renameSession does not set.
func readClaudeNativeTitle(f *os.File, expected os.FileInfo, id string) (string, error) {
	opened, err := f.Stat()
	if err != nil || !sameClaudeTranscript(expected, opened) {
		return "", errClaudeActionChanged
	}
	title, scanErr := scanClaudeNativeTitle(f, opened.Size(), id)
	current, err := f.Stat()
	pathCurrent, pathErr := os.Stat(f.Name())
	if err != nil || pathErr != nil || !sameClaudeTranscript(opened, current) || !sameClaudeTranscript(current, pathCurrent) {
		return "", errClaudeActionChanged
	}
	return title, scanErr
}

func scanClaudeNativeTitle(f *os.File, size int64, id string) (string, error) {
	var fragments [][]byte
	rowBytes := 0
	consume := func(part []byte, complete bool) (string, error) {
		rowBytes += len(part)
		if rowBytes > claudeNativeTitleLineBytes {
			return "", errClaudeTitleUnconfirmed
		}
		if !complete {
			fragments = append(fragments, part)
			return "", nil
		}
		line := part
		if len(fragments) != 0 {
			line = make([]byte, 0, rowBytes)
			line = append(line, part...)
			for i := len(fragments) - 1; i >= 0; i-- {
				line = append(line, fragments[i]...)
			}
		}
		fragments, rowBytes = nil, 0
		return claudeNativeCustomTitle(line, id), nil
	}
	end, scanned := size, int64(0)
	for end > 0 && scanned < claudeNativeTitleScanBytes {
		length := min(int64(64<<10), end, claudeNativeTitleScanBytes-scanned)
		start := end - length
		data := make([]byte, length)
		n, err := f.ReadAt(data, start)
		if err != nil && err != io.EOF || int64(n) != length {
			return "", errClaudeActionChanged
		}
		for cursor := len(data); cursor > 0; {
			newline := bytes.LastIndexByte(data[:cursor], '\n')
			part := data[newline+1 : cursor]
			complete := newline >= 0 || start == 0
			title, err := consume(part, complete)
			if err != nil || title != "" {
				return title, err
			}
			if newline < 0 {
				break
			}
			cursor = newline
		}
		end, scanned = start, scanned+length
	}
	if end > 0 {
		return "", errClaudeTitleUnconfirmed
	}
	return "", nil
}
