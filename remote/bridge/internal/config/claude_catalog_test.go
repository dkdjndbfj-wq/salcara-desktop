package config

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func TestClaudeDesktopRoutesStableHonestScopedAndNoHiddenDefault(t *testing.T) {
	a := LocalAccount{Models: []string{"deepseek-chat", "grok-fixture", "gpt-fixture", "claude-sonnet-4-6", "deepseek-chat"}, Model: "old-hidden-model"}
	routes, labels := ClaudeDesktopCatalog(a)
	if len(routes) != 4 {
		t.Fatal("duplicate/hidden default entered the replacement catalog")
	}
	for _, model := range a.Models {
		route := ClaudeDesktopModelRoute(model)
		if labels[route] != model {
			t.Fatal("label hides actual provider model")
		}
		if !strings.HasPrefix(model, "claude-") && !regexp.MustCompile(`^claude-salcara-v1-[0-9]+$`).MatchString(route) {
			t.Fatal("unsafe native compatibility route")
		}
		if actual, ok := ResolveClaudeDesktopModel(a, route); !ok || actual != model {
			t.Fatal("catalog route did not map to real model")
		}
	}
	if _, ok := ResolveClaudeDesktopModel(a, ClaudeDesktopModelRoute("other-key-model")); ok {
		t.Fatal("route bypassed Key catalog")
	}
	if _, ok := ResolveClaudeDesktopModel(a, "old-hidden-model"); ok {
		t.Fatal("legacy default bypassed catalog")
	}
	if len(ClaudeDesktopModelRoute(strings.Repeat("x", 200))) > 200 {
		t.Fatal("compatibility ID exceeds native model limit")
	}
}

func TestClaudeDesktopCatalogDoesNotSilentlyTruncateAt200(t *testing.T) {
	a := LocalAccount{}
	for i := 0; i < 1000; i++ {
		a.Models = append(a.Models, fmt.Sprintf("deepseek-catalog-%d", i))
	}
	routes, labels := ClaudeDesktopCatalog(a)
	if len(routes) != len(a.Models) || len(labels) != len(a.Models) {
		t.Fatal("replacement model menu silently truncated the Key catalog")
	}
	if actual, ok := ResolveClaudeDesktopModel(a, routes[999]); !ok || actual != a.Models[999] {
		t.Fatal("last model is not routable")
	}
}
