package hubclient

import (
	"context"
	"testing"

	"salcara/bridge/internal/agents"
)

type creationModelAgent struct{ *aggregationFixtureAgent }

func (a *creationModelAgent) StartWithOptions(ctx context.Context, cwd, prompt, approval string, opts agents.TurnOptions) (string, error) {
	return a.Start(ctx, cwd, prompt, opts.Model, approval)
}

func TestCreationModelCapabilityRequiresStarterAndRemainsCreationOnly(t *testing.T) {
	c, base, cwd := aggregationFixture(t, "hermes")
	base.descriptor.Capabilities.ModelOverride = false
	base.descriptor.Capabilities.StartModelOverride = true
	if publishedRemoteCapabilities(base, base.descriptor.Capabilities).StartModelOverride {
		t.Fatal("declared start capability without executable contract was published")
	}
	command := map[string]any{"type": "session.start", "tool": "hermes", "cwd": cwd, "prompt": "task", "model": "native-choice"}
	assertCapabilityRefusal(t, c, command)
	a := &creationModelAgent{base}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	caps := publishedRemoteCapabilities(a, a.descriptor.Capabilities)
	if !caps.StartModelOverride || caps.ModelOverride {
		t.Fatalf("creation capability changed: %+v", caps)
	}
	if _, err := c.Dispatch(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	if len(base.startModels) != 1 || base.startModels[0] != "native-choice" {
		t.Fatalf("creation model lost: %v", base.startModels)
	}
	assertCapabilityRefusal(t, c, map[string]any{"type": "session.send", "sessionKey": "hermes:s", "text": "next", "model": "native-choice"})
	command["effort"] = "high"
	if _, err := c.Dispatch(context.Background(), command); err == nil {
		t.Fatal("creation-only model support granted an unsupported reasoning effort")
	}
	if len(base.started) != 1 || len(base.sent) != 0 {
		t.Fatal("unsupported turn reached adapter")
	}
}
