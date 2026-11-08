//go:build !windows

package codexaccounts

import (
	"os"
	"path/filepath"
)

type ownerOnlyProtector struct{}

func platformProtector() Protector { return ownerOnlyProtector{} }
func (ownerOnlyProtector) Protect(data []byte) ([]byte, error) {
	return append([]byte(nil), data...), nil
}
func (ownerOnlyProtector) Unprotect(data []byte) ([]byte, error) {
	return append([]byte(nil), data...), nil
}
func securePath(path string, dir bool) error {
	mode := os.FileMode(0600)
	if dir {
		mode = 0700
	}
	return os.Chmod(path, mode)
}
func checkPrivatePath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0077 != 0 {
		return ErrSaveFailed
	}
	return nil
}
func replaceFile(from, to string) error {
	if err := os.Rename(from, to); err != nil {
		return err
	}
	dir, err := os.OpenFile(filepath.Dir(to), os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
