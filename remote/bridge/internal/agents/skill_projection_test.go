package agents

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestCodexSkillProjectionPreservesOnlyNativeSkillIdentity(t *testing.T) {
	parts := []cxUserInput{
		{Type: "text", Text: "literal $review /review @review"},
		{Type: "mention", Name: "review", Path: "C:/not-a-skill"},
		{Type: "skill", Name: "review", Path: "C:/项目/first/SKILL.md"},
		{Type: "skill", Name: "review", Path: "C:/项目/second/SKILL.md"},
		{Type: "skill", Name: "review", Path: "C:/项目/first/SKILL.md"},
	}
	raw, _ := json.Marshal(parts)
	want := []protocol.SkillReference{{Name: "review", Path: parts[2].Path}, {Name: "review", Path: parts[3].Path}}
	e, ok := codexItemEvent("codex:session", cxItem{Type: "userMessage", ID: "user", Content: raw}, true, "C:/项目")
	if !ok || !reflect.DeepEqual(e.Skills, want) || e.Text != "literal $review /review @review\n@review" {
		t.Fatalf("native identities merged, lost, or inferred: %+v", e)
	}
	items, _ := json.Marshal(cxItem{Type: "userMessage", ID: "user", Content: raw})
	history := codexHistory("codex:session", cxThread{Turns: []cxTurn{{Items: []json.RawMessage{items}}}})
	if len(history) != 1 || !reflect.DeepEqual(history[0].Skills, want) {
		t.Fatalf("history discarded native references: %+v", history)
	}
	for _, typ := range []string{"agentMessage", "reasoning", "commandExecution"} {
		e, _ := codexItemEvent("codex:session", cxItem{Type: typ, Text: "$review", Content: raw}, true, "")
		if len(e.Skills) != 0 {
			t.Fatal("non-user event acquired user skills", typ)
		}
	}
	skillOnly, _ := json.Marshal(parts[2:4])
	e, ok = codexItemEvent("codex:session", cxItem{Type: "userMessage", Content: skillOnly}, true, "")
	if !ok || e.Text != "" || !reflect.DeepEqual(e.Skills, want) {
		t.Fatal("skill-only native input disappeared or duplicated its label", e)
	}
}

func TestCodexSkillProjectionRejectsMalformedAndBoundsMetadata(t *testing.T) {
	for _, input := range []string{
		`{"type":"skill","name":"review","path":"/SKILL.md"}`,
		`[{"type":"text","text":"$review"},{"type":"mention","name":"review","path":"/SKILL.md"}]`,
		`[{"type":"skill","name":"review"}]`,
		`[{"type":"skill","name":"bad\nname","path":"/SKILL.md"}]`,
	} {
		if refs := codexUserInputSkills(json.RawMessage(input)); len(refs) != 0 {
			t.Fatal("noncanonical content acquired skill identity", refs)
		}
	}
	var parts []cxUserInput
	for i := 0; i < 12; i++ {
		parts = append(parts, cxUserInput{Type: "skill", Name: "review", Path: "/" + strings.Repeat("x", i+1) + "/SKILL.md"})
	}
	raw, _ := json.Marshal(parts)
	if refs := codexUserInputSkills(raw); len(refs) != 8 {
		t.Fatal("unbounded skill metadata", len(refs))
	}
}
