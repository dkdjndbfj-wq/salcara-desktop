package agents

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestCodexMetadataSnapshotSurvivesBackgroundReaders(t *testing.T) {
	a, transport, _ := metadataCodexFixture(t)
	ctx := WithConfigSnapshot(context.Background(), config.Config{})
	a.connSig = codexSig(settingsForContext(ctx, a.settings))
	connection, signature := a.conn, a.connSig
	if signature == codexSig(a.settings()) {
		t.Fatal("fixture must distinguish frozen remote settings from local defaults")
	}
	transport.hook = func() {
		var workers sync.WaitGroup
		for range 8 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				read, err := a.ensureRead(context.Background())
				if err != nil || read != connection {
					t.Errorf("background reader replaced frozen runtime: %v", err)
				}
			}()
		}
		workers.Wait()
	}
	if _, err := a.SessionMetadata(ctx, "fixture-old"); err != nil {
		t.Fatalf("background reading invalidated remote admission: %v", err)
	}
	if a.conn != connection || a.connSig != signature {
		t.Fatal("read-only lookup changed the runtime source")
	}
}

type codexReadConnectionTransport struct {
	conn   *rpcConn
	thread cxThread
}

func (w *codexReadConnectionTransport) Close() error { return nil }
func (w *codexReadConnectionTransport) Write(data []byte) (int, error) {
	var request struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		return 0, err
	}
	var result any
	switch request.Method {
	case "thread/list":
		result = map[string]any{"data": []cxThread{w.thread}}
	case "thread/read":
		result = map[string]any{"thread": w.thread}
	case "thread/turns/list":
		result = map[string]any{"data": []cxTurn{}}
	default:
		return 0, errors.New("unexpected non-read RPC")
	}
	response, err := json.Marshal(map[string]any{"id": request.ID, "result": result})
	if err != nil {
		return 0, err
	}
	w.conn.handleLine(response)
	return len(data), nil
}

func TestCodexHistoryReadsReuseRuntimeWithoutWeakeningScopedSourceChecks(t *testing.T) {
	a, _, _ := newResumeSafetyAgent(t, cxThread{}, false)
	a.connSig = "frozen-phone-runtime"
	connection := a.conn
	a.conn.stdin = &codexReadConnectionTransport{conn: connection, thread: cxThread{
		ID: "original", Cwd: t.TempDir(), Status: cxThreadStatus{Type: "idle"},
	}}
	// Pending approval makes any attempted cross-source restart fail promptly.
	// Read-only operations must be able to show the current worker's history.
	a.aps.addRequest("codex:original", "codex", "fixture-ref", protocol.Event{Kind: "command"})
	if _, err := a.Sessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.SessionsPage(context.Background(), "", 10, ""); err != nil {
		t.Fatalf("paged phone directory replaced the current runtime: %v", err)
	}
	if _, err := a.readThread(context.Background(), "original"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.SessionMetadata(context.Background(), "original"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := a.openCodexMessagesPage(context.Background(), "original", "", 2); err != nil {
		t.Fatal(err)
	}
	a.streamTailItems(context.Background(), "original", "")
	if _, err := a.ensure(context.Background()); err == nil {
		t.Fatal("execution silently reused another source while approval was pending")
	}
	if _, err := a.ensureRead(WithConfigSnapshot(context.Background(), config.Config{})); err == nil {
		t.Fatal("explicitly scoped read bypassed its source check")
	}
	if a.conn != connection || a.connSig != "frozen-phone-runtime" {
		t.Fatal("history reader changed runtime ownership")
	}
}
