package agents

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	a.reconcileTurnReverts(ctx, id)
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
	connection, err := a.ensureRead(ctx)
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
		item.historyReadVersion++
		item.historyItemDigests, item.historyTurnDigests, _ = codexHistoryDigests(metadata.Thread.Turns)
	}
	if item.seen == nil {
		item.seen = map[string]bool{}
	}
	for _, seen := range historyKeys(metadata.Thread) {
		item.seen[seen] = true
	}
	info := item.info
	a.mu.Unlock()
	page, inside, err := paginateMessageHistory(key, a.historyEvents(key, metadata.Thread), position.Inside, limit)
	if err != nil {
		return info, nil, "", err
	}
	if cursor == "" {
		pending := a.aps.snapshot(key)
		if len(pending) > 0 {
			info.Status = "waiting_approval"
		}
		page = append(page, pending...)
		page = append(page, a.revertedHistoryEvents(id)...)
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

const codexTailItemLimit = 4096
const codexTailByteLimit = 16 << 20

// thread/read's legacy fallback is oldest first and may contain the full
// transcript. Never retain/replay its entire digest index in the background.
func codexHistoryWindow(turns []cxTurn) []cxTurn {
	if len(turns) > 100 {
		return turns[len(turns)-100:]
	}
	return turns
}

// Fingerprints are local-only and never contain raw private tool payloads.
// Stable item IDs survive a host inserting/reordering an earlier item. Older
// malformed items without a usable ID fall back to their bounded turn index.
func codexHistoryItemDigest(turn string, index int, raw json.RawMessage) (string, [32]byte) {
	var identity struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &identity)
	key := turn + ":index:" + itoa(index)
	if validChildID(identity.ID) {
		key = turn + ":id:" + identity.ID
	}
	return key, sha256.Sum256(raw)
}

func codexHistoryTurnDigest(turn cxTurn) [32]byte {
	state := turn
	state.Items = nil
	raw, _ := json.Marshal(state)
	return sha256.Sum256(raw)
}

func codexHistoryDigests(turns []cxTurn) (map[string][32]byte, map[string][32]byte, bool) {
	items, states := map[string][32]byte{}, map[string][32]byte{}
	bytes := 0
	for _, turn := range turns {
		if len(states) >= 100 {
			return items, states, false
		}
		states[turn.ID] = codexHistoryTurnDigest(turn)
		for index, raw := range turn.Items {
			bytes += len(raw)
			if len(items) >= codexTailItemLimit || bytes > codexTailByteLimit {
				return items, states, false
			}
			key, digest := codexHistoryItemDigest(turn.ID, index, raw)
			if previous, exists := items[key]; exists && previous != digest {
				return items, states, false
			}
			items[key] = digest
		}
	}
	return items, states, true
}

func codexChangedHistory(thread cxThread, previousItems, previousTurns, nextTurns map[string][32]byte) cxThread {
	changed := thread
	changed.Turns = nil
	for _, turn := range thread.Turns {
		fresh := turn
		fresh.Items = nil
		for i, raw := range turn.Items {
			key, digest := codexHistoryItemDigest(turn.ID, i, raw)
			if previous, exists := previousItems[key]; !exists || previous != digest {
				fresh.Items = append(fresh.Items, raw)
			}
		}
		// Initial reads seed known historical states. Only an actually changed
		// state can update the stable turn, not invent a fresh task completion.
		if previous, exists := previousTurns[turn.ID]; exists && previous == nextTurns[turn.ID] {
			fresh.Status = ""
		}
		changed.Turns = append(changed.Turns, fresh)
	}
	return changed
}

// The external-thread watcher starts at the opening's newest turn. It must not
// replay all old turns merely because the phone intentionally fetched only two
// messages. Its bounded read walks back to that known turn, retaining its new
// or changed items and actual turn-state transitions. A compacted/missing or
// over-budget boundary asks for a fresh authoritative snapshot, never a silent
// loss or an unbounded background full-history read.
func (a *codexAgent) streamTailItems(ctx context.Context, id, baseline string) {
	a.mu.Lock()
	current := a.threads[id]
	if current == nil || !current.tailHistory || current.tailBaselineTurn != baseline {
		a.mu.Unlock()
		return
	}
	current.historyReadVersion++
	version := current.historyReadVersion
	a.mu.Unlock()
	connection, err := a.ensureRead(ctx)
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
	itemDigests, turnDigests, bounded := codexHistoryDigests(turns)
	a.mu.Lock()
	t := a.threadLocked(id)
	if !t.tailHistory || t.tailBaselineTurn != baseline || t.historyReadVersion != version {
		a.mu.Unlock()
		return
	}
	if !complete || !bounded {
		t.tailBaselineTurn = newest
		t.historyItemDigests, t.historyTurnDigests = itemDigests, turnDigests
		a.mu.Unlock()
		a.emit(protocol.Event{SessionKey: "codex:" + id, Type: "notice", ID: "history-gap:codex:" + newest, TS: nowMs(), Level: "warn", Text: "[salcara:history-gap:v1] 正在重新读取电脑上的原会话。"})
		return
	}
	slices.Reverse(turns)
	thread := codexChangedHistory(cxThread{ID: id, Cwd: t.info.Cwd, UpdatedAt: t.info.UpdatedAt / 1000, Turns: turns}, t.historyItemDigests, t.historyTurnDigests, turnDigests)
	t.tailBaselineTurn = newest
	t.historyItemDigests, t.historyTurnDigests = itemDigests, turnDigests
	a.mu.Unlock()
	for _, event := range a.historyEvents("codex:"+id, thread) {
		a.emit(event)
	}
}
