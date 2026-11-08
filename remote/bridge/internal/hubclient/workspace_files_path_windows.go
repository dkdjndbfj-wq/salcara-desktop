//go:build windows

package hubclient

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func workspaceLocalRoot(path string) bool {
	// Reject UNC, device namespaces and mapped network drives before any stat
	// or open can contact SMB. Only fixed local disks/RAM disks are supported.
	if strings.HasPrefix(path, `\`) {
		return false
	}
	volume := filepath.VolumeName(path)
	if len(volume) != 2 || volume[1] != ':' {
		return false
	}
	p, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return false
	}
	typ := windows.GetDriveType(p)
	return typ == windows.DRIVE_FIXED || typ == windows.DRIVE_RAMDISK
}

func workspaceFinalPath(f *os.File) (string, error) {
	buf := make([]uint16, 32768)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &buf[0], uint32(len(buf)), 0)
	if err != nil || n == 0 || n >= uint32(len(buf)) {
		return "", errWorkspaceFiles
	}
	p := windows.UTF16ToString(buf[:n])
	if strings.HasPrefix(p, `\\?\UNC\`) {
		return "", errWorkspaceFiles
	}
	p = strings.TrimPrefix(p, `\\?\`)
	return filepath.Clean(p), nil
}

func workspaceRootReadFile(root *os.Root, name string) (*os.File, error) { return root.Open(name) }

func workspaceVerifyHandle(root, file *os.File, expectedRoot, rel string, regular bool) error {
	rootPath, err := workspaceFinalPath(root)
	if err != nil || !workspaceSamePath(rootPath, expectedRoot) {
		return errWorkspaceFiles
	}
	actual, err := workspaceFinalPath(file)
	if err != nil || !IsWithin(rootPath, actual, true) || !workspaceSamePath(actual, filepath.Join(rootPath, filepath.FromSlash(rel))) {
		return errWorkspaceFiles
	}
	var st windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &st) != nil || st.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || regular && st.NumberOfLinks != 1 {
		return errWorkspaceFiles
	}
	return nil
}
