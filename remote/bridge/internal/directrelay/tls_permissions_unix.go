//go:build !windows

package directrelay

import (
	"errors"
	"fmt"
	"os"

	"salcara/bridge/internal/atomicfile"
)

func writeDirectTLSIdentityFile(path string, bundle []byte) error {
	return atomicfile.WriteFile(path, bundle, 0o600)
}

func protectDirectTLSIdentityFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect direct TLS identity file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("direct TLS identity path must be a regular file")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("protect direct TLS private key: %w", err)
	}
	return nil
}

func verifyDirectTLSIdentityFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect direct TLS identity file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("direct TLS identity path must be a regular file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("direct TLS identity file permissions must be owner-only")
	}
	return nil
}
