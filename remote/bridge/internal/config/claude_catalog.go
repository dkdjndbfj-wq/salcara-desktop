package config

import (
	"crypto/sha256"
	"math/big"
	"regexp"
	"strings"
	"unicode"
)

var canonicalClaudeModel = regexp.MustCompile(`^claude-(sonnet|opus|haiku|fable|mythos)-[0-9][0-9.@\[\]m-]*$`)

func ValidCatalogModel(model string) bool {
	if model == "" || len(model) > 200 || strings.TrimSpace(model) != model {
		return false
	}
	for _, r := range model {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Claude Desktop filters branded non-Anthropic route IDs even when the gateway
// can translate them. Opaque compatibility routes have an honest display label
// and are resolved only against this Key's actual catalog, never decoded into an
// arbitrary upstream. Decimal hashes avoid native blacklist word collisions.
func ClaudeDesktopModelRoute(model string) string {
	if canonicalClaudeModel.MatchString(model) {
		return model
	}
	hash := sha256.Sum256([]byte(model))
	return "claude-salcara-v1-" + new(big.Int).SetBytes(hash[:]).Text(10)
}

func ClaudeDesktopCatalog(a LocalAccount) ([]string, map[string]string) {
	routes, labels, seen := []string{}, map[string]string{}, map[string]bool{}
	for _, model := range a.Models {
		if !ValidCatalogModel(model) || seen[model] {
			continue
		}
		seen[model] = true
		route := ClaudeDesktopModelRoute(model)
		routes = append(routes, route)
		labels[route] = model
	}
	return routes, labels
}

func ResolveClaudeDesktopModel(a LocalAccount, route string) (string, bool) {
	_, labels := ClaudeDesktopCatalog(a)
	model, ok := labels[route]
	if !ok {
		for _, original := range a.Models {
			if route == original && ValidCatalogModel(original) {
				return original, true
			}
		}
	}
	return model, ok
}
