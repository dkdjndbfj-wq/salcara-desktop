package config

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// NativeDesktopSourceIdentity binds a desktop lease to the API actually
// applied by Salcara. Phone-only RemoteAPI choices belong to CLI workers.
// Hash the projection so no endpoint or credential marker leaves the config.
func (c Config) NativeDesktopSourceIdentity() string {
	a, ok := c.AuthorizedAppliedToolAccount("codex-desktop")
	if !ok {
		return ""
	}
	checked := a
	if ValidateLocalAccount(&checked) != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join([]string{"codex-native-v1", a.ID, AccountFingerprint(a), a.GatewayLaunchLease}, "\x00")))
	return hex.EncodeToString(sum[:])
}
