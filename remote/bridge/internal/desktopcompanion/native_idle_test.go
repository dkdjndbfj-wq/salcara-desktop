package desktopcompanion

import (
	"context"
	"encoding/json"
	"testing"
)

func TestNativeIdleDoesNotTreatAnUnknownOrWaitingThreadAsSafe(t *testing.T) {
	for _, raw := range []string{`null`, `{}`, `"active"`, `{"type":"idle","activeFlags":["waitingOnUserInput"]}`, `{"type":"active","activeFlags":[]}`} {
		if nativeThreadIdle(json.RawMessage(raw)) {
			t.Fatalf("unknown/busy status accepted: %s", raw)
		}
	}
	for _, raw := range []string{`"idle"`, `{"type":"idle","activeFlags":[]}`, `"systemError"`} {
		if !nativeThreadIdle(json.RawMessage(raw)) {
			t.Fatalf("terminal status rejected: %s", raw)
		}
	}
}

func TestNativeStationIdleRequiresEveryAuthorizedThread(t *testing.T) {
	f := gatewayFixture(t)
	for _, status := range []string{"idle", "active", ""} {
		f.result = func(map[string]any) any {
			return nativeEnvelope(map[string]any{"schemaVersion": 4, "pinnedThreads": []any{}, "threads": []any{map[string]any{"id": targetID, "kind": "codex", "hostId": "local", "status": status}}})
		}
		err := f.service.NativeStationIdle(context.Background())
		if (err == nil) != (status == "idle") {
			t.Fatalf("status=%s err=%v", status, err)
		}
	}
	f.result = func(map[string]any) any {
		return nativeEnvelope(map[string]any{"schemaVersion": 4, "pinnedThreads": []any{}, "threads": []any{}})
	}
	if f.service.NativeStationIdle(context.Background()) == nil {
		t.Fatal("missing authorized thread treated as idle")
	}
}
