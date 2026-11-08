package agents

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

type deliveryFixtureWriter struct{ write func([]byte) (int, error) }

func (w deliveryFixtureWriter) Write(b []byte) (int, error) { return w.write(b) }
func (deliveryFixtureWriter) Close() error                  { return nil }
func deliveryFixtureConn(write func([]byte) (int, error)) *rpcConn {
	return &rpcConn{stdin: deliveryFixtureWriter{write}, pending: map[int64]chan rpcResult{}, pendingMethods: map[int64]string{}, done: make(chan struct{}), killCh: make(chan struct{})}
}

func TestRPCSentMutationCancellationKeepsTypedUncertainty(t *testing.T) {
	for _, method := range []string{"thread/start", "turn/start", "turn/steer", "thread/read", "thread/turns/list"} {
		ctx, cancel := context.WithCancel(context.Background())
		c := deliveryFixtureConn(func(b []byte) (int, error) { cancel(); return len(b), nil })
		err := c.Call(ctx, method, nil, nil)
		if !errors.Is(err, context.Canceled) {
			t.Fatal("lost cancellation identity")
		}
		want := "command_delivery_uncertain"
		if rpcShutdownReadOnly(method) {
			want = ""
		}
		if protocol.ExecutionErrorCode(err) != want {
			t.Fatalf("%s: wrong delivery boundary: %v", method, err)
		}
	}
}

func TestRPCExpiredQueuedWriteNeverDispatches(t *testing.T) {
	var writes atomic.Int32
	c := deliveryFixtureConn(func(b []byte) (int, error) { writes.Add(1); return len(b), nil })
	c.wmu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- c.Call(ctx, "thread/start", nil, nil) }()
	<-ctx.Done()
	c.wmu.Unlock()
	if err := <-result; !errors.Is(err, context.DeadlineExceeded) || protocol.ExecutionErrorCode(err) != "" {
		t.Fatalf("unsent call uncertainty: %v", err)
	}
	if writes.Load() != 0 {
		t.Fatal("expired task wrote to the worker")
	}
}

func TestRPCPartialWriteCrashAndMalformedMutationResultCannotAuthorizeReplay(t *testing.T) {
	for _, failure := range []string{"partial-write", "short-write", "normal-crash", "malformed-result"} {
		var c *rpcConn
		c = deliveryFixtureConn(func(b []byte) (int, error) {
			switch failure {
			case "partial-write":
				return len(b) / 2, io.ErrClosedPipe
			case "short-write":
				return len(b) / 2, nil
			case "normal-crash":
				c.shutdown(errors.New("fixture process crash"))
			case "malformed-result":
				var req struct {
					ID int64 `json:"id"`
				}
				_ = json.Unmarshal(b, &req)
				c.pending[req.ID] <- rpcResult{result: json.RawMessage(`{"thread":`)}
			}
			return len(b), nil
		})
		var out map[string]any
		if err := c.Call(context.Background(), "thread/start", nil, &out); protocol.ExecutionErrorCode(err) != "command_delivery_uncertain" {
			t.Fatalf("%s lost uncertainty: %v", failure, err)
		}
	}
}
