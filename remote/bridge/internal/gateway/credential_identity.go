package gateway

import "salcara/bridge/internal/config"

// CredentialBudgetIdentity is an internal scheduling key, never an identity or
// authorization proof. All schedulers use the same endpoint normalization and
// protected reference epoch. Callers must not expose it in diagnostics or APIs.
func CredentialBudgetIdentity(a config.LocalAccount) (string, bool) { return poolCredentialIdentity(a) }
