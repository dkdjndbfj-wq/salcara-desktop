//go:build windows

package hubclient

import (
	"os"
	"path/filepath"
	"strings"
)

// EvalSymlinks follows a junction before callers can inspect its destination.
// Resolve one local component at a time, checking every link destination before
// accessing it. UNC/device/mapped-drive targets never reach Stat/Open.
func resolveCwdPath(path string) (string, error) {
	return resolveLocalCwdPath(path, 0)
}

func resolveLocalCwdPath(path string, links int) (string, error) {
	if links > 40 || !filepath.IsAbs(path) || !workspaceLocalRoot(path) {
		return "", ErrNotAllowed
	}
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	current := volume + string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(path[len(volume):], string(filepath.Separator)), string(filepath.Separator))
	for i, part := range parts {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(current)
			if err != nil {
				return "", err
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(current), target)
			}
			if !workspaceLocalRoot(target) {
				return "", ErrNotAllowed
			}
			return resolveLocalCwdPath(filepath.Join(append([]string{target}, parts[i+1:]...)...), links+1)
		}
		if info.Mode()&os.ModeType != os.ModeDir {
			return "", ErrNotAllowed
		}
	}
	return current, nil
}
