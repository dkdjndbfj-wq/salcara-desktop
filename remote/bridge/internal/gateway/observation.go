package gateway

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"salcara/bridge/internal/usageledger"
)

type observationWriter struct {
	http.ResponseWriter
	status      int
	writeFailed bool
	started     time.Time
	firstByte   *int64
}

func (w *observationWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *observationWriter) Write(b []byte) (int, error) {
	if len(b) > 0 && w.firstByte == nil {
		ms := time.Since(w.started).Milliseconds()
		w.firstByte = &ms
	}
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(b)
	if err != nil || n != len(b) {
		w.writeFailed = true
	}
	return n, err
}
func (w *observationWriter) Flush() {
	if w.status == 0 {
		w.status = 200
	}
	if err := http.NewResponseController(w.ResponseWriter).Flush(); err != nil {
		w.writeFailed = true
	}
}
func (w *observationWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observationWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("hijacking unavailable")
	}
	conn, rw, err := h.Hijack()
	if err == nil {
		w.status = 101
	}
	return conn, rw, err
}

type requestObservation struct {
	writer                                      *observationWriter
	request                                     *http.Request
	observer                                    func(usageledger.Observation)
	data                                        usageledger.Observation
	collector                                   usageledger.Collector
	dispatched, streaming, streamComplete, skip bool
	reason                                      string
	retryAfter                                  time.Duration
	watch                                       *responseWatch
}

func beginRequestObservation(w http.ResponseWriter, r *http.Request, observer func(usageledger.Observation), route string) (http.ResponseWriter, *requestObservation) {
	if observer == nil {
		observer = func(usageledger.Observation) {}
	}
	writer := &observationWriter{ResponseWriter: w, started: time.Now()}
	o := &requestObservation{writer: writer, request: r, observer: observer, data: usageledger.Observation{StartedAt: time.Now(), Route: route, State: "rejected"}}
	return writer, o
}
func (o *requestObservation) account(id string, epoch uint64) {
	if o != nil {
		o.data.AccountID = id
		o.data.AccountEpoch = epoch
	}
}
func (o *requestObservation) model(model string, secrets ...string) {
	if o == nil || !usageledger.ValidateModel(model) {
		return
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(model, secret) {
			return
		}
	}
	o.data.Model = model
}
func (o *requestObservation) state(state string) {
	if o != nil {
		o.data.State = state
	}
}
func (o *requestObservation) dispatch() {
	if o != nil {
		o.dispatched = true
		o.data.State = "unknown"
	}
}
func (o *requestObservation) response(status int) {
	if o != nil {
		o.data.UpstreamHTTPStatus = status
	}
}
func (o *requestObservation) json(raw []byte) {
	if o != nil {
		o.collector.ObserveJSON(raw)
	}
}
func (o *requestObservation) sse(raw []byte) {
	if o != nil {
		o.collector.ObserveSSE(raw)
	}
}
func (o *requestObservation) event(raw []byte) {
	if o != nil {
		o.collector.ObserveEvent(raw)
	}
}
func (o *requestObservation) stream(completed bool) {
	if o != nil {
		o.streaming, o.streamComplete = true, completed
	}
}

func (o *requestObservation) failure(stage, reason string, retryAfter time.Duration) {
	if o != nil {
		o.data.FailureStage, o.reason, o.retryAfter = stage, reason, retryAfter
	}
}

func (o *requestObservation) finish() {
	if o == nil || o.skip {
		return
	}
	defer func() { _ = recover() }()
	o.data.HTTPStatus = o.writer.status
	if o.data.HTTPStatus == 0 {
		o.data.HTTPStatus = 200
	}
	o.data.DurationMS = time.Since(o.data.StartedAt).Milliseconds()
	o.data.Tokens = o.collector.Tokens()
	o.data.UsageFinal = o.collector.Final()
	o.data.ProviderCost = o.collector.ProviderCost()
	o.data.ServiceTier = o.collector.ServiceTier()
	o.data.FirstByteMS = o.writer.firstByte
	if o.watch != nil {
		o.data.FirstByteMS = o.watch.firstByte(o.data.StartedAt)
	}
	o.data.RoutingReason = "fixed_account"
	switch {
	case o.request.Context().Err() != nil:
		o.data.State = "cancelled"
		o.failure("cancelled", "cancelled", 0)
	case o.writer.writeFailed:
		o.data.State = "interrupted"
		o.failure("cancelled", "cancelled", 0)
	case o.streaming && !o.streamComplete:
		o.data.State = o.collector.State()
		if o.data.State != "failed" && o.data.State != "incomplete" {
			o.data.State = "interrupted"
		}
	case o.data.HTTPStatus >= 400:
		if o.dispatched {
			o.data.State = "failed"
		}
	case o.dispatched:
		o.data.State = o.collector.State()
	}
	if o.reason == "" {
		switch o.data.State {
		case "interrupted", "incomplete":
			o.failure("stream", "stream_interrupted", 0)
		case "failed":
			o.failure("upstream", "unknown_result", 0)
		case "rejected":
			o.data.FailureStage = "authorization"
		default:
			o.reason = "fixed_account"
		}
	}
	if o.dispatched {
		o.data.Attempts = []usageledger.Attempt{{AccountID: o.data.AccountID, AccountEpoch: o.data.AccountEpoch, Model: o.data.Model, State: o.data.State, HTTPStatus: o.data.HTTPStatus, UpstreamHTTPStatus: o.data.UpstreamHTTPStatus, DurationMS: o.data.DurationMS, FirstByteMS: o.data.FirstByteMS, Reason: o.reason, RetryAfterMS: o.retryAfter.Milliseconds()}}
	}
	o.observer(o.data)
}
