package agents

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"salcara/bridge/internal/protocol"
)

// SessionMetadataReader is an optional exact-session, read-only lookup. It
// must not read full conversation history, resume a session, emit events or
// change worker ownership. Metadata is not authorization: the caller must
// still validate its session/surface, pairing and explicitly allowed project.
type SessionMetadataReader interface {
	SessionMetadata(context.Context, string) (protocol.SessionInfo, error)
}

var errSessionMetadata = errors.New("session_metadata_unavailable: 无法只读确认这个会话的项目")

func sessionMetadataCwd(cwd string) bool {
	if !filepath.IsAbs(cwd) || len(cwd) > 4096 || !utf8.ValidString(cwd) || strings.TrimSpace(cwd) != cwd {
		return false
	}
	for _, ch := range cwd {
		if unicode.IsControl(ch) {
			return false
		}
	}
	return true
}
