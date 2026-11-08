//go:build !windows

package toolcfg

import (
	"errors"
	"os"
	"syscall"
)

func serviceProfilePathKey(path string) string   { return path }
func serviceProfileInvalidComponent(string) bool { return false }
func serviceProfileUnsafeInfo(info os.FileInfo) bool {
	return info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0
}
func serviceProfileCheckLinks(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return ErrCodexProfileUnsafe
	}
	return nil
}
func tryServiceProfileLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return err == nil, err
}
func unlockServiceProfile(f *os.File)                    { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
func serviceProfileStoredMode(mode uint32) uint32        { return mode }
func serviceProfilePrivateSecurity(bool) (string, error) { return "", nil }
func serviceProfileGetSecurity(*os.File) (string, error) { return "", nil }
func serviceProfileValidSecurity(value string) bool      { return value == "" }
func serviceProfileSecurityEqual(a, b string) bool       { return a == b }
func serviceProfileSetSecurity(_ *os.File, value string) error {
	if value != "" {
		return ErrCodexProfileUnsafe
	}
	return nil
}
func syncServiceProfileDir(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return ErrCodexProfileWrite
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return ErrCodexProfileWrite
	}
	return nil
}
