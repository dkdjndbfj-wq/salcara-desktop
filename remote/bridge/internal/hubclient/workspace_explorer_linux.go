//go:build linux

package hubclient

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"salcara/bridge/internal/protocol"
)

type explorerMount struct {
	path  string
	local bool
}

func explorerMounts() ([]explorerMount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, errExplorerLocation
	}
	defer f.Close()
	reader := bufio.NewScanner(io.LimitReader(f, 4<<20))
	reader.Buffer(make([]byte, 4096), 64<<10)
	mounts := []explorerMount{}
	for reader.Scan() {
		parts := strings.SplitN(reader.Text(), " - ", 2)
		if len(parts) != 2 {
			return nil, errExplorerLocation
		}
		before, after := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(before) < 6 || len(after) < 3 || len(mounts) >= 16384 {
			return nil, errExplorerLocation
		}
		mount := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(before[4])
		if !filepath.IsAbs(mount) {
			return nil, errExplorerLocation
		}
		local := false
		switch after[0] {
		case "ext2", "ext3", "ext4", "btrfs", "xfs", "zfs", "f2fs", "overlay", "tmpfs", "ramfs", "vfat", "exfat", "ntfs", "ntfs3", "fuseblk", "iso9660", "udf", "squashfs":
			local = true
		}
		mounts = append(mounts, explorerMount{mount, local})
	}
	if reader.Err() != nil || len(mounts) == 0 {
		return nil, errExplorerLocation
	}
	return mounts, nil
}

func explorerLocalPath(value string) bool {
	if !filepath.IsAbs(value) || strings.HasPrefix(value, "//") {
		return false
	}
	mounts, err := explorerMounts()
	if err != nil {
		return false
	}
	best, local := -1, false
	for _, mount := range mounts {
		if len(mount.path) >= best && IsWithin(mount.path, value, false) {
			best, local = len(mount.path), mount.local
		}
	}
	return local
}

func explorerRoots() ([]protocol.Project, error) {
	mounts, err := explorerMounts()
	if err != nil {
		return nil, err
	}
	paths := map[string]bool{}
	for _, mount := range mounts {
		paths[mount.path] = mount.local
	}
	roots := []protocol.Project{}
	for value, local := range paths {
		if !local {
			continue
		}
		st, err := os.Lstat(value)
		if err != nil || !st.IsDir() || explorerIsLink(st) {
			continue
		}
		name := filepath.Base(value) + " (" + value + ")"
		if value == "/" {
			name = "文件系统 (/)"
		}
		roots = append(roots, protocol.Project{Path: value, Name: name})
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i].Path < roots[j].Path })
	if len(roots) > 128 {
		roots = roots[:128]
	}
	return roots, nil
}

func explorerIsLink(info os.FileInfo) bool { return info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 }
