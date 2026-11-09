package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// Only a different kernel boot on the same installed machine proves that an
// orphaned local worker and all of its descendants can no longer be running.
// Wall clocks, elapsed time and a root PID disappearing do not prove that.
// Both identities are hashed before they enter the persisted session record.
type deepSeekBootProof struct {
	Host string `json:"host"`
	Boot string `json:"boot"`
}

func (p deepSeekBootProof) valid() bool {
	for _, value := range []string{p.Host, p.Boot} {
		if len(value) != sha256.Size*2 {
			return false
		}
		if _, err := hex.DecodeString(value); err != nil {
			return false
		}
	}
	return true
}

func (p deepSeekBootProof) provesRestart(previous deepSeekBootProof) bool {
	return p.valid() && previous.valid() && p.Host == previous.Host && p.Boot != previous.Boot
}

func deepSeekBootFromReaders(platform string, readHost, readBoot func() (string, error)) deepSeekBootProof {
	host, err := readHost()
	if err != nil {
		return deepSeekBootProof{}
	}
	boot, err := readBoot()
	if err != nil {
		return deepSeekBootProof{}
	}
	return deepSeekBootFromIDs(platform, host, boot)
}

func deepSeekBootFromIDs(platform, host, boot string) deepSeekBootProof {
	host, boot = deepSeekCanonicalBootID(host), deepSeekCanonicalBootID(boot)
	if platform == "" || host == "" || boot == "" {
		return deepSeekBootProof{}
	}
	hostHash := sha256.Sum256([]byte("salcara-deepseek-host-v1\x00" + platform + "\x00" + host))
	bootHash := sha256.Sum256([]byte("salcara-deepseek-boot-v1\x00" + platform + "\x00" + boot))
	return deepSeekBootProof{Host: hex.EncodeToString(hostHash[:]), Boot: hex.EncodeToString(bootHash[:])}
}

// The supported platform APIs return UUIDs, or a 32-hex Linux machine-id.
// Reject missing/default/malformed identifiers instead of accepting a weak
// fallback such as hostname, installation path, or a boot-time estimate.
func deepSeekCanonicalBootID(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.HasPrefix(value, "{") && strings.HasSuffix(value, "}") {
		value = value[1 : len(value)-1]
	}
	if len(value) == 36 {
		if value[8] != '-' || value[13] != '-' || value[18] != '-' || value[23] != '-' {
			return ""
		}
		value = strings.ReplaceAll(value, "-", "")
	}
	if len(value) != 32 || value == strings.Repeat("0", 32) || value == strings.Repeat("f", 32) {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return value
}
