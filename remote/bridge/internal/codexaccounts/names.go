package codexaccounts

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	nameModeDefault = "default"
	nameModeCustom  = "custom"
)

func validAccountName(name string) bool {
	return len(name) <= 320 && utf8.ValidString(name) && !strings.ContainsFunc(name, unicode.IsControl)
}

// Email is an unverified provider display hint, not an identity/routing key.
// Never derive it from a custom label or copy the label into SDK auth metadata.
func accountEmailHint(value string) string {
	value = strings.TrimSpace(value)
	if !validAccountName(value) || !strings.Contains(value, "@") || strings.ContainsFunc(value, unicode.IsSpace) {
		return ""
	}
	return value
}

func tokenEmailHint(c credential) string {
	for _, token := range []string{c.IDToken, c.AccessToken} {
		if email := accountEmailHint(stringValue(displayClaims(token), "email")); email != "" {
			return email
		}
	}
	return ""
}

func defaultAccountName(c credential) string {
	if c.Email != "" {
		return c.Email
	}
	return "Codex account"
}

// Old vaults cannot prove whether a saved non-empty name was a user edit.
// Preserve it conservatively. This migration is metadata-only and is persisted
// together with the next successful vault write, never by changing token epochs.
func migrateAccountName(c credential) credential {
	if c.NameMode != "" {
		return c
	}
	if c.Email == "" {
		c.Email = tokenEmailHint(c)
	}
	if c.Email == "" {
		c.Email = accountEmailHint(c.Name)
	}
	c.NameMode = nameModeCustom
	if c.Name == "" || c.Name == "Codex account" {
		c.NameMode = nameModeDefault
		c.Name = defaultAccountName(c)
	}
	return c
}

func preserveCustomAccountName(next *credential, current credential) {
	current = migrateAccountName(current)
	next.NameRevision = current.NameRevision
	if current.NameMode == nameModeCustom {
		next.Name, next.NameMode = current.Name, nameModeCustom
	}
}

// Rotation/retry may finish after Rename. Merge the current display preference
// at commit, not the stale credential snapshot captured before network I/O.
func mergeRotatedAccountName(next *credential, current credential) {
	current = migrateAccountName(current)
	if next.Email == "" {
		next.Email = current.Email
	}
	next.NameMode, next.NameRevision = current.NameMode, current.NameRevision
	if current.NameMode == nameModeCustom {
		next.Name = current.Name
	} else {
		next.Name = defaultAccountName(*next)
	}
}

// Rename changes only display metadata. An empty name restores the email
// default. The identity CAS prevents a stale dialog renaming a replaced account;
// neither credential epoch nor active requests/usage observations are retired.
func (s *Service) Rename(ctx context.Context, id, name string, expectedIdentityEpoch uint64) (Account, error) {
	if !validAccountName(name) {
		return Account{}, ErrInvalidImport
	}
	name = strings.TrimSpace(name)
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Account{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Account{}, err
	}
	current, ok := s.vault.Accounts[id]
	if !ok {
		return Account{}, ErrNotFound
	}
	if expectedIdentityEpoch == 0 || current.IdentityEpoch != expectedIdentityEpoch {
		return Account{}, ErrConflict
	}
	current = migrateAccountName(current)
	current.NameMode = nameModeCustom
	current.Name = name
	if name == "" {
		current.NameMode = nameModeDefault
		current.Name = defaultAccountName(current)
	}
	current.NameRevision++
	next := cloneVault(s.vault)
	next.Accounts[id] = current
	if err := writeVault(s.path, s.protector, next); err != nil {
		return Account{}, err
	}
	s.vault = next
	return s.accountLocked(current), nil
}
