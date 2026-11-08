//go:build !windows

package codexaccounts

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// vaultLock is an OS-owned lease. Unlike an in-memory mutex it fences a
// second Bridge process that points at the same protected credential file.
type vaultLock struct{ file *os.File }

func acquireVaultLock(vaultPath string) (*vaultLock, error) {
	dir := filepath.Dir(vaultPath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, ErrSaveFailed
	}
	if err := securePath(dir, true); err != nil {
		return nil, ErrSaveFailed
	}
	lockPath := vaultPath + ".lock"
	if info, err := os.Lstat(lockPath); err == nil && !info.Mode().IsRegular() {
		return nil, ErrSaveFailed
	} else if err != nil && !os.IsNotExist(err) {
		return nil, ErrSaveFailed
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrSaveFailed
	}
	if err := securePath(lockPath, false); err != nil {
		_ = f.Close()
		return nil, ErrSaveFailed
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrVaultBusy
		}
		return nil, ErrSaveFailed
	}
	return &vaultLock{file: f}, nil
}

func (l *vaultLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
