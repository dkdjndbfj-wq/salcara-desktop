package desktopcompanion

import (
	"context"
	"testing"
)

func TestNativeDirectoryPreservesScopedPinsAndSidebarOrder(t *testing.T) {
	f := gatewayFixture(t)
	f.descriptor.SessionKeys = []string{"codex:" + targetID, "codex:" + otherID}
	f.write(t)
	f.result = func(cmd map[string]any) any {
		return nativeEnvelope(map[string]any{"schemaVersion": 4,
			"pinnedThreads": []any{
				map[string]any{"id": controllerID, "kind": "codex", "hostId": "local", "pinnedIndex": 1},
				map[string]any{"id": targetID, "kind": "codex", "hostId": "local", "pinnedIndex": 4, "updatedAt": 1},
			},
			"threads": []any{
				map[string]any{"id": otherID, "kind": "codex", "hostId": "local", "pinnedIndex": 2, "updatedAt": 1000},
				map[string]any{"id": targetID, "kind": "codex", "hostId": "local", "updatedAt": 2000},
			}})
	}
	ss, err := f.service.NativeList(context.Background())
	if err != nil || len(ss) != 2 {
		t.Fatal(ss, err)
	}
	if ss[0].SessionKey != "codex:"+targetID || ss[0].PinnedIndex != 4 || ss[0].SidebarIndex != 1 ||
		ss[1].SessionKey != "codex:"+otherID || ss[1].PinnedIndex != 0 || ss[1].SidebarIndex != 2 {
		t.Fatal("native pinned/sidebar order was discarded or inferred from recency", ss)
	}
}

func TestNativeDirectoryFallsBackOnlyToActualPinnedArrayPosition(t *testing.T) {
	f := gatewayFixture(t)
	f.result = func(cmd map[string]any) any {
		return nativeEnvelope(map[string]any{"schemaVersion": 4, "pinnedThreads": []any{
			map[string]any{"id": controllerID, "kind": "codex", "hostId": "local"},
			map[string]any{"id": targetID, "kind": "codex", "hostId": "local", "pinnedIndex": -1},
		}, "threads": []any{}})
	}
	ss, err := f.service.NativeList(context.Background())
	if err != nil || len(ss) != 1 || ss[0].PinnedIndex != 2 || ss[0].SidebarIndex != 1 {
		t.Fatal(ss, err)
	}
}
