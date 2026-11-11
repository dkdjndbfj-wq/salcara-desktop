package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
)

func TestRemoteDiscoverCanceledRequestDoesNotSaveConnectionOrObservation(t *testing.T) {
	for _, connect := range []bool{false, true} {
		name := "detect"
		if connect {
			name = "connect"
		}
		t.Run(name, func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var upstream *httptest.Server
			upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
				remoteSupportFixturePing(w, remoteSupportObservation(upstream.URL, true), true)
			}))
			defer upstream.Close()
			s, _ := newTestServer(t)
			if err := s.d.Store.Update(func(c *config.Config) error {
				c.ConnectRemote(upstream.URL, upstream.URL+"/salcara-hub")
				c.RemoteConnections[0].RemoteSupport = remoteSupportObservation(upstream.URL, true)
				c.ConnectRemote("https://active.fixture.example", "https://active.fixture.example/salcara-hub")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := s.d.Store.Get()
			body, _ := json.Marshal(map[string]any{"url": upstream.URL, "connect": connect})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, "/api/remote/discover", strings.NewReader(string(body))).WithContext(ctx)
			result := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				s.handleRemoteDiscover(result, req)
				close(done)
			}()
			<-entered
			cancel()
			close(release)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("canceled discovery did not return")
			}
			if !reflect.DeepEqual(before, s.d.Store.Get()) {
				t.Fatal("canceled discovery persisted a connection or changed a saved observation")
			}
			if result.Code == http.StatusOK {
				t.Fatal("canceled discovery reported success")
			}
		})
	}
}
