package agents

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"salcara/bridge/internal/protocol"
)

const steerOperation = "11111111-2222-4333-8444-555555555555"

func steerFixture(t *testing.T, beforeReply func(*codexAgent, string), replyTurn string) (*codexAgent, *[]map[string]any) {
	t.Helper()
	a, _, _ := newResumeSafetyAgent(t, cxThread{}, false)
	thread := a.threadLocked("target")
	thread.loaded, thread.turnID = true, "turn-1"
	thread.info.Status, thread.info.ActiveTurnID = "running", "turn-1"
	requests := []map[string]any{}
	c := a.conn
	c.stdin = deliveryFixtureWriter{write: func(b []byte) (int, error) {
		var req map[string]any
		if err := json.Unmarshal(b, &req); err != nil {
			return 0, err
		}
		requests = append(requests, req)
		method := req["method"].(string)
		if beforeReply != nil {
			beforeReply(a, method)
		}
		var result any
		switch method {
		case "config/read":
			cfg, _ := guardedProviderConfig(Settings{RelayRoot: "https://fixture.invalid"})
			result = map[string]any{"config": cfg}
		case "turn/steer":
			result = map[string]any{"turnId": replyTurn}
		default:
			t.Errorf("unexpected RPC: %s", method)
		}
		out, _ := json.Marshal(map[string]any{"id": req["id"], "result": result})
		c.handleLine(out)
		return len(b), nil
	}}
	return a, &requests
}

func TestCodexSteerUsesExactTurnWithoutOverridesAndDeduplicatesConcurrentCommands(t *testing.T) {
	a, requests := steerFixture(t, nil, "turn-1")
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := a.SteerTurn(context.Background(), "target", "focus tests", "turn-1", steerOperation, nil)
			if err != nil || !result.Accepted || result.TurnID != "turn-1" || result.OperationID != steerOperation {
				t.Errorf("steer result: %+v %v", result, err)
			}
		}()
	}
	wg.Wait()
	if len(*requests) != 2 {
		t.Fatalf("duplicate dispatched: %d", len(*requests))
	}
	req := (*requests)[1]
	params := req["params"].(map[string]any)
	if req["method"] != "turn/steer" || len(params) != 3 || params["expectedTurnId"] != "turn-1" || params["threadId"] != "target" {
		t.Fatalf("wrong protocol: %+v", req)
	}
	if _, err := a.SteerTurn(context.Background(), "target", "changed", "turn-1", steerOperation, nil); err == nil {
		t.Fatal("operation payload conflict accepted")
	}
	if _, err := a.SteerTurn(context.Background(), "target", "next", "turn-1", "22222222-2222-4333-8444-555555555555", nil); err != nil {
		t.Fatal(err)
	}
	if len(*requests) != 4 {
		t.Fatal("new input on same turn was incorrectly deduplicated")
	}
}

func TestCodexSteerRejectsUnownedStaleAndWaitingThreadsWithoutRPC(t *testing.T) {
	for _, change := range []string{"unloaded", "stale", "unknown", "approval", "permissions", "provider", "closed"} {
		t.Run(change, func(t *testing.T) {
			a, requests := steerFixture(t, nil, "turn-1")
			tr := a.threads["target"]
			switch change {
			case "unloaded":
				tr.loaded = false
			case "stale":
				tr.turnID = "turn-2"
			case "unknown":
				tr.statusUnconfirmed = true
			case "approval":
				tr.info.Status = "waiting_approval"
			case "permissions":
				tr.permissionUnconfirmed = true
			case "provider":
				a.connSig = "different-api"
			case "closed":
				a.conn = nil
			}
			_, err := a.SteerTurn(context.Background(), "target", "input", "turn-1", steerOperation, nil)
			if !errors.Is(err, ErrTurnControlStale) || len(*requests) != 0 {
				t.Fatalf("unsafe dispatch: %v %d", err, len(*requests))
			}
		})
	}
}

func TestCodexSteerRechecksAfterProviderReadAndDoesNotReviveCompletedTurn(t *testing.T) {
	a, requests := steerFixture(t, func(a *codexAgent, method string) {
		if method == "config/read" {
			a.mu.Lock()
			a.threads["target"].turnID = "turn-2"
			a.mu.Unlock()
		}
	}, "turn-1")
	_, err := a.SteerTurn(context.Background(), "target", "input", "turn-1", steerOperation, nil)
	if !errors.Is(err, ErrTurnControlStale) || len(*requests) != 1 {
		t.Fatalf("completed target was used: %v %+v", err, *requests)
	}
}

func TestCodexSteerLateConsentRevocationAndInvalidAcknowledgement(t *testing.T) {
	t.Run("revocation", func(t *testing.T) {
		a, requests := steerFixture(t, nil, "turn-1")
		n := 0
		denied := errors.New("synthetic revoked permission")
		_, err := a.SteerTurn(context.Background(), "target", "input", "turn-1", steerOperation, func() error {
			n++
			if n >= 3 {
				return denied
			}
			return nil
		})
		if !errors.Is(err, denied) || len(*requests) != 1 {
			t.Fatalf("late revocation lost: %v %d", err, len(*requests))
		}
	})
	t.Run("wrong-turn-reply", func(t *testing.T) {
		a, requests := steerFixture(t, nil, "wrong-turn")
		for i := 0; i < 2; i++ {
			_, err := a.SteerTurn(context.Background(), "target", "input", "turn-1", steerOperation, nil)
			if protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" {
				t.Fatal(err)
			}
		}
		if len(*requests) != 2 {
			t.Fatal("uncertain steer was repeated")
		}
	})
	t.Run("completed-before-reply", func(t *testing.T) {
		a, _ := steerFixture(t, func(a *codexAgent, method string) {
			if method == "turn/steer" {
				a.mu.Lock()
				a.threads["target"].turnID = ""
				a.threads["target"].info.Status = "idle"
				a.mu.Unlock()
			}
		}, "turn-1")
		result, err := a.SteerTurn(context.Background(), "target", "input", "turn-1", steerOperation, nil)
		if err != nil || !result.Accepted || a.threads["target"].turnID != "" || a.threads["target"].info.Status != "idle" {
			t.Fatal("late acceptance revived finished task", err)
		}
	})
}
