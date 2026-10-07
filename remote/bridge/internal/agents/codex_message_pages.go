package agents

import (
	"context"
	"errors"
	"slices"

	"salcara/bridge/internal/protocol"
)

type codexMessageCursor struct {
	Version int    `json:"v"`
	Session string `json:"s"`
	Source  string `json:"c,omitempty"`
	Window  int    `json:"w"`
	Inside  string `json:"i,omitempty"`
	Legacy  bool   `json:"l,omitempty"`
}

func unsupportedHistoryMethod(err error) bool {
	var rpc *rpcError
	return errors.As(err, &rpc) && (rpc.Code == -32601 || rpc.Code == -32602)
}

// Official experimental app-server paging. full is essential: the default
// summary view would silently omit tool outputs and detailed history.
func readCodexTurns(ctx context.Context, connection *rpcConn, id, cursor string, limit int) ([]cxTurn, string, error) {
	var result struct {
		Data       []cxTurn `json:"data"`
		NextCursor string   `json:"nextCursor"`
	}
	params := map[string]any{"threadId": id, "limit": limit, "sortDirection": "desc", "itemsView": "full"}
	if cursor != "" {
		params["cursor"] = cursor
	}
	if err := connection.Call(ctx, "thread/turns/list", params, &result); err != nil {
		return nil, "", err
	}
	if result.Data == nil || len(result.Data) > limit || len(result.NextCursor) > 2048 || cursor != "" && cursor == result.NextCursor {
		return nil, "", errors.New("电脑返回的历史读取位置无效，请刷新")
	}
	seen := map[string]bool{}
	for _, turn := range result.Data {
		if turn.ID == "" || seen[turn.ID] {
			return nil, "", errors.New("电脑返回的历史回合无效")
		}
		seen[turn.ID] = true
	}
	return result.Data, result.NextCursor, nil
}

func (a *codexAgent) openCodexMessagesPage(ctx context.Context, id, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	key := "codex:" + id
	position := codexMessageCursor{Version: 1, Session: key, Window: 1}
	if cursor != "" {
		if readCursor(cursor, &position) != nil || position.Version != 1 || position.Session != key || position.Window < 1 || position.Window > 5 {
			return protocol.SessionInfo{}, nil, "", errors.New("历史读取位置不匹配，请刷新")
		}
	} else if limit > 2 {
		position.Window = 5
	}
	if position.Legacy {
		info, page, inside, err := a.openHistoryPage(ctx, id, position.Inside, limit, paginateMessageHistory)
		if err != nil || inside == "" {
			return info, page, "", err
		}
		position.Inside = inside
		return info, page, cursorValue(position), nil
	}
	connection, err := a.ensure(ctx)
	if err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	var metadata struct {
		Thread cxThread `json:"thread"`
	}
	if err = connection.Call(ctx, "thread/read", map[string]any{"threadId": id, "includeTurns": false}, &metadata); err != nil {
		return protocol.SessionInfo{}, nil, "", err
	}
	if metadata.Thread.ID != id {
		return protocol.SessionInfo{}, nil, "", errors.New("会话身份不一致，已取消读取")
	}
	turns, hostNext, err := readCodexTurns(ctx, connection, id, position.Source, position.Window)
	if err != nil {
		// Only a definite unsupported method/parameter on the first read permits
		// the old full-snapshot path. Never retry auth, network, or host failures.
		if cursor != "" || !unsupportedHistoryMethod(err) {
			return protocol.SessionInfo{}, nil, "", err
		}
		position.Legacy = true
		info, page, inside, legacyErr := a.openHistoryPage(ctx, id, "", limit, paginateMessageHistory)
		if legacyErr != nil || inside == "" {
			return info, page, "", legacyErr
		}
		position.Inside = inside
		return info, page, cursorValue(position), nil
	}
	newest := ""
	if len(turns) > 0 {
		newest = turns[0].ID
	}
	slices.Reverse(turns)
	metadata.Thread.Turns = turns
	a.mu.Lock()
	item := a.applyThreadLocked(metadata.Thread)
	item.opened = true
	if cursor == "" {
		item.seen = map[string]bool{}
		item.tailHistory, item.tailBaselineTurn = true, newest
	}
	if item.seen == nil {
		item.seen = map[string]bool{}
	}
	for _, seen := range historyKeys(metadata.Thread) {
		item.seen[seen] = true
	}
	info := item.info
	a.mu.Unlock()
	page, inside, err := paginateMessageHistory(key, codexHistory(key, metadata.Thread), position.Inside, limit)
	if err != nil {
		return info, nil, "", err
	}
	if cursor == "" {
		pending := a.aps.snapshot(key)
		if len(pending) > 0 {
			info.Status = "waiting_approval"
		}
		page = append(page, pending...)
	}
	if inside == "" && hostNext == "" {
		return info, page, "", nil
	}
	position.Inside = inside
	if inside == "" {
		position.Source, position.Window = hostNext, 5
	}
	next := cursorValue(position)
	if len(next) > 4096 {
		return info, nil, "", errors.New("历史读取位置过长，请刷新")
	}
	return info, page, next, nil
}

// The external-thread watcher starts at the opening's newest turn. It must not
// replay all old turns merely because the phone intentionally fetched only two
// messages. Its bounded read walks back to that known turn, retaining its new
// items. A compacted/missing boundary asks for a fresh authoritative snapshot.
func (a *codexAgent) streamTailItems(ctx context.Context, id, baseline string) {
	connection, err := a.ensure(ctx)
	if err != nil {
		return
	}
	var turns []cxTurn
	cursor, newest, complete := "", "", false
	for n := 0; n < 20; n++ {
		page, next, err := readCodexTurns(ctx, connection, id, cursor, 5)
		if err != nil {
			return
		}
		if newest == "" && len(page) > 0 {
			newest = page[0].ID
		}
		for _, turn := range page {
			turns = append(turns, turn)
			if turn.ID == baseline {
				complete = true
				break
			}
		}
		if complete || next == "" {
			complete = complete || baseline == ""
			break
		}
		cursor = next
	}
	a.mu.Lock()
	t := a.threadLocked(id)
	if !t.tailHistory || t.tailBaselineTurn != baseline {
		a.mu.Unlock()
		return
	}
	if !complete {
		t.tailBaselineTurn = newest
		for _, turn := range turns {
			for i := range turn.Items {
				t.seen[turn.ID+":"+itoa(i)] = true
			}
		}
		a.mu.Unlock()
		a.emit(protocol.Event{SessionKey: "codex:" + id, Type: "notice", ID: "history-gap:codex:" + newest, TS: nowMs(), Level: "warn", Text: "[salcara:history-gap:v1] 正在重新读取电脑上的原会话。"})
		return
	}
	slices.Reverse(turns)
	thread := cxThread{ID: id, Cwd: t.info.Cwd, UpdatedAt: t.info.UpdatedAt / 1000}
	for _, turn := range turns {
		fresh := turn
		fresh.Items = nil
		for i, raw := range turn.Items {
			itemKey := turn.ID + ":" + itoa(i)
			if !t.seen[itemKey] {
				t.seen[itemKey] = true
				fresh.Items = append(fresh.Items, raw)
			}
		}
		// This watcher emits newly observed items only; normal status refresh
		// owns completion status, avoiding historical completion notifications.
		fresh.Status = ""
		thread.Turns = append(thread.Turns, fresh)
	}
	t.tailBaselineTurn = newest
	a.mu.Unlock()
	for _, event := range codexHistory("codex:"+id, thread) {
		a.emit(event)
	}
}
