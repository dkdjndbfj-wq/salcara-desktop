//go:build windows

package hubclient

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"salcara/bridge/internal/protocol"
)

func explorerRoots() ([]protocol.Project, error) {
	buf := make([]uint16, 512)
	n, err := windows.GetLogicalDriveStrings(uint32(len(buf)), &buf[0])
	if err != nil || n >= uint32(len(buf)) {
		return nil, errExplorerLocation
	}
	roots := []protocol.Project{}
	for begin, i := 0, 0; i < int(n); i++ {
		if buf[i] != 0 {
			continue
		}
		root := windows.UTF16ToString(buf[begin:i])
		begin = i + 1
		if explorerLocalPath(root) {
			roots = append(roots, protocol.Project{Path: root, Name: "本地磁盘 (" + strings.TrimSuffix(root, `\`) + ")"})
		}
	}
	return roots, nil
}

func explorerLocalPath(value string) bool {
	if !filepath.IsAbs(value) || strings.HasPrefix(value, "\\") {
		return false
	}
	volume := filepath.VolumeName(value)
	if len(volume) != 2 || volume[1] != ':' {
		return false
	}
	p, err := windows.UTF16PtrFromString(volume + "\\")
	if err != nil {
		return false
	}
	kind := windows.GetDriveType(p)
	return kind == windows.DRIVE_FIXED || kind == windows.DRIVE_RAMDISK || kind == windows.DRIVE_REMOVABLE || kind == windows.DRIVE_CDROM
}

func explorerIsLink(info os.FileInfo) bool {
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return true
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
}
