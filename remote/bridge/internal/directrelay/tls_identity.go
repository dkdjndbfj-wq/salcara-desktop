package directrelay

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"sort"
	"strings"
	"time"
)

// TLSIdentity is the local server identity advertised by a direct-pairing QR.
// Fingerprint is SHA-256 over the exact DER certificate bytes, not its public
// key. A phone must compare this value before sending its pairing credential.
type TLSIdentity struct {
	Certificate tls.Certificate
	Fingerprint string
	Hosts       []string
}

// LoadOrCreateTLSIdentity loads a private PEM bundle or atomically creates one
// self-signed ECDSA certificate containing the supplied local IP/DNS SANs.
// The single-file bundle keeps certificate and key replacement transactional.
// A changed host set requires re-pairing; the existing identity is never
// silently replaced because doing so would invalidate a phone's pinned trust.
func LoadOrCreateTLSIdentity(bundlePath string, hosts []string) (TLSIdentity, error) {
	if strings.TrimSpace(bundlePath) == "" {
		return TLSIdentity{}, errors.New("direct TLS identity path is empty")
	}
	normalized, err := normalizeTLSHosts(hosts)
	if err != nil {
		return TLSIdentity{}, err
	}

	if info, statErr := os.Lstat(bundlePath); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return TLSIdentity{}, errors.New("direct TLS identity path must be a regular file")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return TLSIdentity{}, fmt.Errorf("inspect direct TLS identity: %w", statErr)
	} else if statErr == nil {
		if err := verifyDirectTLSIdentityFile(bundlePath); err != nil {
			return TLSIdentity{}, fmt.Errorf("direct TLS identity is not private: %w", err)
		}
	}
	bundle, err := os.ReadFile(bundlePath)
	if err == nil {
		return parseTLSIdentity(bundlePath, bundle, normalized)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return TLSIdentity{}, fmt.Errorf("read direct TLS identity: %w", err)
	}

	bundle, err = createTLSIdentityBundle(normalized)
	if err != nil {
		return TLSIdentity{}, err
	}
	if err := writeDirectTLSIdentityFile(bundlePath, bundle); err != nil {
		// Another process may have created the identity between our read and
		// write. Load the winner only if it is a valid, compatible identity.
		if permissionErr := verifyDirectTLSIdentityFile(bundlePath); permissionErr == nil {
			if existing, readErr := os.ReadFile(bundlePath); readErr == nil {
				return parseTLSIdentity(bundlePath, existing, normalized)
			}
		}
		return TLSIdentity{}, fmt.Errorf("save direct TLS identity: %w", err)
	}
	if err := protectDirectTLSIdentityFile(bundlePath); err != nil {
		return TLSIdentity{}, err
	}
	return parseTLSIdentity(bundlePath, bundle, normalized)
}

func normalizeTLSHosts(hosts []string) ([]string, error) {
	set := make(map[string]struct{}, len(hosts))
	for _, raw := range hosts {
		host := strings.TrimSpace(strings.Trim(raw, "[]"))
		if host == "" || strings.ContainsAny(host, "/\\%\x00\r\n\t ") {
			return nil, fmt.Errorf("invalid direct TLS host %q", raw)
		}
		if ip := net.ParseIP(host); ip != nil {
			host = ip.String()
		} else {
			if strings.Contains(host, ":") {
				return nil, fmt.Errorf("invalid direct TLS host %q", raw)
			}
			host = strings.ToLower(strings.TrimSuffix(host, "."))
			if host != "localhost" {
				return nil, fmt.Errorf("direct TLS host %q must be a local IP address or localhost", raw)
			}
		}
		set[host] = struct{}{}
	}
	if len(set) == 0 {
		return nil, errors.New("no direct TLS hosts were supplied")
	}
	result := make([]string, 0, len(set))
	for host := range set {
		result = append(result, host)
	}
	sort.Strings(result)
	return result, nil
}

func createTLSIdentityBundle(hosts []string) ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate direct TLS key: %w", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		return nil, fmt.Errorf("generate direct TLS serial: %w", err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Salcara Direct Device"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(5, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, host)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("create direct TLS certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode direct TLS key: %w", err)
	}
	bundle := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})...)
	return bundle, nil
}

func parseTLSIdentity(path string, bundle []byte, requiredHosts []string) (TLSIdentity, error) {
	certBlock, rest := pem.Decode(bundle)
	if certBlock == nil || certBlock.Type != "CERTIFICATE" || len(certBlock.Headers) != 0 {
		return TLSIdentity{}, errors.New("direct TLS identity has no valid certificate")
	}
	keyBlock, rest := pem.Decode(rest)
	if keyBlock == nil || keyBlock.Type != "EC PRIVATE KEY" || len(keyBlock.Headers) != 0 || len(bytesTrimSpace(rest)) != 0 {
		return TLSIdentity{}, errors.New("direct TLS identity has no valid private key")
	}
	parsed, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return TLSIdentity{}, fmt.Errorf("parse direct TLS certificate: %w", err)
	}
	if !bytes.Equal(parsed.RawIssuer, parsed.RawSubject) || parsed.CheckSignature(parsed.SignatureAlgorithm, parsed.RawTBSCertificate, parsed.Signature) != nil {
		return TLSIdentity{}, errors.New("direct TLS certificate is not self-signed")
	}
	now := time.Now()
	if now.Before(parsed.NotBefore) || !now.Before(parsed.NotAfter) {
		return TLSIdentity{}, errors.New("direct TLS certificate is outside its validity period")
	}
	if !containsServerAuth(parsed.ExtKeyUsage) {
		return TLSIdentity{}, errors.New("direct TLS certificate is not valid for server use")
	}
	if len(parsed.DNSNames)+len(parsed.IPAddresses) == 0 {
		return TLSIdentity{}, errors.New("direct TLS certificate has no subject alternative names")
	}
	for _, host := range requiredHosts {
		if err := parsed.VerifyHostname(host); err != nil {
			return TLSIdentity{}, fmt.Errorf("direct TLS identity does not cover %s; re-pairing is required", host)
		}
	}
	certificate, err := tls.X509KeyPair(bundleCertPEM(certBlock), keyBlockPEM(keyBlock))
	if err != nil {
		return TLSIdentity{}, fmt.Errorf("direct TLS certificate/key mismatch: %w", err)
	}
	privateKey, ok := certificate.PrivateKey.(*ecdsa.PrivateKey)
	if len(certificate.Certificate) != 1 || !ok || privateKey == nil || privateKey.D == nil || privateKey.Curve == nil || privateKey.Curve.Params().Name != "P-256" {
		return TLSIdentity{}, errors.New("direct TLS identity must contain one certificate and one private key")
	}
	fingerprint := sha256.Sum256(parsed.Raw)
	return TLSIdentity{Certificate: certificate, Fingerprint: hex.EncodeToString(fingerprint[:]), Hosts: append([]string(nil), requiredHosts...)}, nil
}

func containsServerAuth(usages []x509.ExtKeyUsage) bool {
	for _, usage := range usages {
		if usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

func validFingerprint(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func bytesTrimSpace(b []byte) []byte { return bytes.TrimSpace(b) }

func bundleCertPEM(block *pem.Block) []byte { return pem.EncodeToMemory(block) }
func keyBlockPEM(block *pem.Block) []byte   { return pem.EncodeToMemory(block) }
