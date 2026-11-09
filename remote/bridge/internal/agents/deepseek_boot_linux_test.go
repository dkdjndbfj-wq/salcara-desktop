//go:build linux

package agents

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeepSeekLinuxBootFileReadIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity")
	if _, err := deepSeekReadBootIDFile(path); err == nil {
		t.Fatal("missing OS identity accepted")
	}
	if err := os.WriteFile(path, []byte(deepSeekTestBootA+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if id, err := deepSeekReadBootIDFile(path); err != nil || deepSeekCanonicalBootID(id) != deepSeekCanonicalBootID(deepSeekTestBootA) {
		t.Fatal("valid kernel UUID not read")
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 129)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := deepSeekReadBootIDFile(path); err == nil {
		t.Fatal("oversized identity accepted")
	}
}
