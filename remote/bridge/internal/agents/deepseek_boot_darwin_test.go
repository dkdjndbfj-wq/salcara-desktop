//go:build darwin

package agents

import (
	"errors"
	"strings"
	"testing"
)

func TestDeepSeekDarwinUsesKernelUUIDAndExactMachineProperty(t *testing.T) {
	for _, tc := range []struct {
		name string
		host string
		boot string
		err  bool
		ok   bool
	}{
		{"valid", `    "IOPlatformUUID" = "` + deepSeekTestHost + `"`, deepSeekTestBootA, false, true},
		{"missing host", `    "IOPlatformSerialNumber" = "not-an-id"`, deepSeekTestBootA, false, false},
		{"duplicate host", `"IOPlatformUUID" = "` + deepSeekTestHost + `"` + "\n" + `"IOPlatformUUID" = "` + deepSeekTestHost + `"`, deepSeekTestBootA, false, false},
		{"clock value forbidden", `"IOPlatformUUID" = "` + deepSeekTestHost + `"`, "{ sec = 1794228564, usec = 0 }", false, false},
		{"command failure", "", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := deepSeekDarwinBoot(func(path string, args ...string) ([]byte, error) {
				if tc.err {
					return nil, errors.New("OS command unavailable")
				}
				switch path {
				case "/usr/sbin/ioreg":
					if strings.Join(args, " ") != "-rd1 -c IOPlatformExpertDevice" {
						t.Fatal("unbounded machine query")
					}
					return []byte(tc.host), nil
				case "/usr/sbin/sysctl":
					if strings.Join(args, " ") != "-n kern.bootsessionuuid" {
						t.Fatal("boot time or untrusted PATH query used")
					}
					return []byte(tc.boot), nil
				default:
					t.Fatal("unexpected process")
					return nil, errors.New("unexpected process")
				}
			})
			if p.valid() != tc.ok {
				t.Fatalf("proof valid=%v, want %v", p.valid(), tc.ok)
			}
		})
	}
}
