package hubclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func TestConnectionSyncWithholdsTokensWhenBindingIsRevokedInFlight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ping") {
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "salcara-hub", "capabilities": []string{"pair.connections.v1"}})
			return
		}
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"computerId": in["computerId"], "bindingId": in["bindingId"], "device": map[string]string{"deviceId": in["deviceId"]}})
	}))
	defer server.Close()
	store, _ := config.Open(filepath.Join(t.TempDir(), "config.json"))
	_ = store.Update(func(cfg *config.Config) error {
		cfg.ConnectRemote(server.URL, server.URL+"/backup/salcara-hub")
		cfg.ConnectRemote(server.URL, server.URL+"/active/salcara-hub")
		cfg.PhoneBindingID, cfg.PhoneHash, cfg.ComputerID = strings.Repeat("a", 64), strings.Repeat("b", 64), "physical-pc"
		return nil
	})
	c := New(Options{Store: store, HTTP: server.Client()})
	base := store.Get()
	c.publishPair(PairStatus{DeviceID: base.DeviceID, Paired: true}, eventIdentity(base))
	ctx := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(base), deviceID: base.DeviceID, bindingID: base.PhoneBindingID, phoneHash: base.PhoneHash})
	type result struct {
		data any
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := c.Dispatch(ctx, map[string]any{"type": "remote.connections.sync"})
		done <- result{data, err}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("sync never reached target")
	}
	_, err := store.RevokePhoneBinding(base.PhoneBindingID, base.PhoneHash)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err == nil || result.data != nil {
			t.Fatal("revoked generation received target credentials")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sync did not stop")
	}
	if len(store.Get().PendingPhoneRevokes) != 2 {
		t.Fatal("target cleanup not retained for the revoked generation")
	}
}

func TestConnectionSyncUsesSavedTargetsAndStablePhoneTokens(t *testing.T) {
	var requests []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/offline/") {
			w.WriteHeader(503)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/ping") {
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "salcara-hub", "capabilities": []string{"pair.connections.v1"}})
			return
		}
		var in map[string]string
		if json.NewDecoder(r.Body).Decode(&in) != nil {
			t.Error("invalid sync body")
		}
		if in["deviceId"] != r.Header.Get("X-Salcara-Device-Id") || r.Header.Get("X-Salcara-Device-Secret") == "" {
			t.Error("target-specific identity missing")
		}
		requests = append(requests, in)
		_ = json.NewEncoder(w).Encode(map[string]any{"computerId": in["computerId"], "bindingId": in["bindingId"], "device": map[string]string{"deviceId": in["deviceId"]}})
	}))
	defer server.Close()
	store, err := config.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = store.Update(func(cfg *config.Config) error {
		cfg.ConnectRemote(server.URL, server.URL+"/backup/salcara-hub")
		cfg.ConnectRemote(server.URL, server.URL+"/offline/salcara-hub")
		cfg.ConnectRemote(server.URL, server.URL+"/active/salcara-hub")
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
		cfg.ComputerID = "physical-fixture"
		return nil
	})
	c := New(Options{Store: store, HTTP: server.Client()})
	base := store.Get()
	c.publishPair(PairStatus{DeviceID: base.DeviceID, Paired: true}, eventIdentity(base))
	ctx := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(base), deviceID: base.DeviceID, bindingID: base.PhoneBindingID, phoneHash: base.PhoneHash})
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "remote.connections.sync"}); err == nil {
		t.Fatal("unguarded caller got credentials")
	}
	if _, err := c.Dispatch(ctx, map[string]any{"type": "remote.connections.sync", "hubUrl": "https://injected.example"}); err == nil {
		t.Fatal("injected URL accepted")
	}
	var first string
	for n := 0; n < 2; n++ {
		result, err := c.Dispatch(ctx, map[string]any{"type": "remote.connections.sync"})
		if err != nil {
			t.Fatal(err)
		}
		rows := result.(map[string]any)["connections"].([]phoneConnection)
		if len(rows) != 2 {
			t.Fatal("missing saved stations")
		}
		for _, row := range rows {
			if strings.Contains(row.HubURL, "/offline/") {
				if row.Error == "" || row.Token != "" {
					t.Fatal("offline station reported success")
				}
				continue
			}
			if row.Error != "" || len(row.Token) != 64 {
				t.Fatal("saved station not imported")
			}
			if first != "" && first != row.Token {
				t.Fatal("retry rotated phone token")
			}
			first = row.Token
			for _, saved := range base.RemoteConnections {
				if row.Token == saved.DeviceSecret {
					t.Fatal("device secret exposed to phone")
				}
			}
		}
	}
	if len(requests) != 2 {
		t.Fatalf("sync count %d", len(requests))
	}
	hash := sha256.Sum256([]byte(first))
	if requests[0]["pairHash"] != hex.EncodeToString(hash[:]) || requests[0]["pairHash"] != requests[1]["pairHash"] {
		t.Fatal("phone token and target hash differ")
	}
	_, _ = store.RevokePhoneBinding(base.PhoneBindingID, base.PhoneHash)
	if _, err := c.Dispatch(ctx, map[string]any{"type": "remote.connections.sync"}); err == nil || len(requests) != 2 {
		t.Fatal("revoked phone reached relay sync")
	}
}

func TestConnectionSyncRejectsWrongComputerAcknowledgement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ping") {
			_ = json.NewEncoder(w).Encode(map[string]any{"service": "salcara-hub", "capabilities": []string{"pair.connections.v1"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"computerId": "wrong-computer", "bindingId": strings.Repeat("a", 64), "device": map[string]string{"deviceId": r.Header.Get("X-Salcara-Device-Id")}})
	}))
	defer server.Close()
	store, _ := config.Open(filepath.Join(t.TempDir(), "config.json"))
	_ = store.Update(func(cfg *config.Config) error {
		cfg.ConnectRemote(server.URL, server.URL+"/backup/salcara-hub")
		cfg.ConnectRemote(server.URL, server.URL+"/active/salcara-hub")
		cfg.PhoneBindingID, cfg.PhoneHash = strings.Repeat("a", 64), strings.Repeat("b", 64)
		cfg.ComputerID = "physical-fixture"
		return nil
	})
	c := New(Options{Store: store, HTTP: server.Client()})
	base := store.Get()
	c.publishPair(PairStatus{DeviceID: base.DeviceID, Paired: true}, eventIdentity(base))
	ctx := withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(base), deviceID: base.DeviceID, bindingID: base.PhoneBindingID, phoneHash: base.PhoneHash})
	result, err := c.Dispatch(ctx, map[string]any{"type": "remote.connections.sync"})
	if err != nil {
		t.Fatal(err)
	}
	row := result.(map[string]any)["connections"].([]phoneConnection)[0]
	if row.Token != "" || !strings.Contains(row.Error, "身份不一致") {
		t.Fatal("unverified identity was imported")
	}
}
