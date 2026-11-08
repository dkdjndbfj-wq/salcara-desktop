package codexaccounts

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestExclusiveWriterRejectsSecondServiceAndReleasesOnClose(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "accounts.vault")
	p := &testProtector{}
	first, err := New(Options{Path: path, Protector: p, ExclusiveWriter: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(Options{Path: path, Protector: p, ExclusiveWriter: true})
	if second != nil || !errors.Is(err, ErrVaultBusy) {
		t.Fatalf("second writer = %#v, err=%v; want ErrVaultBusy", second, err)
	}
	first.Close()
	reopened, err := New(Options{Path: path, Protector: p, ExclusiveWriter: true})
	if err != nil {
		t.Fatalf("reopen after close: %v", err)
	}
	reopened.Close()
}

func TestNonExclusiveFixtureCanShareServiceForReadOnlyCompatibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "accounts.vault")
	p := &testProtector{}
	first, err := New(Options{Path: path, Protector: p})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := New(Options{Path: path, Protector: p})
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}
