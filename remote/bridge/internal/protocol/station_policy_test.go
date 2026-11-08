package protocol

import (
	"encoding/json"
	"testing"
)

func TestStationPolicyJSONRejectsVersionConfusion(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"apiBases":[],"allowOfficialAccounts":false}`,
		`{"version":1,"apiBases":null,"allowAllAccounts":true}`,
		`{"version":2,"apiBases":[],"externalApiBases":["https://external.test"],"reverseProxyApiBases":["https://proxy.test"],"allowOfficialAccounts":false}`,
	} {
		var policy StationPolicy
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			t.Fatalf("supported policy rejected: %v", err)
		}
	}
	for _, raw := range []string{
		`{"version":1,"apiBases":[],"externalApiBases":[]}`,
		`{"version":1,"apiBases":[],"reverseProxyApiBases":[]}`,
		`{"version":2,"apiBases":[],"allowAllAccounts":false}`,
		`{"version":3,"apiBases":[]}`,
		`{"version":2,"version":1,"apiBases":[]}`,
		`{"version":2,"apiBases":[],"APIBases":["https://other.test"]}`,
		`{"version":2,"apiBases":[],"futureAllowance":true}`,
		`{"version":2,"apiBases":null}`,
		`{"version":2,"apiBases":[],"externalApiBases":null}`,
		`{"version":2,"apiBases":[],"allowOfficialAccounts":null}`,
		`{"version":2,"apiBases":[],"reverseProxyApiBases":"https://proxy.test"}`,
		`{"apiBases":[]}`,
		`null`,
		`{"version":2,"apiBases":[]} {}`,
	} {
		policy := StationPolicy{Version: 1, APIBases: []string{"https://original.test"}}
		if err := json.Unmarshal([]byte(raw), &policy); err == nil {
			t.Errorf("ambiguous policy accepted: %s", raw)
		}
		if policy.Version != 1 || len(policy.APIBases) != 1 || policy.APIBases[0] != "https://original.test" {
			t.Errorf("failed decode changed existing policy: %s", raw)
		}
	}
}
