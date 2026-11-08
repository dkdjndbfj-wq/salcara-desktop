package hubclient

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"salcara/bridge/internal/protocol"
)

// ErrNotAllowed is the PROTOCOL.md §3 error for a cwd outside the allowed project folders.
var ErrNotAllowed = errors.New("这个文件夹没有被允许远程操作")

// CheckCwd verifies cwd lies inside one of the allowed project folders (or is one). Symlinks are resolved on
// both sides so a link inside a project can't point outside it; ".." is cleaned away first; on Windows the
// case-folding only selects a candidate; filesystem identity proves the root.
// Returns the resolved cwd.
func CheckCwd(cwd string, projects []protocol.Project) (string, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return "", errors.New("请选择项目文件夹")
	}
	if !filepath.IsAbs(cwd) {
		return "", ErrNotAllowed
	}
	clean := filepath.Clean(cwd)
	if runtime.GOOS == "windows" && !workspaceLocalRoot(clean) {
		// Reject caller-selected SMB/device paths before any filesystem access.
		return "", ErrNotAllowed
	}
	real, err := resolveCwdPath(clean)
	if err != nil {
		if errors.Is(err, ErrNotAllowed) {
			return "", ErrNotAllowed
		}
		return "", errors.New("文件夹不存在：" + clean)
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return "", errors.New("不是文件夹：" + clean)
	}
	win := runtime.GOOS == "windows"
	for _, p := range projects {
		if p.Path == "" || !filepath.IsAbs(p.Path) || win && !workspaceLocalRoot(p.Path) {
			continue
		}
		root, err := resolveCwdPath(filepath.Clean(p.Path))
		if err != nil {
			continue
		}
		if IsWithin(root, real, win) && sameCwdAncestor(root, real) {
			return real, nil
		}
	}
	return "", ErrNotAllowed
}

// IsWithin alone is insufficient on Windows volumes with case-sensitive
// directories. Walk the resolved target back to the candidate root depth and
// compare actual filesystem objects, not two folded spellings.
func sameCwdAncestor(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	ancestor := target
	if rel != "." {
		for range strings.Split(rel, string(filepath.Separator)) {
			ancestor = filepath.Dir(ancestor)
		}
	}
	a, err := os.Stat(root)
	if err != nil || !a.IsDir() {
		return false
	}
	b, err := os.Stat(ancestor)
	return err == nil && b.IsDir() && os.SameFile(a, b)
}

func resolve(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// IsWithin reports whether target equals root or is below it. It is pure string logic so Windows paths
// (backslashes, drive letters, any case) can be checked — and tested — on every OS.
func IsWithin(root, target string, windows bool) bool {
	r, t := normPath(root, windows), normPath(target, windows)
	if r == "" || t == "" {
		return false
	}
	if t == r {
		return true
	}
	if strings.HasSuffix(r, "/") { // "/" or "c:/"
		return strings.HasPrefix(t, r)
	}
	return strings.HasPrefix(t, r+"/")
}

func normPath(p string, windows bool) string {
	if windows {
		p = strings.ReplaceAll(p, `\`, "/")
		p = strings.ToLower(p)
		// Win32 namespace prefixes (\\?\C:\x) and trailing dots/spaces are normalized by Windows itself.
		p = strings.TrimPrefix(p, "//?/")
	}
	if p == "" {
		return ""
	}
	c := path.Clean(p)
	if windows && len(c) == 2 && c[1] == ':' {
		c += "/"
	}
	return c
}
