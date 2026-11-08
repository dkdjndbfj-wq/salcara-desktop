package hubclient

import (
	"context"
	"testing"

	"salcara/bridge/internal/agents"
)

func TestAggregationDesktopSuffixRoutingAndBusyFamilyAreExplicit(t *testing.T) {
	c, a, _ := aggregationFixture(t, "hermes")
	b := &sharedAccountExtensionWorker{aggregationFixtureAgent: &aggregationFixtureAgent{
		fakeAgent: &fakeAgent{id: "hermes-desktop"}, descriptor: aggregationDescriptor("hermes-desktop")}, busy: true}
	b.descriptor.APIFamily = ""
	c.SetManager(&fakeManager{list: []agents.Agent{a, b}})
	route, err := c.routeForAgent("hermes-desktop")
	if err != nil || route.ID != "hermes-desktop" || route.Family != "hermes-desktop" {
		t.Fatalf("extension routing was implicitly aliased: route=%+v error=%v", route, err)
	}
	if routes := c.routesForFamily("hermes"); len(routes) != 1 || routes[0].ID != "hermes" {
		t.Fatal("an unrelated extension joined the API family")
	}
	if err := c.checkRemoteAPIChange(context.Background(), "hermes", "hermes:s"); err != nil {
		t.Fatalf("an unrelated busy extension blocked an isolated API: %v", err)
	}
	if err := c.checkRemoteAPIChange(context.Background(), "hermes-desktop", "hermes-desktop:s"); err == nil {
		t.Fatal("the actual extension family ignored its active worker")
	}
	b.descriptor.APIFamily = "hermes"
	if routes := c.routesForFamily("hermes"); len(routes) != 2 {
		t.Fatal("explicit family sharing was lost")
	}
	if err := c.checkRemoteAPIChange(context.Background(), "hermes", "hermes:s"); err == nil {
		t.Fatal("an explicitly shared busy extension no longer blocked the family")
	}
}
