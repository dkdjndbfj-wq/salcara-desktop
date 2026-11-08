package console

import (
	"net"
	"net/netip"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
	"salcara/bridge/internal/config"
)

// defaultAPIAccountName is an offline display hint, never a credential or
// provider identity. Only the host contributes; paths cannot leak into names.
func defaultAPIAccountName(base string) (string, error) {
	root, _, err := config.APIBase(base)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(root)
	if err != nil {
		return "", err
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	name := host
	if addr, err := netip.ParseAddr(host); err == nil {
		name = addr.String()
		if addr.Is6() && u.Port() == "" {
			name = "[" + name + "]"
		}
	} else {
		// Public suffixes distinguish example.co.uk from co.uk, and hosted
		// tenants such as account.github.io from the shared github.io service.
		if ascii, err := idna.Lookup.ToASCII(host); err == nil {
			name = strings.ToLower(ascii)
		}
		suffix, icann := publicsuffix.PublicSuffix(name)
		if _, err := publicsuffix.EffectiveTLDPlusOne(name); err == nil && (icann || strings.Contains(suffix, ".")) {
			name = strings.TrimSuffix(name, "."+suffix)
		}
		// Strip only conventional transport prefixes while another site label
		// remains. An actual tenant named "api" must not become empty.
		for {
			first, rest, ok := strings.Cut(name, ".")
			if !ok || rest == "" || (first != "www" && first != "api" && first != "gateway" && first != "api-gateway") {
				break
			}
			name = rest
		}
	}
	port := u.Port()
	limit := 80
	if port != "" {
		limit -= len(port) + 1
	}
	if runes := []rune(name); len(runes) > limit {
		name = string(runes[:limit])
	}
	if port != "" {
		if strings.Contains(name, ":") {
			name = net.JoinHostPort(name, port)
		} else {
			name += ":" + port
		}
	}
	return name, nil
}

func applyDefaultAPIAccountName(a *config.LocalAccount) error {
	// Official account naming is backed by its own vault and email metadata.
	if strings.TrimSpace(a.Name) != "" || a.SourceType == "official_account" || a.CredentialRef != "" {
		return nil
	}
	name, err := defaultAPIAccountName(a.BaseURL)
	if err == nil {
		a.Name = name
	}
	return err
}
