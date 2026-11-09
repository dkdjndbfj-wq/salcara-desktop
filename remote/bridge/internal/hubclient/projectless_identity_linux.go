//go:build linux

package hubclient

import (
	"fmt"
	"os"
	"syscall"
)

func projectlessDirectoryIdentity(file *os.File) (string, error) {
	var info syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &info); err != nil {
		return "", err
	}
	if info.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return "", errProjectless
	}
	return fmt.Sprintf("linux:%x:%x", info.Dev, info.Ino), nil
}
