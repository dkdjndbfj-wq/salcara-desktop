//go:build windows

package toolcfg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCodexServiceProfileWindowsSecurity(t *testing.T) {
	m := serviceProfileFixture(t, true)
	release, err := LockCodexProfile(context.Background(), m.home)
	if err != nil {
		t.Fatal("lock", err)
	}
	defer release()
	d, err := openServiceProfileDir(m.state, true)
	if err != nil {
		t.Fatal("directory", err)
	}
	defer d.close()
	f, err := d.root.Open(".")
	if err != nil {
		t.Fatal("directory handle", err)
	}
	defer f.Close()
	security, err := serviceProfilePrivateSecurity(true)
	if err != nil {
		t.Fatal("descriptor", err)
	}
	if err := serviceProfileSetSecurity(f, security); err != nil {
		t.Fatal("directory ACL", err)
	}
	if err := f.Chmod(0700); err != nil {
		t.Fatal("directory mode", err)
	}
	if err := d.private(); err != nil {
		t.Fatal("private", err)
	}
	security, err = serviceProfilePrivateSecurity(false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.write("test.json", serviceProfileFile{Exists: true, Data: []byte("synthetic"), Mode: serviceProfileStoredMode(0600), Security: security}); err != nil {
		t.Fatal("write", err)
	}
	got, err := d.read("test.json", 1024)
	if err != nil {
		t.Fatal("read", err)
	}
	if got.Security != security {
		t.Fatalf("private DACL did not round trip: got %q want %q", got.Security, security)
	}
}

func TestCodexServiceProfileWindowsLockedTargetRollsBackWithoutTruncation(t *testing.T) {
	for _, name := range []string{"auth.json", "config.toml"} {
		t.Run(name, func(t *testing.T) {
			m := serviceProfileFixture(t, true)
			original := serviceProfileFiles(t, m.home)
			status := serviceProfileReading(t, m)
			path, err := syscall.UTF16PtrFromString(filepath.Join(m.home, name))
			if err != nil {
				t.Fatal(err)
			}
			handle, err := syscall.CreateFile(path, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.CloseHandle(handle)
			result, err := m.Apply(context.Background(), status.Revision, serviceProfileParams("a"), serviceProfileAllow)
			if err == nil || result.State != "idle" {
				t.Fatalf("locked target accepted or failed to rollback: %+v %v", result, err)
			}
			if !equalServiceProfileProofs(serviceProfileProofs(original), serviceProfileProofs(serviceProfileFiles(t, m.home))) {
				t.Fatal("locked original truncated or other file not restored")
			}
			for _, dir := range []string{m.home, m.state} {
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".salcara-profile-") {
						t.Fatal("failure left credential temporary file")
					}
				}
			}
		})
	}
}

func TestCodexServiceProfileWindowsDACLComparison(t *testing.T) {
	for _, tc := range []struct {
		name  string
		a, b  string
		equal bool
	}{
		{"legacy inherited metadata", "O:SYG:SYD:(A;ID;FA;;;SY)", "D:AI(A;ID;FA;;;SY)", true},
		{"protected metadata", "D:P(A;;FA;;;SY)", "D:PAI(A;;FA;;;SY)", true},
		{"protection changed", "D:P(A;;FA;;;SY)", "D:AI(A;;FA;;;SY)", false},
		{"access broadened", "D:P(A;;FR;;;SY)", "D:P(A;;FA;;;SY)", false},
		{"principal added", "D:P(A;;FA;;;SY)", "D:P(A;;FA;;;SY)(A;;FR;;;WD)", false},
		{"inheritance changed", "D:P(A;;FA;;;SY)", "D:P(A;OI;FA;;;SY)", false},
		{"ACE order changed", "D:P(A;;FA;;;SY)(D;;FR;;;WD)", "D:P(D;;FR;;;WD)(A;;FA;;;SY)", false},
		{"invalid descriptor", "D:P(A;;FA;;;SY)", "not a descriptor", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceProfileSecurityEqual(tc.a, tc.b); got != tc.equal {
				t.Fatalf("security equality = %v, want %v", got, tc.equal)
			}
			if got := serviceProfileSecurityEqual(tc.b, tc.a); got != tc.equal {
				t.Fatalf("reverse security equality = %v, want %v", got, tc.equal)
			}
		})
	}
}
