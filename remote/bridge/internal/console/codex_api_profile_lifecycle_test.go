package console

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

// Pause after Store has published a transition but before the initiating
// handler can persist/restart/return. The callback never holds the Store lock.
func profileLifecycleBarrier(t *testing.T, s *Server, matches func(config.Config, config.Config) bool) (<-chan struct{}, func()) {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var captured atomic.Bool
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	s.d.Store.OnChange(func(old, next config.Config) {
		if matches(old, next) && captured.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	})
	return entered, unblock
}

func awaitProfileLifecycleSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(10 * time.Second):
		t.Fatal("lifecycle request did not reach or leave its barrier")
	}
}

func profileLifecycleRequest(t *testing.T, s *Server, h http.Handler, path string, body any, unblock func()) <-chan *httptest.ResponseRecorder {
	t.Helper()
	result := make(chan *httptest.ResponseRecorder, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result <- localRequest(s, h, http.MethodPost, path, body)
	}()
	t.Cleanup(func() {
		unblock()
		awaitProfileLifecycleSignal(t, done)
	})
	return result
}

func awaitProfileLifecycleResponse(t *testing.T, result <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case response := <-result:
		return response
	case <-time.After(10 * time.Second):
		t.Fatal("lifecycle request did not finish")
		return nil
	}
}

func profileLifecycleNetworkBody(c config.Config) map[string]any {
	// Empty Ports exercises the full stop/restart transaction without opening
	// a listening socket or contacting any upstream provider.
	return map[string]any{"bindScope": "loopback", "ports": []int{}, "clientHost": "localhost", "revision": apiNetworkRevision(c.CodexAPI.AuthEpoch)}
}

func profileLifecycleRetirement(old, next config.Config) bool {
	return old.CodexAPIActive() && !next.CodexAPI.Enabled && next.CodexAPI.RuntimeLease == ""
}

func TestCodexServiceProfileLifecycleNetworkRestartRejectsConcurrentStop(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	t.Cleanup(s.CloseAPIListeners)
	before := s.d.Store.Get()
	entered, unblock := profileLifecycleBarrier(t, s, profileLifecycleRetirement)
	network := profileLifecycleRequest(t, s, h, "/api/local/codex-api/network", profileLifecycleNetworkBody(before), unblock)
	awaitProfileLifecycleSignal(t, entered)
	retired := s.d.Store.Get()
	if retired.CodexAPIActive() || retired.CodexAPI.AuthEpoch == before.CodexAPI.AuthEpoch {
		t.Fatal("network request did not pause after runtime retirement")
	}
	stop := localRequest(s, h, http.MethodPost, "/api/local/codex-api/enabled", map[string]bool{"enabled": false})
	if stop.Code != http.StatusConflict {
		t.Fatalf("stop during network restart reported %d, want conflict", stop.Code)
	}
	if current := s.d.Store.Get(); current.CodexAPI.AuthEpoch != retired.CodexAPI.AuthEpoch || current.CodexAPIActive() || !reflect.DeepEqual(current.CodexAPI.Network, before.CodexAPI.Network) {
		t.Fatal("rejected stop mutated the in-progress network transaction")
	}
	unblock()
	if response := awaitProfileLifecycleResponse(t, network); response.Code != http.StatusOK {
		t.Fatalf("network transaction failed after release: %d", response.Code)
	}
	if current := s.d.Store.Get(); !current.CodexAPIActive() || current.CodexAPI.Network.ClientHost != "localhost" || len(current.CodexAPI.Network.Ports) != 0 {
		t.Fatal("network restart did not commit the requested socket-free settings")
	}
	stop = localRequest(s, h, http.MethodPost, "/api/local/codex-api/enabled", map[string]bool{"enabled": false})
	if stop.Code != http.StatusOK || s.d.Store.Get().CodexAPIActive() {
		t.Fatalf("stop after completed network restart failed: %d", stop.Code)
	}
	if got := codexAPIAdmissionStatus(s, runtimeRevokeClientKey); got != http.StatusServiceUnavailable {
		t.Fatalf("a successful final stop left request admission active: %d", got)
	}
}

func TestCodexServiceProfileLifecycleStopRejectsConcurrentNetworkRestart(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	s, h := newTestServer(t)
	seedCodexAPIRuntime(t, s)
	t.Cleanup(s.CloseAPIListeners)
	before := s.d.Store.Get()
	entered, unblock := profileLifecycleBarrier(t, s, profileLifecycleRetirement)
	stop := profileLifecycleRequest(t, s, h, "/api/local/codex-api/enabled", map[string]bool{"enabled": false}, unblock)
	awaitProfileLifecycleSignal(t, entered)
	retired := s.d.Store.Get()
	// Use the current retirement revision so a stale-revision rejection cannot
	// accidentally conceal a missing lifecycle mutex.
	network := localRequest(s, h, http.MethodPost, "/api/local/codex-api/network", profileLifecycleNetworkBody(retired))
	if network.Code != http.StatusConflict {
		t.Fatalf("network restart entered an in-progress stop: %d", network.Code)
	}
	start := localRequest(s, h, http.MethodPost, "/api/local/codex-api/enabled", map[string]bool{"enabled": true})
	if start.Code != http.StatusConflict {
		t.Fatalf("start entered an in-progress stop: %d", start.Code)
	}
	if current := s.d.Store.Get(); current.CodexAPI.AuthEpoch != retired.CodexAPI.AuthEpoch || current.CodexAPIActive() || !reflect.DeepEqual(current.CodexAPI.Network, before.CodexAPI.Network) {
		t.Fatal("rejected lifecycle operation changed the stopped snapshot")
	}
	unblock()
	if response := awaitProfileLifecycleResponse(t, stop); response.Code != http.StatusOK || s.d.Store.Get().CodexAPIActive() {
		t.Fatalf("stop did not complete while preserving retirement: %d", response.Code)
	}
	network = localRequest(s, h, http.MethodPost, "/api/local/codex-api/network", profileLifecycleNetworkBody(s.d.Store.Get()))
	if network.Code != http.StatusOK || s.d.Store.Get().CodexAPIActive() || s.d.Store.Get().CodexAPI.Enabled {
		t.Fatalf("later network settings revived a successfully stopped service: %d", network.Code)
	}
}

func TestCodexServiceProfileLifecycleApplyRejectsConcurrentToggles(t *testing.T) {
	for _, reapply := range []bool{false, true} {
		t.Run(map[bool]string{false: "apply", true: "reapply"}[reapply], func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			s, h, home, original := codexProfileFixture(t)
			t.Cleanup(s.CloseAPIListeners)
			if reapply {
				applyCodexProfile(t, s, h)
			}
			before := readCodexProfileView(t, s, h)
			entered, unblock := profileLifecycleBarrier(t, s, func(old, next config.Config) bool {
				return next.CodexServiceProfile.Lease != "" && next.CodexServiceProfile.Lease != old.CodexServiceProfile.Lease
			})
			apply := profileLifecycleRequest(t, s, h, "/api/local/codex-api/profile/apply", codexProfileApplyBody(t, before), unblock)
			awaitProfileLifecycleSignal(t, entered)
			armed := s.d.Store.Get()
			if armed.CodexServiceProfileCredential() == "" || armed.CodexServiceProfile.Generation == before.Profile.Generation {
				t.Fatal("profile did not pause inside its new arm callback")
			}
			for _, enabled := range []bool{false, true} {
				response := localRequest(s, h, http.MethodPost, "/api/local/codex-api/enabled", map[string]bool{"enabled": enabled})
				if response.Code != http.StatusConflict {
					t.Fatalf("toggle enabled=%t entered profile transaction: %d", enabled, response.Code)
				}
			}
			network := localRequest(s, h, http.MethodPost, "/api/local/codex-api/network", profileLifecycleNetworkBody(armed))
			if network.Code != http.StatusConflict {
				t.Fatalf("network restart entered profile transaction: %d", network.Code)
			}
			if current := s.d.Store.Get(); !current.CodexAPIActive() || current.CodexAPI.AuthEpoch != armed.CodexAPI.AuthEpoch || current.CodexServiceProfile != armed.CodexServiceProfile {
				t.Fatal("rejected toggle retired the accepted profile generation")
			}
			unblock()
			if response := awaitProfileLifecycleResponse(t, apply); response.Code != http.StatusOK {
				t.Fatalf("profile transaction did not finish after release: %d", response.Code)
			}
			current := readCodexProfileView(t, s, h)
			if !current.Active || current.Profile.Generation != armed.CodexServiceProfile.Generation {
				t.Fatal("profile completion lost the generation protected by the lifecycle lock")
			}
			stop := localRequest(s, h, http.MethodPost, "/api/local/codex-api/enabled", map[string]bool{"enabled": false})
			if stop.Code != http.StatusOK || s.d.Store.Get().CodexAPIActive() || s.d.Store.Get().CodexServiceProfileCredential() != "" {
				t.Fatalf("stop after profile commit failed: %d", stop.Code)
			}
			assertCodexProfileFiles(t, home, original)
		})
	}
}
