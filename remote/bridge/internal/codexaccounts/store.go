package codexaccounts

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type vault struct {
	Version          int                        `json:"version"`
	Epochs           map[string]uint64          `json:"epochs"`
	Accounts         map[string]credential      `json:"accounts"`
	ResetRedemptions map[string]resetRedemption `json:"reset_redemptions,omitempty"`
	CleanupReceipts  map[string]cleanupReceipt  `json:"cleanup_receipts,omitempty"`
}
type resetRedemption struct {
	ID            string     `json:"id"`
	PreviousID    string     `json:"previous_id,omitempty"`
	IdentityEpoch uint64     `json:"identity_epoch"`
	State         string     `json:"state"`
	CreatedAt     time.Time  `json:"created_at"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}
type envelope struct {
	Version int    `json:"version"`
	Payload []byte `json:"payload"`
}

func readVault(path string, protector Protector) (vault, error) {
	v := vault{Version: BundleVersion, Epochs: map[string]uint64{}, Accounts: map[string]credential{}, ResetRedemptions: map[string]resetRedemption{}, CleanupReceipts: map[string]cleanupReceipt{}}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4*MaxImportBytes {
		return v, ErrSaveFailed
	}
	if err = checkPrivatePath(path); err != nil {
		return v, ErrSaveFailed
	}
	if err = checkPrivatePath(filepath.Dir(path)); err != nil {
		return v, ErrSaveFailed
	}
	f, err := os.Open(path)
	if err != nil {
		return v, ErrSaveFailed
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4*MaxImportBytes+1))
	if err != nil || len(data) > 4*MaxImportBytes {
		return v, ErrSaveFailed
	}
	if _, err = strictJSONLimit(data, 4*MaxImportBytes); err != nil {
		return v, ErrSaveFailed
	}
	var env envelope
	if json.Unmarshal(data, &env) != nil || env.Version != BundleVersion {
		return v, ErrSaveFailed
	}
	plain, err := protector.Unprotect(env.Payload)
	if err != nil {
		return v, ErrSaveFailed
	}
	defer wipe(plain)
	if _, err = strictJSON(plain); err != nil {
		return v, ErrSaveFailed
	}
	if json.Unmarshal(plain, &v) != nil || v.Version != BundleVersion || v.Accounts == nil || v.Epochs == nil || len(v.Accounts) > MaxAccounts {
		return v, ErrSaveFailed
	}
	for id, c := range v.Accounts {
		c = migrateAccountName(c)
		// Keep the pre-existing label format readable (it allowed tabs). New
		// Rename requests are stricter, but migration must not brick old vaults.
		if len(c.Name) > 320 || strings.ContainsAny(c.Name, "\r\n\x00") || c.Email != "" && accountEmailHint(c.Email) != c.Email || c.NameMode != nameModeDefault && c.NameMode != nameModeCustom {
			return v, ErrSaveFailed
		}
		if c.IdentityEpoch == 0 {
			c.IdentityEpoch = c.Epoch
		}
		v.Accounts[id] = c
		refreshOnly := c.AccessToken == "" && !c.ExpiresAt.IsZero() && c.Status != StatusReady && (c.RefreshToken != "" || c.Status == StatusNeedsLogin)
		if id == "" || id != c.ID || c.Epoch == 0 || c.Epoch != v.Epochs[id] || c.IdentityEpoch > c.Epoch || c.AccessToken == "" && !refreshOnly || len(c.AccessToken) > maxTokenBytes || len(c.RefreshToken) > maxTokenBytes || len(c.IDToken) > maxTokenBytes {
			return v, ErrSaveFailed
		}
	}
	if v.ResetRedemptions == nil {
		v.ResetRedemptions = map[string]resetRedemption{}
	}
	for id, redemption := range v.ResetRedemptions {
		c, ok := v.Accounts[id]
		if !ok || redemption.IdentityEpoch != c.IdentityEpoch {
			delete(v.ResetRedemptions, id)
			continue
		}
		if !validRedeemRequestID(redemption.ID) || !validResetRedemptionState(redemption.State) || redemption.PreviousID != "" && (!validRedeemRequestID(redemption.PreviousID) || redemption.PreviousID == redemption.ID) {
			return v, ErrSaveFailed
		}
		if redemption.State == resetStateConsuming {
			redemption.State = resetStateUnknown
			v.ResetRedemptions[id] = redemption
		}
	}
	if v.CleanupReceipts == nil {
		v.CleanupReceipts = map[string]cleanupReceipt{}
	}
	if len(v.CleanupReceipts) > MaxAccounts {
		return v, ErrSaveFailed
	}
	for id, receipt := range v.CleanupReceipts {
		if id != receipt.ID || !validCleanupIdentity(receipt.CleanupIdentity) || receipt.CreatedAt.IsZero() {
			return v, ErrSaveFailed
		}
		// Never allow a historical receipt to stand in for a live or newer ID.
		if _, exists := v.Accounts[id]; exists || v.Epochs[id] != receipt.Epoch+1 {
			delete(v.CleanupReceipts, id)
		}
	}
	return v, nil
}

func writeVault(path string, protector Protector, v vault) error {
	plain, err := json.Marshal(v)
	if err != nil || len(plain) > MaxImportBytes {
		return ErrSaveFailed
	}
	defer wipe(plain)
	protected, err := protector.Protect(plain)
	if err != nil {
		return ErrSaveFailed
	}
	defer wipe(protected)
	data, err := json.Marshal(envelope{Version: BundleVersion, Payload: protected})
	if err != nil {
		return ErrSaveFailed
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return ErrSaveFailed
	}
	if err = securePath(dir, true); err != nil {
		return ErrSaveFailed
	}
	// Never follow a credential-file symlink on load or replacement.
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return ErrSaveFailed
	} else if err != nil && !os.IsNotExist(err) {
		return ErrSaveFailed
	}
	f, err := os.CreateTemp(dir, ".codex-vault-*")
	if err != nil {
		return ErrSaveFailed
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = securePath(tmp, false); err != nil {
		f.Close()
		return ErrSaveFailed
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return ErrSaveFailed
	}
	if err = replaceFile(tmp, path); err != nil {
		return ErrSaveFailed
	}
	return nil
}

func cloneVault(v vault) vault {
	next := vault{Version: BundleVersion, Epochs: make(map[string]uint64, len(v.Epochs)), Accounts: make(map[string]credential, len(v.Accounts)), ResetRedemptions: make(map[string]resetRedemption, len(v.ResetRedemptions)), CleanupReceipts: make(map[string]cleanupReceipt, len(v.CleanupReceipts))}
	for k, c := range v.Accounts {
		next.Accounts[k] = c
	}
	for k, epoch := range v.Epochs {
		next.Epochs[k] = epoch
	}
	for k, redemption := range v.ResetRedemptions {
		next.ResetRedemptions[k] = redemption
	}
	for k, receipt := range v.CleanupReceipts {
		next.CleanupReceipts[k] = receipt
	}
	return next
}
func wipe(data []byte) {
	for i := range data {
		data[i] = 0
	}
}
