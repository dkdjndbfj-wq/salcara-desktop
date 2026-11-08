package agents

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

type claudeReloadInvocation struct {
	source, body string
	frame        map[string]any
}

// This mock models the official SDK contract: reload_skills rereads native
// definitions, but exposes only SlashCommand metadata to the bridge. Its source
// path is controlled solely by the test and never appears in a native response.
type claudeReloadFixture struct {
	source, loadedSource, loadedBody string
	catalog                          []map[string]any
	reject                           bool
	reloads                          int
	onReload                         func()
	invocations                      []claudeReloadInvocation
}

func newClaudeReloadFixture(t *testing.T) (*claudeAgent, *claudeProc, *claudeReloadFixture) {
	t.Helper()
	a, p, _ := newClaudeResultFixture(t)
	f := &claudeReloadFixture{catalog: []map[string]any{{"name": "plugin:review", "description": "Unchanged description", "argumentHint": ""}}}
	p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) {
		var frame map[string]any
		if err := json.Unmarshal(data, &frame); err != nil {
			t.Fatal(err)
		}
		switch frame["type"] {
		case "control_request":
			response := map[string]any{"subtype": "success", "request_id": frame["request_id"], "response": map[string]any{}}
			if frame["request"].(map[string]any)["subtype"] == "reload_skills" {
				f.reloads++
				if f.onReload != nil {
					f.onReload()
				}
				if f.reject {
					response["subtype"], response["error"] = "error", "fixture reload failed"
				} else {
					catalog := f.catalog
					if f.source != "" {
						body, err := os.ReadFile(f.source)
						if err != nil {
							catalog = []map[string]any{}
							f.loadedSource, f.loadedBody = "", ""
						} else {
							f.loadedSource, f.loadedBody = f.source, string(body)
						}
					}
					response["response"] = map[string]any{"skills": catalog}
				}
			}
			raw, _ := json.Marshal(map[string]any{"type": "control_response", "response": response})
			a.handleLine(p, raw)
		case "user":
			f.invocations = append(f.invocations, claudeReloadInvocation{source: f.loadedSource, body: f.loadedBody, frame: frame})
			raw, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "session_id": p.id, "uuid": fmt.Sprintf("reload-result-%d", len(f.invocations))})
			a.handleLine(p, raw)
		}
		return len(data), nil
	}}
	return a, p, f
}

func TestClaudeFeaturesEverySkillSendReloadsUnchangedMetadata(t *testing.T) {
	a, p, f := newClaudeReloadFixture(t)
	f.source = filepath.Join(t.TempDir(), "SKILL.md")
	writeBody := func(body string) {
		t.Helper()
		if err := os.WriteFile(f.source, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeBody("first body")
	catalog, err := a.Features(context.Background(), p.cwd, p.id)
	if err != nil || len(catalog.Skills) != 1 || catalog.Skills[0].Path != "" {
		t.Fatalf("unexpected native catalog: %+v, %v", catalog, err)
	}
	opts := TurnOptions{Skills: []protocol.SkillReference{{Name: "plugin:review"}}}
	for _, body := range []string{"first body", "updated body"} {
		writeBody(body)
		before := f.reloads
		if err := a.SendWithOptions(context.Background(), p.id, "inspect", opts); err != nil {
			t.Fatal(err)
		}
		last := f.invocations[len(f.invocations)-1]
		if f.reloads != before+1 || last.body != body || last.source != f.source {
			t.Fatalf("same-metadata send reused a stale native definition: %+v", last)
		}
	}
	oldSource := f.source
	f.source = filepath.Join(t.TempDir(), "SKILL.md")
	if err := os.Rename(oldSource, f.source); err != nil {
		t.Fatal(err)
	}
	refreshed, err := a.Features(context.Background(), p.cwd, p.id)
	if err != nil || !reflect.DeepEqual(catalog, refreshed) {
		t.Fatalf("native metadata invented body/path identity: %+v, %v", refreshed, err)
	}
	before := f.reloads
	if err := a.SendWithOptions(context.Background(), p.id, "inspect after move", opts); err != nil {
		t.Fatal(err)
	}
	last := f.invocations[len(f.invocations)-1]
	if f.reloads != before+1 || last.source != f.source || last.body != "updated body" {
		t.Fatalf("send reused the definition from the old source: %+v", last)
	}
	f.reject = true
	if err := a.SendWithOptions(context.Background(), p.id, "must not send", opts); err == nil || !strings.HasPrefix(err.Error(), "feature_control_rejected:") || len(f.invocations) != 3 {
		t.Fatalf("failed reload reused a previous definition: invocations=%d err=%v", len(f.invocations), err)
	}
	f.reject = false
	if err := os.Remove(f.source); err != nil {
		t.Fatal(err)
	}
	if err := a.SendWithOptions(context.Background(), p.id, "must not send", opts); err == nil || !strings.HasPrefix(err.Error(), "skill_unavailable:") || len(f.invocations) != 3 {
		t.Fatalf("removed skill reached the native user frame: invocations=%d err=%v", len(f.invocations), err)
	}
}

func TestClaudeFeaturesBuiltinOrDuplicateNameCannotSelectSkill(t *testing.T) {
	for _, builtinFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(builtinFirst), func(t *testing.T) {
			a, p, f := newClaudeReloadFixture(t)
			shadowed := []map[string]any{{"name": "shadowed", "description": "User skill"}, {"name": "shadowed", "description": "Native command", "builtin": true}}
			if builtinFirst {
				shadowed[0], shadowed[1] = shadowed[1], shadowed[0]
			}
			f.catalog = append(f.catalog, shadowed...)
			f.catalog = append(f.catalog,
				map[string]any{"name": "native", "builtin": true},
				map[string]any{"name": "duplicate", "description": "First source"},
				map[string]any{"name": "duplicate", "description": "Second source"})
			catalog, err := a.Features(context.Background(), p.cwd, p.id)
			if err != nil || !catalog.SkillsSupported || len(catalog.Skills) != 1 || catalog.Skills[0].Name != "plugin:review" {
				t.Fatalf("ambiguous native commands advertised as selectable skills: %+v, %v", catalog, err)
			}
			for _, name := range []string{"shadowed", "native", "duplicate"} {
				err := a.SendWithOptions(context.Background(), p.id, "must not send", TurnOptions{Skills: []protocol.SkillReference{{Name: name}}})
				if err == nil || !strings.HasPrefix(err.Error(), "skill_unavailable:") || len(f.invocations) != 0 {
					t.Fatalf("ambiguous selection %q reached a user frame: %v", name, err)
				}
			}
		})
	}
}

func TestClaudeFeaturesAttachmentsAreReadBeforeNativeReload(t *testing.T) {
	a, p, f := newClaudeReloadFixture(t)
	path := filepath.Join(t.TempDir(), "fixture.png")
	body := []byte("synthetic image")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	// A deterministic filesystem change during reload demonstrates that pending
	// attachment IO cannot delay a previously acknowledged skill reload.
	f.onReload = func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.SendWithOptions(context.Background(), p.id, "inspect", TurnOptions{Images: []string{path}, Skills: []protocol.SkillReference{{Name: "plugin:review"}}}); err != nil {
		t.Fatal(err)
	}
	content := f.invocations[0].frame["message"].(map[string]any)["content"].([]any)
	image := content[0].(map[string]any)
	if len(content) != 2 || image["type"] != "image" || image["source"].(map[string]any)["data"] != base64.StdEncoding.EncodeToString(body) {
		t.Fatalf("prepared attachment was lost after reload: %+v", content)
	}
}

func TestClaudeFeaturesReloadCannotBypassAdmissionGuard(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelled), func(t *testing.T) {
			a, p, f := newClaudeReloadFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			revoked := false
			denied := errors.New("fixture permission revoked")
			f.onReload = func() {
				if cancelled {
					cancel()
				} else {
					revoked = true
				}
			}
			err := a.SendWithOptions(ctx, p.id, "must not send", TurnOptions{
				Skills: []protocol.SkillReference{{Name: "plugin:review"}},
				ValidatePermission: func() error {
					if revoked {
						return denied
					}
					return nil
				},
			})
			want := denied
			if cancelled {
				want = context.Canceled
			}
			if !errors.Is(err, want) || len(f.invocations) != 0 {
				t.Fatalf("acknowledged reload bypassed final admission: invocations=%d err=%v", len(f.invocations), err)
			}
		})
	}
}
