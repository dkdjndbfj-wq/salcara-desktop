//go:build windows

package toolcfg

import (
	"os"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

func serviceProfilePathKey(path string) string { return strings.ToLower(path) }
func serviceProfileInvalidComponent(part string) bool {
	return strings.ContainsAny(part, ":*?<>|\"") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ")
}
func serviceProfileUnsafeInfo(info os.FileInfo) bool {
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return true
	}
	if data, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
		return data.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return false
}
func serviceProfileCheckLinks(f *os.File) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return err
	}
	if info.NumberOfLinks != 1 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return ErrCodexProfileUnsafe
	}
	return nil
}
func tryServiceProfileLock(f *os.File) (bool, error) {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if err == windows.ERROR_LOCK_VIOLATION || err == windows.ERROR_IO_PENDING {
		return false, nil
	}
	return err == nil, err
}
func unlockServiceProfile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
func serviceProfileStoredMode(mode uint32) uint32 {
	if mode&0200 != 0 {
		return 0666
	}
	return 0444
}
func serviceProfilePrivateSecurity(dir bool) (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	inherit := ""
	if dir {
		inherit = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString("D:PAI(A;" + inherit + ";FA;;;" + user.User.Sid.String() + ")(A;" + inherit + ";FA;;;SY)")
	if err != nil {
		return "", err
	}
	return sd.String(), nil
}
func serviceProfileGetSecurity(f *os.File) (string, error) {
	sd, err := windows.GetSecurityInfo(windows.Handle(f.Fd()), windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return "", err
	}
	return sd.String(), nil
}
func serviceProfileValidSecurity(value string) bool {
	_, err := windows.SecurityDescriptorFromString(value)
	return value != "" && err == nil
}

// Compare the DACL that this transaction actually restores. Older inherited
// descriptors may include owner/group fields even when GetSecurityInfo only
// requests the DACL. SetSecurityInfo can also add AUTO_INHERITED bookkeeping.
// Neither changes access; ACEs, their order/flags, and inheritance protection
// must still match exactly. Keep original descriptors in the journal.
func serviceProfileSecurityEqual(a, b string) bool {
	if a == b {
		return true
	}
	left, err := serviceProfileDACLKey(a)
	if err != nil {
		return false
	}
	right, err := serviceProfileDACLKey(b)
	return err == nil && left == right
}

func serviceProfileDACLKey(value string) (string, error) {
	sd, err := windows.SecurityDescriptorFromString(value)
	if err != nil {
		return "", err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return "", err
	}
	control, _, err := sd.Control()
	if err != nil {
		return "", err
	}
	canonical, err := windows.NewSecurityDescriptor()
	if err != nil {
		return "", err
	}
	if err := canonical.SetDACL(dacl, true, false); err != nil {
		return "", err
	}
	if err := canonical.SetControl(windows.SE_DACL_PROTECTED, control&windows.SE_DACL_PROTECTED); err != nil {
		return "", err
	}
	key := canonical.String()
	runtime.KeepAlive(sd)
	if key == "" {
		return "", ErrCodexProfileUnsafe
	}
	return key, nil
}

func serviceProfileSetSecurity(f *os.File, value string) error {
	sd, err := windows.SecurityDescriptorFromString(value)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	// Open a WRITE_DAC handle, then prove it is the same kernel file before
	// changing permissions. ReOpenFile cannot reliably reopen directory handles
	// on supported Windows versions. Never apply an ACL through a bare path.
	path, err := windows.UTF16PtrFromString(f.Name())
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(path, windows.READ_CONTROL|windows.WRITE_DAC, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	permissionFile := os.NewFile(uintptr(h), f.Name())
	if permissionFile == nil {
		windows.CloseHandle(h)
		return ErrCodexProfileUnsafe
	}
	defer permissionFile.Close()
	before, beforeErr := f.Stat()
	permissionInfo, permissionErr := permissionFile.Stat()
	if beforeErr != nil || permissionErr != nil || !os.SameFile(before, permissionInfo) || serviceProfileUnsafeInfo(permissionInfo) {
		return ErrCodexProfileUnsafe
	}
	flags := windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	control, _, err := sd.Control()
	if err != nil {
		return err
	}
	if control&windows.SE_DACL_PROTECTED != 0 {
		flags = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	return windows.SetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.SECURITY_INFORMATION(flags), nil, nil, dacl, nil)
}

// Files are flushed before atomic rooted replacement. Windows does not expose
// a supported fsync(directory) equivalent; the journal covers process crashes,
// not a promise against storage-controller or power-loss corruption.
func syncServiceProfileDir(*os.Root) error { return nil }
