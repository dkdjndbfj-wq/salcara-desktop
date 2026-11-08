package config

import (
	"net/http"
	"testing"
)

func TestAccountHeadersKeepAuthenticationAndFraming(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "https://example.test", nil)
	r.Header.Set("Authorization", "Bearer selected")
	r.Header.Set("x-api-key", "selected-api-key")
	ApplyAccountHeaders(r, LocalAccount{ExtraHeaders: map[string]string{
		" AUTHORIZATION ": "Bearer other", "X-API-Key": "other", "Host": "other.example", "Connection": "upgrade", "Content-Length": "123",
		" X-Channel ": " custom ", "anthropic-beta": "fixture-beta",
	}})
	if r.Header.Get("Authorization") != "Bearer selected" || r.Header.Get("x-api-key") != "selected-api-key" || r.Header.Get("Host") != "" || r.Header.Get("Connection") != "" || r.Header.Get("Content-Length") != "" {
		t.Fatal("extra headers replaced authentication or framing")
	}
	if r.Header.Get("X-Channel") != "custom" || r.Header.Get("anthropic-beta") != "fixture-beta" {
		t.Fatal("channel headers missing")
	}
}
