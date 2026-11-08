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

const maxClaudeSkillReceiptBytes = 1 << 20

// This is display metadata for an explicitly selected skill, not execution
// ownership or evidence that the model ran a skill. The native SDK user UUID
// binds it to exactly one root input. Nothing is matched by text or timestamp.
// It remains useful after the Hub cache expires or the Bridge restarts.
type claudeSkillReceipt struct {
	Origin            *protocol.MessageOrigin     `json:"origin,omitempty"`
	Version           int                         `json:"version"`
	SessionID         string                      `json:"sessionId"`
	UUID              string                      `json:"uuid"`
	ClientID          string                      `json:"clientId,omitempty"`
	Text              string                      `json:"text"`
	Skills            []protocol.SkillReference   `json:"skills"`
	Attachments       []protocol.AttachmentRef    `json:"attachments,omitempty"`
	ContextReferences []protocol.ContextReference `json:"contextReferences,omitempty"`
}

func skillReceiptUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func skillReceiptClientID(id string) string {
	if skillReceiptUUID(id) {
		return id
	}
	return ""
}

func (h *claudeHistory) skillReceiptPath(session, uuid string) string {
	if h == nil || !validChildID(session) || !skillReceiptUUID(uuid) {
		return ""
	}
	// Hash the session component to avoid platform-dependent filenames while
	// the JSON envelope still verifies the complete exact session identity.
	digest := sha256.Sum256([]byte(session))
	return filepath.Join(filepath.Dir(h.dir), "salcara", "skill-receipts", hex.EncodeToString(digest[:]), uuid+".json")
}

func (h *claudeHistory) safeSkillReceiptPath(path string) bool {
	home, err := filepath.EvalSymlinks(filepath.Dir(h.dir))
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(home, resolved)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func validClaudeSkillReceipt(r claudeSkillReceipt, session, uuid string) bool {
	return r.Version == 1 && r.SessionID == session && r.UUID == uuid &&
		(len(r.Skills) > 0 || len(r.ContextReferences) > 0 || r.Origin != nil) && protocol.ValidCrossTaskOrigin(r.Origin, "claude:"+r.SessionID) && (r.Origin == nil || r.Origin.ReceiptID == r.ClientID) && len(r.Text) <= 64<<10 && len(r.Attachments) <= 4 &&
		(r.ClientID == "" || skillReceiptClientID(r.ClientID) == r.ClientID) &&
		ValidateTurnFeatures(TurnOptions{Skills: r.Skills}) == nil && protocol.ValidateContextReferences(r.ContextReferences) == nil
}

func (h *claudeHistory) saveSkillReceipt(session, uuid, text, clientID string, skills []protocol.SkillReference, attachments []protocol.AttachmentRef, contextRefs ...[]protocol.ContextReference) error {
	r := claudeSkillReceipt{Version: 1, SessionID: session, UUID: uuid, ClientID: skillReceiptClientID(clientID),
		Text: text, Skills: append([]protocol.SkillReference(nil), skills...), Attachments: append([]protocol.AttachmentRef(nil), attachments...)}
	if len(contextRefs) > 0 {
		r.ContextReferences = protocol.CloneContextReferences(contextRefs[0])
	}
	return h.saveInputReceipt(r)
}

func (h *claudeHistory) saveInputReceipt(r claudeSkillReceipt) error {
	session, uuid := r.SessionID, r.UUID
	path := h.skillReceiptPath(session, uuid)
	data, err := json.Marshal(r)
	if path == "" || !validClaudeSkillReceipt(r, session, uuid) || err != nil || len(data) > maxClaudeSkillReceiptBytes {
		return errors.New("skill_receipt_unavailable: 无法保存技能消息，请缩短内容后重试")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil || !h.safeSkillReceiptPath(dir) {
		return errors.New("skill_receipt_unavailable: 无法保存技能消息，请检查电脑的配置目录")
	}
	// Each send gets a fresh native UUID. Never overwrite an earlier receipt.
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errors.New("skill_receipt_unavailable: 技能消息标识冲突，请重试")
	}
	f, err := os.CreateTemp(dir, ".pending-*.json")
	if err != nil {
		return errors.New("skill_receipt_unavailable: 无法保存技能消息")
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
		return errors.New("skill_receipt_unavailable: 无法保存技能消息")
	}
	return nil
}

func (h *claudeHistory) readSkillReceipt(session, uuid string) (claudeSkillReceipt, bool) {
	var r claudeSkillReceipt
	path := h.skillReceiptPath(session, uuid)
	if path == "" {
		return r, false
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > maxClaudeSkillReceiptBytes || !h.safeSkillReceiptPath(path) {
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
	data, err := io.ReadAll(io.LimitReader(f, maxClaudeSkillReceiptBytes+1))
	if err != nil || len(data) > maxClaudeSkillReceiptBytes || json.Unmarshal(data, &r) != nil || !validClaudeSkillReceipt(r, session, uuid) {
		return claudeSkillReceipt{}, false
	}
	return r, true
}

func (h *claudeHistory) projectSkillReceipt(session string, l *claudeLine, mapped []protocol.Event) []protocol.Event {
	if l.Type != "user" || l.IsMeta || l.IsCompactSummary || l.IsSidechain || l.ParentTU != nil ||
		l.SessionID != "" && l.SessionID != session || l.SessionIDT != "" && l.SessionIDT != session ||
		!claudeUserOrigin(l).human {
		return mapped
	}
	var message claudeMessage
	if json.Unmarshal(l.Message, &message) != nil || message.Role != "user" {
		return mapped
	}
	r, ok := h.readSkillReceipt(session, l.UUID)
	if !ok {
		return mapped
	}
	// Claude may serialize slash commands as a command wrapper. With an exact
	// owned UUID we can retain the original user input without parsing that
	// wrapper or mistaking an arbitrary /command for an invoked skill.
	e := protocol.Event{SessionKey: "claude:" + session, Tool: "claude", TS: l.ts(), Type: "message", ID: r.UUID,
		ClientID: r.ClientID, Role: "user", Text: r.Text, Final: true, Skills: r.Skills, Attachments: r.Attachments}
	e.ContextReferences = protocol.CloneContextReferences(r.ContextReferences)
	e.MessageOrigin = protocol.CloneMessageOrigin(r.Origin)
	for i := range mapped {
		if mapped[i].Type == "message" && mapped[i].Role == "user" && mapped[i].ParentID == "" && mapped[i].ID == r.UUID {
			mapped[i] = e
			return mapped
		}
	}
	return append([]protocol.Event{e}, mapped...)
}
