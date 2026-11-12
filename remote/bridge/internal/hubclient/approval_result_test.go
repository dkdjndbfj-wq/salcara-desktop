package hubclient

import (
	"context"
	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
	"testing"
)

type typedApprovalFixture struct {
	*approvalScopeDispatchAgent
	response agents.ApprovalResponse
	err      error
}

type typedApprovalFixtureError string

func (e typedApprovalFixtureError) Error() string              { return string(e) }
func (e typedApprovalFixtureError) ExecutionErrorCode() string { return string(e) }

func (a *typedApprovalFixture) RespondForSessionResult(_ context.Context, _, _ string, response agents.ApprovalResponse) (bool, error) {
	a.response = response
	return a.err == nil, a.err
}

func TestTypedApprovalDispatchPreservesOriginOperationAndUncertainty(t *testing.T) {
	for _, mode := range []string{"hub", "direct", "local"} {
		for _, code := range []string{"command_delivery_uncertain", "approval_not_sent", "approval_rejected"} {
			t.Run(mode+"/"+code, func(t *testing.T) {
				var c *Client
				var ctx context.Context
				cmd := map[string]any{"type": "approval.respond", "tool": "codex", "controlSurface": "cli", "sessionKey": "codex:original", "approvalId": "fixture-approval", "decision": "allow", "operationId": "11111111-1111-4111-8111-111111111111"}
				if mode == "hub" {
					var cfg config.Config
					var env protocol.CommandEnvelope
					c, cfg, env = approvalScopeDispatchFixture(t, "restricted")
					env.Command = cmd
					cmd["accountId"] = APIHandle(cfg, cfg.LocalAccounts[0].ID)
					ctx = approvalScopeDispatchContext(cfg, env)
				} else {
					account := directAccount("a", "https://fixture.invalid")
					c = statusFixture(t, config.Config{LocalAccounts: []config.LocalAccount{account}, DirectAPI: map[string]config.RemoteAPI{"codex": {AccountID: account.ID}}})
					c.o.Store.SetRuntimeGatewayKey("fixture-runtime")
					ctx = WithLocalExecution(context.Background())
					if mode == "direct" {
						ctx = directFixtureContext(t, c)
						cmd["accountId"] = APIHandle(c.o.Store.Get(), account.ID)
					}
				}
				worker := &typedApprovalFixture{approvalScopeDispatchAgent: &approvalScopeDispatchAgent{fakeAgent: &fakeAgent{id: "codex"}}, err: typedApprovalFixtureError(code)}
				c.SetManager(&fakeManager{list: []agents.Agent{worker}})
				_, err := c.Dispatch(ctx, cmd)
				if protocol.ExecutionErrorCode(err) != code {
					t.Fatal("typed error lost", code, err)
				}
				by := "phone"
				if mode == "local" {
					by = "desktop"
				}
				if worker.response.By != by || worker.response.OperationID != cmd["operationId"] {
					t.Fatal("origin/operation lost", worker.response)
				}
			})
		}
	}
}
