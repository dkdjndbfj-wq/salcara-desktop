package agents

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestSkillNativeCatalogAppearanceSurvivesRestartWithoutChangingInvocation(t *testing.T) {
	a, f, _ := newCodexFeatureFixture(t)
	a.stateFile = filepath.Join(t.TempDir(), "state.json")
	skillPath := f.skills[0]["path"].(string)
	if err := os.MkdirAll(filepath.Dir(skillPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillPath, []byte("# review"), 0600); err != nil {
		t.Fatal(err)
	}
	var imageData bytes.Buffer
	if err := png.Encode(&imageData, image.NewNRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(skillPath), "icon.png"), imageData.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	f.skills[0]["interface"] = map[string]any{"displayName": "代码检查", "brandColor": "#8A42EF", "iconSmall": "icon.png"}
	catalog, err := a.Features(context.Background(), f.cwd, "feature-thread")
	if err != nil || len(catalog.Skills) != 1 || catalog.Skills[0].IconSmall == "" {
		t.Fatal(catalog, err)
	}
	expected := skillReferenceWithDisplay(catalog.Skills[0])
	operation := "11111111-2222-4333-8444-555555555555"
	if err := a.SendWithOptions(context.Background(), "feature-thread", "body", TurnOptions{Skills: []protocol.SkillReference{{Name: "review", Path: skillPath, DisplayName: "untrusted phone label"}}, ClientUserMessageID: operation}); err != nil {
		t.Fatal(err)
	}
	calls := featureCalls(f, "turn/start")
	for _, raw := range calls[0]["input"].([]any) {
		part := raw.(map[string]any)
		if part["type"] == "skill" && (len(part) != 3 || part["name"] != "review" || part["path"] != skillPath) {
			t.Fatal("display changed execution", part)
		}
	}
	live := a.projectInputReceipt(protocol.Event{Type: "message", Role: "user", SessionKey: "codex:feature-thread", ID: "native-message", ClientID: operation})
	if !reflect.DeepEqual(live.Skills, []protocol.SkillReference{expected}) {
		t.Fatal("catalog snapshot not retained", live.Skills)
	}
	if err := os.Remove(skillPath); err != nil {
		t.Fatal(err)
	}
	restarted := &codexAgent{stateFile: a.stateFile}
	history := restarted.projectInputReceipt(protocol.Event{Type: "message", Role: "user", SessionKey: "codex:feature-thread", ID: "native-message"})
	if !reflect.DeepEqual(history.Skills, live.Skills) || history.Text != "body" {
		t.Fatal("restart lost deleted skill appearance", history)
	}
}

func TestSkillCatalogIconConfinementAndNormalization(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "skill")
	if err := os.MkdirAll(skillDir, 0700); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skill, []byte("# skill"), 0600); err != nil {
		t.Fatal(err)
	}
	source := image.NewNRGBA(image.Rect(0, 0, 128, 64))
	source.Set(0, 0, color.NRGBA{R: 255, A: 255})
	var buf bytes.Buffer
	if err := png.Encode(&buf, source); err != nil {
		t.Fatal(err)
	}
	icon := filepath.Join(skillDir, "icon.png")
	outside := filepath.Join(root, "outside.png")
	for _, path := range []string{icon, outside} {
		if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	value := readSkillCatalogIcon(skill, "icon.png")
	if !validSkillIcon(value) || value == "" {
		t.Fatal("valid icon missing")
	}
	data, _ := base64.StdEncoding.DecodeString(value[len(skillIconPrefix):])
	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width != 32 || config.Height != 16 {
		t.Fatalf("wrong normalized image: %+v %v", config, err)
	}
	for _, path := range []string{outside, "../outside.png", "https://example.com/icon.png", "data:image/png;base64,AAAA", "missing.png"} {
		if readSkillCatalogIcon(skill, path) != "" {
			t.Fatalf("unsafe icon accepted: %s", path)
		}
	}
	if err := os.Symlink(outside, filepath.Join(skillDir, "link.png")); err == nil {
		if readSkillCatalogIcon(skill, "link.png") != "" {
			t.Fatal("symlink escape accepted")
		}
	}
	if err := os.WriteFile(icon, []byte(`<svg onload="alert(1)"/>`), 0600); err != nil {
		t.Fatal(err)
	}
	if readSkillCatalogIcon(skill, icon) != "" {
		t.Fatal("SVG was accepted")
	}
}

func TestSkillDisplayMetadataCannotAlterIdentity(t *testing.T) {
	original := protocol.Skill{Name: "review", Path: "/a/SKILL.md", DisplayName: "代码检查", BrandColor: "#8A42EF"}
	ref := skillReferenceWithDisplay(original)
	if ref.Name != "review" || ref.Path != "/a/SKILL.md" || ref.DisplayName != original.DisplayName || ValidateTurnFeatures(TurnOptions{Skills: []protocol.SkillReference{ref}}) != nil {
		t.Fatal("snapshot lost")
	}
	for _, bad := range []protocol.SkillReference{{Name: "review", DisplayName: "bad\nname"}, {Name: "review", BrandColor: "url(https://example.com)"}, {Name: "review", IconSmall: "https://example.com/icon.png"}} {
		if ValidateTurnFeatures(TurnOptions{Skills: []protocol.SkillReference{bad}}) == nil {
			t.Fatal("invalid appearance accepted")
		}
	}
}
