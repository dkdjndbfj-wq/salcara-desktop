package config

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"reflect"
	"strings"
)

// ServiceProfileBinding authorizes one completed global Codex configuration
// transaction. Generation identifies its disk receipt; Lease identifies its
// runtime credential. Neither is a Workbench selection or a manual client key.
type ServiceProfileBinding struct {
	ClientID     string
	ClientEpoch  string
	ServiceEpoch string
	Lease        string
	Generation   string
	Model        string
}

func validServiceProfileToken(value string) bool {
	return value != "" && len(value) <= 256 && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

// CodexServiceProfileKeyFor derives a prospective credential without arming it.
// Callers can write this key to disk before publishing the matching generation.
// It returns empty if the exact client/service snapshot is no longer usable.
func (c Config) CodexServiceProfileKeyFor(expectedClient NamedAPIClient, expectedServiceEpoch, lease string) string {
	client, ok := c.APIClient(expectedClient.ID)
	if !ok || !c.CodexAPIActive() || c.GatewayRuntimeKey == "" || c.CodexAPI.ImagesMode == "images_only" ||
		expectedServiceEpoch == "" || c.CodexAPI.AuthEpoch != expectedServiceEpoch ||
		client.AuthEpoch == "" || client.Disabled || ValidateAPIClient(client) != nil ||
		!reflect.DeepEqual(client, expectedClient) || !validServiceProfileToken(lease) {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(c.GatewayRuntimeKey))
	_, _ = mac.Write([]byte(strings.Join([]string{"salcara-service-profile-v1", client.ID, client.AuthEpoch, expectedServiceEpoch, lease}, "\x00")))
	return "sp-salcara-" + hex.EncodeToString(mac.Sum(nil))
}

// CodexServiceProfileClient resolves only a completely committed runtime
// binding. A key that was merely derived for an in-progress file write is inert.
func (c Config) CodexServiceProfileClient() (NamedAPIClient, bool) {
	b := c.CodexServiceProfile
	client, ok := c.APIClient(b.ClientID)
	if !ok || b.ClientEpoch == "" || b.ClientEpoch != client.AuthEpoch ||
		!validServiceProfileToken(b.Generation) || c.CodexServiceProfileKeyFor(client, b.ServiceEpoch, b.Lease) == "" {
		return NamedAPIClient{}, false
	}
	if _, allowed := ResolveAPIClientModel(client, b.Model); !allowed {
		return NamedAPIClient{}, false
	}
	return client, true
}

func (c Config) CodexServiceProfileCredential() string {
	client, ok := c.CodexServiceProfileClient()
	if !ok {
		return ""
	}
	b := c.CodexServiceProfile
	return c.CodexServiceProfileKeyFor(client, b.ServiceEpoch, b.Lease)
}

func (c Config) ResolveCodexServiceProfileClient(key string) (NamedAPIClient, bool) {
	expected := c.CodexServiceProfileCredential()
	if expected == "" || subtle.ConstantTimeCompare([]byte(expected), []byte(key)) != 1 {
		return NamedAPIClient{}, false
	}
	return c.CodexServiceProfileClient()
}

// ArmCodexServiceProfile publishes authorization only after a file transaction
// has its final generation. It compares the full Store-owned client snapshot
// and service epoch under the same lock; no configuration write is required.
func (s *Store) ArmCodexServiceProfile(expectedClient NamedAPIClient, expectedServiceEpoch, lease, generation, model string) bool {
	s.mu.Lock()
	if !validServiceProfileToken(generation) || s.cfg.CodexServiceProfileKeyFor(expectedClient, expectedServiceEpoch, lease) == "" {
		s.mu.Unlock()
		return false
	}
	if _, allowed := ResolveAPIClientModel(expectedClient, model); !allowed {
		s.mu.Unlock()
		return false
	}
	old := s.cfg.Clone()
	s.cfg.CodexServiceProfile = ServiceProfileBinding{ClientID: expectedClient.ID, ClientEpoch: expectedClient.AuthEpoch,
		ServiceEpoch: expectedServiceEpoch, Lease: lease, Generation: generation, Model: model}
	cur := s.cfg.Clone()
	subs := append([]func(old, new Config){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(old, cur)
	}
	return true
}

// RevokeCodexServiceProfile immediately retires only the caller's exact lease,
// including when persistent settings cannot be written. Empty never means all.
func (s *Store) RevokeCodexServiceProfile(lease string) bool {
	s.mu.Lock()
	if lease == "" || s.cfg.CodexServiceProfile.Lease != lease {
		s.mu.Unlock()
		return false
	}
	old := s.cfg.Clone()
	s.cfg.CodexServiceProfile = ServiceProfileBinding{}
	cur := s.cfg.Clone()
	subs := append([]func(old, new Config){}, s.subs...)
	s.mu.Unlock()
	for _, f := range subs {
		f(old, cur)
	}
	return true
}
