package desktopcompanion

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestNativeModelSchemaTransportAndExactOptions(t *testing.T) {
	f := gatewayFixture(t)
	f.capabilities.ModelOverride = true
	f.modelTransport = NativeModelOverrideTransport
	f.modelOptions = &NativeModelSupport{Model: true, Thinking: []string{"low", "high"}}
	st, err := f.service.NativeStatus(context.Background())
	if err != nil || !st.Capabilities.ModelOverride || st.ModelOverrideTransport != NativeModelOverrideTransport {
		t.Fatal(st, err)
	}
	options := NativeSendOptions{Model: "gpt-fixture", Thinking: "high"}
	if err := f.service.NativeSendWithOptions(context.Background(), "codex:"+targetID, "fixture", operationID, options); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	if last["model"] != options.Model || last["thinking"] != options.Thinking || last["sessionKey"] != "codex:"+targetID || last["operationId"] != operationID {
		t.Fatal("options lost", last)
	}
	for _, bad := range []NativeSendOptions{{Thinking: "max"}, {Model: "bad\nmodel"}, {Model: " "}} {
		if err := f.service.NativeSendWithOptions(context.Background(), "codex:"+targetID, "fixture", operationID, bad); !errors.Is(err, ErrDesktopCapability) {
			t.Fatal("unsupported native options passed", bad, err)
		}
	}
	f.gatewayFailure = "CAPABILITY_UNAVAILABLE"
	if err := f.service.NativeSendWithOptions(context.Background(), "codex:"+targetID, "fixture", operationID, options); !errors.Is(err, ErrDesktopCapability) {
		t.Fatal("host downgrade must be definitively unsent", err)
	}
}

func TestNativeModelContractDoesNotWeakenSourceBinding(t *testing.T) {
	f := gatewayFixture(t)
	f.capabilities.ModelOverride = true
	f.modelTransport = NativeModelOverrideTransport
	f.modelOptions = &NativeModelSupport{Model: true, Thinking: []string{}}
	source := "fixture-source-a"
	f.service.o.NativeSourceIdentity = func() string { return source }
	if _, err := f.service.NativeStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
	source = "fixture-source-b"
	before := f.count()
	if err := f.service.NativeSendWithOptions(context.Background(), "codex:"+targetID, "fixture", operationID, NativeSendOptions{Model: "gpt-fixture"}); !errors.Is(err, ErrActivationRequired) {
		t.Fatal(err)
	}
	if f.count() != before {
		t.Fatal("changed source reached native gateway")
	}
}

func TestNativeModelSupportRejectsUnprovenOrMalformedMetadata(t *testing.T) {
	for _, contract := range []struct {
		transport string
		support   *NativeModelSupport
	}{
		{"", &NativeModelSupport{Model: true}}, {NativeModelOverrideTransport, nil},
		{NativeModelOverrideTransport, &NativeModelSupport{Model: false}},
		{NativeModelOverrideTransport, &NativeModelSupport{Model: true, Thinking: []string{"unknown"}}},
		{NativeModelOverrideTransport, &NativeModelSupport{Model: true, Thinking: []string{"low", "low"}}},
	} {
		if validNativeModelSupport(true, contract.transport, contract.support) {
			t.Fatal("unproven contract accepted", contract)
		}
	}
}

func TestNativeModelSourceGuardRunsAfterStatusAndBeforeSend(t *testing.T) {
	f := gatewayFixture(t)
	f.capabilities.ModelOverride = true
	f.modelTransport = NativeModelOverrideTransport
	f.modelOptions = &NativeModelSupport{Model: true, Thinking: []string{}}
	var changed atomic.Bool
	f.statusHook = func() { changed.Store(true) }
	sentinel := errors.New("synthetic frozen source changed")
	options := NativeSendOptions{Model: "gpt-fixture", ValidateSource: func() error {
		if changed.Load() {
			return sentinel
		}
		return nil
	}}
	if err := f.service.NativeSendWithOptions(context.Background(), "codex:"+targetID, "fixture", operationID, options); !errors.Is(err, sentinel) {
		t.Fatal("source guard did not execute after awaited status", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, call := range f.calls {
		if call["type"] == "send" {
			t.Fatal("frozen source changed before native send")
		}
	}
}
