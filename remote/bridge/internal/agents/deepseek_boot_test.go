package agents

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const deepSeekTestHost = "acbd18db-4cc2-485c-8def-654fccc4a4d8"
const deepSeekTestBootA = "37b51d19-4a75-43e4-8345-6f652d2d635a"
const deepSeekTestBootB = "73feffa4-b7f6-4bb6-8e44-cf984c85f6e8"

func TestDeepSeekBootProofRequiresSameMachineAndDifferentKernelBoot(t *testing.T) {
	old := deepSeekBootFromIDs("windows", deepSeekTestHost, deepSeekTestBootA)
	if !old.valid() {
		t.Fatal("valid OS identifiers did not produce a proof")
	}
	for _, tc := range []struct {
		name string
		now  deepSeekBootProof
		want bool
	}{
		{"same boot", old, false},
		{"restarted same machine", deepSeekBootFromIDs("windows", deepSeekTestHost, deepSeekTestBootB), true},
		{"copied state to another machine", deepSeekBootFromIDs("windows", deepSeekTestBootB, deepSeekTestBootB), false},
		{"copied state to another OS", deepSeekBootFromIDs("linux", deepSeekTestHost, deepSeekTestBootB), false},
		{"unavailable boot probe", deepSeekBootProof{}, false},
		{"malformed persisted hash", deepSeekBootProof{Host: old.Host, Boot: "not-a-boot"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.now.provesRestart(old); got != tc.want {
				t.Fatalf("restart proof = %v, want %v", got, tc.want)
			}
		})
	}
	if old.provesRestart(deepSeekBootProof{}) {
		t.Fatal("legacy record without proof was unlocked")
	}
	encoded, _ := json.Marshal(old)
	if strings.Contains(string(encoded), deepSeekTestHost) || strings.Contains(string(encoded), deepSeekCanonicalBootID(deepSeekTestHost)) || strings.Contains(string(encoded), deepSeekTestBootA) {
		t.Fatal("raw machine or boot identifier was persisted")
	}
	if normalized := deepSeekBootFromIDs("windows", "{"+strings.ToUpper(deepSeekTestHost)+"}\r\n", deepSeekCanonicalBootID(deepSeekTestBootA)); normalized != old {
		t.Fatal("formatting differences invented a different boot or machine")
	}
}

func TestDeepSeekBootProbeRejectsFailuresAndDefaultIDs(t *testing.T) {
	good := func() (string, error) { return deepSeekTestHost, nil }
	failed := func() (string, error) { return deepSeekTestHost, errors.New("OS probe unavailable") }
	for _, readers := range [][2]func() (string, error){{failed, good}, {good, failed}} {
		if p := deepSeekBootFromReaders("linux", readers[0], readers[1]); p != (deepSeekBootProof{}) {
			t.Fatal("OS read error did not fail closed")
		}
	}
	for _, bad := range []string{"", "localhost", "2026-11-09T12:00:00Z", "1234", strings.Repeat("0", 32), strings.Repeat("f", 32), "zzbd18db4cc2485c8def654fccc4a4d8", "acbd18db--4cc2-485c-8def654fccc4a4d8"} {
		if deepSeekBootFromIDs("linux", bad, deepSeekTestBootA).valid() || deepSeekBootFromIDs("linux", deepSeekTestHost, bad).valid() {
			t.Errorf("accepted invalid machine/boot identity %q", bad)
		}
	}
}

func TestDeepSeekCurrentBootReturnsStableProofOrUnknown(t *testing.T) {
	first, second := deepSeekCurrentBoot(), deepSeekCurrentBoot()
	t.Logf("native boot proof available: %v", first.valid())
	if first != (deepSeekBootProof{}) && !first.valid() || second != (deepSeekBootProof{}) && !second.valid() {
		t.Fatal("OS probe returned partial proof")
	}
	if first.valid() && second.valid() && first != second {
		t.Fatal("OS identity changed without a machine restart")
	}
}
