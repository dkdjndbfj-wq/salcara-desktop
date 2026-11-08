package hubclient

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/protocol"
)

func testAttachmentStore(t *testing.T) *attachmentStore {
	t.Helper()
	return &attachmentStore{dir: t.TempDir(), bindingsFile: filepath.Join(t.TempDir(), "bindings.json"),
		pending: map[string]*attachmentUpload{}, bindings: map[string]map[string]bool{}}
}

func TestAttachmentStoreBindsAndReadsBySession(t *testing.T) {
	s := testAttachmentStore(t)
	id := "att_0123456789abcdef0123456789abcdef"
	// Minimal JPEG signature plus payload; the store validates the magic bytes
	// before making the attachment readable.
	raw := append([]byte{0xff, 0xd8, 0xff, 0xe0}, []byte("salcara-image")...)
	encoded := base64.StdEncoding.EncodeToString(raw)
	complete, err := s.put(id, "image/jpeg", 0, 1, encoded)
	if err != nil || !complete {
		t.Fatalf("put = complete:%v err:%v", complete, err)
	}
	if err := s.bind([]string{id}, "codex:thread-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.read(id, "codex:thread-b", 0, 90_000); err == nil {
		t.Fatal("cross-session read unexpectedly succeeded")
	}
	got, err := s.read(id, "codex:thread-a", 0, 90_000)
	if err != nil {
		t.Fatal(err)
	}
	if got["complete"] != true || got["size"] != int64(len(raw)) {
		t.Fatalf("unexpected read metadata: %#v", got)
	}
	path, ok := s.find(id)
	if !ok {
		t.Fatal("completed attachment was not persisted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	// A bridge restart can still authorize the persisted opaque reference while
	// the short-lived upload bytes are inside their retention window.
	s2 := testAttachmentStore(t)
	s2.dir, s2.bindingsFile, s2.bindings = s.dir, s.bindingsFile, map[string]map[string]bool{}
	s2.loadBindings()
	if _, err := s2.read(id, "codex:thread-a", 0, 90_000); err != nil {
		t.Fatalf("persisted binding was not restored: %v", err)
	}
}

func TestAttachmentStoreRejectsBadImage(t *testing.T) {
	s := testAttachmentStore(t)
	s.dir = t.TempDir()
	s.bindingsFile = t.TempDir() + string(os.PathSeparator) + "bindings.json"
	id := "att_abcdefabcdefabcdefabcdefabcdefab"
	bad := base64.StdEncoding.EncodeToString([]byte("not an image"))
	if _, err := s.put(id, "image/png", 0, 1, bad); err == nil {
		t.Fatal("bad image was accepted")
	}
}

func TestComputerUseFrameIsOpaqueAndBoundToItsActualSession(t *testing.T) {
	previous := phoneAttachments
	phoneAttachments = testAttachmentStore(t)
	defer func() { phoneAttachments = previous }()
	image := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\ncomputer-use-fixture"))
	event := protocol.Event{SessionKey: "codex:computer-owner", ID: "actual-call", Type: "tool", Kind: "mcp", Status: "done", Computer: &protocol.ComputerActivity{Version: 1, Surface: "computer", Action: "screenshot", Provider: "computer"}, ImageData: []string{image}}
	decorateEventMedia(&event)
	if len(event.ImageData) != 0 || len(event.Attachments) != 1 || event.Computer == nil || event.ID != "actual-call" {
		t.Fatalf("lost screenshot ownership: %+v", event)
	}
	if _, err := phoneAttachments.read(event.Attachments[0].ID, event.SessionKey, 0, 90000); err != nil {
		t.Fatal(err)
	}
	if _, err := phoneAttachments.read(event.Attachments[0].ID, "codex:another-thread", 0, 90000); err == nil {
		t.Fatal("computer frame crossed thread boundary")
	}
}

func TestDecorateEventMediaNeverLeaksMissingImagePath(t *testing.T) {
	previous := phoneAttachments
	phoneAttachments = testAttachmentStore(t)
	phoneAttachments.dir = t.TempDir()
	defer func() { phoneAttachments = previous }()

	missing := filepath.Join(t.TempDir(), "generated.png")
	event := protocol.Event{SessionKey: "codex:missing-image", Type: "tool", Kind: "read", Detail: missing}
	decorateEventMedia(&event)
	if event.Detail != "" || len(event.Attachments) != 0 {
		t.Fatalf("missing image path leaked or became an attachment: %#v", event)
	}

	for _, kind := range []string{"read", "image_view", "image_generation"} {
		path := filepath.Join(t.TempDir(), "generated.png")
		if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nfixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		event = protocol.Event{SessionKey: "codex:generated-" + kind, Type: "tool", Kind: kind, Detail: path}
		decorateEventMedia(&event)
		if event.Detail != "" || len(event.Attachments) != 1 || event.Attachments[0].Mime != "image/png" {
			t.Fatalf("valid %s image was not converted to an opaque ref: %#v", kind, event)
		}
	}

	data := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nbase64-fixture"))
	event = protocol.Event{SessionKey: "codex:generated-base64", Type: "tool", Kind: "read", Detail: "image-base64:" + data}
	decorateEventMedia(&event)
	if event.Detail != "" || len(event.Attachments) != 1 || event.Attachments[0].Mime != "image/png" {
		t.Fatalf("base64 image was not converted to an opaque ref: %#v", event)
	}

	event = protocol.Event{SessionKey: "claude:assistant-image", Type: "message", ImageData: []string{data}}
	decorateEventMedia(&event)
	if len(event.ImageData) != 0 || len(event.Attachments) != 1 || event.Attachments[0].Mime != "image/png" {
		t.Fatalf("message image was not converted to an opaque ref: %#v", event)
	}
}
