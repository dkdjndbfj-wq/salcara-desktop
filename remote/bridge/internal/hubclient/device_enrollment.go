package hubclient

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/enrollment"
	"salcara/bridge/internal/hubingress"
	"salcara/bridge/internal/protocol"
)

// The public declaration grants no authority. A device can be admitted only
// after the fixed API-origin issuer verifies the signed Hub challenge itself.
type DeviceEnrollmentPolicy struct {
	Version           int            `json:"version"`
	HubInstanceID     string         `json:"hubInstanceId"`
	IdentityPublicKey string         `json:"identityPublicKey"`
	Audience          string         `json:"audience"`
	PolicyRevision    string         `json:"policyRevision"`
	Issuers           []sourceIssuer `json:"issuers"`
}

func enrollmentError(status int, code string) error {
	return &RemoteHTTPError{StatusCode: status, Code: code}
}

func enrollmentIssuer(info Discovery, account config.LocalAccount) (sourceIssuer, string, error) {
	p := info.DeviceEnrollment
	base, err := config.NormalizeStationAPIBase(account.BaseURL)
	origin := accountOrigin(account)
	if !info.HasCapability("device.enrollment.api.v1") || p == nil {
		return sourceIssuer{}, "", enrollmentError(409, "api_enrollment_unsupported")
	}
	if err != nil || account.SourceType != "api_key" || account.CredentialRef != "" || account.Key == "" || !sourceIssuerSameOrigin(info.HubURL, origin) || info.Identity == nil || p.Version != 1 || p.HubInstanceID != info.Identity.HubInstanceID || p.IdentityPublicKey != info.Identity.IdentityPublicKey || !enrollment.Identifier(p.Audience) || !enrollment.Identifier(p.PolicyRevision) || len(p.Issuers) > 32 {
		return sourceIssuer{}, "", enrollmentError(403, "enrollment_issuer_untrusted")
	}
	var issuer sourceIssuer
	matches := 0
	for _, candidate := range p.Issuers {
		key, keyErr := base64.RawURLEncoding.Strict().DecodeString(candidate.PublicKey)
		if candidate.DeviceEnrollment && enrollment.Identifier(candidate.ID) && keyErr == nil && len(key) == ed25519.PublicKeySize && sourceIssuerSameOrigin(candidate.EnrollmentURL, origin) && sourceContains(candidate.Origins, origin) && sourceContains(candidate.APIBases, base) && sourceContains(candidate.SourceTypes, "site_api") {
			issuer, matches = candidate, matches+1
		}
	}
	if matches != 1 {
		return sourceIssuer{}, "", enrollmentError(403, "enrollment_issuer_untrusted")
	}
	return issuer, base, nil
}

func CheckDeviceEnrollment(info Discovery, account config.LocalAccount) error {
	_, _, err := enrollmentIssuer(info, account)
	return err
}

func enrollmentHTTP(ctx context.Context, info Discovery, method, path string, body any) (*http.Request, *http.Client, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, info.HubURL+"/v1"+path, bytes.NewReader(raw))
	if err != nil {
		return nil, nil, enrollmentError(400, "invalid_request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req = hubingress.Bind(req, info.HubURL+"/v1")
	if info.Identity != nil {
		req = hubingress.BindIdentity(req, info.Identity.HubInstanceID, info.Identity.IdentityPublicKey, info.HubURL+"/v1")
	}
	return req, &http.Client{Transport: hubingress.New(nil, ""), Timeout: hubAuthenticatedRequestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

// AcquireDeviceEnrollmentGrant keeps the API key on its exact HTTPS issuer
// origin. current is checked at every boundary and no proof is persisted.
func AcquireDeviceEnrollmentGrant(ctx context.Context, info Discovery, account config.LocalAccount, deviceID, secret, requestID string, current func() error) (string, error) {
	issuer, base, err := enrollmentIssuer(info, account)
	if err != nil {
		return "", err
	}
	if current == nil || !enrollment.RequestID(requestID) || deviceID == "" || secret == "" {
		return "", enrollmentError(400, "invalid_request")
	}
	if err = current(); err != nil {
		return "", err
	}
	hash := sha256.Sum256([]byte(secret))
	expected := enrollment.Challenge{Purpose: enrollment.ChallengePurpose, Version: 1, Issuer: issuer.ID, HubInstanceID: info.Identity.HubInstanceID, Audience: info.DeviceEnrollment.Audience, APIBase: base, PolicyRevision: info.DeviceEnrollment.PolicyRevision, DeviceID: deviceID, DeviceSecretHash: hex.EncodeToString(hash[:]), RequestID: requestID}
	request := map[string]any{"version": 1, "issuer": issuer.ID, "apiBase": base, "deviceId": deviceID, "deviceSecretHash": expected.DeviceSecretHash, "requestId": requestID}
	req, client, err := enrollmentHTTP(ctx, info, http.MethodPost, "/device/enrollment/challenge", request)
	if err != nil {
		return "", err
	}
	challengeEnvelope, err := readEnrollmentEnvelope(client, req)
	if err != nil {
		return "", err
	}
	hubKey, _ := base64.StdEncoding.Strict().DecodeString(info.Identity.IdentityPublicKey)
	raw, err := enrollment.VerifyEnvelope(challengeEnvelope, enrollment.ChallengePurpose, hubKey)
	if err != nil {
		return "", enrollmentError(403, "enrollment_grant_invalid")
	}
	challenge, err := enrollment.ParseChallenge(raw)
	if err != nil || !enrollment.ValidTime(challenge.IssuedAt, challenge.ExpiresAt, time.Now().Unix()) {
		return "", enrollmentError(403, "enrollment_challenge_expired")
	}
	expected.ChallengeID, expected.IssuedAt, expected.ExpiresAt = challenge.ChallengeID, challenge.IssuedAt, challenge.ExpiresAt
	if challenge != expected {
		return "", enrollmentError(403, "enrollment_grant_invalid")
	}
	if err = current(); err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{"version": 1, "challenge": challengeEnvelope, "credential": map[string]string{"type": "api_key", "value": account.Key}})
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, issuer.EnrollmentURL, bytes.NewReader(body))
	if err != nil {
		return "", enrollmentError(400, "invalid_request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Direct transport deliberately has no Hub routing or authentication state.
	client = &http.Client{Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	grantEnvelope, err := readEnrollmentEnvelope(client, req)
	if err != nil {
		return "", err
	}
	issuerKey, _ := base64.RawURLEncoding.Strict().DecodeString(issuer.PublicKey)
	raw, err = enrollment.VerifyEnvelope(grantEnvelope, enrollment.GrantPurpose, issuerKey)
	if err != nil {
		return "", enrollmentError(403, "enrollment_grant_invalid")
	}
	grant, err := enrollment.ParseGrant(raw)
	if err != nil || !enrollment.ValidTime(grant.IssuedAt, grant.ExpiresAt, time.Now().Unix()) || grant.ExpiresAt > challenge.ExpiresAt || grant.IssuedAt < challenge.IssuedAt {
		return "", enrollmentError(403, "enrollment_grant_invalid")
	}
	actual := grant.Challenge
	actual.Purpose, actual.IssuedAt, actual.ExpiresAt = challenge.Purpose, challenge.IssuedAt, challenge.ExpiresAt
	if actual != challenge {
		return "", enrollmentError(403, "enrollment_grant_invalid")
	}
	if err = current(); err != nil {
		return "", err
	}
	wire, _ := json.Marshal(grantEnvelope)
	header := base64.RawURLEncoding.EncodeToString(wire)
	if len(header) > enrollment.MaxHeaderBytes {
		return "", enrollmentError(403, "enrollment_grant_invalid")
	}
	return header, nil
}

func readEnrollmentEnvelope(client *http.Client, req *http.Request) (enrollment.Envelope, error) {
	resp, err := client.Do(req)
	if err != nil {
		return enrollment.Envelope{}, enrollmentError(503, "api_verifier_unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return enrollment.Envelope{}, checkResp(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, enrollment.MaxEnvelopeBytes+1))
	if err != nil {
		return enrollment.Envelope{}, enrollmentError(503, "api_verifier_unavailable")
	}
	value, err := enrollment.ParseEnvelope(raw)
	if err != nil {
		return enrollment.Envelope{}, enrollmentError(403, "enrollment_grant_invalid")
	}
	return value, nil
}

// RegisterRemoteDevice sends a single mutation. A transport error leaves the
// outcome unknown; callers must check these same credentials before any retry.
func RegisterRemoteDevice(ctx context.Context, info Discovery, device protocol.Device, secret, grant string) error {
	if ctx.Err() != nil {
		return enrollmentError(409, "enrollment_not_sent")
	}
	if device.DeviceID == "" || secret == "" || len(grant) > enrollment.MaxHeaderBytes {
		return enrollmentError(400, "invalid_request")
	}
	req, client, err := enrollmentHTTP(ctx, info, http.MethodPost, "/device/register", device)
	if err != nil {
		return err
	}
	req.Header.Set("X-Salcara-Device-Id", device.DeviceID)
	req.Header.Set("X-Salcara-Device-Secret", secret)
	if grant != "" {
		req.Header.Set("X-Salcara-Enrollment-Grant", grant)
	}
	resp, err := client.Do(req)
	if err != nil {
		if hubingress.RequestWasNotSent(err) {
			return enrollmentError(409, "enrollment_not_sent")
		}
		return enrollmentError(409, "enrollment_result_unknown")
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return checkResp(resp)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (256<<10)+1))
	var ack struct {
		OK     bool `json:"ok"`
		Device struct {
			DeviceID string `json:"deviceId"`
		} `json:"device"`
	}
	if err != nil || len(raw) > 256<<10 || json.Unmarshal(raw, &ack) != nil || !ack.OK || ack.Device.DeviceID != device.DeviceID {
		return enrollmentError(409, "enrollment_result_unknown")
	}
	return nil
}
