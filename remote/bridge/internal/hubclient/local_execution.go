package hubclient

import (
	"context"
	"errors"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

type localExecutionKey struct{}
type directExecutionKey struct{}
type directExecutionValidatorKey struct{}
type remoteSettingsEpochKey struct{}

// WithRemoteSettingsEpoch freezes the authenticated console's status/API form
// scope before Dispatch can wait. It grants no execution or pairing authority.
func WithRemoteSettingsEpoch(ctx context.Context, epoch string) context.Context {
	return context.WithValue(ctx, remoteSettingsEpochKey{}, epoch)
}

func validateRemoteSettingsEpoch(ctx context.Context, typ string, cfg config.Config) error {
	epoch, present := ctx.Value(remoteSettingsEpochKey{}).(string)
	if !present {
		return nil
	}
	if !isLocalExecution(ctx) || (typ != "agents.status" && typ != "agents.api.set") || epoch == "" || epoch != cfg.RemoteConnectionEpoch {
		return errors.New("连接方式或中转站已变化，请刷新设置后重试")
	}
	return nil
}

type directExecutionValidator struct {
	identity string
	validate func() error
}

// The authenticated Direct relay attaches this immutable pairing lease. The
// authenticated loopback console may also attach a narrow, separately frozen
// pairing/connection-epoch validator for Direct status/API configuration only.
// No submitted command can create it; revoke/rebind/connection change retires it.
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

// WithDirectExecution is used by the authenticated, TLS-pinned direct phone
// relay and the console's validated Direct configuration route. It keeps execution local to this Bridge (there is no Hub
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
