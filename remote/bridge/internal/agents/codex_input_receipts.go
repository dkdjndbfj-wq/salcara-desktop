package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"salcara/bridge/internal/protocol"
)

// An exact client UUID binds the receipt to a native user message. Once the
// native item ID is observed, a second exact binding survives native history
// versions that omit clientId. No message is identified by text or timestamp.
type codexInputReceipt struct {
	Origin            *protocol.MessageOrigin     `json:"origin,omitempty"`
	Version           int                         `json:"version"`
	SessionID         string                      `json:"sessionId"`
	ClientID          string                      `json:"clientId"`
	NativeID          string                      `json:"nativeId,omitempty"`
	Text              string                      `json:"text"`
	Skills            []protocol.SkillReference   `json:"skills,omitempty"`
	ContextReferences []protocol.ContextReference `json:"contextReferences,omitempty"`
}

const maxCodexInputReceiptBytes = 1 << 20

func (a *codexAgent) inputReceiptPath(session, kind, identity string) string {
	if a.stateFile == "" || session == "" || len(session) > 512 || identity == "" || len(identity) > 512 {
		return ""
	}
	digest := sha256.Sum256([]byte(session + "\x00" + kind + "\x00" + identity))
	return filepath.Join(filepath.Dir(a.stateFile), "codex-input-receipts", hex.EncodeToString(digest[:])+".json")
}

func (a *codexAgent) safeInputReceiptPath(path string) bool {
	root, err := filepath.EvalSymlinks(filepath.Dir(a.stateFile))
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, resolved)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func validCodexInputReceipt(r codexInputReceipt) bool {
	return r.Version == 1 && r.SessionID != "" && len(r.SessionID) <= 512 && skillReceiptUUID(r.ClientID) &&
		len(r.NativeID) <= 512 && len(r.Text) <= 64<<10 && (len(r.Skills) > 0 || len(r.ContextReferences) > 0 || r.Origin != nil) && protocol.ValidCrossTaskOrigin(r.Origin, "codex:"+r.SessionID) && (r.Origin == nil || r.Origin.ReceiptID == r.ClientID) &&
		ValidateTurnFeatures(TurnOptions{Skills: r.Skills}) == nil && protocol.ValidateContextReferences(r.ContextReferences) == nil
}

func (a *codexAgent) saveInputReceipt(r codexInputReceipt, kind, identity string) error {
	path := a.inputReceiptPath(r.SessionID, kind, identity)
	data, err := json.Marshal(r)
	if path == "" || err != nil || !validCodexInputReceipt(r) || len(data) > maxCodexInputReceiptBytes {
		return errors.New("input_receipt_unavailable: 无法保存消息引用，请缩短内容后重试")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil || !a.safeInputReceiptPath(dir) {
		return errors.New("input_receipt_unavailable: 无法保存消息引用，请检查电脑配置目录")
	}
	// Atomic publication and no replacement protect another frozen input with
	// the same operation UUID. Existing identical receipts are idempotent.
	if old, ok := a.readInputReceipt(r.SessionID, kind, identity); ok {
		oldData, _ := json.Marshal(old)
		if string(oldData) == string(data) {
			return nil
		}
		return errors.New("input_receipt_unavailable: 消息标识已被使用，请重试")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("input_receipt_unavailable: 消息引用文件不可用")
	}
	f, err := os.CreateTemp(dir, ".pending-*.json")
	if err != nil {
		return errors.New("input_receipt_unavailable: 无法保存消息引用")
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		return errors.New("input_receipt_unavailable: 无法保存消息引用")
	}
	return nil
}

func (a *codexAgent) readInputReceipt(session, kind, identity string) (codexInputReceipt, bool) {
	var r codexInputReceipt
	path := a.inputReceiptPath(session, kind, identity)
	if path == "" {
		return r, false
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > maxCodexInputReceiptBytes || !a.safeInputReceiptPath(path) {
		return r, false
	}
	f, err := os.Open(path)
	if err != nil {
		return r, false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return r, false
	}
	data, err := io.ReadAll(io.LimitReader(f, maxCodexInputReceiptBytes+1))
	if err != nil || len(data) > maxCodexInputReceiptBytes || json.Unmarshal(data, &r) != nil || !validCodexInputReceipt(r) || r.SessionID != session {
		return codexInputReceipt{}, false
	}
	if kind == "client" && r.ClientID != identity || kind == "native" && r.NativeID != identity {
		return codexInputReceipt{}, false
	}
	return r, true
}

func (a *codexAgent) projectInputReceipt(e protocol.Event) protocol.Event {
	if e.Type != "message" || e.Role != "user" || e.ParentID != "" || e.MessageOrigin != nil || !strings.HasPrefix(e.SessionKey, "codex:") {
		return e
	}
	session := strings.TrimPrefix(e.SessionKey, "codex:")
	r, ok := a.readInputReceipt(session, "native", e.ID)
	if !ok && skillReceiptUUID(e.ClientID) {
		r, ok = a.readInputReceipt(session, "client", e.ClientID)
		if ok && e.ID != "" {
			r.NativeID = e.ID
			// Failure to persist this optional binding never invents history:
			// the exact native client ID can still recover the frozen receipt.
			_ = a.saveInputReceipt(r, "native", e.ID)
		}
	}
	if !ok || e.ClientID != "" && e.ClientID != r.ClientID {
		return e
	}
	e.Text, e.ClientID = r.Text, r.ClientID
	e.MessageOrigin = protocol.CloneMessageOrigin(r.Origin)
	e.Skills = append([]protocol.SkillReference(nil), r.Skills...)
	e.ContextReferences = protocol.CloneContextReferences(r.ContextReferences)
	return e
}
