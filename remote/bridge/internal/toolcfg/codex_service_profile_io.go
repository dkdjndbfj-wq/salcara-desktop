package toolcfg

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// LockCodexProfile serializes cooperating writers to this exact profile across
// managers, state directories and processes. It is not reentrant. A launcher
// must acquire it once around its outer transaction, then call
// RequireCodexProfileUnowned before making its own backup or changing files.
// OS-owned locks are released even when their owning process exits abruptly.
func LockCodexProfile(ctx context.Context, home string) (release func(), err error) {
	home, err = canonicalServiceProfilePath(home)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	r, err := openServiceProfileDir(home, true)
	if err != nil {
		return nil, ErrCodexProfileUnsafe
	}
	var f *os.File
	keep := false
	defer func() {
		if !keep {
			if f != nil {
				f.Close()
			}
			r.close()
		}
	}()
	info, statErr := r.root.Lstat(serviceProfileLockName)
	if statErr == nil && (!info.Mode().IsRegular() || serviceProfileUnsafeInfo(info)) || statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, ErrCodexProfileUnsafe
	}
	f, err = r.root.OpenFile(serviceProfileLockName, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrCodexProfileUnsafe
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || serviceProfileUnsafeInfo(opened) || serviceProfileCheckLinks(f) != nil {
		return nil, ErrCodexProfileUnsafe
	}
	for {
		locked, lockErr := tryServiceProfileLock(f)
		if lockErr != nil {
			return nil, ErrCodexProfileUnsafe
		}
		if locked {
			break
		}
		timer := time.NewTimer(15 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	pathInfo, err := r.root.Lstat(serviceProfileLockName)
	if err != nil || !os.SameFile(opened, pathInfo) || serviceProfileUnsafeInfo(pathInfo) || r.check() != nil {
		return nil, ErrCodexProfileUnsafe
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var once sync.Once
	keep = true
	return func() { once.Do(func() { unlockServiceProfile(f); f.Close(); r.close() }) }, nil
}

// RequireCodexProfileUnowned inspects only the service ownership marker. The
// caller must hold LockCodexProfile until its entire transaction finishes.
// Even an unknown/corrupt marker is an ownership barrier, never an invitation
// to adopt the current service credential into a different backup system.
func RequireCodexProfileUnowned(home string) error {
	home, err := canonicalServiceProfilePath(home)
	if err != nil {
		return err
	}
	r, err := openServiceProfileDir(home, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrCodexProfileUnsafe
	}
	defer r.close()
	marker, err := r.read(serviceProfileMarkerName, 4096)
	if err != nil {
		return err
	}
	if marker.Exists {
		return ErrCodexProfileOwned
	}
	return nil
}

type serviceProfileDir struct {
	path     string
	root     *os.Root
	identity os.FileInfo
}

func canonicalServiceProfilePath(path string) (string, error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
		return "", ErrCodexProfileInvalid
	}
	path = filepath.FromSlash(path)
	for _, part := range strings.FieldsFunc(path[len(filepath.VolumeName(path)):], func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == ".." || serviceProfileInvalidComponent(part) {
			return "", ErrCodexProfileInvalid
		}
	}
	clean := filepath.Clean(path)
	if filepath.Dir(clean) == clean || strings.HasPrefix(filepath.VolumeName(clean), `\\`) {
		return "", ErrCodexProfileInvalid
	}
	return clean, nil
}

// Walk one rooted component at a time: MkdirAll on a full path could create
// directories through a concurrently inserted link before a later check.
func openServiceProfileDir(path string, create bool) (*serviceProfileDir, error) {
	path, err := canonicalServiceProfilePath(path)
	if err != nil {
		return nil, err
	}
	volume := filepath.VolumeName(path)
	root, err := os.OpenRoot(volume + string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	parts := strings.FieldsFunc(strings.TrimPrefix(path, volume), func(r rune) bool { return r == os.PathSeparator })
	for _, name := range parts {
		info, e := root.Lstat(name)
		if errors.Is(e, os.ErrNotExist) && create {
			if e = root.Mkdir(name, 0700); e != nil && !errors.Is(e, os.ErrExist) {
				root.Close()
				return nil, e
			}
			info, e = root.Lstat(name)
		}
		if e != nil {
			root.Close()
			return nil, e
		}
		if !info.IsDir() || serviceProfileUnsafeInfo(info) {
			root.Close()
			return nil, ErrCodexProfileUnsafe
		}
		next, e := root.OpenRoot(name)
		root.Close()
		if e != nil {
			return nil, e
		}
		opened, e := next.Stat(".")
		if e != nil || !os.SameFile(info, opened) {
			next.Close()
			return nil, ErrCodexProfileUnsafe
		}
		root = next
	}
	info, err := root.Stat(".")
	if err != nil {
		root.Close()
		return nil, err
	}
	return &serviceProfileDir{path: path, root: root, identity: info}, nil
}

func (d *serviceProfileDir) close() {
	if d != nil && d.root != nil {
		d.root.Close()
	}
}
func (d *serviceProfileDir) check() error {
	// A rooted handle prevents path escape. Reopen/check additionally rejects a
	// renamed/replaced profile, so work on an old handle cannot be published as
	// the profile currently selected at this fixed path.
	current, err := openServiceProfileDir(d.path, false)
	if err != nil {
		return ErrCodexProfileUnsafe
	}
	defer current.close()
	if !os.SameFile(current.identity, d.identity) {
		return ErrCodexProfileUnsafe
	}
	return nil
}

func (d *serviceProfileDir) private() error {
	if err := d.check(); err != nil {
		return err
	}
	f, err := d.root.Open(".")
	if err != nil {
		return ErrCodexProfileUnsafe
	}
	defer f.Close()
	security, err := serviceProfilePrivateSecurity(true)
	if err != nil || serviceProfileSetSecurity(f, security) != nil || f.Chmod(0700) != nil {
		return ErrCodexProfileUnsafe
	}
	return d.check()
}

func (d *serviceProfileDir) read(name string, limit int) (serviceProfileFile, error) {
	var out serviceProfileFile
	if err := d.check(); err != nil {
		return out, err
	}
	info, err := d.root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil || !info.Mode().IsRegular() || serviceProfileUnsafeInfo(info) || info.Size() > int64(limit) {
		return out, ErrCodexProfileUnsafe
	}
	f, err := d.root.Open(name)
	if err != nil {
		return out, ErrCodexProfileUnsafe
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || serviceProfileCheckLinks(f) != nil {
		return out, ErrCodexProfileUnsafe
	}
	security, err := serviceProfileGetSecurity(f)
	if err != nil {
		return out, ErrCodexProfileUnsafe
	}
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(data) > limit {
		return out, ErrCodexProfileUnsafe
	}
	final, err := f.Stat()
	pathInfo, pathErr := d.root.Lstat(name)
	finalSecurity, securityErr := serviceProfileGetSecurity(f)
	if err != nil || pathErr != nil || securityErr != nil || !os.SameFile(opened, final) || !os.SameFile(final, pathInfo) || opened.Size() != final.Size() || !opened.ModTime().Equal(final.ModTime()) || opened.Mode() != final.Mode() || security != finalSecurity || serviceProfileUnsafeInfo(pathInfo) || d.check() != nil {
		return out, ErrCodexProfileChanged
	}
	return serviceProfileFile{Exists: true, Data: data, Mode: uint32(final.Mode().Perm()), Security: security}, nil
}

func (d *serviceProfileDir) write(name string, value serviceProfileFile) error {
	if err := d.check(); err != nil {
		return err
	}
	if info, err := d.root.Lstat(name); err == nil && (!info.Mode().IsRegular() || serviceProfileUnsafeInfo(info)) || err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrCodexProfileUnsafe
	}
	if !value.Exists {
		if err := d.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return ErrCodexProfileWrite
		}
		return syncServiceProfileDir(d.root)
	}
	token, err := newServiceProfileToken()
	if err != nil {
		return ErrCodexProfileWrite
	}
	temporary := ".salcara-profile-" + token + ".tmp"
	f, err := d.root.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrCodexProfileWrite
	}
	defer func() { _ = d.root.Chmod(temporary, 0600); _ = d.root.Remove(temporary) }()
	defer f.Close()
	// Restrict a temporary credential before writing any bytes. Windows mode
	// bits alone do not do this; its platform implementation sets a private ACL.
	private, err := serviceProfilePrivateSecurity(false)
	if err != nil || serviceProfileSetSecurity(f, private) != nil {
		return ErrCodexProfileUnsafe
	}
	if _, err = f.Write(value.Data); err != nil {
		return ErrCodexProfileWrite
	}
	if err = serviceProfileSetSecurity(f, value.Security); err != nil {
		return ErrCodexProfileUnsafe
	}
	if err = f.Chmod(os.FileMode(value.Mode)); err != nil {
		return ErrCodexProfileWrite
	}
	if err = f.Sync(); err != nil {
		return ErrCodexProfileWrite
	}
	if err = f.Close(); err != nil {
		return ErrCodexProfileWrite
	}
	if err = d.check(); err != nil {
		return err
	}
	if info, e := d.root.Lstat(name); e == nil && (!info.Mode().IsRegular() || serviceProfileUnsafeInfo(info)) || e != nil && !errors.Is(e, os.ErrNotExist) {
		return ErrCodexProfileUnsafe
	}
	if err = d.root.Rename(temporary, name); err != nil {
		return ErrCodexProfileWrite
	}
	return syncServiceProfileDir(d.root)
}
