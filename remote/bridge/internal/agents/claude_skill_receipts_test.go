package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestClaudeSkillSendEchoAndRestartedHistoryUseExactReceipt(t *testing.T) {
	a, rec, logDir, cwd := newClaudeFeatureFixture(t, "supported")
	operation := "11111111-2222-4333-8444-555555555555"
	skills := []protocol.SkillReference{{Name: "plugin:review"}}
	id, err := a.StartWithOptions(context.Background(), cwd, "inspect", "ask", TurnOptions{Skills: skills, ClientUserMessageID: operation})
	if err != nil {
		t.Fatal(err)
	}
	rec.wait(t, "skill completion", func(e protocol.Event) bool { return e.Type == "turn" && e.Status == "completed" })
	var user protocol.Event
	for _, e := range rec.all() {
		if e.Type == "message" && e.Role == "user" {
			user = e
		}
	}
	if !skillReceiptUUID(user.ID) || user.ClientID != operation || user.Text != "inspect" || !reflect.DeepEqual(user.Skills, skills) {
		t.Fatalf("live skill selection lost exact identity: %+v", user)
	}
	var nativeID string
	for _, f := range readLog(t, logDir, "frames.jsonl") {
		if f["type"] == "user" {
			nativeID, _ = f["uuid"].(string)
			content := f["message"].(map[string]any)["content"].([]any)
			if content[0].(map[string]any)["text"] != "/plugin:review inspect" {
				t.Fatal("display projection removed the native invocation prefix")
			}
		}
	}
	if nativeID != user.ID {
		t.Fatal("native SDK input did not carry the receipt UUID")
	}
	// A new history instance has no process-local maps. The recorded native
	// UUID is the only association, even when Claude serializes a command wrapper.
	transcriptDir := filepath.Join(a.home, "projects", "fixture")
	if err := os.MkdirAll(transcriptDir, 0700); err != nil {
		t.Fatal(err)
	}
	row := map[string]any{"type": "user", "uuid": nativeID, "sessionId": id, "cwd": cwd,
		"message": map[string]any{"role": "user", "content": "<command-name>/plugin:review</command-name>"}}
	data, _ := json.Marshal(row)
	if err := os.WriteFile(filepath.Join(transcriptDir, id+".jsonl"), append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := newClaudeHistory(filepath.Join(a.home, "projects"))
	_, history, err := restarted.open(id)
	if err != nil || len(history) != 1 || history[0].ID != user.ID || history[0].ClientID != operation ||
		history[0].Text != user.Text || !reflect.DeepEqual(history[0].Skills, skills) {
		t.Fatalf("restart/history lost receipt: %+v, %v", history, err)
	}
}

func TestClaudeSkillReceiptNeverGuessesFromSlashOrCrossesScope(t *testing.T) {
	h := newClaudeHistory(filepath.Join(t.TempDir(), "projects"))
	uuid := "11111111-2222-4333-8444-555555555555"
	refs := []protocol.SkillReference{{Name: "same", Path: "C:/exact/SKILL.md"}}
	if err := h.saveSkillReceipt("session", uuid, "/same original", uuid, refs, nil); err != nil {
		t.Fatal(err)
	}
	message := json.RawMessage(`{"role":"user","content":"/same original"}`)
	for _, variant := range []string{"uuid", "session", "peer", "assistant", "sidechain", "parent", "meta", "compact", "missing UUID"} {
		t.Run(variant, func(t *testing.T) {
			line := claudeLine{Type: "user", UUID: uuid, SessionIDT: "session", Message: message}
			session := "session"
			switch variant {
			case "uuid":
				line.UUID = "21111111-2222-4333-8444-555555555555"
			case "session":
				session = "other"
			case "peer":
				line.Origin = json.RawMessage(`{"kind":"peer"}`)
			case "assistant":
				line.Type = "assistant"
			case "sidechain":
				line.IsSidechain = true
			case "parent":
				parent := "tool"
				line.ParentTU = &parent
			case "meta":
				line.IsMeta = true
			case "compact":
				line.IsCompactSummary = true
			case "missing UUID":
				line.UUID = ""
			}
			mapped := []protocol.Event{{Type: "message", ID: line.UUID, Role: "user", Text: "/same original"}}
			got := h.projectSkillReceipt(session, &line, mapped)
			if len(got) != 1 || len(got[0].Skills) != 0 || got[0].ClientID != "" {
				t.Fatal("unrelated message acquired receipt metadata", got)
			}
		})
	}
	r, ok := h.readSkillReceipt("session", uuid)
	if !ok || !reflect.DeepEqual(r.Skills, refs) {
		t.Fatal("receipt discarded exact path identity", r)
	}
}

func TestClaudeSkillReceiptMalformedFilesFailClosed(t *testing.T) {
	h := newClaudeHistory(filepath.Join(t.TempDir(), "projects"))
	uuid := "11111111-2222-4333-8444-555555555555"
	if err := h.saveSkillReceipt("session", uuid, "text", "", []protocol.SkillReference{{Name: "review"}}, nil); err != nil {
		t.Fatal(err)
	}
	path := h.skillReceiptPath("session", uuid)
	if err := h.saveSkillReceipt("session", uuid, "replacement", "", []protocol.SkillReference{{Name: "other"}}, nil); err == nil {
		t.Fatal("existing receipt overwritten")
	}
	for _, content := range []string{`{`, strings.Repeat("x", maxClaudeSkillReceiptBytes+1),
		`{"version":1,"sessionId":"other","uuid":"` + uuid + `","skills":[{"name":"review"}]}`} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if _, ok := h.readSkillReceipt("session", uuid); ok {
			t.Fatal("corrupt/cross-session receipt accepted")
		}
	}
	if h.skillReceiptPath("../other", uuid) != "" || h.skillReceiptPath("session", "../other") != "" {
		t.Fatal("receipt path escaped its exact namespace")
	}
}
