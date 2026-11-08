package hubclient

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const maxSideQuestionHistories = 64

type sideQuestionHistory struct {
	options  *protocol.SideQuestionInputOptions
	scope    string
	snapshot agents.SideQuestionSnapshot
	at       time.Time
}
type sideQuestionCache struct {
	mu        sync.Mutex
	sequence  uint64
	tickets   map[string]uint64
	histories map[string]sideQuestionHistory
	catalogs  map[string]sideQuestionHistory
}

func sideQuestionCommand(typ string) bool {
	return typ == "session.side-question.catalog" || typ == "session.side-question.ask" || typ == "session.side-question.tools" || persistentSideQuestionCommand(typ)
}

func persistentSideQuestionCommand(typ string) bool {
	switch typ {
	case "session.side-question.open", "session.side-question.read", "session.side-question.send", "session.side-question.delete":
		return true
	}
	return false
}
func cloneSideSnapshot(snapshot agents.SideQuestionSnapshot) agents.SideQuestionSnapshot {
	snapshot.Messages = append([]agents.SideQuestionMessage(nil), snapshot.Messages...)
	return snapshot
}
func (c *Client) beginSideQuestionHistory(key string) uint64 {
	c.sideQuestions.mu.Lock()
	defer c.sideQuestions.mu.Unlock()
	if c.sideQuestions.tickets == nil {
		c.sideQuestions.tickets = map[string]uint64{}
	}
	if len(c.sideQuestions.tickets) >= 256 {
		c.sideQuestions.tickets = map[string]uint64{}
	}
	c.sideQuestions.sequence++
	c.sideQuestions.tickets[key] = c.sideQuestions.sequence
	return c.sideQuestions.sequence
}
func sideSnapshotScope(ctx context.Context, cfg config.Config, a agents.Agent, id string) string {
	return featureCatalogScope(ctx, cfg, a, "", "", id)
}
func (c *Client) rememberSideQuestionHistory(ctx context.Context, cfg config.Config, cmd, result map[string]any, ticket uint64) {
	key := str(cmd, "sessionKey")
	a, id, err := agentFor(c.manager(), key)
	if err != nil || requireAgentCapability(a, "sideQuestion") != nil || ctx.Err() != nil {
		return
	}
	info, ok := result["session"].(protocol.SessionInfo)
	if !ok || info.Archived || info.ControlSurface == "desktop" || info.ControlSurface == "read-only" {
		return
	}
	events, ok := result["events"].([]protocol.Event)
	if !ok {
		return
	}
	scope := sideSnapshotScope(ctx, cfg, a, id)
	if c.o.Store != nil && scope != sideSnapshotScope(ctx, executionConfig(ctx, c.o.Store.Get()), a, id) {
		return
	}
	snapshot := agents.SideQuestionSnapshot{Truncated: str(result, "nextCursor") != ""}
	positions := map[string]int{}
	for _, event := range events {
		if event.Type != "message" || event.ParentID != "" || event.Role != "user" && event.Role != "assistant" || strings.TrimSpace(event.Text) == "" {
			continue
		}
		if event.SessionKey != key || !utf8.ValidString(event.Text) || len(event.Text) > 64<<10 || !agents.ValidTurnID(event.ID) {
			snapshot.Truncated = true
			continue
		}
		message := agents.SideQuestionMessage{ID: event.ID, Role: event.Role, Text: event.Text}
		if index, exists := positions[event.ID]; exists {
			snapshot.Messages[index] = message
		} else {
			positions[event.ID] = len(snapshot.Messages)
			snapshot.Messages = append(snapshot.Messages, message)
		}
	}
	c.sideQuestions.mu.Lock()
	defer c.sideQuestions.mu.Unlock()
	if c.sideQuestions.tickets[key] != ticket {
		return
	}
	delete(c.sideQuestions.tickets, key)
	if c.sideQuestions.histories == nil {
		c.sideQuestions.histories = map[string]sideQuestionHistory{}
	}
	if prior, ok := c.sideQuestions.histories[key]; ok && prior.scope == scope && str(cmd, "cursor") != "" {
		// Earlier pages precede the current page. Existing IDs retain the newer
		// snapshot returned by the most recent-page read.
		ids := map[string]bool{}
		for _, message := range prior.snapshot.Messages {
			ids[message.ID] = true
		}
		older := []agents.SideQuestionMessage{}
		for _, message := range snapshot.Messages {
			if !ids[message.ID] {
				older = append(older, message)
			}
		}
		snapshot.Messages = append(older, prior.snapshot.Messages...)
		snapshot.Truncated = snapshot.Truncated || prior.snapshot.Truncated
	}
	total := 0
	for _, message := range snapshot.Messages {
		total += len(message.Text)
	}
	for len(snapshot.Messages) > 80 || total > 64<<10 {
		total -= len(snapshot.Messages[0].Text)
		snapshot.Messages = snapshot.Messages[1:]
		snapshot.Truncated = true
	}
	data, _ := json.Marshal(snapshot)
	sum := sha256.Sum256(append([]byte(scope), data...))
	snapshot.Revision = hex.EncodeToString(sum[:])
	if len(c.sideQuestions.histories) >= maxSideQuestionHistories {
		evictSideHistory(c.sideQuestions.histories)
	}
	c.sideQuestions.histories[key] = sideQuestionHistory{scope: scope, snapshot: cloneSideSnapshot(snapshot), at: time.Now()}
}
func evictSideHistory(values map[string]sideQuestionHistory) {
	oldest := ""
	var at time.Time
	for key, value := range values {
		if oldest == "" || value.at.Before(at) {
			oldest, at = key, value.at
		}
	}
	delete(values, oldest)
}
func (c *Client) dispatchSideQuestion(ctx context.Context, cfg config.Config, cmd map[string]any) (any, error) {
	persistent := persistentSideQuestionCommand(str(cmd, "type"))
	mutate := str(cmd, "type") == "session.side-question.ask" || str(cmd, "type") == "session.side-question.send"
	invalid := errors.New("side_question_invalid: 侧边提问参数无效")
	for field, raw := range cmd {
		switch field {
		case "type", "tool", "agent", "sessionKey", "accountId", "controlSurface":
			if text, ok := raw.(string); !ok || len(text) > 1024 {
				return nil, invalid
			}
		case "question", "expectedRevision", "operationId", "previousOperationId":
			if text, ok := raw.(string); (!mutate && !(str(cmd, "type") == "session.side-question.delete" && field == "operationId")) || !ok || len(text) > 16000 {
				return nil, invalid
			}
		case "chainId", "cursor":
			if text, ok := raw.(string); !persistent || !ok || len(text) > 128 {
				return nil, invalid
			}
		case "selection":
			if _, ok := raw.(map[string]any); !mutate || !ok {
				return nil, invalid
			}
		case "model", "effort", "attachments":
			if !mutate || persistent {
				return nil, invalid
			}
		default:
			return nil, invalid
		}
	}
	if str(cmd, "controlSurface") != "cli" {
		return nil, errors.New("side_question_unavailable: 当前控制方式不支持侧边提问")
	}
	a, id, err := agentFor(c.manager(), str(cmd, "sessionKey"))
	if err != nil {
		return nil, err
	}
	if err := validateAdapterCommand(a, cmd); err != nil {
		return nil, err
	}
	if err := requireAgentCapability(a, "sideQuestion"); err != nil {
		return nil, err
	}
	var request protocol.SideQuestionRequest
	data, err := json.Marshal(cmd)
	if err != nil || len(data) > 64<<10 || json.Unmarshal(data, &request) != nil {
		return nil, invalid
	}
	if mutate && !agents.ValidSideQuestionRequest(str(cmd, "sessionKey"), request) {
		return nil, invalid
	}
	scope := sideSnapshotScope(ctx, cfg, a, id)
	admitted := false
	guard := func() error {
		// Admission is serialized with API changes, but the HTTP response does
		// not hold this lock and cannot prevent main-turn steering or stopping.
		c.taskConfigMu.Lock()
		defer c.taskConfigMu.Unlock()
		if !admitted {
			if err := c.checkUpdateCommand(cmd); err != nil {
				return err
			}
			admitted = true
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		c.expireUpdateLocked()
		if c.updatePrepared {
			return errors.New("程序正在更新")
		}
		if err := c.validateCommandGuard(ctx, str(cmd, "type")); err != nil {
			return err
		}
		current := cfg
		if c.o.Store != nil {
			current = executionConfig(ctx, c.o.Store.Get())
		}
		if env, ok := executionSourceFromContext(ctx); ok {
			if err := c.validateExecutionSourceFor(env, current); err != nil {
				return err
			}
			if err := c.bindExecutionStation(ctx, env); err != nil {
				return err
			}
		}
		manager := c.manager()
		if manager == nil || manager.Get(a.ID()) != a || scope != sideSnapshotScope(ctx, current, a, id) {
			return errors.New("side_question_source_changed: API 或连接已经变化")
		}
		if err := c.validateExecutionAccountFor(ctx, current, a.ID(), str(cmd, "sessionKey"), str(cmd, "accountId")); err != nil {
			return err
		}
		return requireAgentCapability(a, "sideQuestion")
	}
	if err := guard(); err != nil {
		return nil, err
	}
	if mutate && !persistent {
		model, effort, ids, err := turnFields(cmd)
		if err != nil || !protocol.ValidModelEffort(effort) {
			return nil, invalid
		}
		if model != "" {
			if _, err := c.validateManagedModel(ctx, cfg, c.apiFamilyForAgent(a.ID()), model); err != nil {
				return nil, err
			}
		}
		if err := validateModelEffort(ctx, cfg, a, model, effort); err != nil {
			return nil, err
		}
		if err := validateModelImages(ctx, cfg, a, model, id, len(ids)); err != nil {
			return nil, err
		}
		request.Model, request.Effort, request.Attachments = model, effort, ids
		images, err := c.attachmentsFor(ctx).resolve(ids)
		if err != nil {
			return nil, err
		}
		total := 0
		for _, path := range images {
			data, err := readAttachmentFile(path, 384<<10)
			if err != nil {
				return nil, errors.New("side_question_limit: 临时聊天图片总大小请控制在 384 KB 内")
			}
			mime := ""
			for candidate, ext := range attachmentExt {
				if strings.HasSuffix(path, ext) && imageMagic(data, candidate) {
					mime = candidate
					break
				}
			}
			if mime == "" {
				return nil, invalid
			}
			encoded := base64.StdEncoding.EncodeToString(data)
			total += len(encoded)
			if total > 512<<10 {
				return nil, errors.New("side_question_limit: 临时聊天图片总大小请控制在 384 KB 内")
			}
			request.Images = append(request.Images, protocol.SideQuestionImage{Mime: mime, Data: encoded})
		}
		if err := guard(); err != nil {
			return nil, err
		}
	}
	if persistent {
		provider, ok := a.(agents.SideQuestionConversationProvider)
		if !ok {
			return nil, errors.New("side_question_unavailable: 当前运行时不支持保存侧边聊天")
		}
		var request protocol.SideQuestionConversationRequest
		if json.Unmarshal(data, &request) != nil {
			return nil, invalid
		}
		request.Action = strings.TrimPrefix(str(cmd, "type"), "session.side-question.")
		if !agents.ValidControlOperationID(request.ChainID) || request.Cursor != "" && request.Action != "read" {
			return nil, invalid
		}
		var snapshot agents.SideQuestionSnapshot
		if request.Action == "open" {
			c.sideQuestions.mu.Lock()
			entry, exists := c.sideQuestions.histories[str(cmd, "sessionKey")]
			if exists && entry.scope == scope {
				snapshot = cloneSideSnapshot(entry.snapshot)
			}
			c.sideQuestions.mu.Unlock()
		}
		// Durable ownership excludes runtime authorization epochs. Every access
		// above is freshly guarded; repairing/restarting a connection does not
		// erase history, while a new phone binding cannot inherit old chains.
		ownerData, _ := json.Marshal([]string{cfg.ComputerID, cfg.PhoneBindingID, cfg.PhoneHash, a.ID(), str(cmd, "sessionKey")})
		ownerHash := sha256.Sum256(ownerData)
		value, err := provider.SideQuestionConversation(ctx, id, hex.EncodeToString(ownerHash[:]), snapshot, request, guard)
		if err != nil {
			return nil, err
		}
		if err := guard(); err != nil {
			return nil, err
		}
		if !validSideConversationResponse(value, str(cmd, "sessionKey"), request) {
			return nil, invalid
		}
		return value, nil
	}
	c.sideQuestions.mu.Lock()
	entry, ok := c.sideQuestions.histories[str(cmd, "sessionKey")]
	if mutate {
		entry, ok = c.sideQuestions.catalogs[scope+"\x00"+request.ExpectedRevision]
	}
	if ok {
		entry.snapshot = cloneSideSnapshot(entry.snapshot)
	}
	c.sideQuestions.mu.Unlock()
	if !ok || entry.scope != scope || time.Since(entry.at) > 20*time.Minute || len(entry.snapshot.Messages) == 0 {
		if mutate {
			return nil, errors.New("side_question_stale: 侧边对话上下文已失效")
		}
		return protocol.SideQuestionCatalog{SessionKey: str(cmd, "sessionKey"), ContextScope: "loadedSnapshot", UnavailableReason: "尚无已加载的对话内容"}, nil
	}
	if mutate && (request.Model != "" && (entry.options == nil || !entry.options.Model) || request.Effort != "" && (entry.options == nil || !entry.options.Effort) || len(request.Attachments) > 0 && (entry.options == nil || !entry.options.Images)) {
		return nil, errors.New("side_question_unavailable: 当前临时聊天没有提供所选输入能力")
	}
	provider := a.(agents.SideQuestionProvider)
	if !mutate {
		// Each opened panel owns a new ephemeral chain even if the parent text
		// has not changed. Subsequent questions keep this exact snapshot.
		var nonce [16]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			return nil, err
		}
		digest := sha256.Sum256(append([]byte(entry.snapshot.Revision), nonce[:]...))
		entry.snapshot.Revision = hex.EncodeToString(digest[:])
		value, err := provider.SideQuestionCatalog(ctx, id, entry.snapshot, guard)
		if err != nil {
			return nil, err
		}
		if err := guard(); err != nil {
			return nil, err
		}
		if value.SessionKey != str(cmd, "sessionKey") || value.ContextScope != "loadedSnapshot" || value.ContextMessages != len(entry.snapshot.Messages) || value.ContextTruncated != entry.snapshot.Truncated || len(value.UnavailableReason) > 4096 || value.Available && !agents.ValidTurnID(value.Revision) {
			return nil, invalid
		}
		if value.Available {
			if value.InputOptions != nil {
				options := *value.InputOptions
				if options.Version != 1 || !options.ReadOnly || options.DefaultModel == "" || len(options.DefaultModel) > 200 {
					return nil, invalid
				}
				entry.options = &options
				if options.Tools != nil {
					if options.Tools.Version != 1 || len(options.Tools.Permissions) != 1 || options.Tools.Permissions[0] != "workspace-write" {
						return nil, invalid
					}
					entry.options.Tools = &protocol.SideQuestionToolOptions{Version: 1, Permissions: []string{"workspace-write"}, Features: options.Tools.Features}
				}
			}
			c.sideQuestions.mu.Lock()
			if c.sideQuestions.catalogs == nil {
				c.sideQuestions.catalogs = map[string]sideQuestionHistory{}
			}
			if len(c.sideQuestions.catalogs) >= 128 {
				evictSideHistory(c.sideQuestions.catalogs)
			}
			catalogKey := scope + "\x00" + value.Revision
			if _, exists := c.sideQuestions.catalogs[catalogKey]; !exists {
				entry.at = time.Now()
				c.sideQuestions.catalogs[catalogKey] = entry
			}
			c.sideQuestions.mu.Unlock()
		}
		return value, nil
	}
	value, err := provider.AskSideQuestion(ctx, id, entry.snapshot, request, guard)
	if err != nil {
		return nil, err
	}
	if err := guard(); err != nil {
		return nil, err
	}
	if value.SessionKey != str(cmd, "sessionKey") || value.OperationID != request.OperationID || value.ContextScope != "loadedSnapshot" || value.ContextMessages != len(entry.snapshot.Messages) || value.ContextTruncated != entry.snapshot.Truncated || value.State != "completed" && value.State != "unconfirmed" || len(value.Answer) > 16<<10 || !utf8.ValidString(value.Answer) || len(value.Error) > 4096 || value.State == "completed" && strings.TrimSpace(value.Answer) == "" {
		return nil, errors.New("side_question_unconfirmed: 侧边提问结果未确认")
	}
	if value.Usage != nil {
		if value.Usage.InputTokens < 0 || value.Usage.OutputTokens < 0 || value.Usage.InputTokens > 9007199254740991 || value.Usage.OutputTokens > 9007199254740991 {
			return nil, invalid
		}
		copy := *value.Usage
		copy.CostUSD = 0
		value.Usage = &copy
	}
	return value, nil
}

func validSideConversationResponse(value protocol.SideQuestionConversation, key string, request protocol.SideQuestionConversationRequest) bool {
	if value.SessionKey != key || value.ChainID != request.ChainID || value.ContextScope != "loadedSnapshot" || !agents.ValidTurnID(value.Revision) || value.ContextMessages < 0 || value.ContextMessages > 80 || len(value.Turns) > 50 || len(value.UnavailableReason) > 4096 || !utf8.ValidString(value.UnavailableReason) {
		return false
	}
	if value.LastOperationID != "" && !agents.ValidControlOperationID(value.LastOperationID) {
		return false
	}
	if value.NextCursor != "" {
		cursor, err := strconv.Atoi(value.NextCursor)
		if err != nil || cursor < 1 || strconv.Itoa(cursor) != value.NextCursor || len(value.Turns) == 0 {
			return false
		}
	}
	if value.Deleted && (value.Available || len(value.Turns) != 0 || value.LastOperationID != "" || value.NextCursor != "") {
		return false
	}
	seen := map[string]bool{}
	for _, turn := range value.Turns {
		if seen[turn.OperationID] || !agents.ValidSideQuestionRequest(key, protocol.SideQuestionRequest{OperationID: turn.OperationID, ExpectedRevision: value.Revision, Question: turn.Question, Selection: turn.Selection}) || len(turn.Answer) > 16<<10 || !utf8.ValidString(turn.Answer) || len(turn.Error) > 4096 || !utf8.ValidString(turn.Error) || turn.CreatedAt < 1 || turn.CreatedAt > 9007199254740991 {
			return false
		}
		if turn.State != "completed" && turn.State != "pending" && turn.State != "unconfirmed" || turn.State == "completed" && strings.TrimSpace(turn.Answer) == "" {
			return false
		}
		seen[turn.OperationID] = true
	}
	if request.Cursor == "" && len(value.Turns) > 0 {
		last := value.Turns[len(value.Turns)-1]
		if value.LastOperationID != last.OperationID || value.Available && last.State != "completed" {
			return false
		}
	}
	data, err := json.Marshal(value)
	return err == nil && len(data) <= 1<<20
}
