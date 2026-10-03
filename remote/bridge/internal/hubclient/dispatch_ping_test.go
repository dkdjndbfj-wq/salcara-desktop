package hubclient

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDispatchDevicePingWithoutManagerOrConfig(t *testing.T) {
	// Zero Client: nil Manager, nil Store, no transport or model. Any attempt to
	// read configuration would panic; a transport probe must remain independent.
	c := &Client{}
	before := time.Now().UnixMilli()
	result, err := c.Dispatch(context.Background(), map[string]any{
		"type": "device.ping", "nonce": "AZaz09_-fixture",
		"secret": "must-not-be-reflected", "sessionKey": "must-not-be-read",
	})
	after := time.Now().UnixMilli()
	if err != nil {
		t.Fatal(err)
	}
	reply, ok := result.(map[string]any)
	if !ok || len(reply) != 2 || reply["nonce"] != "AZaz09_-fixture" {
		t.Fatalf("unexpected finite ping reply: %#v", result)
	}
	receivedAt, ok := reply["receivedAt"].(int64)
	if !ok || receivedAt < before || receivedAt > after {
		t.Fatalf("receivedAt is not the receipt time in Unix milliseconds: %#v", reply["receivedAt"])
	}
	encoded, err := json.Marshal(reply)
	if err != nil || len(encoded) > 128 || strings.Contains(string(encoded), "must-not") {
		t.Fatalf("probe returned unexpected data: %s (%v)", encoded, err)
	}
}

func TestDispatchDevicePingDoesNotWaitForManagerLock(t *testing.T) {
	c := &Client{}
	c.mgrMu.Lock()
	defer c.mgrMu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := c.Dispatch(context.Background(), map[string]any{"type": "device.ping", "nonce": "independent"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("device.ping waited on manager startup")
	}
}

func TestDispatchDevicePingNonceBoundsAndTypes(t *testing.T) {
	c := &Client{}
	for _, nonce := range []string{"a", "0", "_", "-", strings.Repeat("A", 64)} {
		result, err := c.Dispatch(context.Background(), map[string]any{"type": "device.ping", "nonce": nonce})
		if err != nil || result.(map[string]any)["nonce"] != nonce {
			t.Fatalf("valid nonce rejected: %q (%v)", nonce, err)
		}
	}
	for _, nonce := range []any{nil, true, 12, []string{"abc"}, map[string]string{"nonce": "abc"}, "", strings.Repeat("a", 65), "nonce space", " leading", "trailing ", "newline\n", "tab\t", "a.b", "x/y", "x?y", "x@y", "中文", "\x00", "a💡"} {
		result, err := c.Dispatch(context.Background(), map[string]any{"type": "device.ping", "nonce": nonce})
		if err == nil || result != nil || len(err.Error()) > 200 {
			t.Fatalf("invalid nonce yielded data or an unbounded error: %#v, %#v, %v", nonce, result, err)
		}
	}
	if result, err := c.Dispatch(context.Background(), map[string]any{"type": "device.ping"}); result != nil || err == nil {
		t.Fatal("missing nonce accepted")
	}
}
