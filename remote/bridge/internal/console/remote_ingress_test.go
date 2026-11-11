package console

import (
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/config"
)

func TestRemoteIngressUpdatesPinnedRouteWithoutRebindingOrActivating(t *testing.T) {
	for _, sameIdentity := range []bool{true, false} {
		name := "same-pin"
		if !sameIdentity {
			name = "replacement"
		}
		t.Run(name, func(t *testing.T) {
			f := newEnrollmentFixture(t)
			s, h := newTestServer(t)
			const root = "https://old.fixture.example"
			if err := s.d.Store.Update(func(c *config.Config) error {
				c.ConnectRemoteWithBotBinding(root, root+"/salcara-hub", "group-a", "saved-invite", "saved-signature")
				c.RemoteConnections[0].HubInstanceID = strings.Repeat("a", 32)
				if !sameIdentity {
					c.RemoteConnections[0].HubInstanceID = strings.Repeat("b", 32)
				}
				c.RemoteConnections[0].IdentityPublicKey = f.publicKey
				c.ConnectRemote("https://active.fixture.example", "https://active.fixture.example/salcara-hub")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := s.d.Store.Get()
			w := localRequest(s, h, "POST", "/api/remote/ingress", map[string]any{"hubUrl": root + "/salcara-hub", "groupId": "group-a", "url": enrollmentFixtureHub})
			after := s.d.Store.Get()
			if !sameIdentity {
				if w.Code != 409 || !reflect.DeepEqual(before, after) {
					t.Fatal("replacement overwrote original route/pin")
				}
				return
			}
			if w.Code != 200 || config.RemoteIdentity(before) != config.RemoteIdentity(after) || f.challengeCount != 0 || f.issuerCount != 0 || f.registerCount != 0 {
				t.Fatalf("route update rebound or activated a device: %d %s", w.Code, w.Body)
			}
			want := before.Clone()
			want.RemoteConnections[0].IngressURL = enrollmentFixtureHub + "/v1"
			if !reflect.DeepEqual(want, after) {
				t.Fatal("route update changed logical profile, credentials or another connection")
			}
		})
	}
}
