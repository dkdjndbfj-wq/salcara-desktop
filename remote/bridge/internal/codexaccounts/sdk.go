package codexaccounts

import (
	"context"
	"encoding/json"
	"strings"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

// SDKLoginConnector is retained as a compatibility adapter for the MIT
// CLIProxyAPI public SDK authenticator. Production main uses BrowserConnector,
// whose callback listener is loopback-only and directly cancellable. This is
// the legacy Codex CLI account flow, not dynamic SIWC OAuth.
// Config.AuthDir is cleared to prevent implicit reads of existing auth files.
// Browser logins share a cancellable process-wide gate. Pinned v7.2.157 starts
// its public Prompt hook after 15 seconds; that hook releases a cancelled login
// instead of retaining the listener until the five-minute callback timeout.
// Cancellation before the hook starts can therefore take up to 15 seconds plus
// SDK listener shutdown. The SDK binds its callback listener on all interfaces;
// its public API exposes no listener override. No callback requests are forged.
// Its internal callback waiter may still remain until its five-minute timeout
// after listener shutdown because the SDK exposes no waiter cancellation API.
// Device mode remains the default and honors context cancellation directly.
type SDKLoginConnector struct {
	Config          *sdkconfig.Config
	Options         sdkauth.LoginOptions
	BrowserCallback bool
	invoke          sdkLoginInvocation
}

type sdkLoginInvocation func(context.Context, *sdkconfig.Config, *sdkauth.LoginOptions) (*coreauth.Auth, error)

var sdkBrowserLoginGate = make(chan struct{}, 1)

// Optional caller prompts have no context contract. At most one such callback
// may remain running if it ignores cancellation; later prompts wait for this
// slot with their own context rather than creating unbounded worker goroutines.
var sdkCallerPromptGate = make(chan struct{}, 1)

func (c SDKLoginConnector) Login(ctx context.Context) (*coreauth.Auth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c.BrowserCallback {
		select {
		case sdkBrowserLoginGate <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		defer func() { <-sdkBrowserLoginGate }()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	// Also releases a prompt waiter when the normal SDK callback wins.
	loginCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var cfg sdkconfig.Config
	if c.Config != nil {
		cfg = *c.Config
	}
	cfg.AuthDir = ""
	opts := c.Options
	opts.Metadata = map[string]string{}
	for key, value := range c.Options.Metadata {
		opts.Metadata[key] = value
	}
	if !c.BrowserCallback {
		opts.Metadata["codex_login_mode"] = "device"
	} else {
		delete(opts.Metadata, "codex_login_mode")
		opts.Prompt = cancellableSDKPrompt(loginCtx, opts.Prompt)
	}
	invoke := c.invoke
	if invoke == nil {
		invoke = invokeSDKLogin
	}
	record, err := invoke(loginCtx, &cfg, &opts)
	if cancelled := loginCtx.Err(); cancelled != nil {
		return nil, cancelled
	}
	return record, err
}

func invokeSDKLogin(ctx context.Context, cfg *sdkconfig.Config, opts *sdkauth.LoginOptions) (*coreauth.Auth, error) {
	manager := sdkauth.NewManager(nil, sdkauth.NewCodexAuthenticator())
	record, _, err := manager.Login(ctx, "codex", cfg, opts)
	return record, err
}

func cancellableSDKPrompt(ctx context.Context, prompt func(string) (string, error)) func(string) (string, error) {
	return func(message string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if prompt == nil {
			<-ctx.Done()
			return "", ctx.Err()
		}
		select {
		case sdkCallerPromptGate <- struct{}{}:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		type result struct {
			input string
			err   error
		}
		done := make(chan result, 1)
		go func() {
			defer func() { <-sdkCallerPromptGate }()
			input, err := prompt(message)
			done <- result{input, err}
		}()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case value := <-done:
			if value.err != nil || strings.TrimSpace(value.input) != "" {
				return value.input, value.err
			}
			// Returning blank would make the SDK stop listening to the prompt
			// error channel. Keep that channel alive for later cancellation.
			<-ctx.Done()
			return "", ctx.Err()
		}
	}
}

// NewSDKRefresher invokes only the SDK's registered Codex executor. The caller
// must install the executor and disable competing SDK refresh owners.
func NewSDKRefresher(manager *coreauth.Manager) Refresher {
	return RefreshFunc(func(ctx context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
		if manager == nil {
			return nil, ErrRefreshFailed
		}
		executor, ok := manager.Executor("codex")
		if !ok || executor == nil {
			return nil, ErrRefreshFailed
		}
		record, err := executor.Refresh(ctx, a)
		if cancelled := ctx.Err(); cancelled != nil {
			return nil, cancelled
		}
		if err != nil {
			return nil, normalizeSDKRefreshError(err)
		}
		return record, nil
	})
}

func credentialFromSDK(record *coreauth.Auth) (credential, error) {
	if record == nil || record.Provider != "codex" {
		return credential{}, ErrInvalidImport
	}
	var raw []byte
	var err error
	if record.Storage != nil {
		raw, err = json.Marshal(record.Storage)
	} else {
		raw, err = json.Marshal(record.Metadata)
	}
	if err != nil {
		return credential{}, ErrInvalidImport
	}
	parsed, err := ParseImport(raw)
	if err != nil || len(parsed.records) != 1 {
		return credential{}, ErrInvalidImport
	}
	c := parsed.records[0]
	if c.Email == "" && record.Label != "" {
		c.Name = record.Label
	}
	if record.Attributes != nil && c.Plan == "" {
		c.Plan = record.Attributes["plan_type"]
	}
	if len(c.Name) > 320 || len(c.Plan) > 64 || strings.ContainsAny(c.Name, "\r\n\x00") || strings.ContainsAny(c.Plan, "\r\n\x00") {
		return credential{}, ErrInvalidImport
	}
	return c, nil
}

// SDKRecord is a backend-only snapshot containing secrets. This is deliberately
// separate from Get/List and must never be returned to the browser or logged.
// Expiry/plan metadata does not validate the token; upstream validates use.
func (s *Service) SDKRecord(ctx context.Context, id string) (*coreauth.Auth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, ok := s.vault.Accounts[id]
	if !ok {
		return nil, ErrNotFound
	}
	if c.TokenSourceMode == "chatgpt_web_session" {
		return nil, ErrConflict
	}
	if c.Status == StatusNeedsLogin && !c.ExpiresAt.IsZero() && !s.now().Before(c.ExpiresAt) {
		return nil, ErrNeedsLogin
	}
	return c.sdk(), nil
}

// SDKStore exposes persisted snapshots to the trusted SDK. Save and Delete are
// intentionally refused: Service is the sole mutation/refresh owner. This stops
// an SDK auto-refresh or stale callback from silently resurrecting credentials.
func (s *Service) SDKStore() coreauth.Store { return snapshotStore{s: s} }

type snapshotStore struct{ s *Service }

func (a snapshotStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	if a.s.closed {
		return nil, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	records := make([]*coreauth.Auth, 0, len(a.s.vault.Accounts))
	for _, c := range a.s.vault.Accounts {
		if c.TokenSourceMode == "chatgpt_web_session" {
			continue
		}
		records = append(records, c.sdk())
	}
	return records, nil
}
func (snapshotStore) Save(context.Context, *coreauth.Auth) (string, error) { return "", ErrConflict }
func (snapshotStore) Delete(context.Context, string) error                 { return ErrConflict }
