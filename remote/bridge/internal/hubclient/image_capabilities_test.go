package hubclient

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"salcara/bridge/internal/protocol"
)

func prepareCapabilityImage(t *testing.T) string {
	t.Helper()
	previous := phoneAttachments
	phoneAttachments = newAttachmentStore()
	phoneAttachments.dir = t.TempDir()
	t.Cleanup(func() { phoneAttachments = previous })
	id := "att_" + strings.Repeat("ab", 16)
	image := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte(strings.Repeat("x", 100))...)
	if done, err := phoneAttachments.put(id, "image/jpeg", 0, 1, base64.StdEncoding.EncodeToString(image)); err != nil || !done {
		t.Fatalf("fixture attachment failed: %v", err)
	}
	return id
}

func TestExplicitTextOnlyModelRejectsImagesBeforeStartingWorker(t *testing.T) {
	for _, typ := range []string{"session.start", "session.send", "session.send-known-model"} {
		t.Run(typ, func(t *testing.T) {
			c, worker, cwd := effortClient(t)
			worker.knownModel = "effort-fixture"
			worker.catalog = &protocol.ModelCatalog{Models: []string{"effort-fixture"}, ModelCapabilities: map[string]protocol.ModelCapability{
				"effort-fixture": {Source: "codex-model-list", InputModalities: []string{"text"}},
			}}
			id := prepareCapabilityImage(t)
			model := "effort-fixture"
			if typ == "session.send-known-model" {
				typ, model = "session.send", ""
			}
			res, err := c.Dispatch(context.Background(), map[string]any{"type": typ, "tool": "codex", "cwd": cwd, "prompt": "must not start", "sessionKey": "codex:1", "text": "must not send", "model": model, "attachments": []any{id}})
			if err == nil || res != nil || !strings.Contains(err.Error(), "仅支持文字") {
				t.Fatalf("res=%v err=%v", res, err)
			}
			if len(worker.started)+len(worker.sent)+len(worker.startOptions)+len(worker.sendOptions) != 0 {
				t.Fatal("text-only image reached worker")
			}
		})
	}
}

func TestImageCapabilityGuardPreservesUnknownAndReportedImageTransport(t *testing.T) {
	for _, fixture := range []struct {
		name, source string
		modalities   []string
	}{
		{"missing-metadata", "codex-model-list", nil},
		{"unknown-relay", "relay-model-list", []string{"text"}},
		{"explicit-image", "codex-model-list", []string{"text", "image"}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			c, worker, _ := effortClient(t)
			worker.catalog = &protocol.ModelCatalog{Models: []string{"effort-fixture"}, ModelCapabilities: map[string]protocol.ModelCapability{
				"effort-fixture": {Source: fixture.source, InputModalities: fixture.modalities},
			}}
			id := prepareCapabilityImage(t)
			if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": "codex:1", "text": "fixture image", "model": "effort-fixture", "attachments": []any{id}}); err != nil {
				t.Fatal(err)
			}
			if len(worker.sendOptions) != 1 || len(worker.sendOptions[0].Images) != 1 {
				t.Fatal("existing compatible image transport changed")
			}
		})
	}
}
