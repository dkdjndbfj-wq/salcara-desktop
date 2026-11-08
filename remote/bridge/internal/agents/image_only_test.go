package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/protocol"
)

func TestCodexImageOnlyTurnDoesNotInventText(t *testing.T) {
	a, transport := permissionFixture(t)
	path := filepath.Join(t.TempDir(), "fixture.png")
	if err := a.SendWithOptions(context.Background(), "permission-thread", "", TurnOptions{Images: []string{path}}); err != nil {
		t.Fatal(err)
	}
	for _, req := range transport.requests {
		if req["method"] != "turn/start" {
			continue
		}
		input := req["params"].(map[string]any)["input"].([]any)
		if len(input) != 1 || input[0].(map[string]any)["type"] != "localImage" || input[0].(map[string]any)["path"] != path {
			t.Fatalf("unexpected image-only input: %+v", input)
		}
		return
	}
	t.Fatal("no turn was sent")
}

func TestCodexEmptyInputCannotQueueAttachmentEcho(t *testing.T) {
	a, _ := permissionFixture(t)
	called := false
	err := a.startTurnLocked(context.Background(), nil, "permission-thread", "  ", TurnOptions{
		Attachments:        []protocol.AttachmentRef{{ID: "att_without_image"}},
		ValidatePermission: func() error { called = true; return nil },
	})
	if err == nil || err.Error() != "请输入内容或添加图片" || called {
		t.Fatalf("empty input reached permission admission: err=%v called=%v", err, called)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.threadLocked("permission-thread").pendingAttachments) != 0 {
		t.Fatal("empty input left an attachment echo for a later turn")
	}
}

func TestClaudeImageOnlyTurnDoesNotInventText(t *testing.T) {
	rec := &recorder{}
	a := newClaudeAgent(rec.sink, func() Settings { return Settings{ClaudeRoot: "http://127.0.0.1:1", ClaudeKey: "synthetic-api-key"} }, "not-launched", t.TempDir())
	t.Cleanup(a.Close)
	p := claudeSyntheticPermissionWorker(t, a, "fixture-session")
	p.mapper = newClaudeMapper("claude:"+p.id, p.cwd)
	path := filepath.Join(t.TempDir(), "fixture.png")
	if err := os.WriteFile(path, []byte("synthetic fixture bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	var written []byte
	p.stdin = &claudeDeliveryFixtureWriter{write: func(data []byte) (int, error) { written = append([]byte(nil), data...); return len(data), nil }}
	refs := []protocol.AttachmentRef{{ID: "att_fixture", Mime: "image/png"}}
	if err := a.writeTurn(p, "", []string{path}, refs); err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Message struct {
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(written, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire.Message.Content) != 1 || wire.Message.Content[0]["type"] != "image" {
		t.Fatalf("unexpected image-only wire: %+v", wire)
	}
	for _, e := range rec.all() {
		if e.Type == "message" && e.Role == "user" {
			if e.Text != "" || len(e.Attachments) != 1 {
				t.Fatal("original image-only echo was lost")
			}
			return
		}
	}
	t.Fatal("no local image-only echo")
}
