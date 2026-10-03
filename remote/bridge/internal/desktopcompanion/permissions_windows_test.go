//go:build windows

package desktopcompanion

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsBackupsHaveProtectedOwnerAndSystemDACL(t *testing.T) {
	s, o := fixture(t, "model = \"fixture-private\"\n")
	if _, err := s.Install(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(o.DataDir, "desktop-companion", "backups", "*.toml"))
	if err != nil || len(files) != 2 {
		t.Fatal("backup missing")
	}
	archives, err := filepath.Glob(filepath.Join(o.DataDir, "desktop-companion", "archives", "*.json"))
	if err != nil || len(archives) != 1 {
		t.Fatal("ownership archive missing")
	}
	var token windows.Token
	if err = windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{filepath.Join(o.DataDir, "desktop-companion"), filepath.Join(o.DataDir, "desktop-companion", "backups"), filepath.Join(o.DataDir, "desktop-companion", "archives"), filepath.Join(o.CodexHome, "config.toml")}
	paths = append(paths, files...)
	paths = append(paths, archives...)
	for _, path := range paths {
		sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		owner, _, err := sd.Owner()
		if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
			t.Fatal("private file owner is not the actual installing user")
		}
		control, _, err := sd.Control()
		if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
			t.Fatal("private ACL inherited broader access")
		}
		dacl, _, err := sd.DACL()
		if err != nil || dacl == nil || dacl.AceCount != 2 {
			t.Fatal("private ACL granted access beyond the two intended principals")
		}
		identities := map[string]bool{}
		for index := uint32(0); index < uint32(dacl.AceCount); index++ {
			var ace *windows.ACCESS_ALLOWED_ACE
			if err := windows.GetAce(dacl, index, &ace); err != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE || ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 {
				t.Fatal("private ACL has a denied or ineffective ACE")
			}
			identities[(*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()] = true
		}
		if len(identities) != 2 || !identities[user.User.Sid.String()] || !identities["S-1-5-18"] {
			t.Fatal("backup ACL did not restrict access to actual user and SYSTEM")
		}
		if err := checkPrivatePermissions(path); err != nil {
			t.Fatal("installed private permissions are not accepted by the read-only verifier", err)
		}
	}
}

func TestWindowsPrivateWriterSetsActualUserOwnerBeforeContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-install-fixture.json")
	if err := writeExclusive(path, []byte(`{"fixture":"not a real credential"}`)); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivatePermissions(path); err != nil {
		t.Fatal("private writer and verifier disagree on file ownership", err)
	}
	if err := atomicWrite(path, []byte(`{"fixture":"replacement"}`)); err != nil {
		t.Fatal(err)
	}
	if err := checkPrivatePermissions(path); err != nil {
		t.Fatal("atomic replacement lost private user ownership", err)
	}
}

func TestWindowsPrivatePermissionVerifierUsesSIDNotSDDLAliases(t *testing.T) {
	// Any identity may be formatted as an alias when Windows knows it. The
	// verifier takes the token user SID, not an SDDL representation of it.
	userSID, err := windows.StringToSid("S-1-5-21-111-222-333-500")
	if err != nil {
		t.Fatal(err)
	}
	owner := userSID.String()
	for name, descriptor := range map[string]string{
		"file":      "O:" + owner + "D:P(A;;FA;;;SY)(A;;FA;;;" + owner + ")",
		"directory": "O:" + owner + "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;" + owner + ")",
	} {
		t.Run(name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(descriptor)
			if err != nil || checkPrivateSecurityDescriptor(sd, userSID) != nil {
				t.Fatal("valid SID-restricted descriptor rejected", err)
			}
		})
	}
	// Exercise an unambiguous well-known alias in a synthetic descriptor,
	// without assigning a real file to this group or changing process identity.
	aliasSID, err := windows.StringToSid("S-1-5-32-544")
	if err != nil {
		t.Fatal(err)
	}
	aliasDescriptor, err := windows.SecurityDescriptorFromString("O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil || !strings.Contains(aliasDescriptor.String(), ";;;BA)") || checkPrivateSecurityDescriptor(aliasDescriptor, aliasSID) != nil {
		t.Fatal("verifier relied on literal SID text instead of ACE identity", err)
	}
}

func TestWindowsPrivatePermissionVerifierRejectsForeignOrBroadenedDescriptor(t *testing.T) {
	userSID, err := windows.StringToSid("S-1-5-21-111-222-333-1001")
	if err != nil {
		t.Fatal(err)
	}
	owner := userSID.String()
	for name, descriptor := range map[string]string{
		"foreign owner":   "O:BAD:P(A;;FA;;;SY)(A;;FA;;;" + owner + ")",
		"unprotected":     "O:" + owner + "D:(A;;FA;;;SY)(A;;FA;;;" + owner + ")",
		"broader group":   "O:" + owner + "D:P(A;;FA;;;SY)(A;;FA;;;BA)",
		"third principal": "O:" + owner + "D:P(A;;FA;;;SY)(A;;FA;;;" + owner + ")(A;;FR;;;WD)",
		"denied ACE":      "O:" + owner + "D:P(D;;FA;;;SY)(A;;FA;;;" + owner + ")",
		"inherit only":    "O:" + owner + "D:P(A;OICIIO;FA;;;SY)(A;;FA;;;" + owner + ")",
		"duplicate user":  "O:" + owner + "D:P(A;;FA;;;" + owner + ")(A;;FA;;;" + owner + ")",
	} {
		t.Run(name, func(t *testing.T) {
			sd, err := windows.SecurityDescriptorFromString(descriptor)
			if err != nil {
				t.Fatal("invalid synthetic descriptor", err)
			}
			if err := checkPrivateSecurityDescriptor(sd, userSID); err == nil {
				t.Fatal("foreign/broadened descriptor accepted")
			}
		})
	}
}

func TestWindowsPrivateReadNeverRepairsBroaderPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "foreign-descriptor-fixture.json")
	if err := os.WriteFile(path, []byte(`{"fixture":"foreign ACL"}`), 0600); err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkPrivatePermissions(path); err == nil {
		t.Fatal("read accepted a foreign/broad descriptor")
	}
	after, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil || before.String() != after.String() {
		t.Fatal("read verifier modified descriptor permissions", err)
	}
}
