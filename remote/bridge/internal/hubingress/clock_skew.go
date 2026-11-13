package hubingress

import "errors"

// ClockSkewDetails is diagnostic evidence, not a trusted time-setting source.
// A signed manifest may be old, so only a future manifest can establish this
// discrepancy. A proof additionally binds a freshly generated random nonce.
type ClockSkewDetails struct {
	Direction  string `json:"direction"`
	OffsetMS   int64  `json:"offsetMs"` // signed time minus local observation
	LocalTime  int64  `json:"localTime"`
	SignedTime int64  `json:"signedTime"`
	Source     string `json:"source"`
}

type ClockSkewError struct {
	Details ClockSkewDetails
	id, key string // authenticated identity; never exposed in diagnostics
}

func (e *ClockSkewError) Error() string {
	return "电脑与中转站时间不一致，无法安全验证连接；请检查系统日期与时间并同步后重试，无需重新登记或更换 API"
}

func newClockSkewError(source string, local, signed int64, identity Manifest) error {
	if !validClockTime(local) || !validClockTime(signed) {
		return errors.New("ingress_identity_mismatch")
	}
	direction := "local_behind"
	if signed < local {
		direction = "local_ahead"
	}
	return &ClockSkewError{Details: ClockSkewDetails{Direction: direction, OffsetMS: signed - local, LocalTime: local, SignedTime: signed, Source: source}, id: identity.HubInstanceID, key: identity.IdentityPublicKey}
}

// Bound diagnostic arithmetic and browser timestamps to the supported calendar.
func validClockTime(value int64) bool { return value > 0 && value <= 253402300799999 }

// ClockSkewMatchesIdentity lets initial URL discovery retain an existing pin
// before exposing a diagnostic from a self-described signed manifest.
func ClockSkewMatchesIdentity(err error, id, key string) bool {
	var clock *ClockSkewError
	return errors.As(err, &clock) && clock.id == id && clock.key == key
}

// ClockSkew returns a detached copy, including through net/http URL wrappers.
// Unauthenticated HTTP error codes and HTTP Date headers never create it.
func ClockSkew(err error) *ClockSkewDetails {
	var clock *ClockSkewError
	if !errors.As(err, &clock) {
		return nil
	}
	details := clock.Details
	return &details
}
