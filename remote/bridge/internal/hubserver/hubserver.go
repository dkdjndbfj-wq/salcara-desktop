// Package hubserver implements a self-hostable Salcara Hub server that speaks
// the same protocol as the official cloud Hub.
//
// All state is in-memory; devices re-register on reconnect, phones re-poll on
// reconnect, so restarts are handled gracefully.
//
// Endpoints:
//
//	GET  /salcara-hub/v1/ping              — discovery handshake
//	POST /salcara-hub/v1/bridge/register   — Bridge announces itself (also called on reconnect)
//	GET  /salcara-hub/v1/bridge/stream     — SSE command stream for Bridge devices
//	POST /salcara-hub/v1/bridge/event      — Bridge pushes agent events in batches
//	POST /salcara-hub/v1/pair              — QR pairing: Bridge redeems a phone scan
//	POST /salcara-hub/v1/pair/revoke       — Revoke a phone binding
//	GET  /salcara-hub/v1/phone/stream      — SSE event stream for paired phones
//	POST /salcara-hub/v1/phone/command     — Phone submits a command to a device
package hubserver

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	protocolVersion = 1
	maxBodyBytes    = 512 << 10 // 512 KiB per request

	deviceTTL     = 15 * time.Minute // unregistered after this much idle time
	pairTokenTTL  = 5 * time.Minute  // QR one-time tokens expire
	eventQueueCap = 8192             // events buffered per phone SSE connection
	pingInterval  = 25 * time.Second // SSE keepalive comment
)

// ──────────────────────────────────────────────────────────────
// Core types
// ──────────────────────────────────────────────────────────────

// device holds everything the Hub knows about one registered Bridge process.
type device struct {
	mu sync.Mutex

	// immutable after registration
	id     string
	secret string // authenticates reconnects; never sent to the phone

	// mutable
	name         string
	os           string
	version      string
	tools        []map[string]any
	projects     []map[string]any
	capabilities []string
	lastSeen     time.Time

	// QR pairing state
	pairRevision int64
	phoneHash    string // sha-256 hex of the phone's binding token
	bindingID    string
	pairExpires  time.Time // zero if no pending QR attempt

	// one-time QR tokens: token → expiry.  A Bridge process generates a QR;
	// the phone scans it and POSTs the token to /pair.
	pendingTokens map[string]time.Time

	// The Hub forwards Bridge events to phones by pushing onto bridgeCh.
	// The Bridge SSE handler drains commandCh.
	bridgeCh  chan sseMsg // Hub → Bridge (commands from phones)
	commandCh chan sseMsg // alias: same as bridgeCh, kept for clarity
}

type sseMsg struct {
	event string
	data  []byte
}

// phoneConn is one open phone SSE connection.
type phoneConn struct {
	deviceID string
	ch       chan sseMsg
}

// Hub is a self-hosted relay server.
type Hub struct {
	o       Options
	mu      sync.RWMutex
	devices map[string]*device
	phones  map[string]*phoneConn // connID → conn

	// CommandFilter is called on every phone command before it is relayed to
	// the Bridge.  Return a non-nil error to reject the command.
	CommandFilter func(deviceID string, cmd map[string]any) error
}

// Options configures the Hub.
type Options struct {
	Logger *log.Logger
	// StationMode is reported in the ping response (remoteAccess.mode).
	// Defaults to "open".
	StationMode string
	// APIBases lists the API origins allowed when StationMode == "restricted".
	APIBases []string
	// StationRevision is the policy version string.  Defaults to "self-hosted-v1".
	StationRevision string
}

// New creates a Hub.  Call Handler() to get the http.Handler.
func New(o Options) *Hub {
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	if o.StationMode == "" {
		o.StationMode = "open"
	}
	if o.StationRevision == "" {
		o.StationRevision = "self-hosted-v1"
	}
	return &Hub{
		o:       o,
		devices: make(map[string]*device),
		phones:  make(map[string]*phoneConn),
	}
}

// Handler returns the http.Handler for the Hub API prefix /salcara-hub/.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /salcara-hub/v1/ping", h.handlePing)
	mux.HandleFunc("POST /salcara-hub/v1/bridge/register", h.handleBridgeRegister)
	mux.HandleFunc("GET /salcara-hub/v1/bridge/stream", h.handleBridgeStream)
	mux.HandleFunc("POST /salcara-hub/v1/bridge/event", h.handleBridgeEvent)
	mux.HandleFunc("POST /salcara-hub/v1/pair", h.handlePair)
	mux.HandleFunc("POST /salcara-hub/v1/pair/revoke", h.handlePairRevoke)
	mux.HandleFunc("GET /salcara-hub/v1/phone/stream", h.handlePhoneStream)
	mux.HandleFunc("POST /salcara-hub/v1/phone/command", h.handlePhoneCommand)
	return mux
}

// ──────────────────────────────────────────────────────────────
// helpers
// ──────────────────────────────────────────────────────────────

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func readBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || len(body) > maxBodyBytes {
		return io.ErrUnexpectedEOF
	}
	return json.Unmarshal(body, v)
}

func writeSSE(w io.Writer, event string, data []byte) {
	_, _ = io.WriteString(w, "event: "+event+"\ndata: ")
	_, _ = w.Write(data)
	_, _ = io.WriteString(w, "\n\n")
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
