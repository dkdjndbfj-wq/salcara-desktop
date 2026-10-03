// Package atomicfile persists configuration without truncating an existing file
// if replacement fails, including when Windows denies delete sharing.
package atomicfile

import (
	"os"
	"path/filepath"
)

// WriteFile prepares a complete file in the destination directory, flushes and
// closes it, then replaces path. A failure leaves the previous file intact.
// The temporary file is private at creation and removed on every failure.
func WriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Chmod(mode.Perm()); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Never fall back to an in-place write: another process may permit writing
	// while refusing replacement, and truncation would break the transaction.
	return os.Rename(tmp, path)
}

// WriteFileKeepMode preserves an existing file's permissions and uses def when
// creating a new file.
func WriteFileKeepMode(path string, data []byte, def os.FileMode) error {
	mode := def
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}
	return WriteFile(path, data, mode)
}
