package agents

import (
	"context"
	"encoding/json"
	"testing"
)

type modelAckTransport struct {
	base      *resumeSafetyTransport
	beforeAck func()
}

func (w *modelAckTransport) Close() error { return nil }
func (w *modelAckTransport) Write(b []byte) (int, error) {
	var r struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return 0, err
	}
	if r.Method == "turn/start" {
		w.beforeAck()
	}
	return w.base.Write(b)
}

func TestCodexModelAckCannotOverwriteSuccessor(t *testing.T) {
	for _, state := range []string{"same-running", "same-completed", "successor-running"} {
		t.Run(state, func(t *testing.T) {
			a, p := permissionFixture(t)
			a.conn.stdin = &modelAckTransport{base: p.base, beforeAck: func() {
				id := "fixture-next-turn"
				if state == "successor-running" {
					id = "successor"
				}
				a.onNotify("turn/started", json.RawMessage(`{"threadId":"permission-thread","turn":{"id":"`+id+`","status":"inProgress"}}`))
				if state == "same-completed" {
					a.onNotify("turn/completed", json.RawMessage(`{"threadId":"permission-thread","turn":{"id":"fixture-next-turn","status":"completed"}}`))
				}
				if state == "successor-running" {
					a.mu.Lock()
					a.threads["permission-thread"].info.Model = "successor-model"
					a.mu.Unlock()
				}
			}}
			if err := a.SendWithOptions(context.Background(), "permission-thread", "synthetic", TurnOptions{Model: "chosen-model"}); err != nil {
				t.Fatal(err)
			}
			want := "chosen-model"
			if state == "successor-running" {
				want = "successor-model"
			}
			if got := a.KnownSessionModel("permission-thread"); got != want {
				t.Fatalf("model=%q want %q", got, want)
			}
		})
	}
}
