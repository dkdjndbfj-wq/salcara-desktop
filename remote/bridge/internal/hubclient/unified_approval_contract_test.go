package hubclient

import (
	"context"
	"reflect"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

// All three adapters deliberately expose the same public contract and reuse
// the same native approval ID. Neither that ID nor a shared API family may
// select the recipient. These are in-memory fixtures, not real CLI workers.
type unifiedApprovalContractAgent struct {
	*approvalScopeDispatchAgent
	descriptor protocol.AgentDescriptor
}

func (a *unifiedApprovalContractAgent) Descriptor() protocol.AgentDescriptor { return a.descriptor }
func (a *unifiedApprovalContractAgent) RespondForSession(ctx context.Context, key, id string, response agents.ApprovalResponse) bool {
	if response.Decision == "allow_session" {
		// This synthetic native request only offers once/deny, just as Claude's
		// legacy prompt does. The common router must preserve the rejection.
		a.scopedCalls = append(a.scopedCalls, approvalScopeDispatchCall{ctx, key, id, response})
		return false
	}
	if response.Answers != nil && !reflect.DeepEqual(response.Answers, map[string][]string{"question": {"Exact choice "}}) {
		a.scopedCalls = append(a.scopedCalls, approvalScopeDispatchCall{ctx, key, id, response})
		return false
	}
	return a.approvalScopeDispatchAgent.RespondForSession(ctx, key, id, response)
}

var unifiedApprovalContractIDs = []string{"codex", "claude", "synthetic-agent"}

func unifiedApprovalContractFixture(t *testing.T, target, mode string) (*Client, config.Config, protocol.CommandEnvelope, map[string]*unifiedApprovalContractAgent) {
	t.Helper()
	c, _, env := approvalScopeDispatchFixture(t, mode)
	byID := map[string]*unifiedApprovalContractAgent{}
	list := []agents.Agent{}
	for _, id := range unifiedApprovalContractIDs {
		d := aggregationDescriptor(id)
		if id == "synthetic-agent" {
			d.APIFamily = "codex" // Credential sharing never changes executor identity.
		}
		a := &unifiedApprovalContractAgent{approvalScopeDispatchAgent: &approvalScopeDispatchAgent{
			fakeAgent: &fakeAgent{id: id}, pending: map[string]string{"fixture-approval": id + ":original"}}, descriptor: d}
		byID[id], list = a, append(list, a)
	}
	c.SetManager(&fakeManager{list: list})
	if err := c.o.Store.Update(func(cfg *config.Config) error {
		cfg.RemoteAPI["claude"] = cfg.RemoteAPI["codex"]
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cfg := c.o.Store.Get()
	if !cfg.RemoteExecutionArmed {
		t.Fatal("fixture bypassed the real remote source guard")
	}
	env.Command["agent"], env.Command["tool"], env.Command["sessionKey"] = target, target, target+":original"
	if mode == "restricted" {
		env.Source.Agent = target
	}
	if err := c.validateExecutionSource(env); err != nil {
		t.Fatalf("invalid synthetic provenance: %v", err)
	}
	return c, cfg, env, byID
}

func assertUnifiedApprovalRecipients(t *testing.T, adapters map[string]*unifiedApprovalContractAgent, target string, targetCalls int, consumed bool) {
	t.Helper()
	for id, a := range adapters {
		wantCalls := 0
		if id == target {
			wantCalls = targetCalls
		}
		if len(a.scopedCalls) != wantCalls || a.legacyCalls != 0 {
			t.Fatalf("wrong recipient or fallback: adapter=%s scoped=%d want=%d legacy=%d", id, len(a.scopedCalls), wantCalls, a.legacyCalls)
		}
		wantPending := !(id == target && consumed)
		if (a.pending["fixture-approval"] == id+":original") != wantPending {
			t.Fatalf("approval lifetime changed in wrong adapter: %s", id)
		}
	}
}

func TestUnifiedApprovalContractRemoteDispatchAcrossAdapters(t *testing.T) {
	for _, target := range unifiedApprovalContractIDs {
		for _, mode := range []string{"open", "restricted"} {
			for _, kind := range []string{"allow_once", "deny", "question"} {
				t.Run(target+"/"+mode+"/"+kind, func(t *testing.T) {
					c, cfg, env, adapters := unifiedApprovalContractFixture(t, target, mode)
					if kind == "deny" {
						env.Command["decision"] = "deny"
					}
					if kind == "question" {
						env.Command["answers"] = map[string]any{"question": []any{"Exact choice "}}
					}
					ctx := approvalScopeDispatchContext(cfg, env)
					if _, err := c.Dispatch(ctx, env.Command); err != nil {
						t.Fatal(err)
					}
					assertUnifiedApprovalRecipients(t, adapters, target, 1, true)
					call := adapters[target].scopedCalls[0]
					if call.sessionKey != target+":original" || call.approvalID != "fixture-approval" || call.response.Decision != env.Command["decision"] || call.response.Message != "Human feedback" {
						t.Fatal("unified interface changed the exact native-scoped response", call)
					}
					if kind == "question" && !reflect.DeepEqual(call.response.Answers, map[string][]string{"question": {"Exact choice "}}) {
						t.Fatal("question answers were flattened, trimmed or remapped")
					}
					guard, ok := commandGuardFromContext(call.ctx)
					proof, hasProof := executionSourceFromContext(call.ctx)
					if !ok || !hasProof || guard.identity != config.RemoteIdentity(cfg) || guard.bindingID != cfg.PhoneBindingID || guard.phoneHash != cfg.PhoneHash || proof.Source != env.Source {
						t.Fatal("remote provenance lost before native adapter")
					}
					if _, err := c.Dispatch(ctx, env.Command); err == nil {
						t.Fatal("already consumed request was accepted twice")
					}
					assertUnifiedApprovalRecipients(t, adapters, target, 2, true)
				})
			}
		}
	}
}

func TestUnifiedApprovalContractInvalidBoundaryNeverFallsThrough(t *testing.T) {
	for _, target := range unifiedApprovalContractIDs {
		for _, mode := range []string{"open", "restricted"} {
			for _, bad := range []string{"wrong_namespace", "unknown_namespace", "wrong_agent", "desktop_surface", "malformed_surface", "missing_session", "bad_decision", "bad_answers", "missing_source", "wrong_account", "revoked_phone", "source_revision", "source_agent", "source_surface", "sibling_approval", "unoffered_decision", "invalid_question", "capability_removed"} {
				if mode == "open" && (bad == "source_revision" || bad == "source_agent" || bad == "source_surface") {
					continue // Open policy intentionally does not require restricted attestations.
				}
				t.Run(target+"/"+mode+"/"+bad, func(t *testing.T) {
					c, cfg, env, adapters := unifiedApprovalContractFixture(t, target, mode)
					other := "codex"
					if target == other {
						other = "claude"
					}
					wantCalls := 0
					switch bad {
					case "wrong_namespace":
						env.Command["sessionKey"] = other + ":original"
					case "unknown_namespace":
						env.Command["sessionKey"] = "not-registered:original"
					case "wrong_agent":
						env.Command["agent"] = other
					case "desktop_surface":
						env.Command["controlSurface"] = "desktop"
						if mode == "restricted" {
							env.Source.ControlSurface = "desktop"
						}
					case "malformed_surface":
						env.Command["controlSurface"] = true
					case "missing_session":
						delete(env.Command, "sessionKey")
					case "bad_decision":
						env.Command["decision"] = "approve-everything"
					case "bad_answers":
						env.Command["answers"] = map[string]any{"question": []any{true}}
					case "missing_source":
						env.Source = nil
					case "wrong_account":
						account := cfg.LocalAccounts[0]
						account.ID, account.Key = "other-account", "synthetic-other-key"
						if err := c.o.Store.Update(func(next *config.Config) error { next.LocalAccounts = append(next.LocalAccounts, account); return nil }); err != nil {
							t.Fatal(err)
						}
						current := c.o.Store.Get()
						env.Command["accountId"] = APIHandle(current, account.ID)
						if mode == "restricted" {
							env.Source.CredentialHandle = APIHandle(current, account.ID)
							env.Source.ConfigRevision = current.AccountSourceRevision(account)
						}
					case "revoked_phone":
						if err := c.o.Store.Update(func(next *config.Config) error { next.PhoneHash = ""; return nil }); err != nil {
							t.Fatal(err)
						}
					case "source_revision":
						env.Source.ConfigRevision = "stale-revision"
					case "source_agent":
						env.Source.Agent = other
					case "source_surface":
						env.Source.ControlSurface = "desktop"
					case "sibling_approval":
						env.Command["sessionKey"] = target + ":sibling"
						wantCalls = 1
					case "unoffered_decision":
						env.Command["decision"] = "allow_session"
						wantCalls = 1
					case "invalid_question":
						env.Command["answers"] = map[string]any{"another-question": []any{"Wrong choice"}}
						wantCalls = 1
					case "capability_removed":
						adapters[target].descriptor.Capabilities.Approval = false
					}
					if _, err := c.Dispatch(approvalScopeDispatchContext(cfg, env), env.Command); err == nil {
						t.Fatal("unsafe remote approval accepted")
					}
					assertUnifiedApprovalRecipients(t, adapters, target, wantCalls, false)
				})
			}
		}
	}
}
