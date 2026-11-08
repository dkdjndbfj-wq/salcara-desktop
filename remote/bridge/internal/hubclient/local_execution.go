package hubclient

import (
	"context"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

type localExecutionKey struct{}
type directExecutionKey struct{}
type directExecutionValidatorKey struct{}
type directExecutionValidator struct {
	identity string
	validate func() error
}

// Only the authenticated Direct relay can attach this immutable pairing lease.
// It outlives one HTTP request, but fails after revoke, rebind, or relay close.
func WithDirectExecutionValidator(ctx context.Context, identity string, validate func() error) context.Context {
	return context.WithValue(WithDirectExecution(ctx), directExecutionValidatorKey{}, directExecutionValidator{identity, validate})
}

func DirectExecutionValidator(ctx context.Context) (string, func() error, bool) {
	lease, ok := ctx.Value(directExecutionValidatorKey{}).(directExecutionValidator)
	return lease.identity, lease.validate, ok && lease.identity != "" && lease.validate != nil
}

// WithLocalExecution is called by the authenticated loopback console only.
// No command field can select it; a Hub guard or source always takes priority.
func WithLocalExecution(ctx context.Context) context.Context {
	return context.WithValue(ctx, localExecutionKey{}, true)
}

// WithDirectExecution is used only by the authenticated, TLS-pinned direct
// phone relay. It keeps execution local to this Bridge (there is no Hub
// request or source grant), while retaining the normal Dispatch validation,
// adapter capability checks, account selection and cancellation boundaries.
// The direct relay itself is responsible for the separate phone pairing and
// command allow-list; callers must never expose this marker to a wire client.
func WithDirectExecution(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, localExecutionKey{}, true)
	return context.WithValue(ctx, directExecutionKey{}, true)
}

func IsDirectExecution(ctx context.Context) bool {
	direct, _ := ctx.Value(directExecutionKey{}).(bool)
	return direct
}

func isLocalExecution(ctx context.Context) bool {
	local, _ := ctx.Value(localExecutionKey{}).(bool)
	_, guarded := commandGuardFromContext(ctx)
	_, sourced := executionSourceFromContext(ctx)
	return local && !guarded && !sourced
}

func executionConfig(ctx context.Context, cfg config.Config) config.Config {
	if isLocalExecution(ctx) {
		cfg.RemoteAPI = nil
		if IsDirectExecution(ctx) {
			// The common catalogue/model checks consume RemoteAPI. This copy is
			// execution-local; it never changes the Hub selection in the store.
			cfg.RemoteAPI = make(map[string]config.RemoteAPI, len(cfg.DirectAPI))
			for family, choice := range cfg.DirectAPI {
				cfg.RemoteAPI[family] = choice
			}
		}
	}
	return cfg
}

func withWorkerSnapshot(ctx context.Context, cfg config.Config) context.Context {
	if isLocalExecution(ctx) {
		if IsDirectExecution(ctx) {
			return agents.WithDirectConfigSnapshot(ctx, cfg)
		}
		return agents.WithLocalConfigSnapshot(ctx, cfg)
	}
	return agents.WithConfigSnapshot(ctx, cfg)
}
