package codexaccounts

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestLoginModeSelectsConfiguredConnectorAndRejectsInvalidMode(t *testing.T) {
	var browserCalls, deviceCalls atomic.Int32
	s, err := New(Options{
		Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{},
		Login: LoginFunc(func(context.Context) (*coreauth.Auth, error) {
			browserCalls.Add(1)
			return sdkFixture("synthetic-browser-access", "synthetic-browser-refresh"), nil
		}),
		DeviceLogin: LoginFunc(func(context.Context) (*coreauth.Auth, error) {
			deviceCalls.Add(1)
			return sdkFixture("synthetic-device-access", "synthetic-device-refresh"), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, mode := range []string{"", LoginModeDevice} {
		login, err := s.StartLogin(context.Background(), LoginRequest{Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		wantMode := mode
		if wantMode == "" {
			wantMode = LoginModeBrowser
		}
		result := waitLogin(t, s, login.ID)
		if result.Status != "complete" || result.Mode != wantMode || result.Account == nil {
			t.Fatalf("mode %q: %+v", mode, result)
		}
	}
	if browserCalls.Load() != 1 || deviceCalls.Load() != 1 {
		t.Fatal("wrong connector selected")
	}
	account := s.List()[0]
	for _, mode := range []string{"unknown", "DEVICE", " device", "browser ", "device\n"} {
		if _, err := s.StartLogin(context.Background(), LoginRequest{Mode: mode, ReplaceID: account.ID}); err != ErrLoginFailed {
			t.Fatalf("invalid mode %q: %v", mode, err)
		}
	}
	current, _ := s.Get(account.ID)
	if current.Epoch != account.Epoch || browserCalls.Load() != 1 || deviceCalls.Load() != 1 {
		t.Fatal("invalid mode changed account or fell back to browser")
	}
}

func TestUnavailableDeviceModeDoesNotFallBackToBrowser(t *testing.T) {
	var calls atomic.Int32
	s, _, _ := newTestService(t, nil, LoginFunc(func(context.Context) (*coreauth.Auth, error) {
		calls.Add(1)
		return sdkFixture("synthetic-access", "synthetic-refresh"), nil
	}))
	if _, err := s.StartLogin(context.Background(), LoginRequest{Mode: LoginModeDevice}); err != ErrLoginFailed {
		t.Fatal("unconfigured device login accepted", err)
	}
	if calls.Load() != 0 {
		t.Fatal("browser silently substituted")
	}
}

type controlledLoginConnector struct {
	entered       chan struct{}
	release       chan struct{}
	cancelled     chan struct{}
	access        string
	callbackCalls atomic.Int32
}

func (c *controlledLoginConnector) Login(ctx context.Context) (*coreauth.Auth, error) {
	return c.LoginWithSessionProgress(ctx, "", nil)
}
func (c *controlledLoginConnector) LoginWithSessionProgress(ctx context.Context, _ string, notify func(LoginProgress)) (*coreauth.Auth, error) {
	if notify != nil {
		notify(LoginProgress{VerificationURL: "https://auth.openai.com/codex/device", UserCode: "SYNTHETIC-CODE"})
	}
	close(c.entered)
	select {
	case <-ctx.Done():
		if c.cancelled != nil {
			close(c.cancelled)
		}
	case <-c.release:
		return sdkFixture(c.access, c.access+"-refresh"), nil
	}
	<-c.release // Deliberately simulate a connector finishing after cancellation.
	return sdkFixture(c.access, c.access+"-refresh"), nil
}
func (c *controlledLoginConnector) SubmitCallback(string, string) error {
	c.callbackCalls.Add(1)
	return nil
}

func TestDeviceSessionRejectsManualCallbackAndLateResultAfterCancel(t *testing.T) {
	device := &controlledLoginConnector{entered: make(chan struct{}), release: make(chan struct{}), access: "synthetic-device"}
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, DeviceLogin: device})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	login, err := s.StartLogin(context.Background(), LoginRequest{Mode: LoginModeDevice})
	if err != nil {
		t.Fatal(err)
	}
	<-device.entered
	if err := s.SubmitLoginCallback(login.ID, "http://localhost:1455/auth/callback?code=fixture&state=fixture"); err != ErrLoginFailed {
		t.Fatal("manual callback accepted for device", err)
	}
	if device.callbackCalls.Load() != 0 {
		t.Fatal("device connector callback invoked")
	}
	if err := s.CancelLogin(login.ID); err != nil {
		t.Fatal(err)
	}
	status, _ := s.LoginStatus(login.ID)
	if status.Status != "cancelled" || status.UserCode != "" || status.VerificationURL != "" {
		t.Fatalf("challenge retained: %+v", status)
	}
	close(device.release)
	s.mu.Lock()
	done := s.logins[login.ID].done
	s.mu.Unlock()
	<-done
	if len(s.List()) != 0 {
		t.Fatal("cancelled device credential saved")
	}
}

func TestCloseWaitsForCancelledLoginConnectorCleanup(t *testing.T) {
	device := &controlledLoginConnector{entered: make(chan struct{}), release: make(chan struct{}), cancelled: make(chan struct{}), access: "synthetic-device"}
	s, err := New(Options{Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, DeviceLogin: device})
	if err != nil {
		t.Fatal(err)
	}
	login, err := s.StartLogin(context.Background(), LoginRequest{Mode: LoginModeDevice})
	if err != nil {
		t.Fatal(err)
	}
	<-device.entered
	if err := s.CancelLogin(login.ID); err != nil {
		t.Fatal(err)
	}
	<-device.cancelled
	closed := make(chan struct{})
	go func() { s.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("close returned before cancelled login cleaned up")
	case <-time.After(20 * time.Millisecond):
	}
	close(device.release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not finish after cleanup")
	}
	if len(s.List()) != 0 {
		t.Fatal("shutdown saved late credentials")
	}
}

func TestNewBrowserLoginFencesOlderDeviceReplacement(t *testing.T) {
	device := &controlledLoginConnector{entered: make(chan struct{}), release: make(chan struct{}), access: "synthetic-old-device"}
	s, err := New(Options{
		Path: filepath.Join(t.TempDir(), "vault"), Protector: &testProtector{}, DeviceLogin: device,
		Login: LoginFunc(func(context.Context) (*coreauth.Auth, error) {
			return sdkFixture("synthetic-new-browser", "synthetic-new-browser-refresh"), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	account := importFixture(t, s)
	old, err := s.StartLogin(context.Background(), LoginRequest{Mode: LoginModeDevice, ReplaceID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	<-device.entered
	newLogin, err := s.StartLogin(context.Background(), LoginRequest{Mode: LoginModeBrowser, ReplaceID: account.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result := waitLogin(t, s, newLogin.ID); result.Status != "complete" || result.Account == nil || result.Account.ID != account.ID {
		t.Fatalf("replacement failed: %+v", result)
	}
	close(device.release)
	s.mu.Lock()
	done := s.logins[old.ID].done
	s.mu.Unlock()
	<-done
	oldResult, _ := s.LoginStatus(old.ID)
	if oldResult.Status != "cancelled" || oldResult.UserCode != "" {
		t.Fatal("old device session remained live")
	}
	record, err := s.SDKRecord(context.Background(), account.ID)
	if err != nil || record.Metadata["access_token"] != "synthetic-new-browser" || len(s.List()) != 1 {
		t.Fatal("late device result replaced newer browser credentials", err)
	}
}
