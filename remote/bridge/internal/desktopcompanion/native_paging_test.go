package desktopcompanion

import (
	"context"
	"errors"
	"testing"
)

func TestNativeHistoryPagesRetainOriginalTargetAndRejectIncompleteCursor(t *testing.T) {
	f := gatewayFixture(t)
	f.result = func(cmd map[string]any) any {
		if cmd["type"] != "read" {
			t.Fatal(cmd)
		}
		if cmd["cursor"] != nil && cmd["cursor"] != "lease-signed-cursor" {
			t.Fatal("cursor changed", cmd)
		}
		more := cmd["cursor"] == nil
		page := map[string]any{"hasMore": more}
		if more {
			page["nextCursor"] = "lease-signed-cursor"
		}
		return nativeEnvelope(map[string]any{"schemaVersion": 1, "thread": map[string]any{"id": targetID, "kind": "codex", "hostId": "local"}, "turns": []any{}, "page": page})
	}
	info, events, next, err := f.service.NativeOpenPage(context.Background(), "codex:"+targetID, "")
	if err != nil || info.ControlSurface != "desktop" || len(events) != 0 || next != "lease-signed-cursor" {
		t.Fatal(info, events, next, err)
	}
	_, _, next, err = f.service.NativeOpenPage(context.Background(), "codex:"+targetID, next)
	if err != nil || next != "" {
		t.Fatal(next, err)
	}
	for _, page := range []map[string]any{{"hasMore": true}, {"hasMore": true, "nextCursor": "lease-signed-cursor"}, {"hasMore": true, "nextCursor": "bad\n"}} {
		f.result = func(map[string]any) any {
			return nativeEnvelope(map[string]any{"schemaVersion": 1, "thread": map[string]any{"id": targetID, "kind": "codex", "hostId": "local"}, "turns": []any{}, "page": page})
		}
		if _, _, _, err := f.service.NativeOpenPage(context.Background(), "codex:"+targetID, "lease-signed-cursor"); !errors.Is(err, ErrDesktopUnavailable) {
			t.Fatal(page, err)
		}
	}
}
