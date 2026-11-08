package console

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"

	"salcara/bridge/internal/config"
)

// apiClientRevision is an opaque, process-bound confirmation token. Neither
// the client's authentication epoch nor its key digest is a public revision.
// Include the complete policy and Store-owned epochs so key rotation, policy
// replacement and a source changing away and back retire the earlier preview.
func apiClientRevision(c config.Config, client config.NamedAPIClient) string {
	if c.GatewayRuntimeKey == "" || client.AuthEpoch == "" {
		return ""
	}
	raw, err := json.Marshal(struct {
		Domain      string
		Client      config.NamedAPIClient
		AuthEpoch   string
		SourceEpoch string
	}{"salcara-api-member-client-v1", client, client.AuthEpoch, c.UsageSourceRevision})
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(c.GatewayRuntimeKey))
	_, _ = mac.Write(raw)
	return "api-client-v1-" + hex.EncodeToString(mac.Sum(nil))
}

// Both omitted is the legacy contract. Supplying either opts into both
// preconditions; a partial/empty pair must never fall back to legacy writes.
func checkAPIMemberRevisions(c config.Config, account config.LocalAccount, client config.NamedAPIClient, source, expectedClient *string) error {
	if source == nil && expectedClient == nil {
		return nil
	}
	if source == nil || expectedClient == nil || checkLocalSourceRevision(c, account, source) != nil {
		return errCodexAPIMemberChanged
	}
	actual := apiClientRevision(c, client)
	if *expectedClient == "" || actual == "" || subtle.ConstantTimeCompare([]byte(actual), []byte(*expectedClient)) != 1 {
		return errCodexAPIMemberChanged
	}
	return nil
}
