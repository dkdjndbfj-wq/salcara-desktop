package desktopcompanion

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strconv"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/protocol"
)

type nativeMessageCursor struct {
	Version int    `json:"v"`
	Scope   string `json:"s"`
	Source  string `json:"c,omitempty"`
	Turns   int    `json:"t"`
	Inside  string `json:"i,omitempty"`
}

// Keep the unread part of a native turn before advancing its host cursor.
// Cutting a host page down to two messages without this layer loses its tools
// and the earlier messages of that same turn on the next request.
func (s *Service) NativeOpenMessagesPage(ctx context.Context, key, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	if limit < 1 || limit > 10 || len(cursor) > 4096 {
		return protocol.SessionInfo{}, nil, "", ErrDesktopRequestUnsent
	}
	d, err := s.activeDescriptor(ctx)
	if err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	scope := key + ":" + strconv.FormatInt(d.ExpiresAt, 10)
	position := nativeMessageCursor{Version: 1, Scope: scope, Turns: 1}
	if cursor != "" {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(cursor)
		if decodeErr != nil || strictJSON(raw, &position) != nil || position.Version != 1 || position.Scope != scope || position.Turns < 1 || position.Turns > 10 {
			return protocol.SessionInfo{}, nil, "", ErrDesktopRequestUnsent
		}
	} else if limit > 2 {
		position.Turns = 5
	}
	info, events, hostNext, err := s.nativeOpenPage(ctx, key, position.Source, position.Turns, true)
	if err != nil {
		return info, nil, "", err
	}
	// A changed lease must not lend old history or pending approvals new authority.
	if info.ControlExpiresAt != d.ExpiresAt {
		return info, nil, "", ErrDesktopScope
	}
	hooks, history := []protocol.Event{}, []protocol.Event{}
	for _, event := range events {
		if event.Type == "approval.request" {
			hooks = append(hooks, event)
		} else {
			history = append(history, event)
		}
	}
	page, inside, err := agents.PaginateMessageHistory(scope, history, position.Inside, limit)
	if err != nil {
		return info, nil, "", err
	}
	if cursor == "" {
		page = append(page, hooks...)
	}
	if inside == "" && hostNext == "" {
		return info, page, "", nil
	}
	position.Inside = inside
	if inside == "" {
		position.Source, position.Turns = hostNext, 5
	}
	raw, err := json.Marshal(position)
	if err != nil {
		return info, nil, "", err
	}
	next := base64.RawURLEncoding.EncodeToString(raw)
	if len(next) > 4096 {
		return info, nil, "", ErrDesktopUnavailable
	}
	return info, page, next, nil
}
