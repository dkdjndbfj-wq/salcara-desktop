package console

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/hubclient"
)

func TestPairQRIsLocalAuthorizedExpiresAndInvalidatesOnStationSwitch(t *testing.T) {
	s, h := newTestServer(t)
	c := s.d.Store.Get()
	if err := s.buildQR(c, hubclient.PairInfo{Ticket: strings.Repeat("a", 64), ExpiresAt: time.Now().Add(time.Minute).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, h, "GET", "/api/pair/qr", nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "image/png" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("QR not protected/no-store")
	}
	state := localRequest(s, h, "GET", "/api/state", nil).Body.String()
	for _, private := range []string{c.DeviceSecret, c.GatewayKey, strings.Repeat("a", 64)} {
		if strings.Contains(state, private) {
			t.Fatal("private credential leaked in state")
		}
	}
	_ = s.d.Store.Update(func(c *config.Config) error {
		c.ConnectRemote("https://new.test", "https://new.test/salcara-hub")
		return nil
	})
	if got := localRequest(s, h, "GET", "/api/pair/qr", nil); got.Code != http.StatusGone {
		t.Fatal("old station QR remained usable locally")
	}
	c = s.d.Store.Get()
	_ = s.buildQR(c, hubclient.PairInfo{Ticket: strings.Repeat("a", 64), ExpiresAt: time.Now().Add(-time.Minute).UnixMilli()})
	if got := localRequest(s, h, "GET", "/api/pair/qr", nil); got.Code != http.StatusGone {
		t.Fatal("expired QR remained available")
	}
}
