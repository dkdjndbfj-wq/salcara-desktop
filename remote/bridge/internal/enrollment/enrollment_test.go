package enrollment

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSignedEnrollmentPurposesAndExactPayload(t *testing.T) {
	now := time.Now().Unix()
	c := Challenge{Purpose: ChallengePurpose, Version: 1, Issuer: "station", HubInstanceID: strings.Repeat("a", 32), Audience: "hub", APIBase: "https://station.example.test", PolicyRevision: "r1", DeviceID: "fixture-device", DeviceSecretHash: strings.Repeat("b", 64), ChallengeID: "challenge-1", RequestID: "11111111-1111-4111-8111-111111111111", IssuedAt: now, ExpiresAt: now + 120}
	raw, _ := json.Marshal(c)
	if _, err := ParseChallenge(raw); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"duplicate", "unknown", "case", "null", "trailing", "missing"} {
		t.Run(scenario, func(t *testing.T) {
			text := string(raw)
			switch scenario {
			case "duplicate":
				text = strings.TrimSuffix(text, "}") + `,"issuer":"other"}`
			case "unknown":
				text = strings.TrimSuffix(text, "}") + `,"extra":true}`
			case "case":
				text = strings.Replace(text, `"issuer":`, `"Issuer":`, 1)
			case "null":
				text = strings.Replace(text, `"issuer":"station"`, `"issuer":null`, 1)
			case "trailing":
				text += "{}"
			case "missing":
				text = strings.Replace(text, `"issuer":"station",`, "", 1)
			}
			if _, err := ParseChallenge([]byte(text)); err == nil {
				t.Fatal("non-exact signed challenge was accepted")
			}
		})
	}
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	proof, err := SignEnvelope(c, ChallengePurpose, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyEnvelope(proof, ChallengePurpose, key.Public().(ed25519.PublicKey)); err != nil {
		t.Fatal(err)
	}
	if _, err = VerifyEnvelope(proof, GrantPurpose, key.Public().(ed25519.PublicKey)); err == nil {
		t.Fatal("challenge signature was accepted as a grant")
	}
	g := Grant{Challenge: c, Subject: "sub2api-user:123", GroupID: 1}
	g.Purpose = GrantPurpose
	raw, _ = json.Marshal(g)
	if _, err := ParseGrant(raw); err != nil {
		t.Fatal(err)
	}
	bad := strings.TrimSuffix(string(raw), "}") + `,"groupId":2}`
	if _, err := ParseGrant([]byte(bad)); err == nil {
		t.Fatal("duplicate signed groupId was accepted")
	}
}
