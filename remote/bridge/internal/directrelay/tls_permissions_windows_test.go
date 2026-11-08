//go:build windows

package directrelay

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsDirectTLSIdentityACLIsOwnerAndSystemOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.pem")
	if err := os.WriteFile(path, []byte("synthetic private bundle"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := protectDirectTLSIdentityFile(path); err != nil {
		t.Fatal(err)
	}
	if err := verifyDirectTLSIdentityFile(path); err != nil {
		t.Fatal("protected identity rejected", err)
	}

	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		t.Fatal(err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil || !owner.Equals(user.User.Sid) {
		t.Fatal("private identity is not owned by current user", err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("private identity DACL is not protected", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		t.Fatal("private identity DACL has unexpected entries", err)
	}

	// A broad inherited/users ACL must be rejected before the PEM is read.
	broad, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	broadDACL, _, err := broad.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, broadDACL, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateTLSIdentity(path, []string{"localhost"}); err == nil {
		t.Fatal("identity with broad DACL was accepted")
	}
}

func TestWindowsDirectTLSIdentityOwnerDoesNotNeedWriteOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.pem")
	if err := os.WriteFile(path, []byte("synthetic private bundle"), 0600); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// The standard Modify grant has no WRITE_OWNER or WRITE_DAC. The owner
	// still has implicit WRITE_DAC and can make its own private key private.
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;SY)(A;;0x1301bf;;;" + user.User.Sid.String() + ")")
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
	if err := protectDirectTLSIdentityFile(path); err != nil {
		t.Fatal("owner could not protect its file without WRITE_OWNER", err)
	}
	if err := verifyDirectTLSIdentityFile(path); err != nil {
		t.Fatal("private owner/SYSTEM ACL was not retained", err)
	}
}
