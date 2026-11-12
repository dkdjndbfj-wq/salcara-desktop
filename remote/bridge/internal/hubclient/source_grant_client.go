package hubclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/protocol"
)

type signedSourceGrant struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}
type sourceIssuer struct {
	ID               string   `json:"id"`
	PublicKey        string   `json:"publicKey"`
	SourceTypes      []string `json:"sourceTypes"`
	AgentIDs         []string `json:"agentIds"`
	Origins          []string `json:"origins"`
	APIBases         []string `json:"apiBases"`
	AuthorizationURL string   `json:"authorizationUrl"`
	DeviceEnrollment bool     `json:"deviceEnrollment,omitempty"`
	EnrollmentURL    string   `json:"enrollmentUrl,omitempty"`
}
type sourceGrantChallenge struct {
	ChallengeID    string          `json:"challengeId"`
	Audience       string          `json:"audience"`
	PolicyRevision string          `json:"policyRevision"`
	DeviceID       string          `json:"deviceId"`
	BindingID      string          `json:"bindingId"`
	RequestID      string          `json:"requestId"`
	CommandHash    string          `json:"commandHash"`
	Command        json.RawMessage `json:"command"`
	ExpiresAt      int64           `json:"expiresAt"`
}
type sourceGrantSnapshot struct {
	Version          int    `json:"version"`
	Issuer           string `json:"issuer"`
	Audience         string `json:"audience"`
	DeviceID         string `json:"deviceId"`
	BindingID        string `json:"bindingId"`
	CredentialHandle string `json:"credentialHandle"`
	Agent            string `json:"agent"`
	ControlSurface   string `json:"controlSurface"`
	SourceType       string `json:"sourceType"`
	Origin           string `json:"origin"`
	APIBase          string `json:"apiBase"`
	ConfigRevision   string `json:"configRevision"`
	PolicyRevision   string `json:"policyRevision"`
	CommandHash      string `json:"commandHash"`
	RequestID        string `json:"requestId"`
	ChallengeID      string `json:"challengeId"`
	Model            string `json:"model,omitempty"`
	IssuedAt         int64  `json:"issuedAt"`
	ExpiresAt        int64  `json:"expiresAt"`
}

type sourceGrantPolicy struct {
	Station  *protocol.StationPolicy `json:"station"`
	Mode     string                  `json:"mode"`
	Revision string                  `json:"revision"`
	Audience string                  `json:"audience"`
	Issuers  []sourceIssuer          `json:"issuers"`
}

func parseSourceGrantChallengeResponse(raw []byte) (sourceGrantChallenge, sourceGrantPolicy, error) {
	var envelope struct {
		Challenge json.RawMessage `json:"challenge"`
		Policy    json.RawMessage `json:"policy"`
	}
	var challenge sourceGrantChallenge
	var policy sourceGrantPolicy
	if len(raw) > protocol.MaxSourcePolicyWireBytes || exactSourceJSON(raw, []string{"challenge", "policy"}, &envelope) != nil || exactSourceJSON(envelope.Challenge, []string{"challengeId", "audience", "policyRevision", "deviceId", "bindingId", "requestId", "commandHash", "command", "expiresAt"}, &challenge) != nil {
		return challenge, policy, errSourceAcquisition
	}
	var fields struct {
		Station  json.RawMessage   `json:"station"`
		Mode     string            `json:"mode"`
		Revision string            `json:"revision"`
		Audience string            `json:"audience"`
		Issuers  []json.RawMessage `json:"issuers"`
	}
	if exactSourceJSON(envelope.Policy, []string{"mode", "revision", "audience", "issuers", "station"}, &fields) != nil || len(fields.Issuers) > 32 {
		return challenge, policy, errSourceAcquisition
	}
	policy.Mode, policy.Revision, policy.Audience = fields.Mode, fields.Revision, fields.Audience
	if len(fields.Station) > 0 {
		var station protocol.StationPolicy
		if len(fields.Station) > protocol.MaxStationPolicyJSONBytes || exactSourceJSON(fields.Station, []string{"version", "apiBases", "externalApiBases", "reverseProxyApiBases", "allowOfficialAccounts"}, &station) != nil || !config.ValidStationPolicy(&station) {
			return challenge, policy, errSourceAcquisition
		}
		policy.Station = &station
	}
	for _, rawIssuer := range fields.Issuers {
		var issuer sourceIssuer
		if exactSourceJSON(rawIssuer, []string{"id", "publicKey", "sourceTypes", "agentIds", "origins", "apiBases", "authorizationUrl", "deviceEnrollment", "enrollmentUrl"}, &issuer) != nil || len(issuer.SourceTypes) > 3 || len(issuer.AgentIDs) > 32 || len(issuer.Origins) > 32 || len(issuer.APIBases) > 32 {
			return challenge, policy, errSourceAcquisition
		}
		policy.Issuers = append(policy.Issuers, issuer)
	}
	return challenge, policy, nil
}

var errSourceAcquisition = errors.New("当前来源未配置可信签发连接器；受限模式未发送任务")

// exactSourceJSON avoids duplicate, unknown, null and case-confusable signed
// members. Error text is constant: issuer responses may contain credentials.
func exactSourceJSON(raw []byte, names []string, target any) error {
	allowed := map[string]bool{}
	for _, name := range names {
		allowed[name] = true
	}
	seen := map[string]bool{}
	d := json.NewDecoder(bytes.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return errSourceAcquisition
	}
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return errSourceAcquisition
		}
		name, ok := token.(string)
		if !ok || !allowed[name] || seen[name] {
			return errSourceAcquisition
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errSourceAcquisition
		}
	}
	last, err := d.Token()
	if err != nil || last != json.Delim('}') || d.Decode(new(any)) != io.EOF || json.Unmarshal(raw, target) != nil {
		return errSourceAcquisition
	}
	return nil
}

func sourceContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func sourceIssuerURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || len(raw) > 4096 || u.String() != raw || strings.ToLower(u.Host) != u.Host || strings.HasSuffix(u.Hostname(), ".") || u.Port() == "443" || strings.Contains(u.Path, "//") || strings.ContainsAny(u.Path, "\\\r\n\x00") {
		return false
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func sourceCommandHash(raw json.RawMessage) (string, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var value any
	if d.Decode(&value) != nil || d.Decode(new(any)) != io.EOF {
		return "", errSourceAcquisition
	}
	b, err := json.Marshal(value)
	if err != nil {
		return "", errSourceAcquisition
	}
	hash := sha256.Sum256(b)
	return hex.EncodeToString(hash[:]), nil
}

// Hub trust anchors authorize public grant validation, not disclosure of an
// unrelated provider's credential. Cross-origin issuers need a separate local
// connector trust decision; the generic adapter has no such authority.
func sourceIssuerSameOrigin(raw, origin string) bool {
	if !sourceIssuerURL(raw) {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme+"://"+u.Host == origin
}

func (c *Client) acquireSourceGrant(ctx context.Context, challengeID string) (any, error) {
	guard, remote := commandGuardFromContext(ctx)
	if !remote || c.o.Store == nil {
		return nil, errSourceAcquisition
	}
	cfg := c.o.Store.Get()
	return c.acquireSourceGrantFor(ctx, challengeID, cfg, func(current config.Config) bool {
		return current.DeviceID == guard.deviceID && config.RemoteIdentity(current) == guard.identity && config.RemoteIdentity(current) == config.RemoteIdentity(cfg) && c.commandPhoneMatches(current, guard.bindingID, guard.phoneHash, guard.eventScope) && c.canUpload(current) && c.commandEventScopeValid(current, guard.eventScope)
	}, false)
}

// station is a trusted saved connection, never a phone-supplied URL. currentness
// binds its credentials to the active owner and is checked under taskConfigMu
// before and after network I/O. Standby additionally permits only a challenge
// whose original command hands control to that exact saved station.
func (c *Client) acquireSourceGrantFor(ctx context.Context, challengeID string, station config.Config, currentness func(config.Config) bool, standby bool) (any, error) {
	if c.o.Store == nil || currentness == nil || !strings.HasPrefix(challengeID, "challenge-") || len(challengeID) > 128 {
		return nil, errSourceAcquisition
	}
	if !c.sourceGrantMu.TryLock() {
		return nil, errSourceAcquisition
	}
	defer c.sourceGrantMu.Unlock()
	c.taskConfigMu.Lock()
	c.expireUpdateLocked()
	if c.updatePrepared || !currentness(c.o.Store.Get()) {
		c.taskConfigMu.Unlock()
		return nil, errSourceAcquisition
	}
	cfg := station
	c.taskConfigMu.Unlock()
	if !cfg.RemoteExecutionArmed {
		return nil, errSourceAcquisition
	}
	challengeCtx, challengeCancel := context.WithTimeout(ctx, hubAuthenticatedRequestTimeout)
	defer challengeCancel()
	req, err := c.requestFor(challengeCtx, cfg, "GET", "/bridge/source/challenge?challengeId="+url.QueryEscape(challengeID), nil, false)
	if err != nil {
		return nil, errSourceAcquisition
	}
	// This endpoint always requires an independent device proof, also on legacy Hubs.
	req.Header.Set("X-Salcara-Device-Id", cfg.DeviceID)
	resp, err := c.o.HTTP.Do(req)
	if err != nil {
		return nil, errSourceAcquisition
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errSourceAcquisition
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxSourcePolicyWireBytes+1))
	if err != nil || len(raw) > protocol.MaxSourcePolicyWireBytes {
		return nil, errSourceAcquisition
	}
	_ = resp.Body.Close()
	challengeCancel()
	challenge, policy, err := parseSourceGrantChallengeResponse(raw)
	if err != nil {
		return nil, errSourceAcquisition
	}
	expectedBinding := cfg.PhoneBindingID
	if cfg.EffectiveRemoteMode() == "bot" {
		group, _, _ := cfg.BotEnrollment()
		scope := c.eventScopeFor(cfg)
		if group == "" || scope == "" || standby {
			return nil, errSourceAcquisition
		}
		binding := sha256.Sum256([]byte("salcara-bot-source-v1\x00" + group + "\x00" + scope))
		expectedBinding = hex.EncodeToString(binding[:])
	}
	if !config.ValidStationPolicy(policy.Station) || policy.Mode != "restricted" || challenge.ChallengeID != challengeID || challenge.DeviceID != cfg.DeviceID || challenge.BindingID != expectedBinding || challenge.PolicyRevision != policy.Revision || challenge.Audience == "" || challenge.Audience != policy.Audience || challenge.ExpiresAt <= time.Now().Unix() {
		return nil, errSourceAcquisition
	}
	hash, err := sourceCommandHash(challenge.Command)
	if err != nil || hash != challenge.CommandHash {
		return nil, errSourceAcquisition
	}
	var command map[string]any
	if json.Unmarshal(challenge.Command, &command) != nil || !sourceGuardExecutionType(str(command, "type")) {
		return nil, errSourceAcquisition
	}
	if _, valid := stationSourceOnly(command); str(command, "type") == "remote.station.switch" && !valid {
		return nil, errSourceAcquisition
	}
	if standby && (str(command, "type") != "remote.station.switch" || config.NormalizeHubURL(str(command, "targetHubUrl")) != config.NormalizeHubURL(cfg.EffectiveHubURL()) || str(command, "targetDeviceId") != cfg.DeviceID || cfg.ComputerID == "" || str(command, "targetComputerId") != cfg.ComputerID || !stationSwitchOperationID(str(command, "operationId"))) {
		return nil, errSourceAcquisition
	}
	agent, surface, handle := commandAgent(command), commandSurface(str(command, "type"), command), strings.TrimSpace(str(command, "accountId"))
	// Native process routes still need actual provider attestation. Managed CLI
	// accounts use either the protected official connector or a scoped issuer;
	// no local source label or login file is sufficient evidence on its own.
	if agent == "" || surface != "cli" || handle == "" {
		return nil, errSourceAcquisition
	}
	resolveCfg := cfg
	if str(command, "type") == "remote.station.switch" {
		target, ok := cfg.SavedRemoteConnection(str(command, "targetHubUrl"), str(command, "targetDeviceId"))
		if !ok {
			return nil, errSourceAcquisition
		}
		resolveCfg = activeStationConfig(cfg, target)
	}
	accountID, ok := AccountForHandle(resolveCfg, handle)
	if !ok {
		return nil, errSourceAcquisition
	}
	account, ok := resolveCfg.LocalAccount(accountID)
	official := config.IsProtectedOfficialAccount(account)
	sourceType := ""
	switch account.SourceType {
	case "api_key":
		sourceType = "site_api"
	case "reverse_proxy":
		sourceType = "verified_proxy"
	}
	if !ok || !official && (sourceType == "" || account.Key == "" || account.CredentialRef != "") || APIHandle(resolveCfg, account.ID) != handle || !resolveCfg.RemoteHubAllowsAccount(resolveCfg.EffectiveHubURL(), account) || !config.StationAllowsAccount(policy.Station, account) {
		return nil, errSourceAcquisition
	}
	base, err := config.NormalizeStationAPIBase(account.BaseURL)
	if err != nil {
		return nil, errSourceAcquisition
	}
	origin := accountOrigin(account)
	var issuer sourceIssuer
	matches := 0
	for _, candidate := range policy.Issuers {
		if sourceIssuerSameOrigin(candidate.AuthorizationURL, origin) && sourceContains(candidate.SourceTypes, sourceType) && (len(candidate.AgentIDs) == 0 || sourceContains(candidate.AgentIDs, agent)) && sourceContains(candidate.Origins, origin) && sourceContains(candidate.APIBases, base) {
			issuer = candidate
			matches++
		}
	}
	// Ambiguous issuers require an operator to choose one; the phone cannot do so.
	if !official && matches != 1 {
		return nil, errSourceAcquisition
	}
	snapshot := sourceGrantSnapshot{Version: 1, Issuer: issuer.ID, Audience: challenge.Audience, DeviceID: challenge.DeviceID, BindingID: challenge.BindingID, CredentialHandle: handle, Agent: agent, ControlSurface: surface, SourceType: sourceType, Origin: origin, APIBase: base, ConfigRevision: resolveCfg.AccountSourceRevision(account), PolicyRevision: challenge.PolicyRevision, CommandHash: hash, RequestID: challenge.RequestID, ChallengeID: challengeID, Model: strings.TrimSpace(str(command, "model"))}
	if official {
		// These are the two production managed gateway routes. Claude Code's
		// Messages requests translate into the same selected official Responses
		// executor; an extension label does not establish an execution route.
		if agent != "codex" && agent != "claude" {
			return nil, errSourceAcquisition
		}
		snapshot.Issuer, snapshot.SourceType = "station-official", "official_account_proxy"
	}
	// Recheck after the challenge fetch, before sending a provider credential.
	// The same check after issuance prevents returning a stale grant.
	sourceCurrent := func() bool {
		current := c.o.Store.Get()
		if c.updatePrepared || !current.RemoteExecutionArmed || challenge.ExpiresAt <= time.Now().Unix() || !currentness(current) {
			return false
		}
		if standby {
			saved, ok := current.SavedRemoteConnection(cfg.EffectiveHubURL(), cfg.DeviceID)
			if !ok {
				return false
			}
			current = activeStationConfig(current, saved)
		}
		if str(command, "type") == "remote.station.switch" {
			target, ok := current.SavedRemoteConnection(str(command, "targetHubUrl"), str(command, "targetDeviceId"))
			if !ok {
				return false
			}
			current = activeStationConfig(current, target)
		}
		latest, ok := current.LocalAccount(account.ID)
		return ok && config.RemoteIdentity(current) == config.RemoteIdentity(resolveCfg) && APIHandle(current, latest.ID) == handle && current.AccountSourceRevision(latest) == snapshot.ConfigRevision
	}
	c.taskConfigMu.Lock()
	c.expireUpdateLocked()
	valid := sourceCurrent()
	c.taskConfigMu.Unlock()
	if !valid {
		return nil, errSourceAcquisition
	}
	if official {
		// Only the authenticated reply to this nonce can carry this attestation.
		// Hub signs it after rechecking the station policy and physical pairing;
		// neither access/refresh tokens nor a login record leave this computer.
		return map[string]any{"officialAccount": snapshot}, nil
	}
	// Credentials appear only in this direct HTTPS body; it is never returned,
	// logged, saved in a receipt, or routed through the Hub.
	grant, err := requestSourceIssuer(ctx, issuer, snapshot, account.Key, nil)
	if err != nil {
		return nil, errSourceAcquisition
	}
	c.taskConfigMu.Lock()
	defer c.taskConfigMu.Unlock()
	c.expireUpdateLocked()
	if !sourceCurrent() {
		return nil, errSourceAcquisition
	}
	return grant, nil
}

func requestSourceIssuer(ctx context.Context, issuer sourceIssuer, expected sourceGrantSnapshot, apiKey string, transport http.RoundTripper) (signedSourceGrant, error) {
	var grant signedSourceGrant
	if !sourceIssuerSameOrigin(issuer.AuthorizationURL, expected.Origin) {
		return grant, errSourceAcquisition
	}
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	body, err := json.Marshal(struct {
		Version    int                 `json:"version"`
		Snapshot   sourceGrantSnapshot `json:"snapshot"`
		Credential struct {
			Type  string `json:"type"`
			Value string `json:"value"`
		} `json:"credential"`
	}{Version: 1, Snapshot: expected, Credential: struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}{Type: "api_key", Value: apiKey}})
	if err != nil {
		return grant, errSourceAcquisition
	}
	req, err := http.NewRequestWithContext(ctx, "POST", issuer.AuthorizationURL, bytes.NewReader(body))
	if err != nil {
		return grant, errSourceAcquisition
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return grant, errSourceAcquisition
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return grant, errSourceAcquisition
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, protocol.MaxSignedSourceGrantBytes+1))
	if err != nil || len(raw) > protocol.MaxSignedSourceGrantBytes || exactSourceJSON(raw, []string{"payload", "signature"}, &grant) != nil || len(grant.Payload) > protocol.MaxSourcePayloadBytes || len(grant.Signature) > 128 {
		return signedSourceGrant{}, errSourceAcquisition
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(grant.Payload)
	if err != nil || len(payload) > protocol.MaxSourceSnapshotBytes {
		return signedSourceGrant{}, errSourceAcquisition
	}
	sig, err := base64.RawURLEncoding.Strict().DecodeString(grant.Signature)
	if err != nil {
		return signedSourceGrant{}, errSourceAcquisition
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(issuer.PublicKey)
	if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(ed25519.PublicKey(key), payload, sig) {
		return signedSourceGrant{}, errSourceAcquisition
	}
	var actual sourceGrantSnapshot
	fields := []string{"version", "issuer", "audience", "deviceId", "bindingId", "credentialHandle", "agent", "controlSurface", "sourceType", "origin", "apiBase", "configRevision", "policyRevision", "commandHash", "requestId", "challengeId", "model", "issuedAt", "expiresAt"}
	if exactSourceJSON(payload, fields, &actual) != nil {
		return signedSourceGrant{}, errSourceAcquisition
	}
	now := time.Now().Unix()
	if actual.IssuedAt > now+30 || actual.IssuedAt < now-330 || actual.ExpiresAt <= now || actual.ExpiresAt <= actual.IssuedAt || actual.ExpiresAt-actual.IssuedAt > 300 {
		return signedSourceGrant{}, errSourceAcquisition
	}
	actual.IssuedAt, actual.ExpiresAt = 0, 0
	if actual != expected {
		return signedSourceGrant{}, errSourceAcquisition
	}
	return grant, nil
}
