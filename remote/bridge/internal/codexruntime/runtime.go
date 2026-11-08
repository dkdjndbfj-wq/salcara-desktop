// Package codexruntime adapts the pinned MIT SDK's stateless Codex executor.
// Its SDK bootstrap server has no credentials and cannot execute requests.
// Salcara selects one protected account before calling the executor directly;
// the SDK scheduler therefore cannot silently substitute another account.
package codexruntime

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"salcara/bridge/internal/atomicfile"
	"salcara/bridge/internal/config"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	sdkapi "github.com/router-for-me/CLIProxyAPI/v7/sdk/api"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

var ErrUnavailable = errors.New("Codex account executor unavailable")

type Runtime struct {
	mu               sync.Mutex
	dir              string
	transport        *http.Transport
	refreshTokenURL  string
	start            chan struct{}
	service          *cliproxy.Service
	cancel           context.CancelFunc
	done             chan error
	executor         coreauth.ProviderExecutor
	closed           bool
	bootstrapAddress string
	websocketURL     string // Package-private loopback fixture seam; production uses the fixed provider.
	websockets       map[*websocket.Conn]context.CancelFunc
	websocketContext context.Context
	websocketStop    context.CancelFunc
}

func New(dir string) *Runtime {
	return &Runtime{dir: dir, transport: newRuntimeHTTPTransport(), refreshTokenURL: officialCodexTokenURL}
}

func (r *Runtime) Ready(ctx context.Context) error { _, err := r.get(ctx); return err }

func (r *Runtime) get(ctx context.Context) (coreauth.ProviderExecutor, error) {
	for {
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			return nil, ErrUnavailable
		}
		if r.executor != nil {
			ex := r.executor
			r.mu.Unlock()
			return ex, nil
		}
		if r.start != nil {
			done := r.start
			r.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			continue
		}
		r.start = make(chan struct{})
		done := r.start
		r.mu.Unlock()
		ex, err := r.bootstrap(ctx)
		r.mu.Lock()
		if r.closed {
			ex, err = nil, ErrUnavailable
		}
		if err == nil && !r.closed {
			r.executor = ex
		}
		close(done)
		r.start = nil
		r.mu.Unlock()
		return ex, err
	}
}

// No account files are ever loaded by this SDK instance. Token refresh and
// persistence belong exclusively to codexaccounts.Service.
type emptyStore struct{}

func (emptyStore) List(context.Context) ([]*coreauth.Auth, error)       { return nil, nil }
func (emptyStore) Save(context.Context, *coreauth.Auth) (string, error) { return "", ErrUnavailable }
func (emptyStore) Delete(context.Context, string) error                 { return ErrUnavailable }

type noTokens struct{}

func (noTokens) Load(context.Context, *sdkconfig.Config) (*cliproxy.TokenClientResult, error) {
	return &cliproxy.TokenClientResult{}, nil
}

type noKeys struct{}

func (noKeys) Load(context.Context, *sdkconfig.Config) (*cliproxy.APIKeyClientResult, error) {
	return &cliproxy.APIKeyClientResult{}, nil
}

func (r *Runtime) bootstrap(ctx context.Context) (coreauth.ProviderExecutor, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, ErrUnavailable
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // The SDK doesn't accept a listener; authenticated probing fences port reuse.
	secret := config.RandomToken(32)
	path := filepath.Join(r.dir, "sdk-bootstrap", "config.yaml")
	cfg, err := sdkconfig.ParseConfigBytes([]byte("host: 127.0.0.1\nport: " + strconv.Itoa(port) + "\nauth-dir: " + strconv.Quote(filepath.Join(r.dir, "sdk-bootstrap", "empty-auth")) + "\ncommercial-mode: true\ndisable-image-generation: passthrough\nremote-management:\n  disable-control-panel: true\napi-keys:\n  - " + secret + "\n"))
	if err != nil {
		return nil, ErrUnavailable
	}
	// Deliberately persist only a harmless, non-secret watcher placeholder.
	if atomicfile.WriteFile(path, []byte("# Salcara executor bootstrap; not a user configuration\n"), 0600) != nil {
		return nil, ErrUnavailable
	}
	manager := coreauth.NewManager(emptyStore{}, nil, nil)
	ready := make(chan coreauth.ProviderExecutor, 1)
	svc, err := cliproxy.NewBuilder().WithConfig(cfg).WithConfigPath(path).
		WithCoreAuthManager(manager).WithTokenClientProvider(noTokens{}).WithAPIKeyClientProvider(noKeys{}).
		WithWatcherFactory(func(string, string, func(*sdkconfig.Config)) (*cliproxy.WatcherWrapper, error) {
			return &cliproxy.WatcherWrapper{}, nil
		}).
		WithServerOptions(sdkapi.WithMiddleware(func(c *gin.Context) {
			if c.Request.Method == http.MethodGet && c.Request.URL.Path == "/salcara-ready" && subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), []byte("Bearer "+secret)) == 1 {
				c.AbortWithStatus(http.StatusNoContent)
				return
			}
			// No models, management, login, inference, or websocket endpoints.
			c.AbortWithStatus(http.StatusForbidden)
		})).WithHooks(cliproxy.Hooks{OnBeforeStart: func(*sdkconfig.Config) {
		manager.StopAutoRefresh()
		if ex, ok := manager.Executor("codex"); ok {
			ready <- ex
		}
	}}).Build()
	if err != nil {
		return nil, ErrUnavailable
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		return nil, ErrUnavailable
	}
	r.service, r.cancel, r.done = svc, cancel, done
	r.bootstrapAddress = "127.0.0.1:" + strconv.Itoa(port)
	r.mu.Unlock()
	go func() { done <- svc.Run(runCtx) }()
	probeCtx, stopProbe := context.WithTimeout(ctx, 10*time.Second)
	defer stopProbe()
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	var ex coreauth.ProviderExecutor
	for {
		select {
		case ex = <-ready:
		case <-ticker.C:
			if ex == nil {
				continue
			}
			req, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/salcara-ready", nil)
			req.Header.Set("Authorization", "Bearer "+secret)
			resp, e := client.Do(req)
			if e == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusNoContent {
					return ex, nil
				}
			}
		case <-done:
			cancel()
			return nil, ErrUnavailable
		case <-probeCtx.Done():
			cancel()
			// Do not consume SDK shutdownOnce before Run has constructed its
			// server. Run observes cancellation after startup and shuts it down.
			select {
			case <-done:
			case <-time.After(15 * time.Second):
			}
			return nil, ErrUnavailable
		}
	}
}

func (r *Runtime) Refresh(ctx context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return r.refresh(ctx, a)
}

// Execute accepts exactly one backend credential snapshot. It never invokes
// Manager.Execute/ExecuteStream or registers accounts in the SDK's pool.
func (r *Runtime) Execute(ctx context.Context, a *coreauth.Auth, body []byte, stream, compact bool) (*http.Response, error) {
	if stream && !compact && executor.DownstreamWebsocket(ctx) {
		return r.executeWebSocket(ctx, a, body)
	}
	opts := executor.Options{Stream: stream, OriginalRequest: body, SourceFormat: translator.FormatCodex, ResponseFormat: translator.FormatCodex}
	if compact {
		opts.Alt = "responses/compact"
	}
	return r.executeOptions(ctx, a, body, opts)
}

// ExecuteImages selects the pinned SDK's actual OpenAI image executor path;
// callers still supply one protected credential and authorize its exact image
// model before entering the SDK. The service selects supported direct models.
func (r *Runtime) ExecuteImages(ctx context.Context, a *coreauth.Auth, body []byte, stream bool, path string) (*http.Response, error) {
	if path != "/v1/images/generations" && path != "/v1/images/edits" {
		return nil, ErrUnavailable
	}
	format := translator.FromString("openai-image")
	return r.executeOptions(ctx, a, body, executor.Options{
		Stream: stream, OriginalRequest: body, SourceFormat: format, ResponseFormat: format,
		Headers:  http.Header{"Content-Type": {"application/json"}},
		Metadata: map[string]any{executor.RequestPathMetadataKey: path},
	})
}

func (r *Runtime) executeOptions(ctx context.Context, a *coreauth.Auth, body []byte, opts executor.Options) (*http.Response, error) {
	ex, err := r.get(ctx)
	if err != nil {
		return nil, err
	}
	var in struct {
		Model string `json:"model"`
	}
	if a == nil || a.Provider != "codex" || json.Unmarshal(body, &in) != nil || in.Model == "" {
		return nil, ErrUnavailable
	}
	ctx, err = r.withSDKTransport(ctx, a)
	if err != nil {
		return nil, err
	}
	req := executor.Request{Model: in.Model, Payload: body}
	if !opts.Stream {
		result, e := ex.Execute(ctx, a, req, opts)
		if e != nil {
			return nil, normalizeProviderError(e)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(result.Payload))}, nil
	}
	streamCtx, cancel := context.WithCancel(ctx)
	result, e := ex.ExecuteStream(streamCtx, a, req, opts)
	if e != nil {
		cancel()
		return nil, normalizeProviderError(e)
	}
	if result == nil || result.Chunks == nil {
		cancel()
		return nil, ErrUnavailable
	}
	// Resolve stream admission before returning HTTP 200. In particular, a
	// first-chunk 401 is still "before output" and AccountExecutor may refresh
	// the same account once. Empty/failed streams must not become successful
	// zero-byte SSE responses. Empty payload chunks are transport noise and do
	// not commit the stream.
	var first executor.StreamChunk
	for {
		select {
		case <-streamCtx.Done():
			cancel()
			return nil, streamCtx.Err()
		case chunk, ok := <-result.Chunks:
			if !ok {
				cancel()
				return nil, ErrUnavailable
			}
			if chunk.Err != nil {
				cancel()
				return nil, normalizeProviderError(chunk.Err)
			}
			if len(chunk.Payload) == 0 {
				continue
			}
			first = chunk
		}
		break
	}
	reader, writer := io.Pipe()
	go func() {
		defer cancel()
		defer writer.Close()
		if _, e := writer.Write(first.Payload); e != nil {
			return
		}
		for {
			select {
			case <-streamCtx.Done():
				_ = writer.CloseWithError(streamCtx.Err())
				return
			case chunk, ok := <-result.Chunks:
				if !ok {
					return
				}
				if chunk.Err != nil {
					_ = writer.CloseWithError(ErrUnavailable)
					return
				}
				if _, e := writer.Write(chunk.Payload); e != nil {
					return
				}
			}
		}
	}()
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: &cancelBody{ReadCloser: reader, cancel: cancel}}, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { b.cancel(); return b.ReadCloser.Close() }

func (r *Runtime) Close() error {
	r.mu.Lock()
	r.closed = true
	starting := r.start
	transport := r.transport
	websocketStop := r.websocketStop
	connections := make(map[*websocket.Conn]context.CancelFunc, len(r.websockets))
	for conn, cancel := range r.websockets {
		connections[conn] = cancel
	}
	r.mu.Unlock()
	if websocketStop != nil {
		websocketStop()
	}
	for conn, cancel := range connections {
		cancel()
		_ = conn.Close()
	}
	if transport != nil {
		transport.CloseIdleConnections()
	}
	if starting != nil {
		// Startup owns the listener until its final readiness result. An early
		// SDK Shutdown is a no-op that consumes its once and leaks a later bind.
		select {
		case <-starting:
		case <-time.After(30 * time.Second):
			return ErrUnavailable
		}
	}
	r.mu.Lock()
	svc, cancel := r.service, r.cancel
	r.mu.Unlock()
	if svc == nil {
		return nil
	}
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	err := svc.Shutdown(ctx) // Fresh context, unlike the SDK's startup-time defer.
	if cancel != nil {
		cancel()
	}
	return err
}
