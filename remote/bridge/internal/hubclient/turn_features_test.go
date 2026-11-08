package hubclient

import (
	"context"
	"reflect"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type turnFeaturesAgent struct {
	*aggregationFixtureAgent
	cwd           string
	featuresCalls []string
	options       []agents.TurnOptions
}

func (a *turnFeaturesAgent) Features(_ context.Context, cwd, id string) (protocol.AgentFeatures, error) {
	a.featuresCalls = append(a.featuresCalls, cwd+"|"+id)
	return protocol.AgentFeatures{CollaborationModes: []string{"default", "plan"}, Skills: []protocol.Skill{{Name: "fixture-skill", Path: "catalog-handle"}}, SkillsSupported: true}, nil
}
func (a *turnFeaturesAgent) SessionMetadata(_ context.Context, id string) (protocol.SessionInfo, error) {
	return protocol.SessionInfo{SessionKey: a.ID() + ":" + id, Cwd: a.cwd, ControlSurface: "cli"}, nil
}
func (a *turnFeaturesAgent) StartWithOptions(_ context.Context, _, _, _ string, opts agents.TurnOptions) (string, error) {
	a.options = append(a.options, opts)
	return "created", nil
}
func (a *turnFeaturesAgent) SendWithOptions(_ context.Context, _, _ string, opts agents.TurnOptions) error {
	a.options = append(a.options, opts)
	return nil
}
func (a *turnFeaturesAgent) ContinueWithOptions(_ context.Context, _, _, _, _ string, opts agents.TurnOptions) (protocol.ContinueResult, error) {
	a.options = append(a.options, opts)
	return protocol.ContinueResult{Accepted: true}, nil
}
func turnFeatureFixture(t *testing.T) (*Client, *turnFeaturesAgent, string) {
	t.Helper()
	c, base, cwd := aggregationFixture(t, "hermes")
	base.descriptor.Capabilities.Features = true
	base.descriptor.Capabilities.Continue = true
	a := &turnFeaturesAgent{aggregationFixtureAgent: base, cwd: cwd}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	return c, a, cwd
}
func TestTurnFeaturesCatalogUsesCheckedProjectOrExactSessionMetadata(t *testing.T) {
	c, a, cwd := turnFeatureFixture(t)
	for _, cmd := range []map[string]any{
		{"type": "features.list", "tool": "hermes", "cwd": cwd, "controlSurface": "cli"},
		{"type": "features.list", "tool": "hermes", "sessionKey": "hermes:old", "cwd": t.TempDir(), "controlSurface": "cli"},
	} {
		out, err := c.Dispatch(context.Background(), cmd)
		if err != nil || !out.(protocol.AgentFeatures).SkillsSupported {
			t.Fatal(out, err)
		}
	}
	if !reflect.DeepEqual(a.featuresCalls, []string{cwd + "|", cwd + "|old"}) || len(a.options) > 0 || len(a.opened) > 0 {
		t.Fatal("feature read retargeted or executed session", a.featuresCalls, a.options)
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "features.list", "tool": "hermes", "cwd": t.TempDir(), "controlSurface": "cli"}); err == nil {
		t.Fatal("unapproved new-task cwd reached catalog")
	}
	if len(a.featuresCalls) != 2 {
		t.Fatal(a.featuresCalls)
	}
}
func TestTurnFeaturesOptionsSurviveStartSendAndContinue(t *testing.T) {
	for _, typ := range []string{"session.start", "session.send", "session.continue"} {
		t.Run(typ, func(t *testing.T) {
			c, a, cwd := turnFeatureFixture(t)
			cmd := map[string]any{"type": typ, "tool": "hermes", "sessionKey": "hermes:old", "cwd": cwd, "text": "input", "prompt": "task", "controlSurface": "cli", "collaborationMode": "plan", "skills": []any{map[string]any{"name": "fixture-skill", "path": "catalog-handle"}}, "expectedTurnId": "old-turn", "operationId": "11111111-2222-4333-8444-555555555555"}
			if _, err := c.Dispatch(context.Background(), cmd); err != nil {
				t.Fatal(err)
			}
			if len(a.options) != 1 || a.options[0].CollaborationMode != "plan" || !reflect.DeepEqual(a.options[0].Skills, []protocol.SkillReference{{Name: "fixture-skill", Path: "catalog-handle"}}) {
				t.Fatal(a.options)
			}
		})
	}
}
func TestTurnFeaturesUnsupportedNativeSteerAndMalformedOptionsFailBeforeSideEffects(t *testing.T) {
	for _, mutation := range []string{"no-capability", "native", "steer", "bad-mode", "bad-skills", "null-skills", "bad-name", "unknown-skill-field"} {
		t.Run(mutation, func(t *testing.T) {
			c, a, _ := turnFeatureFixture(t)
			cmd := map[string]any{"type": "session.send", "sessionKey": "hermes:old", "text": "input", "controlSurface": "cli", "collaborationMode": "plan"}
			switch mutation {
			case "no-capability":
				a.descriptor.Capabilities.Features = false
			case "native":
				cmd["controlSurface"] = "desktop"
			case "steer":
				cmd["type"] = "session.steer"
			case "bad-mode":
				cmd["collaborationMode"] = true
			case "bad-skills":
				cmd["skills"] = "skill"
			case "null-skills":
				cmd["skills"] = nil
			case "bad-name":
				cmd["skills"] = []any{map[string]any{"name": "bad\nname"}}
			case "unknown-skill-field":
				cmd["skills"] = []any{map[string]any{"name": "fixture-skill", "command": "must not be dropped"}}
			}
			if _, err := c.Dispatch(context.Background(), cmd); err == nil {
				t.Fatal("invalid feature accepted")
			}
			if len(a.options)+len(a.featuresCalls)+len(a.started)+len(a.sent)+len(a.opened) != 0 {
				t.Fatal("refusal happened after a side effect")
			}
		})
	}
	c, base, _ := aggregationFixture(t, "hermes")
	base.descriptor.Capabilities.Features = true
	if caps := publishedRemoteCapabilities(base, base.descriptor.Capabilities); caps.Features {
		t.Fatal("descriptor invented missing executable contracts")
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "features.list", "tool": "hermes", "cwd": t.TempDir()}); err == nil {
		t.Fatal("adapter without Features accepted catalog")
	}
}
func TestTurnFeaturesEmptySelectionPreservesOmittedMode(t *testing.T) {
	c, a, _ := turnFeatureFixture(t)
	a.descriptor.Capabilities.Features = false
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": "hermes:old", "text": "input", "skills": []any{}}); err != nil {
		t.Fatal(err)
	}
	if len(a.options) != 0 || len(a.sent) != 1 {
		t.Fatal("empty selection forced feature transport", a.options, a.sent)
	}
}
func TestTurnFeaturesCatalogHasSourceAndAccountGuard(t *testing.T) {
	c, store, _, env := sourceGuardFixture(t, "api_key")
	env.Command["type"] = "features.list"
	if !sourceGuardExecutionType("features.list") {
		t.Fatal("feature catalog bypasses signed source guard")
	}
	if err := c.validateExecutionSource(env); err != nil {
		t.Fatal(err)
	}
	copyEnv := env
	copyEnv.Source = nil
	if err := c.validateExecutionSource(copyEnv); err == nil {
		t.Fatal("unsigned features read accepted")
	}
	if err := store.Update(func(cfg *config.Config) error { cfg.LocalAccounts[0].Key = "changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := c.validateExecutionSource(env); err == nil {
		t.Fatal("stale API source accepted")
	}
}
