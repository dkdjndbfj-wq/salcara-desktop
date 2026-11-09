//go:build windows

package agents

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// SYSTEM_BOOT_ENVIRONMENT_INFORMATION has a kernel-generated BootIdentifier.
// Unlike LastBootUpTime or uptime subtraction, clock changes cannot invent a
// restart. The GUID remains unchanged while the same kernel is resumed.
type deepSeekWindowsBootEnvironment struct {
	BootIdentifier windows.GUID
	FirmwareType   uint32
	_              uint32
	BootFlags      uint64
}

func deepSeekCurrentBoot() deepSeekBootProof {
	return deepSeekBootFromReaders("windows", deepSeekWindowsMachineID, func() (string, error) {
		return deepSeekWindowsBootID(windows.NtQuerySystemInformation)
	})
}

func deepSeekWindowsMachineID() (string, error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Cryptography`, registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return "", err
	}
	defer key.Close()
	value, _, err := key.GetStringValue("MachineGuid")
	return value, err
}

func deepSeekWindowsBootID(query func(int32, unsafe.Pointer, uint32, *uint32) error) (string, error) {
	var info deepSeekWindowsBootEnvironment
	var returned uint32
	if err := query(windows.SystemBootEnvironmentInformation, unsafe.Pointer(&info), uint32(unsafe.Sizeof(info)), &returned); err != nil {
		return "", err
	}
	// Older kernels may return only GUID + firmware type. A missing GUID or an
	// unexpected partial response cannot authorize recovery.
	if returned < uint32(unsafe.Sizeof(info.BootIdentifier))+4 || returned > uint32(unsafe.Sizeof(info)) {
		return "", errors.New("incomplete Windows boot identity")
	}
	value := info.BootIdentifier.String()
	if deepSeekCanonicalBootID(value) == "" {
		return "", errors.New("invalid Windows boot identity")
	}
	return value, nil
}
