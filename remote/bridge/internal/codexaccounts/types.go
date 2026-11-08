// Package codexaccounts owns Codex account credentials. UI-facing types deliberately
// contain no tokens. SDKStore is a trusted backend adapter, never an HTTP response.
package codexaccounts

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	BundleVersion       = 1
	MaxImportBytes      = 1024 * 1024
	MaxAccounts         = 64
	maxTokenBytes       = 64 * 1024
	StatusReady         = "ready"
	StatusExpired       = "expired"
	StatusNeedsLogin    = "needs_login"
	StatusRefreshFailed = "refresh_failed"
	StatusSaveFailed    = "save_failed"
	LoginModeBrowser    = "browser"
	LoginModeDevice     = "device"
)

var (
	ErrInvalidImport           = errors.New("invalid Codex credential import")
	ErrNotFound                = errors.New("Codex account not found")
	ErrConflict                = errors.New("Codex account changed during operation")
	ErrSaveFailed              = errors.New("Codex credentials could not be saved")
	ErrRefreshFailed           = errors.New("Codex credential refresh failed")
	ErrNeedsLogin              = errors.New("Codex account requires login")
	ErrClosed                  = errors.New("Codex account service closed")
	ErrLoginFailed             = errors.New("Codex login failed")
	ErrUsageUnavailable        = errors.New("Codex account usage unavailable")
	ErrSubscriptionUnavailable = errors.New("Codex account subscription unavailable")
	ErrResetUnavailable        = errors.New("Codex quota reset unavailable")
	// ErrVaultBusy means another Bridge process currently owns the credential
	// vault. A second process must fail closed instead of racing token rotation.
	ErrVaultBusy    = errors.New("Codex account vault is already in use")
	ErrResetUnknown = errors.New("Codex quota reset result unknown")
)

// Account is safe to serialize to a browser. Metadata and JWT claims are display
// hints only; none of these fields establishes authentication or entitlement.
type Account struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	Email              string     `json:"email,omitempty"`
	NameCustomized     bool       `json:"nameCustomized"`
	Provider           string     `json:"provider"`
	Mode               string     `json:"mode"`
	Plan               string     `json:"plan,omitempty"`
	ExpiresAt          *time.Time `json:"expiresAt,omitempty"`
	Status             string     `json:"status"`
	Epoch              uint64     `json:"epoch"`
	IdentityEpoch      uint64     `json:"identityEpoch"`
	MetadataUnverified bool       `json:"metadataUnverified"`
	// Usage is populated only from the provider's authenticated usage endpoint.
	// Plan remains the unverified import/JWT hint above for compatibility.
	Usage UsageSnapshot `json:"usage"`
	// Subscription is display metadata only. JWT/import hints are never treated
	// as verified entitlement or routing authority.
	Subscription SubscriptionMetadata `json:"subscription"`
	Cleanup      *CleanupStatus       `json:"cleanup,omitempty"`
}

// ResetCreditRequest is a safe, durable redemption handle. It never contains
// provider credentials. Unknown means the network outcome was ambiguous and
// must be resolved by refreshing usage rather than blindly replaying the POST.
type ResetCreditRequest struct {
	RedeemRequestID string     `json:"redeemRequestId"`
	IdentityEpoch   uint64     `json:"identityEpoch"`
	State           string     `json:"state"`
	CreatedAt       time.Time  `json:"createdAt"`
	CompletedAt     *time.Time `json:"completedAt,omitempty"`
}

type ImportPreview struct {
	Version  int       `json:"version"`
	Source   string    `json:"source"`
	Accounts []Account `json:"accounts"`
}

// PreparedImport is opaque: credentials cannot be serialized through this type.
type PreparedImport struct {
	records []credential
	source  string
}

// Protector is the boundary for at-rest protection. A nil option selects DPAPI
// on Windows and owner-only filesystem protection on other operating systems.
type Protector interface {
	Protect([]byte) ([]byte, error)
	Unprotect([]byte) ([]byte, error)
}

// Refresher and LoginConnector are trusted backend interfaces. Their SDK records
// contain secrets and must never be serialized or logged by callers.
type Refresher interface {
	Refresh(context.Context, *coreauth.Auth) (*coreauth.Auth, error)
}
type RefreshFunc func(context.Context, *coreauth.Auth) (*coreauth.Auth, error)

func (f RefreshFunc) Refresh(ctx context.Context, a *coreauth.Auth) (*coreauth.Auth, error) {
	return f(ctx, a)
}

type LoginConnector interface {
	Login(context.Context) (*coreauth.Auth, error)
}

// ProgressLoginConnector can report a user-visible device challenge without
// placing provider tokens or raw response bodies in the browser contract.
type ProgressLoginConnector interface {
	LoginWithProgress(context.Context, func(LoginProgress)) (*coreauth.Auth, error)
}

// SessionProgressLoginConnector binds an out-of-band callback to the Service's
// opaque login session. SubmitCallback accepts a complete callback URL only;
// implementations must validate it against that active session without issuing
// a request to the URL.
type SessionProgressLoginConnector interface {
	LoginWithSessionProgress(context.Context, string, func(LoginProgress)) (*coreauth.Auth, error)
	SubmitCallback(string, string) error
}
type LoginProgress struct {
	AuthorizationURL string     `json:"authorizationURL,omitempty"`
	VerificationURL  string     `json:"verificationURL,omitempty"`
	UserCode         string     `json:"userCode,omitempty"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
}
type LoginFunc func(context.Context) (*coreauth.Auth, error)

func (f LoginFunc) Login(ctx context.Context) (*coreauth.Auth, error) { return f(ctx) }

type Options struct {
	Path                 string
	Protector            Protector
	Refresher            Refresher
	Login                LoginConnector
	DeviceLogin          LoginConnector
	UsageFetcher         UsageFetcher
	SubscriptionFetcher  SubscriptionFetcher
	ResetCreditConsumer  ResetCreditConsumer
	Now                  func() time.Time
	RefreshTimeout       time.Duration
	UsageTimeout         time.Duration
	UsageFreshFor        time.Duration
	SubscriptionTimeout  time.Duration
	SubscriptionFreshFor time.Duration
	ResetCreditTimeout   time.Duration
	// ExclusiveWriter enables an OS-held lock for production vaults. Callers
	// exposing account mutation across process boundaries should enable it.
	ExclusiveWriter bool
}

type LoginRequest struct {
	ReplaceID string `json:"replaceId,omitempty"`
	Name      string `json:"name,omitempty"`
	Mode      string `json:"mode,omitempty"`
}
type LoginSession struct {
	ID        string   `json:"id"`
	Mode      string   `json:"mode"`
	Status    string   `json:"status"`
	Account   *Account `json:"account,omitempty"`
	Error     string   `json:"error,omitempty"`
	ErrorCode string   `json:"errorCode,omitempty"`
	LoginProgress
}

type credential struct {
	ID                      string    `json:"id"`
	Epoch                   uint64    `json:"epoch"`
	IdentityEpoch           uint64    `json:"identity_epoch"`
	Name                    string    `json:"name"`
	Email                   string    `json:"email,omitempty"`
	NameMode                string    `json:"name_mode,omitempty"`
	NameRevision            uint64    `json:"name_revision,omitempty"`
	Plan                    string    `json:"plan,omitempty"`
	AccountID               string    `json:"account_id,omitempty"`
	AccessToken             string    `json:"access_token"`
	RefreshToken            string    `json:"refresh_token,omitempty"`
	IDToken                 string    `json:"id_token,omitempty"`
	ExpiresAt               time.Time `json:"expires_at,omitempty"`
	Status                  string    `json:"status,omitempty"`
	SubscriptionActiveUntil time.Time `json:"subscription_active_until,omitempty"`
	SubscriptionObservedAt  time.Time `json:"subscription_observed_at,omitempty"`
	SubscriptionSource      string    `json:"subscription_source,omitempty"`
	// Web sessions can be inspected for quota but are never projected into the
	// Codex API gateway or refreshed with the Codex CLI OAuth client.
	TokenSourceMode string `json:"token_source_mode,omitempty"`
}

func (c credential) account(now time.Time) Account {
	c = migrateAccountName(c)
	status := c.Status
	if status == "" || status == StatusReady {
		status = StatusReady
		if c.AccessToken == "" || !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt) {
			status = StatusExpired
		}
	}
	name, email, plan := c.Name, c.Email, c.Plan
	for _, secret := range []string{c.AccessToken, c.RefreshToken, c.IDToken} {
		if secret != "" {
			if strings.Contains(name, secret) {
				name = "Codex account"
			}
			if strings.Contains(plan, secret) {
				plan = ""
			}
			if strings.Contains(email, secret) {
				email = ""
			}
		}
	}
	a := Account{ID: c.ID, Name: name, Email: email, NameCustomized: c.NameMode == nameModeCustom, Provider: "codex", Mode: "legacy_codex_cli", Plan: plan, Status: status, Epoch: c.Epoch, IdentityEpoch: c.IdentityEpoch, MetadataUnverified: true, Usage: unverifiedUsage(plan), Subscription: subscriptionMetadata(c, now)}
	if c.TokenSourceMode == "chatgpt_web_session" {
		a.Mode = c.TokenSourceMode
	}
	// Refresh-only imports use an internal past expiry to force the first owned
	// refresh. Do not expose that sentinel as a real credential-expiry date.
	if c.AccessToken != "" && !c.ExpiresAt.IsZero() {
		expiry := c.ExpiresAt
		a.ExpiresAt = &expiry
	}
	return a
}

func (c credential) sdk() *coreauth.Auth {
	c = migrateAccountName(c)
	meta := map[string]any{"type": "codex", "access_token": c.AccessToken, "refresh_token": c.RefreshToken, "id_token": c.IDToken, "account_id": c.AccountID, "email": c.Email}
	if !c.ExpiresAt.IsZero() {
		meta["expired"] = c.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return &coreauth.Auth{ID: c.ID, Provider: "codex", Label: c.Name, RegistrationEpoch: c.Epoch, Generation: c.Epoch, Metadata: meta, Attributes: map[string]string{"plan_type": c.Plan, "salcara_identity_epoch": strconv.FormatUint(c.IdentityEpoch, 10)}}
}
