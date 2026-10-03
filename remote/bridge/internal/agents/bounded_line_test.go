package agents

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestReadBoundedLineExactBoundaryDrainAndResync(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader(strings.Repeat("a", 31)+"\n"+strings.Repeat("b", 97)+"\nnormal\nlast"), 16)
	line, err := ReadBoundedLine(reader, 32)
	if err != nil || string(line) != strings.Repeat("a", 31)+"\n" || cap(line) > 32 {
		t.Fatal("exact boundary rejected", len(line), cap(line), err)
	}
	line, err = ReadBoundedLine(reader, 32)
	if len(line) != 0 || !errors.Is(err, ErrLineTooLarge) {
		t.Fatal("oversized line retained", len(line), err)
	}
	line, err = ReadBoundedLine(reader, 32)
	if err != nil || string(line) != "normal\n" {
		t.Fatal("reader failed to resynchronize", string(line), err)
	}
	line, err = ReadBoundedLine(reader, 32)
	if string(line) != "last" || !errors.Is(err, io.EOF) {
		t.Fatal("unterminated final record lost", string(line), err)
	}
}

func TestReadBoundedLineOversizedFinalRecordAndEmptyInput(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader(strings.Repeat("a", 98)), 16)
	if line, err := ReadBoundedLine(reader, 32); len(line) != 0 || !errors.Is(err, ErrLineTooLarge) {
		t.Fatal(len(line), err)
	}
	if line, err := ReadBoundedLine(reader, 32); len(line) != 0 || !errors.Is(err, io.EOF) {
		t.Fatal(len(line), err)
	}
}

type rpcFixtureWriter struct{ calls chan []byte }

func (w *rpcFixtureWriter) Write(data []byte) (int, error) {
	w.calls <- append([]byte(nil), data...)
	return len(data), nil
}
func (w *rpcFixtureWriter) Close() error { return nil }

func TestRPCOversizedRecordFailsPendingWithoutClosingAndNextCallStillWorks(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	input := &rpcFixtureWriter{calls: make(chan []byte, 4)}
	notified := make(chan string, 1)
	c := &rpcConn{stdin: input, pending: map[int64]chan rpcResult{}, done: make(chan struct{}), onNotify: func(method string, _ json.RawMessage) { notified <- method }}
	readDone := make(chan error, 1)
	go func() { readDone <- c.readMessages(reader, 128) }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	readResult, sendResult := make(chan error, 1), make(chan error, 1)
	go func() { readResult <- c.Call(ctx, "thread/read", nil, nil) }()
	<-input.calls
	go func() { sendResult <- c.Call(ctx, "turn/start", nil, nil) }()
	<-input.calls
	if _, err := io.WriteString(writer, `{"id":1,"result":"`+strings.Repeat("x", 256)+"\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-readResult:
		if !errors.Is(err, ErrRPCResponseTooLarge) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("oversized read left RPC waiting")
	}
	select {
	case err := <-sendResult:
		if !errors.Is(err, ErrRPCDeliveryTooLarge) {
			t.Fatal("mutating request uncertainty lost", err)
		}
	case <-ctx.Done():
		t.Fatal("oversized record left send waiting")
	}
	if !c.alive() {
		t.Fatal("read limit closed the connection")
	}
	select {
	case <-c.done:
		t.Fatal("read limit shut down connection")
	default:
	}
	if _, err := io.WriteString(writer, `{"method":"fixture/notify","params":{}}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case method := <-notified:
		if method != "fixture/notify" {
			t.Fatal(method)
		}
	case <-ctx.Done():
		t.Fatal("normal notification after oversized record lost")
	}
	normalResult := make(chan error, 1)
	go func() {
		var out struct {
			OK bool `json:"ok"`
		}
		err := c.Call(ctx, "thread/read", nil, &out)
		if err == nil && !out.OK {
			err = errors.New("response lost")
		}
		normalResult <- err
	}()
	var next struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(<-input.calls, &next); err != nil {
		t.Fatal(err)
	}
	response, _ := json.Marshal(map[string]any{"id": next.ID, "result": map[string]any{"ok": true}})
	if _, err := writer.Write(append(response, '\n')); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-normalResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("next normal RPC hung")
	}
	writer.Close()
	if err := <-readDone; !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func TestRPCNewRequestDuringOversizedDrainIsNotIncorrectlyFailed(t *testing.T) {
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	input := &rpcFixtureWriter{calls: make(chan []byte, 4)}
	c := &rpcConn{stdin: input, pending: map[int64]chan rpcResult{}, done: make(chan struct{})}
	readDone := make(chan error, 1)
	go func() { readDone <- c.readMessages(reader, 64) }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	old := make(chan error, 1)
	go func() { old <- c.Call(ctx, "thread/read", nil, nil) }()
	<-input.calls
	if _, err := io.WriteString(writer, strings.Repeat("x", 192)); err != nil {
		t.Fatal(err)
	}
	for {
		c.mu.Lock()
		count := len(c.pending)
		c.mu.Unlock()
		if count == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("oversized line never detached pending calls")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-old:
		t.Fatal("old request settled before oversized line was drained")
	default:
	}
	next := make(chan error, 1)
	go func() { next <- c.Call(ctx, "thread/read", nil, nil) }()
	var request struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(<-input.calls, &request); err != nil {
		t.Fatal(err)
	}
	response, _ := json.Marshal(map[string]any{"id": request.ID, "result": map[string]any{"ok": true}})
	if _, err := writer.Write(append(append([]byte{'\n'}, response...), '\n')); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-old:
		if !errors.Is(err, ErrRPCResponseTooLarge) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("old request hung after drain")
	}
	select {
	case err := <-next:
		if err != nil {
			t.Fatal("new request was incorrectly failed by old frame", err)
		}
	case <-ctx.Done():
		t.Fatal("new response was lost after drain")
	}
	if !c.alive() {
		t.Fatal("connection was closed")
	}
	writer.Close()
	if err := <-readDone; !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}
