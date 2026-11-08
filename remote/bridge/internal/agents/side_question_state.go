package agents

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/protocol"
)

const (
	maxSideQuestionSessions = 64
	maxSideQuestionMessages = 80
	maxSideQuestionSnapshot = 64 << 10
	maxSideQuestionContext  = 96 << 10
	maxSideQuestionAnswer   = 16 << 10
	maxSideQuestionTurns    = 6
	sideQuestionLifetime    = 20 * time.Minute
	sideQuestionTimeout     = 40 * time.Second
)

var (
	errSideQuestionStale   = errors.New("side_question_stale: 独立提问的对话快照或 API 来源已变化")
	errSideQuestionBusy    = errors.New("side_question_busy: 上一个独立提问仍在处理中")
	errSideQuestionUnknown = errors.New("side_question_unconfirmed: 上次独立提问结果未确认，不会重复发送同一提问链")
	errSideQuestionLimit   = errors.New("side_question_limit: 临时提问已达到轮次或上下文上限")
	errSideQuestionExpired = errors.New("side_question_expired: 临时提问已过期")
	errSideQuestionInvalid = errors.New("side_question_invalid: 独立提问内容或所选对话快照无效")
)

// Runtime must be a comparable, immutable worker/connection identity (normally
// a pointer). Keeping the pointer also prevents an allocator address reuse from
// making an old catalog valid again. Source includes the route's authorization
// epoch, not only settings that could be restored after an intervening switch.
type sideQuestionBinding struct {
	Source     string
	Runtime    any
	Persistent string
}

type sideQuestionTurn struct {
	Question  string
	Selection *protocol.ContextReference
	Answer    string
	Images    []protocol.SideQuestionImage
}

type sideQuestionInput struct {
	Snapshot      SideQuestionSnapshot
	Previous      []sideQuestionTurn
	Question      string
	Selection     *protocol.ContextReference
	Model, Effort string
	Images        []protocol.SideQuestionImage
}

type sideQuestionAnswer struct {
	Text  string
	Usage *protocol.Usage
}

// A transport must make at most one HTTP request and return attempted=true as
// soon as dispatch is attempted. Only private, fixed gateway failure messages
// may be exposed; arbitrary transport error text is never forwarded.
type sideQuestionTransport func(context.Context, sideQuestionInput) (sideQuestionAnswer, bool, error)

type sideQuestionOperation struct {
	hash     string
	result   protocol.SideQuestionResult
	finished bool
}

type sideQuestionState struct {
	key      string
	revision string
	reading  string
	binding  sideQuestionBinding
	snapshot SideQuestionSnapshot
	created  time.Time
	expired  bool
	turns    []sideQuestionTurn
	receipts map[string]*sideQuestionOperation
	last     string
	pending  *sideQuestionOperation
}

// This registry is independent of native turn admission: it participates only
// in API-switch busy checks. It never owns a parent worker's turnMu or stdin.
type sideQuestionRegistry struct {
	mu                sync.Mutex
	sessions          map[string]*sideQuestionState
	active            int
	persistentDir     string
	documents         map[string]*sideConversationDocument
	persistentRunning map[string]string
}

func sideQuestionCatalogView(key string, snapshot SideQuestionSnapshot) protocol.SideQuestionCatalog {
	return protocol.SideQuestionCatalog{SessionKey: key, ContextScope: "loadedSnapshot", ContextMessages: len(snapshot.Messages), ContextTruncated: snapshot.Truncated}
}

func cloneSideQuestionSelection(ref *protocol.ContextReference) *protocol.ContextReference {
	if ref == nil {
		return nil
	}
	refs := protocol.CloneContextReferences([]protocol.ContextReference{*ref})
	return &refs[0]
}

func cloneSideQuestionResult(result protocol.SideQuestionResult) protocol.SideQuestionResult {
	if result.Usage != nil {
		usage := *result.Usage
		result.Usage = &usage
	}
	return result
}

func sideQuestionHash(value any) string {
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validSideQuestionSnapshot(snapshot SideQuestionSnapshot) bool {
	if !ValidTurnID(snapshot.Revision) || len(snapshot.Messages) == 0 || len(snapshot.Messages) > maxSideQuestionMessages {
		return false
	}
	total := 0
	for _, message := range snapshot.Messages {
		if !ValidTurnID(message.ID) || message.Role != "user" && message.Role != "assistant" || strings.TrimSpace(message.Text) == "" || !utf8.ValidString(message.Text) {
			return false
		}
		total += len(message.Text)
		if total > maxSideQuestionSnapshot {
			return false
		}
	}
	return true
}

func (r *sideQuestionRegistry) activeTurns() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active > 0
}

// Clear text and answers on expiry. Catalog admission may reclaim this entry;
// any subsequent catalog receives a fresh revision and cannot replay the old
// request. Pending HTTP work is never reclaimed or given a second reservation.
func (s *sideQuestionState) expire(now time.Time) bool {
	if s.expired || s.pending == nil && now.Sub(s.created) >= sideQuestionLifetime {
		s.expired = true
		s.snapshot.Messages, s.turns = nil, nil
		for _, receipt := range s.receipts {
			receipt.result.Answer, receipt.result.Usage = "", nil
		}
	}
	return s.expired
}

func (r *sideQuestionRegistry) catalog(ctx context.Context, key string, snapshot SideQuestionSnapshot, binding sideQuestionBinding, guard func() error) (protocol.SideQuestionCatalog, error) {
	view := sideQuestionCatalogView(key, snapshot)
	if !validSideQuestionSnapshot(snapshot) {
		view.UnavailableReason = "没有可用于独立提问的已加载对话"
		return view, nil
	}
	if guard == nil || binding.Source == "" {
		return view, errSideQuestionStale
	}
	if err := guard(); err != nil {
		return view, err
	}
	reading := sideQuestionHash(snapshot)
	r.mu.Lock()
	if r.sessions == nil {
		r.sessions = make(map[string]*sideQuestionState)
	}
	for revision, old := range r.sessions {
		if old.expire(time.Now()) {
			delete(r.sessions, revision)
		}
	}
	var s *sideQuestionState
	for _, candidate := range r.sessions {
		if candidate.key == key && candidate.reading == reading && candidate.binding == binding {
			s = candidate
			break
		}
	}
	if s == nil {
		if len(r.sessions) >= maxSideQuestionSessions {
			view.UnavailableReason = "同时打开的临时提问数量已达上限"
		} else {
			snapshot.Messages = append([]SideQuestionMessage(nil), snapshot.Messages...)
			s = &sideQuestionState{key: key, revision: newUUID(), reading: reading, binding: binding, snapshot: snapshot, created: time.Now(), receipts: make(map[string]*sideQuestionOperation)}
			r.sessions[s.revision] = s
		}
	}
	if view.UnavailableReason == "" {
		view.Revision = s.revision
		switch {
		case s.expired:
			view.UnavailableReason = "临时提问已过期"
		case s.pending != nil:
			view.UnavailableReason = "上一个独立提问仍在处理中"
		case s.last != "" && s.receipts[s.last].result.State != "completed":
			view.UnavailableReason = "上次独立提问结果未确认，不会自动重试"
		case len(s.turns) >= maxSideQuestionTurns:
			view.UnavailableReason = "临时提问已达 6 轮"
		default:
			view.Available = true
		}
	}
	r.mu.Unlock()
	// The callback may wait on task/source locks. Never run it under r.mu.
	if err := guard(); err != nil {
		return sideQuestionCatalogView(key, snapshot), err
	}
	return view, ctx.Err()
}

func sideQuestionInputBytes(input sideQuestionInput) int {
	messages, err := sideQuestionMessages(input)
	if err != nil {
		return maxSideQuestionContext + 1
	}
	// Count the actual quoted JSON, reference metadata, delimiters and retained
	// turns seen by the model. Reserve 2 KiB for provider instructions/envelopes;
	// both built-in transports use less than this. Text-only counting could let
	// many message IDs or JSON escaping exceed the advertised context limit.
	total := 2 << 10
	for _, message := range messages {
		total += len(message.Role) + 32
		for _, block := range message.Content {
			total += len(block.Text) + 16
		}
	}
	return total
}

func (r *sideQuestionRegistry) ask(ctx context.Context, key string, snapshot SideQuestionSnapshot, request protocol.SideQuestionRequest, binding sideQuestionBinding, guard func() error, transport sideQuestionTransport, closed <-chan struct{}) (protocol.SideQuestionResult, error) {
	result := protocol.SideQuestionResult{SessionKey: key, OperationID: request.OperationID, State: "unconfirmed", ContextScope: "loadedSnapshot", ContextMessages: len(snapshot.Messages), ContextTruncated: snapshot.Truncated}
	// An explicit selection is user-supplied quoted data, just like a reference
	// on a normal send. It need not remain in the bounded recent-message slice.
	if !ValidSideQuestionRequest(key, request) || !utf8.ValidString(request.Question) || !validSideQuestionSnapshot(snapshot) {
		return result, errSideQuestionInvalid
	}
	if guard == nil || transport == nil || binding.Source == "" {
		return result, errSideQuestionStale
	}
	if err := guard(); err != nil {
		return result, err
	}
	hash := sideQuestionHash([]any{request, request.Images})
	r.mu.Lock()
	s := r.sessions[request.ExpectedRevision]
	if s == nil || s.key != key || s.reading != sideQuestionHash(snapshot) || s.binding != binding {
		r.mu.Unlock()
		return result, errSideQuestionStale
	}
	if s.expire(time.Now()) {
		r.mu.Unlock()
		return result, errSideQuestionExpired
	}
	if previous := s.receipts[request.OperationID]; previous != nil {
		if previous.hash != hash {
			r.mu.Unlock()
			return result, errSideQuestionStale
		}
		receipt := cloneSideQuestionResult(previous.result)
		r.mu.Unlock()
		if err := guard(); err != nil {
			return result, err
		}
		return receipt, ctx.Err()
	}
	if s.pending != nil {
		r.mu.Unlock()
		return result, errSideQuestionBusy
	}
	if s.last != "" && s.receipts[s.last].result.State != "completed" {
		r.mu.Unlock()
		return result, errSideQuestionUnknown
	}
	if request.PreviousOperationID != s.last {
		r.mu.Unlock()
		return result, errSideQuestionStale
	}
	input := sideQuestionInput{Snapshot: s.snapshot, Question: request.Question, Selection: cloneSideQuestionSelection(request.Selection), Model: request.Model, Effort: request.Effort, Images: append([]protocol.SideQuestionImage(nil), request.Images...)}
	input.Snapshot.Messages = append([]SideQuestionMessage(nil), s.snapshot.Messages...)
	for _, turn := range s.turns {
		turn.Selection = cloneSideQuestionSelection(turn.Selection)
		turn.Images = append([]protocol.SideQuestionImage(nil), turn.Images...)
		input.Previous = append(input.Previous, turn)
	}
	// Reserve enough room to retain the whole bounded next answer. A rejected
	// limit never reaches the network and cannot leave a partly retained chain.
	if len(s.turns) >= maxSideQuestionTurns || sideQuestionInputBytes(input)+maxSideQuestionAnswer > maxSideQuestionContext || sideQuestionImageBytes(input) > 512<<10 {
		r.mu.Unlock()
		return result, errSideQuestionLimit
	}
	result.Error = "独立提问正在处理中"
	op := &sideQuestionOperation{hash: hash, result: result}
	s.receipts[request.OperationID], s.pending = op, op
	previousID := s.last
	s.last = request.OperationID
	r.active++
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, sideQuestionTimeout)
	defer cancel()
	go func() {
		select {
		case <-closed:
			cancel()
		case <-ctx.Done():
		}
	}()
	var answer sideQuestionAnswer
	var attempted bool
	err := ctx.Err()
	if err == nil {
		err = guard() // pending is visible to API switches before the final guard
	}
	if err == nil {
		answer, attempted, err = transport(ctx, input)
	}
	if err == nil {
		err = guard()
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && (strings.TrimSpace(answer.Text) == "" || len(answer.Text) > maxSideQuestionAnswer || !utf8.ValidString(answer.Text)) {
		err = errSideQuestionUnknown
	}
	result.Answer, result.Usage, result.Error = "", nil, "独立提问结果未确认，未自动重试"
	var gatewayFailure *sideQuestionGatewayError
	if attempted && errors.As(err, &gatewayFailure) {
		result.Error = gatewayFailure.Error()
	}
	if err == nil {
		result.State, result.Answer, result.Usage, result.Error = "completed", answer.Text, answer.Usage, ""
	}
	r.mu.Lock()
	r.active--
	s.pending = nil
	if !attempted && err != nil {
		delete(s.receipts, request.OperationID)
		s.last = previousID
	} else {
		op.finished, op.result = true, cloneSideQuestionResult(result)
		if result.State == "completed" {
			s.turns = append(s.turns, sideQuestionTurn{Question: request.Question, Selection: cloneSideQuestionSelection(request.Selection), Answer: result.Answer, Images: append([]protocol.SideQuestionImage(nil), request.Images...)})
		}
	}
	r.mu.Unlock()
	if !attempted && err != nil {
		return result, err
	}
	return cloneSideQuestionResult(result), nil
}
