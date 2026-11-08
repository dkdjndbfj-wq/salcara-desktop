package codexaccounts

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func waitSDKResult(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("SDK stand-in did not exit")
		return nil
	}
}
func TestSDKBrowserLoginGateWaitsForCancelledListenerCleanup(t *testing.T) {
	firstEntered, cleanup, secondEntered := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var running, max atomic.Int32
	first := SDKLoginConnector{BrowserCallback: true, invoke: func(ctx context.Context, cfg *sdkconfig.Config, opts *sdkauth.LoginOptions) (*coreauth.Auth, error) {
		if cfg.AuthDir != "" || opts.Prompt == nil || opts.Metadata["codex_login_mode"] != "" {
			t.Error("browser SDK options incorrect")
		}
		current := running.Add(1)
		max.Store(current)
		defer running.Add(-1)
		close(firstEntered)
		_, err := opts.Prompt("fixture callback prompt")
		<-cleanup
		return nil, err
	}}
	second := SDKLoginConnector{BrowserCallback: true, invoke: func(context.Context, *sdkconfig.Config, *sdkauth.LoginOptions) (*coreauth.Auth, error) {
		current := running.Add(1)
		if current > max.Load() {
			max.Store(current)
		}
		defer running.Add(-1)
		close(secondEntered)
		return sdkFixture("synthetic-sdk-access", "synthetic-sdk-refresh"), nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	firstDone := make(chan error, 1)
	go func() { _, err := first.Login(ctx); firstDone <- err }()
	<-firstEntered
	cancel()
	secondDone := make(chan error, 1)
	go func() { _, err := second.Login(context.Background()); secondDone <- err }()
	select {
	case <-secondEntered:
		t.Fatal("new SDK login acquired occupied callback listener")
	case <-time.After(25 * time.Millisecond):
	}
	close(cleanup)
	if err := waitSDKResult(t, firstDone); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled login not cancelled", err)
	}
	if err := waitSDKResult(t, secondDone); err != nil {
		t.Fatal(err)
	}
	if max.Load() != 1 {
		t.Fatal("SDK callback sessions overlapped")
	}
}

func TestSDKQueuedBrowserLoginCancelsWithoutInvokingSDK(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	first := SDKLoginConnector{BrowserCallback: true, invoke: func(context.Context, *sdkconfig.Config, *sdkauth.LoginOptions) (*coreauth.Auth, error) {
		close(entered)
		<-release
		return nil, ErrLoginFailed
	}}
	done := make(chan error, 1)
	go func() { _, err := first.Login(context.Background()); done <- err }()
	<-entered
	var calls atomic.Int32
	queued := SDKLoginConnector{BrowserCallback: true, invoke: func(context.Context, *sdkconfig.Config, *sdkauth.LoginOptions) (*coreauth.Auth, error) {
		calls.Add(1)
		return nil, ErrLoginFailed
	}}
	ctx, cancel := context.WithCancel(context.Background())
	queuedDone := make(chan error, 1)
	go func() { _, err := queued.Login(ctx); queuedDone <- err }()
	cancel()
	if err := waitSDKResult(t, queuedDone); !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatal("cancelled queued login invoked SDK", err)
	}
	close(release)
	_ = waitSDKResult(t, done)
}

func TestSDKDeviceLoginDoesNotAcquireBrowserGateAndOptionsAreCopied(t *testing.T) {
	sdkBrowserLoginGate <- struct{}{}
	defer func() { <-sdkBrowserLoginGate }()
	cfg := &sdkconfig.Config{AuthDir: "synthetic-config-dir-not-read"}
	meta := map[string]string{"custom": "fixture"}
	c := SDKLoginConnector{Config: cfg, Options: sdkauth.LoginOptions{Metadata: meta}, invoke: func(_ context.Context, actual *sdkconfig.Config, opts *sdkauth.LoginOptions) (*coreauth.Auth, error) {
		if actual.AuthDir != "" || opts.Metadata["codex_login_mode"] != "device" {
			t.Error("device options incorrect")
		}
		opts.Metadata["custom"] = "modified"
		actual.AuthDir = "modified"
		return sdkFixture("synthetic-access", "synthetic-refresh"), nil
	}}
	if _, err := c.Login(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.AuthDir != "synthetic-config-dir-not-read" || meta["custom"] != "fixture" {
		t.Fatal("connector mutated caller options")
	}
}

func TestSDKSuccessfulCallbackReleasesDefaultPromptWaiter(t *testing.T) {
	promptDone := make(chan error, 1)
	c := SDKLoginConnector{BrowserCallback: true, invoke: func(_ context.Context, _ *sdkconfig.Config, opts *sdkauth.LoginOptions) (*coreauth.Auth, error) {
		go func() { _, err := opts.Prompt("fixture callback prompt"); promptDone <- err }()
		return sdkFixture("synthetic-sdk-access", "synthetic-sdk-refresh"), nil
	}}
	record, err := c.Login(context.Background())
	if err != nil || record == nil {
		t.Fatal("successful callback discarded", err)
	}
	if err = waitSDKResult(t, promptDone); !errors.Is(err, context.Canceled) {
		t.Fatal("successful SDK login retained prompt waiter", err)
	}
}

func TestSDKComposedPromptPreservesCallbackAndCancellationAfterBlank(t *testing.T) {
	callback := "http://localhost:1455/auth/callback?error=synthetic-fixture"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := cancellableSDKPrompt(ctx, func(message string) (string, error) {
		if message != "fixture" {
			t.Error("prompt message changed")
		}
		return callback, nil
	})("fixture")
	if err != nil || got != callback {
		t.Fatal("caller callback lost", err)
	}
	blankEntered := make(chan struct{})
	blank := cancellableSDKPrompt(ctx, func(string) (string, error) { close(blankEntered); return "", nil })
	done := make(chan error, 1)
	go func() { _, err := blank("fixture"); done <- err }()
	<-blankEntered
	cancel()
	if err = waitSDKResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal("blank input disabled cancellation", err)
	}
}

func TestSDKCallerPromptWorkersRemainBoundedWhenCallerIgnoresCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	first := cancellableSDKPrompt(ctx, func(string) (string, error) { close(entered); <-release; return "", nil })
	done := make(chan error, 1)
	go func() { _, err := first("fixture"); done <- err }()
	<-entered
	cancel()
	if err := waitSDKResult(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	var secondCalls atomic.Int32
	nextCtx, nextCancel := context.WithCancel(context.Background())
	second := cancellableSDKPrompt(nextCtx, func(string) (string, error) { secondCalls.Add(1); return "", nil })
	nextDone := make(chan error, 1)
	go func() { _, err := second("fixture"); nextDone <- err }()
	nextCancel()
	if err := waitSDKResult(t, nextDone); !errors.Is(err, context.Canceled) || secondCalls.Load() != 0 {
		t.Fatal("unbounded optional prompt workers", err)
	}
	close(release)
	// Ensure the optional worker releases its slot before another test uses it.
	deadline := time.Now().Add(time.Second)
	for len(sdkCallerPromptGate) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(sdkCallerPromptGate) != 0 {
		t.Fatal("optional prompt worker did not exit")
	}
}

// This opt-in integration test invokes only the pinned SDK's local callback
// listener. No browser is opened, no callback is sent, no auth file is read, and
// no provider/token endpoint is called. It takes the SDK's real 15-second delay.
func TestPinnedSDKCallbackCancellationNoProviderRequests(t *testing.T) {
	if os.Getenv("SALCARA_TEST_SDK_CALLBACK") != "1" {
		t.Skip("set SALCARA_TEST_SDK_CALLBACK=1 for the synthetic 15-second SDK listener test")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	c := SDKLoginConnector{BrowserCallback: true, Options: sdkauth.LoginOptions{NoBrowser: true, CallbackPort: port}, invoke: func(ctx context.Context, cfg *sdkconfig.Config, opts *sdkauth.LoginOptions) (*coreauth.Auth, error) {
		close(started)
		return invokeSDKLogin(ctx, cfg, opts)
	}}
	result := make(chan error, 1)
	begin := time.Now()
	go func() { _, err := c.Login(ctx); result <- err }()
	<-started
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("real SDK callback login did not return cancellation", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("public SDK prompt did not release cancelled callback listener")
	}
	if time.Since(begin) > 19*time.Second {
		t.Fatalf("callback cancellation exceeded SDK prompt/shutdown bound: %s", time.Since(begin))
	}
	// A successful local bind proves our listener is released, without sending
	// an HTTP cancellation request to this or any potentially foreign port.
	rebound, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal("cancelled SDK callback listener remains occupied", err)
	}
	rebound.Close()
}
