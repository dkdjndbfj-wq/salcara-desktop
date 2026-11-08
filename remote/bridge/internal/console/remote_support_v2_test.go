package console

import (
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

func TestStationV2RemoteSupportIncludesExternalAndProxyScopes(t *testing.T) {
	for _, sourceType := range []string{"api_key", "reverse_proxy"} {
		policy := &protocol.StationPolicy{Version: 2, APIBases: []string{}}
		if sourceType == "api_key" {
			policy.ExternalAPIBases = []string{"https://source.test"}
		} else {
			policy.ReverseProxyAPIBases = []string{"https://source.test"}
		}
		observation := &config.StationObservation{Mode: "open", Revision: "r2", Station: policy, CheckedAt: time.Now().UnixMilli()}
		if got := stationRemoteSupport(observation, nil, "https://hub.test"); got.APIRemote != "supported" || got.AccountRemote != "unsupported" {
			t.Fatalf("missing aggregate support: %+v", got)
		}
		account := &config.LocalAccount{SourceType: sourceType, BaseURL: "https://source.test/v1"}
		if got := stationRemoteSupport(observation, account, "https://hub.test"); got.APIRemote != "supported" {
			t.Fatal("permitted account not shown")
		}
		if sourceType == "api_key" {
			account.SourceType = "reverse_proxy"
		} else {
			account.SourceType = "api_key"
		}
		if got := stationRemoteSupport(observation, account, "https://hub.test"); got.APIRemote != "unsupported" {
			t.Fatal("other source class falsely advertised")
		}
	}
}
