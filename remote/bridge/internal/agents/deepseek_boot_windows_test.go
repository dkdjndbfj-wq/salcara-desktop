//go:build windows

package agents

import (
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestDeepSeekWindowsBootProbeFailsClosed(t *testing.T) {
	guid, err := windows.GUIDFromString("{" + deepSeekTestBootA + "}")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		size uint32
		zero bool
		err  error
		ok   bool
	}{
		{"current layout", 32, false, nil, true},
		{"legacy layout", 20, false, nil, true},
		{"missing size", 0, false, nil, false},
		{"truncated GUID", 15, false, nil, false},
		{"unexpected size", 64, false, nil, false},
		{"empty GUID", 32, true, nil, false},
		{"native error", 32, false, errors.New("unsupported kernel query"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := deepSeekWindowsBootID(func(class int32, out unsafe.Pointer, size uint32, returned *uint32) error {
				if class != windows.SystemBootEnvironmentInformation || size != 32 {
					t.Fatal("wrong kernel query or buffer layout")
				}
				if !tc.zero {
					(*deepSeekWindowsBootEnvironment)(out).BootIdentifier = guid
				}
				*returned = tc.size
				return tc.err
			})
			if (err == nil && deepSeekCanonicalBootID(id) != "") != tc.ok {
				t.Fatalf("accepted=%v, want %v", err == nil, tc.ok)
			}
		})
	}
}
