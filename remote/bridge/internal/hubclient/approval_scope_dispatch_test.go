package hubclient

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type approvalScopeDispatchCall struct {
	ctx                    context.Context
	sessionKey, approvalID string
	response               agents.ApprovalResponse
}

type approvalScopeDispatchAgent struct {
	*fakeAgent
	pending     map[string]string
	scopedCalls []approvalScopeDispatchCall
	legacyCalls int
}

func (a *approvalScopeDispatchAgent) RespondForSession(ctx context.Context, sessionKey, approvalID string, response agents.ApprovalResponse) bool {
	a.scopedCalls = append(a.scopedCalls, approvalScopeDispatchCall{ctx, sessionKey, approvalID, response})
	if a.pending[approvalID] != sessionKey || sessionKey == "" {
		return false
	}
	delete(a.pending, approvalID)
	return true
}

func (a *approvalScopeDispatchAgent) RespondWithAnswers(approvalID string, _ agents.ApprovalResponse) bool {
	a.legacyCalls++
	if _, exists := a.pending[approvalID]; !exists {
		return false
	}
	delete(a.pending, approvalID)
	return true
}

type approvalIDOnlyDispatchAgent struct {
	*fakeAgent
	pending bool
	calls   int
}

func (a *approvalIDOnlyDispatchAgent) Respond(approvalID, _, _ string) bool {
	a.calls++
	if approvalID != "fixture-approval" || !a.pending {
		return false
	}
	a.pending = false
	return true
}

func approvalScopeDispatchFixture(t *testing.T, mode string) (*Client, config.Config, protocol.CommandEnvelope) {
	t.Helper()
	c, store, account, env := sourceGuardFixture(t, "api_key")
	if err := store.Update(func(cfg *config.Config) error {
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("c", 64)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := store.Get()
	c.publishPair(PairStatus{DeviceID: cfg.DeviceID, Paired: true}, eventIdentity(cfg))
	env.DeviceID, env.BindingID, env.PhoneHash = cfg.DeviceID, cfg.PhoneBindingID, cfg.PhoneHash
	env.Command = map[string]any{"type": "approval.respond", "agent": "codex", "tool": "codex", "controlSurface": "cli",
		"sessionKey": "codex:original", "accountId": APIHandle(cfg, account.ID), "model": account.Model,
		"approvalId": "fixture-approval", "decision": "allow", "message": "Human feedback"}
	if mode == "open" {
		env.Source = &protocol.ExecutionSource{Mode: "open", PolicyRevision: "approval-policy-r1", Station: env.Source.Station}
	}
	return c, cfg, env
}

func approvalScopeDispatchContext(cfg config.Config, env protocol.CommandEnvelope) context.Context {
	ctx := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID,
		bindingID: cfg.PhoneBindingID, phoneHash: cfg.PhoneHash})
	return withExecutionSource(ctx, env)
}

func TestApprovalScopeDispatchRemoteUsesExactSessionAndStructuredResponse(t *testing.T) {
	for _, mode := range []string{"restricted", "open"} {
		t.Run(mode, func(t *testing.T) {
			c, cfg, env := approvalScopeDispatchFixture(t, mode)
			a := &approvalScopeDispatchAgent{fakeAgent: &fakeAgent{id: "codex"}, pending: map[string]string{"fixture-approval": "codex:sibling"}}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			env.Command["answers"] = map[string]any{"question-id": []any{"Exact label with spaces "}}
			if _, err := c.Dispatch(approvalScopeDispatchContext(cfg, env), env.Command); err == nil {
				t.Fatal("original session answered a known sibling approval")
			}
			if a.pending["fixture-approval"] != "codex:sibling" || len(a.scopedCalls) != 1 || a.scopedCalls[0].sessionKey != "codex:original" || a.legacyCalls != 0 {
				t.Fatal("sibling rejection consumed the approval or discarded its exact session scope")
			}
			env.Command["sessionKey"] = "codex:sibling"
			result, err := c.Dispatch(approvalScopeDispatchContext(cfg, env), env.Command)
			if err != nil || result == nil || len(a.pending) != 0 || len(a.scopedCalls) != 2 || a.legacyCalls != 0 {
				t.Fatalf("matching session did not answer through the scoped adapter: result=%v err=%v", result, err)
			}
			call := a.scopedCalls[1]
			want := agents.ApprovalResponse{Decision: "allow", Message: "Human feedback", Answers: map[string][]string{"question-id": {"Exact label with spaces "}}}
			if call.sessionKey != "codex:sibling" || call.approvalID != "fixture-approval" || !reflect.DeepEqual(call.response, want) {
				t.Fatalf("scoped approval dispatch changed its request: %+v", call)
			}
			guard, guarded := commandGuardFromContext(call.ctx)
			proof, proved := executionSourceFromContext(call.ctx)
			if !guarded || guard.identity != config.RemoteIdentity(cfg) || guard.deviceID != cfg.DeviceID || guard.bindingID != cfg.PhoneBindingID || guard.phoneHash != cfg.PhoneHash || !proved || proof.Source != env.Source {
				t.Fatal("scoped adapter lost its authorized command context")
			}
		})
	}
}

func TestApprovalScopeDispatchWrongAccountRejectedBeforeResponder(t *testing.T) {
	for _, mode := range []string{"restricted", "open"} {
		t.Run(mode, func(t *testing.T) {
			c, cfg, env := approvalScopeDispatchFixture(t, mode)
			other := cfg.LocalAccounts[0]
			other.ID, other.Key = "other-account", "synthetic-other-key"
			if err := c.o.Store.Update(func(next *config.Config) error {
				next.LocalAccounts = append(next.LocalAccounts, other)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			cfg = c.o.Store.Get()
			handle := APIHandle(cfg, other.ID)
			env.Command["accountId"] = handle
			if mode == "restricted" {
				env.Source.CredentialHandle, env.Source.ConfigRevision = handle, cfg.AccountSourceRevision(other)
			}
			if err := c.validateExecutionSource(env); err != nil {
				t.Fatalf("fixture did not provide a valid proof for the other saved account: %v", err)
			}
			a := &approvalScopeDispatchAgent{fakeAgent: &fakeAgent{id: "codex"}, pending: map[string]string{"fixture-approval": "codex:original"}}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			if _, err := c.Dispatch(approvalScopeDispatchContext(cfg, env), env.Command); err == nil || !strings.Contains(err.Error(), "API 与当前执行来源不一致") {
				t.Fatalf("wrong account was not rejected by the execution account guard: %v", err)
			}
			if len(a.scopedCalls) != 0 || a.legacyCalls != 0 || a.pending["fixture-approval"] != "codex:original" {
				t.Fatal("wrong account reached the responder or consumed the approval")
			}
		})
	}
}

func TestApprovalScopeDispatchRemoteDoesNotUseLegacyIDOnlyAdapter(t *testing.T) {
	for _, mode := range []string{"restricted", "open"} {
		t.Run(mode, func(t *testing.T) {
			c, cfg, env := approvalScopeDispatchFixture(t, mode)
			a := &approvalIDOnlyDispatchAgent{fakeAgent: &fakeAgent{id: "codex"}, pending: true}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			if _, err := c.Dispatch(approvalScopeDispatchContext(cfg, env), env.Command); err == nil {
				t.Fatal("remote approval silently used an ID-only adapter")
			}
			if !a.pending || a.calls != 0 {
				t.Fatal("remote rejection reached the legacy adapter or consumed its approval")
			}
			if _, err := c.Dispatch(context.Background(), env.Command); err != nil {
				t.Fatalf("direct local approval lost legacy adapter compatibility: %v", err)
			}
			if a.pending || a.calls != 1 {
				t.Fatal("direct local approval did not reach the legacy adapter exactly once")
			}
		})
	}
}

func TestApprovalScopeDispatchRemoteRejectsMissingSession(t *testing.T) {
	for _, mode := range []string{"restricted", "open"} {
		t.Run(mode, func(t *testing.T) {
			c, cfg, env := approvalScopeDispatchFixture(t, mode)
			delete(env.Command, "sessionKey")
			a := &approvalScopeDispatchAgent{fakeAgent: &fakeAgent{id: "codex"}, pending: map[string]string{"fixture-approval": "codex:original"}}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			if _, err := c.Dispatch(approvalScopeDispatchContext(cfg, env), env.Command); err == nil || !strings.Contains(err.Error(), "远程审批缺少会话归属") {
				t.Fatalf("remote approval without a session was not rejected: %v", err)
			}
			if len(a.scopedCalls) != 0 || a.legacyCalls != 0 || a.pending["fixture-approval"] != "codex:original" {
				t.Fatal("unscoped remote approval reached or consumed a pending request")
			}
		})
	}
}
