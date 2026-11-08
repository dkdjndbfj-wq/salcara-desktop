package codexaccounts

import "crypto/sha256"

type credentialDigest [sha256.Size]byte

// credentialConflictLocked rejects two vault references to the same local
// credential material. Account/profile claims are intentionally excluded:
// they are display metadata, not proof that two imports have the same owner.
//
// The caller holds s.mu. A replacement may reuse only the target account's
// own current or pending credential. Pending rotations for every other account
// remain reserved even though a failed vault write has not published them yet.
func (s *Service) credentialConflictLocked(incoming []credential, replaceID string) bool {
	access := make(map[credentialDigest]struct{}, len(s.vault.Accounts)+len(s.pending)+len(incoming))
	refresh := make(map[credentialDigest]struct{}, len(s.vault.Accounts)+len(s.pending)+len(incoming))

	reserve := func(c credential) {
		if digest, ok := tokenDigest(c.AccessToken); ok {
			access[digest] = struct{}{}
		}
		if digest, ok := tokenDigest(c.RefreshToken); ok {
			refresh[digest] = struct{}{}
		}
	}
	addIncoming := func(c credential) bool {
		if digest, ok := tokenDigest(c.AccessToken); ok {
			if _, exists := access[digest]; exists {
				return false
			}
			access[digest] = struct{}{}
		}
		if digest, ok := tokenDigest(c.RefreshToken); ok {
			if _, exists := refresh[digest]; exists {
				return false
			}
			refresh[digest] = struct{}{}
		}
		return true
	}

	for id, c := range s.vault.Accounts {
		if id != replaceID {
			reserve(c)
		}
	}
	for id, pending := range s.pending {
		if id != replaceID {
			reserve(pending.credential)
		}
	}
	for _, c := range incoming {
		if !addIncoming(c) {
			return true
		}
	}
	return false
}

func tokenDigest(token string) (credentialDigest, bool) {
	if token == "" {
		return credentialDigest{}, false
	}
	return sha256.Sum256([]byte(token)), true
}
