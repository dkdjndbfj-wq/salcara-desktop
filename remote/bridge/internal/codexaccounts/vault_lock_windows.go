//go:build windows

package codexaccounts

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

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
	p, err := windows.UTF16PtrFromString(lockPath)
	if err != nil {
		return nil, ErrSaveFailed
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_ALWAYS, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if err == windows.ERROR_SHARING_VIOLATION || err == windows.ERROR_LOCK_VIOLATION {
			return nil, ErrVaultBusy
		}
		return nil, ErrSaveFailed
	}
	// windows.Handle is already an integer OS handle. Converting through an
	// unsafe pointer is both unnecessary and rejected by go vet; os.NewFile
	// takes the same uintptr representation used by CreateFile.
	f := os.NewFile(uintptr(h), lockPath)
	if f == nil {
		_ = windows.CloseHandle(h)
		return nil, ErrSaveFailed
	}
	if err := securePath(lockPath, false); err != nil {
		_ = f.Close()
		return nil, ErrSaveFailed
	}
	return &vaultLock{file: f}, nil
}

func (l *vaultLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}
