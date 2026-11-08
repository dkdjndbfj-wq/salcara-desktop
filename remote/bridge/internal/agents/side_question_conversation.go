package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"salcara/bridge/internal/atomicfile"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

const maxSideConversationFile = 32 << 20
const sideConversationPage = 50
const maxSideConversationPageBytes = 1 << 20

var errSideConversationStorage = errors.New("side_question_storage: 侧问记录读取或保存失败，请刷新记录确认结果，勿重复发送")

type SideQuestionConversationProvider interface {
	SideQuestionConversation(context.Context, string, string, SideQuestionSnapshot, protocol.SideQuestionConversationRequest, func() error) (protocol.SideQuestionConversation, error)
}

type sideConversationTurn struct {
	protocol.SideQuestionExchange
	Hash string `json:"requestHash"`
}

type sideConversationDocument struct {
	Version              int                    `json:"version"`
	Owner                string                 `json:"owner"`
	Key                  string                 `json:"sessionKey"`
	ChainID              string                 `json:"chainId"`
	Source               string                 `json:"source"`
	Snapshot             SideQuestionSnapshot   `json:"snapshot"`
	Turns                []sideConversationTurn `json:"turns"`
	Deleted              bool                   `json:"deleted,omitempty"`
	LastContextTruncated bool                   `json:"lastContextTruncated,omitempty"`
}

// These identifiers are supplied by Bridge admission, never phone fields. The
// immutable route identity survives reconnects; every read/send still passes a
// new live guard. A saved history cannot grant execution or restore a lease.
func sideConversationStorageKey(owner, key, chain string) string {
	return sideQuestionHash([]string{owner, key, chain})
}

// This private disk identity survives process restarts and model changes, but
// replacing an account's credential cannot authorize sending its old context
// through a different account. Protected token refresh retains IdentityEpoch.
// The digest is never exposed as a public authorization revision.
func sideConversationSource(account config.LocalAccount, target string) string {
	credential := account.Key
	if account.CredentialRef != "" {
		credential = fmt.Sprintf("%s:%d", account.CredentialRef, account.CredentialEpoch)
	}
	return sideQuestionHash([]string{account.ID, account.BaseURL, account.SourceType, account.ProviderID, credential, target})
}

func (r *sideQuestionRegistry) loadConversationLocked(storageKey string) (*sideConversationDocument, error) {
	if r.persistentDir == "" {
		if doc := r.documents[storageKey]; doc != nil {
			data, _ := json.Marshal(doc)
			var copy sideConversationDocument
			if json.Unmarshal(data, &copy) != nil {
				return nil, errSideConversationStorage
			}
			return &copy, nil
		}
		return nil, os.ErrNotExist
	}
	directory, err := os.Lstat(r.persistentDir)
	if err != nil {
		return nil, err
	}
	if !directory.IsDir() || directory.Mode()&os.ModeSymlink != 0 {
		return nil, errSideConversationStorage
	}
	path := filepath.Join(r.persistentDir, storageKey+".json")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSideConversationFile {
		return nil, errSideConversationStorage
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errSideConversationStorage
	}
	data, err := io.ReadAll(io.LimitReader(f, maxSideConversationFile+1))
	if err != nil || len(data) > maxSideConversationFile {
		return nil, errSideConversationStorage
	}
	var doc sideConversationDocument
	if json.Unmarshal(data, &doc) != nil || doc.Version != 1 || !ValidControlOperationID(doc.ChainID) || doc.Owner == "" || sideConversationStorageKey(doc.Owner, doc.Key, doc.ChainID) != storageKey {
		return nil, errSideConversationStorage
	}
	if !doc.Deleted && !validSideQuestionSnapshot(doc.Snapshot) {
		return nil, errSideConversationStorage
	}
	for _, turn := range doc.Turns {
		if !ValidControlOperationID(turn.OperationID) || !workflowText(turn.Question, 4000, true) || len(turn.Answer) > maxSideQuestionAnswer || !utf8.ValidString(turn.Answer) || (turn.State != "completed" && turn.State != "pending" && turn.State != "unconfirmed") || turn.State == "completed" && strings.TrimSpace(turn.Answer) == "" {
			return nil, errSideConversationStorage
		}
	}
	return &doc, nil
}

func (r *sideQuestionRegistry) saveConversationLocked(storageKey string, doc *sideConversationDocument) error {
	data, err := json.Marshal(doc)
	if err != nil || len(data) > maxSideConversationFile {
		return errSideConversationStorage
	}
	if r.persistentDir == "" {
		if r.documents == nil {
			r.documents = map[string]*sideConversationDocument{}
		}
		var copy sideConversationDocument
		if json.Unmarshal(data, &copy) != nil {
			return errSideConversationStorage
		}
		r.documents[storageKey] = &copy
		return nil
	}
	if err := os.MkdirAll(r.persistentDir, 0700); err != nil {
		return errSideConversationStorage
	}
	info, err := os.Lstat(r.persistentDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errSideConversationStorage
	}
	if protectSideConversationDirectory(r.persistentDir) != nil {
		return errSideConversationStorage
	}
	path := filepath.Join(r.persistentDir, storageKey+".json")
	if old, err := os.Lstat(path); err == nil && !old.Mode().IsRegular() || err != nil && !os.IsNotExist(err) {
		return errSideConversationStorage
	}
	if atomicfile.WriteFile(path, data, 0600) != nil {
		return errSideConversationStorage
	}
	return nil
}

func sideConversationHead(doc *sideConversationDocument) string {
	if len(doc.Turns) == 0 {
		return ""
	}
	return doc.Turns[len(doc.Turns)-1].OperationID
}

func sideConversationRevision(doc *sideConversationDocument, binding sideQuestionBinding) string {
	return sideQuestionHash([]string{doc.Owner, doc.Key, doc.ChainID, binding.Source, fmt.Sprintf("%p", binding.Runtime), sideConversationHead(doc)})
}

func sideConversationView(doc *sideConversationDocument, binding sideQuestionBinding, cursor string) (protocol.SideQuestionConversation, error) {
	view := protocol.SideQuestionConversation{SideQuestionCatalog: sideQuestionCatalogView(doc.Key, doc.Snapshot), ChainID: doc.ChainID, LastOperationID: sideConversationHead(doc), Turns: []protocol.SideQuestionExchange{}, Deleted: doc.Deleted}
	view.Revision = sideConversationRevision(doc, binding)
	view.ContextTruncated = view.ContextTruncated || doc.LastContextTruncated
	if doc.Deleted {
		view.UnavailableReason = "侧边聊天已删除"
		return view, nil
	}
	end := len(doc.Turns)
	if cursor != "" {
		value, err := strconv.Atoi(cursor)
		if err != nil || value < 0 || value > end || strconv.Itoa(value) != cursor {
			return view, errSideQuestionInvalid
		}
		end = value
	}
	// JSON escaping can make a page much larger than its plain text. Bound
	// the actual wire representation so Hub can retain a complete receipt.
	start, pageBytes := end, 16<<10
	for start > 0 && end-start < sideConversationPage {
		encoded, err := json.Marshal(doc.Turns[start-1].SideQuestionExchange)
		if err != nil || len(encoded)+16<<10 > maxSideConversationPageBytes {
			return view, errSideConversationStorage
		}
		if pageBytes+len(encoded)+1 > maxSideConversationPageBytes {
			break
		}
		pageBytes += len(encoded) + 1
		start--
	}
	if start > 0 {
		view.NextCursor = strconv.Itoa(start)
	}
	for _, turn := range doc.Turns[start:end] {
		copy := turn.SideQuestionExchange
		copy.Selection = cloneSideQuestionSelection(copy.Selection)
		view.Turns = append(view.Turns, copy)
	}
	view.Available = true
	if len(doc.Turns) > 0 {
		switch doc.Turns[len(doc.Turns)-1].State {
		case "pending":
			view.Available, view.UnavailableReason = false, "侧边问题正在回答，可以收起后再查看"
		case "unconfirmed":
			view.Available, view.UnavailableReason = false, "上次问题结果未确认，历史已保留，不会自动重发"
		}
	}
	return view, nil
}

// Keep the full visible history on disk, but bound each model request. Older
// side turns may be omitted from the request; no saved messages are deleted.
func persistentSideInput(doc *sideConversationDocument, request protocol.SideQuestionConversationRequest) (sideQuestionInput, error) {
	input := sideQuestionInput{Snapshot: doc.Snapshot, Question: request.Question, Selection: cloneSideQuestionSelection(request.Selection)}
	for _, turn := range doc.Turns {
		if turn.State == "completed" {
			input.Previous = append(input.Previous, sideQuestionTurn{Question: turn.Question, Answer: turn.Answer, Selection: cloneSideQuestionSelection(turn.Selection)})
		}
	}
	for sideQuestionInputBytes(input)+maxSideQuestionAnswer > maxSideQuestionContext && len(input.Previous) > 0 {
		input.Previous = input.Previous[1:]
		input.Snapshot.Truncated = true
	}
	if sideQuestionInputBytes(input)+maxSideQuestionAnswer > maxSideQuestionContext {
		return input, errSideQuestionLimit
	}
	return input, nil
}

func (r *sideQuestionRegistry) conversation(ctx context.Context, key, owner string, snapshot SideQuestionSnapshot, request protocol.SideQuestionConversationRequest, binding sideQuestionBinding, guard func() error, transport sideQuestionTransport, closed <-chan struct{}) (protocol.SideQuestionConversation, error) {
	var empty protocol.SideQuestionConversation
	if owner == "" || len(owner) > 256 || binding.Persistent == "" || !ValidControlOperationID(request.ChainID) || guard == nil {
		return empty, errSideQuestionInvalid
	}
	if request.Model != "" || request.Effort != "" || len(request.Attachments) != 0 || len(request.Images) != 0 {
		return empty, errSideQuestionInvalid
	}
	if request.Action != "open" && request.Action != "read" && request.Action != "send" && request.Action != "delete" {
		return empty, errSideQuestionInvalid
	}
	if request.Action == "send" && (!ValidSideQuestionRequest(key, request.SideQuestionRequest) || transport == nil) || request.Action == "delete" && !ValidControlOperationID(request.OperationID) {
		return empty, errSideQuestionInvalid
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if err := guard(); err != nil {
		return empty, err
	}
	storageKey := sideConversationStorageKey(owner, key, request.ChainID)
	r.mu.Lock()
	doc, err := r.loadConversationLocked(storageKey)
	if os.IsNotExist(err) && request.Action == "open" && validSideQuestionSnapshot(snapshot) {
		doc = &sideConversationDocument{Version: 1, Owner: owner, Key: key, ChainID: request.ChainID, Source: binding.Persistent, Snapshot: snapshot, Turns: []sideConversationTurn{}}
		err = r.saveConversationLocked(storageKey, doc)
	}
	if err != nil {
		r.mu.Unlock()
		if os.IsNotExist(err) {
			return empty, errors.New("side_question_missing: 未找到这条侧边聊天，手机上的历史仍保留")
		}
		return empty, errSideConversationStorage
	}
	// A fresh phone owner may explicitly delete its own saved record after an
	// account replacement. This grants no read or execution access to that source.
	if doc.Owner != owner || doc.Key != key || doc.ChainID != request.ChainID || request.Action != "delete" && doc.Source != binding.Persistent {
		r.mu.Unlock()
		return empty, errSideQuestionStale
	}
	if doc.Deleted {
		view, err := sideConversationView(doc, binding, "")
		r.mu.Unlock()
		if guardErr := guard(); guardErr != nil {
			return empty, guardErr
		}
		return view, err
	}
	if len(doc.Turns) > 0 {
		last := &doc.Turns[len(doc.Turns)-1]
		if last.State == "pending" && r.persistentRunning[storageKey] != last.OperationID {
			last.State, last.Error = "unconfirmed", "连接重启前的提问结果未确认，不会自动重发"
			if err := r.saveConversationLocked(storageKey, doc); err != nil {
				r.mu.Unlock()
				return empty, err
			}
		}
	}
	if request.Action == "delete" {
		doc.Deleted, doc.Turns, doc.Snapshot.Messages = true, nil, nil
		doc.LastContextTruncated = false
		if err := r.saveConversationLocked(storageKey, doc); err != nil {
			r.mu.Unlock()
			return empty, err
		}
		view, err := sideConversationView(doc, binding, "")
		r.mu.Unlock()
		if guardErr := guard(); guardErr != nil {
			return empty, guardErr
		}
		return view, err
	}
	if request.Action != "send" {
		view, err := sideConversationView(doc, binding, request.Cursor)
		r.mu.Unlock()
		if guardErr := guard(); guardErr != nil {
			return empty, guardErr
		}
		return view, err
	}
	requestHash := sideQuestionHash([]any{request.Question, request.Selection, request.PreviousOperationID})
	for _, prior := range doc.Turns {
		if prior.OperationID == request.OperationID {
			if prior.Hash != requestHash {
				r.mu.Unlock()
				return empty, errSideQuestionStale
			}
			view, err := sideConversationView(doc, binding, "")
			r.mu.Unlock()
			if guardErr := guard(); guardErr != nil {
				return empty, guardErr
			}
			return view, err
		}
	}
	if request.ExpectedRevision != sideConversationRevision(doc, binding) || request.PreviousOperationID != sideConversationHead(doc) {
		r.mu.Unlock()
		return empty, errSideQuestionStale
	}
	if len(doc.Turns) > 0 && doc.Turns[len(doc.Turns)-1].State != "completed" {
		r.mu.Unlock()
		return empty, errSideQuestionUnknown
	}
	input, err := persistentSideInput(doc, request)
	if err != nil {
		r.mu.Unlock()
		return empty, err
	}
	previousTruncated := doc.LastContextTruncated
	doc.LastContextTruncated = input.Snapshot.Truncated
	doc.Turns = append(doc.Turns, sideConversationTurn{SideQuestionExchange: protocol.SideQuestionExchange{OperationID: request.OperationID, Question: request.Question, Selection: cloneSideQuestionSelection(request.Selection), State: "pending", CreatedAt: time.Now().UnixMilli()}, Hash: requestHash})
	// Commit the operation before dispatch. A crash then becomes an unknown
	// result, never another model request on reconnect.
	if err := r.saveConversationLocked(storageKey, doc); err != nil {
		r.mu.Unlock()
		return empty, err
	}
	if r.persistentRunning == nil {
		r.persistentRunning = map[string]string{}
	}
	r.persistentRunning[storageKey] = request.OperationID
	r.active++
	r.mu.Unlock()

	callCtx, cancel := context.WithTimeout(ctx, sideQuestionTimeout)
	defer cancel()
	go func() {
		select {
		case <-closed:
			cancel()
		case <-callCtx.Done():
		}
	}()
	var answer sideQuestionAnswer
	var attempted bool
	err = guard()
	if err == nil {
		err = callCtx.Err()
	}
	if err == nil {
		answer, attempted, err = transport(callCtx, input)
	}
	if err == nil {
		err = guard()
	}
	if err == nil {
		err = callCtx.Err()
	}
	if err == nil && (strings.TrimSpace(answer.Text) == "" || len(answer.Text) > maxSideQuestionAnswer || !utf8.ValidString(answer.Text)) {
		err = errSideQuestionUnknown
	}
	r.mu.Lock()
	r.active--
	delete(r.persistentRunning, storageKey)
	latest, loadErr := r.loadConversationLocked(storageKey)
	if loadErr != nil {
		r.mu.Unlock()
		return empty, errSideConversationStorage
	}
	if latest.Deleted {
		view, viewErr := sideConversationView(latest, binding, "")
		r.mu.Unlock()
		if guardErr := guard(); guardErr != nil {
			return empty, guardErr
		}
		return view, viewErr
	}
	if sideConversationHead(latest) != request.OperationID {
		r.mu.Unlock()
		return empty, errSideQuestionStale
	}
	last := &latest.Turns[len(latest.Turns)-1]
	if !attempted && err != nil {
		latest.Turns = latest.Turns[:len(latest.Turns)-1]
		latest.LastContextTruncated = previousTruncated
	} else if err == nil {
		last.State, last.Answer = "completed", answer.Text
	} else {
		last.State, last.Error = "unconfirmed", "侧边提问结果未确认，历史已保存，不会自动重试"
		var failure *sideQuestionGatewayError
		if errors.As(err, &failure) {
			last.Error = failure.Error()
		}
	}
	if attempted {
		latest.LastContextTruncated = input.Snapshot.Truncated
	}
	if saveErr := r.saveConversationLocked(storageKey, latest); saveErr != nil {
		r.mu.Unlock()
		return empty, saveErr
	}
	view, viewErr := sideConversationView(latest, binding, "")
	view.ContextTruncated = view.ContextTruncated || input.Snapshot.Truncated
	r.mu.Unlock()
	if !attempted && err != nil {
		return empty, err
	}
	if guardErr := guard(); guardErr != nil {
		return empty, guardErr
	}
	return view, viewErr
}
