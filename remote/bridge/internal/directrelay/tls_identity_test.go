package directrelay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadOrCreateTLSIdentityPersistsPinnedCertificateAndSANs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "direct-identity.pem")
	hosts := []string{"192.168.4.20", "localhost", "::1", "192.168.4.20"}
	first, err := LoadOrCreateTLSIdentity(path, hosts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateTLSIdentity(path, []string{"::1", "localhost", "192.168.4.20"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint == "" || first.Fingerprint != second.Fingerprint {
		t.Fatalf("fingerprint did not persist: first=%q second=%q", first.Fingerprint, second.Fingerprint)
	}
	if len(first.Certificate.Certificate) != 1 {
		t.Fatalf("certificate chain length = %d, want self-signed leaf", len(first.Certificate.Certificate))
	}
	parsed, err := x509.ParseCertificate(first.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"192.168.4.20", "localhost", "::1"} {
		if err := parsed.VerifyHostname(host); err != nil {
			t.Fatalf("certificate does not verify SAN %q: %v", host, err)
		}
	}
	if err := parsed.VerifyHostname("192.168.4.21"); err == nil {
		t.Fatal("certificate unexpectedly accepts an unlisted address")
	}
	hash := sha256.Sum256(parsed.Raw)
	if got, want := first.Fingerprint, hex.EncodeToString(hash[:]); got != want {
		t.Fatalf("fingerprint = %q, want SHA-256 of exact DER %q", got, want)
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("identity bundle not atomically persisted: info=%v err=%v", info, err)
	}
}

func TestTLSIdentityRejectsChangedSANWithoutReplacingPin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "direct-identity.pem")
	first, err := LoadOrCreateTLSIdentity(path, []string{"192.168.4.20"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateTLSIdentity(path, []string{"192.168.4.21"}); err == nil {
		t.Fatal("changed local endpoint unexpectedly replaced a pinned identity")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("failed SAN validation changed the saved private identity")
	}
	second, err := LoadOrCreateTLSIdentity(path, []string{"192.168.4.20"})
	if err != nil || first.Fingerprint != second.Fingerprint {
		t.Fatalf("original pin not retained after rejected host change: second=%#v err=%v", second, err)
	}
}

func TestLoadOrCreateTLSIdentityRejectsMalformedHosts(t *testing.T) {
	for _, hosts := range [][]string{
		nil,
		{""},
		{"wss://192.168.4.20/direct/ws"},
		{"example.com"},
		{"192.168.4.20:47832"},
		{"fe80::1%eth0"},
	} {
		t.Run(hex.EncodeToString([]byte(joinHosts(hosts))), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "identity.pem")
			if _, err := LoadOrCreateTLSIdentity(path, hosts); err == nil {
				t.Fatalf("unexpectedly accepted hosts %#v", hosts)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid hosts left an identity file: %v", err)
			}
		})
	}
}

func TestLoadOrCreateTLSIdentityRejectsInvalidAndMismatchedBundles(t *testing.T) {
	t.Run("malformed PEM", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "identity.pem")
		if err := os.WriteFile(path, []byte("not a certificate"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateTLSIdentity(path, []string{"localhost"}); err == nil {
			t.Fatal("malformed existing identity was replaced")
		}
	})

	t.Run("mismatched key", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "identity.pem")
		if _, err := LoadOrCreateTLSIdentity(path, []string{"localhost"}); err != nil {
			t.Fatal(err)
		}
		bundle, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, rest := pem.Decode(bundle)
		keyBlock, rest := pem.Decode(rest)
		if keyBlock == nil {
			t.Fatal("generated bundle has no key")
		}
		other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		der, err := x509.MarshalECPrivateKey(other)
		if err != nil {
			t.Fatal(err)
		}
		mutated := append(pem.EncodeToMemory(mustCertificateBlock(t, bundle)), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})...)
		mutated = append(mutated, rest...)
		if err := os.WriteFile(path, mutated, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateTLSIdentity(path, []string{"localhost"}); err == nil {
			t.Fatal("certificate with a different key was accepted")
		}
	})
}

func TestLoadOrCreateTLSIdentityProtectsExistingPrivateBundle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows DACL coverage is exercised by tls_permissions_windows_test.go")
	}
	path := filepath.Join(t.TempDir(), "identity.pem")
	if _, err := LoadOrCreateTLSIdentity(path, []string{"localhost"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateTLSIdentity(path, []string{"localhost"}); err == nil {
		t.Fatal("existing private key with broad permissions was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateTLSIdentity(path, []string{"localhost"}); err != nil {
		t.Fatal("secure existing private key rejected", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("private bundle mode = %v, err=%v; want 0600", info.Mode().Perm(), err)
	}
}

func TestLoadOrCreateTLSIdentityRejectsNonRegularPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "identity.pem")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateTLSIdentity(path, []string{"localhost"}); err == nil {
		t.Fatal("directory path was accepted as an identity bundle")
	}
}

func mustCertificateBlock(t *testing.T, bundle []byte) *pem.Block {
	t.Helper()
	block, _ := pem.Decode(bundle)
	if block == nil || block.Type != "CERTIFICATE" {
		t.Fatal("bundle has no certificate block")
	}
	return block
}

func joinHosts(hosts []string) string {
	joined := ""
	for _, host := range hosts {
		joined += host + "|"
	}
	return joined
}

// Keep net imported in this test as an independent check that IPv6 SAN
// canonicalization follows Go's standard IP parser.
func TestTLSIdentityNormalizesIPv6SAN(t *testing.T) {
	got, err := normalizeTLSHosts([]string{"0:0:0:0:0:0:0:1"})
	if err != nil || len(got) != 1 || net.ParseIP(got[0]) == nil || got[0] != "::1" {
		t.Fatalf("normalized IPv6 host = %#v, err=%v", got, err)
	}
}
