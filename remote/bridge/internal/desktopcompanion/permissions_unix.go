//go:build !windows

package desktopcompanion

import (
	"errors"
	"os"
	"syscall"
)

func checkPrivatePermissions(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() || info.Mode().Perm()&0077 != 0 {
		return errors.New("not owner-private")
	}
	return nil
}

func setPrivatePermissions(path string, directory bool) error {
	mode := os.FileMode(0600)
	if directory {
		mode = 0700
	}
	return os.Chmod(path, mode)
}
