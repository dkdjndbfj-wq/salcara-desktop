//go:build linux

package hubclient

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func workspaceLocalRoot(path string) bool { return filepath.IsAbs(path) }

func workspaceFinalPath(f *os.File) (string, error) {
	p, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", f.Fd()))
	if err != nil || !filepath.IsAbs(p) {
		return "", errWorkspaceFiles
	}
	return filepath.Clean(p), nil
}

func workspaceRootReadFile(root *os.Root, name string) (*os.File, error) {
	// A concurrently substituted FIFO must not stall the Bridge waiting for a
	// writer. The opened handle is checked for regular-file identity afterward.
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

func workspaceVerifyHandle(root, file *os.File, expectedRoot, rel string, regular bool) error {
	rootPath, err := workspaceFinalPath(root)
	if err != nil || !workspaceSamePath(rootPath, expectedRoot) {
		return errWorkspaceFiles
	}
	actual, err := workspaceFinalPath(file)
	if err != nil || !IsWithin(rootPath, actual, false) || !workspaceSamePath(actual, filepath.Join(rootPath, filepath.FromSlash(rel))) {
		return errWorkspaceFiles
	}
	var st syscall.Stat_t
	var rootStat syscall.Stat_t
	if syscall.Fstat(int(file.Fd()), &st) != nil || syscall.Fstat(int(root.Fd()), &rootStat) != nil || st.Dev != rootStat.Dev || regular && st.Nlink != 1 {
		return errWorkspaceFiles
	}
	return nil
}
