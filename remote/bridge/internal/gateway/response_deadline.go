package gateway

import (
	"context"
	"io"
	"sync"
	"time"
)

const defaultFirstByteTimeout = 2 * time.Minute
const defaultStreamIdleTimeout = 5 * time.Minute

type responseWatch struct {
	mu             sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	timer          *time.Timer
	body           io.ReadCloser
	idle           time.Duration
	first, stopped bool
	expired        string
	deadline       time.Time
	firstRead      time.Time
}

func newResponseWatch(parent context.Context, first, idle time.Duration) *responseWatch {
	if first <= 0 || first > defaultFirstByteTimeout {
		first = defaultFirstByteTimeout
	}
	if idle <= 0 || idle > defaultStreamIdleTimeout {
		idle = defaultStreamIdleTimeout
	}
	ctx, cancel := context.WithCancel(parent)
	w := &responseWatch{ctx: ctx, cancel: cancel, idle: idle, first: true, deadline: time.Now().Add(first)}
	w.timer = time.AfterFunc(first, func() {
		w.mu.Lock()
		if w.stopped {
			w.mu.Unlock()
			return
		}
		// A read can refresh the deadline after an already queued callback.
		// Check the deadline under the same lock so that callback cannot expire
		// a stream which has just made progress.
		if remaining := time.Until(w.deadline); remaining > 0 {
			w.timer.Reset(remaining)
			w.mu.Unlock()
			return
		}
		w.expired = "stream_idle_timeout"
		if w.first {
			w.expired = "headers_timeout"
		}
		w.stopped = true
		body := w.body
		w.mu.Unlock()
		cancel()
		if body != nil {
			_ = body.Close()
		}
	})
	return w
}
func (w *responseWatch) wrap(body io.ReadCloser) io.ReadCloser {
	w.mu.Lock()
	w.body = body
	expired := w.expired != ""
	w.mu.Unlock()
	if expired {
		_ = body.Close()
	}
	return &watchedBody{ReadCloser: body, watch: w}
}
func (w *responseWatch) reason() string { w.mu.Lock(); defer w.mu.Unlock(); return w.expired }
func (w *responseWatch) firstByte(started time.Time) *int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.firstRead.IsZero() {
		return nil
	}
	ms := w.firstRead.Sub(started).Milliseconds()
	return &ms
}
func (w *responseWatch) stop() {
	w.mu.Lock()
	w.stopped = true
	w.timer.Stop()
	w.mu.Unlock()
	w.cancel()
}

type watchedBody struct {
	io.ReadCloser
	watch *responseWatch
}

func (b *watchedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.watch.mu.Lock()
		if !b.watch.stopped {
			if b.watch.first {
				b.watch.firstRead = time.Now()
			}
			b.watch.first = false
			b.watch.deadline = time.Now().Add(b.watch.idle)
			b.watch.timer.Reset(b.watch.idle)
		}
		b.watch.mu.Unlock()
	}
	return n, err
}
