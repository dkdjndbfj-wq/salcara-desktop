package hubclient

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"salcara/bridge/internal/config"
	"salcara/bridge/internal/desktopcompanion"
)

type admissionDesktop struct {
	routeDesktop
	statusHook func()
	sends      atomic.Int32
	approvals  atomic.Int32
}

func (d *admissionDesktop) NativeStatus(context.Context) (desktopcompanion.NativeConnection, error) {
	if d.statusHook != nil {
		d.statusHook()
	}
	return d.st, nil
}
func (d *admissionDesktop) NativeSend(context.Context, string, string, string) error {
	d.sends.Add(1)
	return nil
}
func (d *admissionDesktop) NativeRespondApproval(context.Context, string, string, string, string, string) error {
	d.approvals.Add(1)
	return nil
}

func remoteAdmissionGuard(cfg config.Config) context.Context {
	return withCommandGuard(context.Background(), commandGuard{identity: config.RemoteIdentity(cfg), deviceID: cfg.DeviceID, bindingID: cfg.PhoneBindingID, phoneHash: cfg.PhoneHash})
}

func TestNativeAdmissionRevokeDuringPreflightNeverSends(t *testing.T) {
	for _, approval := range []bool{false, true} {
		t.Run(map[bool]string{false: "send", true: "approval"}[approval], func(t *testing.T) {
			c, cfg, _ := stationSwitchFixture(t, nil, nil)
			key := "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"
			d := &admissionDesktop{routeDesktop: routeDesktop{st: liveDesktop(key)}}
			d.st.Capabilities.Approval = true
			d.st.ApprovalTransport = desktopcompanion.NativeApprovalTransport
			d.statusHook = func() { c.publishPairState(PairStatus{DeviceID: cfg.DeviceID}, "", true) }
			c.o.Desktop = d
			cmd := map[string]any{"type": "desktop.session.send", "controlSurface": "desktop", "sessionKey": key, "text": "synthetic prompt", "operationId": "11111111-2222-4333-8444-555555555555"}
			if approval {
				cmd = map[string]any{"type": "desktop.approval.respond", "sessionKey": key, "approvalId": "22222222-2222-4333-8444-555555555555", "decision": "allow", "operationId": "11111111-2222-4333-8444-555555555555"}
			}
			if _, err := c.Dispatch(remoteAdmissionGuard(cfg), cmd); err == nil {
				t.Fatal("revoked native request accepted")
			}
			if d.sends.Load()+d.approvals.Load() != 0 {
				t.Fatal("native executor called after revoke")
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			if len(c.nativeAdmissions) != 0 {
				t.Fatal("native admission leaked")
			}
		})
	}
}

func TestNativeAdmissionCancellationIsPairScopedNotTransportScoped(t *testing.T) {
	c, cfg, target := stationSwitchFixture(t, nil, nil)
	ctx, finish := c.nativeAdmissionContext(remoteAdmissionGuard(cfg), map[string]any{"type": "desktop.session.send"})
	defer finish()
	if err := desktopcompanion.CheckRemoteAdmission(ctx); err != nil {
		t.Fatal(err)
	}
	c.Kick() // same phone, same station: a temporary reconnect is not revoke
	if ctx.Err() != nil {
		t.Fatal("transport reconnect canceled admitted request")
	}
	c.publishPair(PairStatus{DeviceID: cfg.DeviceID, Paired: true}, eventIdentity(cfg))
	if err := desktopcompanion.CheckRemoteAdmission(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.o.Store.Update(func(next *config.Config) error { *next = activeStationConfig(*next, target); return nil }); err != nil {
		t.Fatal(err)
	}
	c.Kick()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("station switch failed to cancel old pending admission")
	}
}

func TestNativeAdmissionLegacyAndLocalCompatibility(t *testing.T) {
	c, store := newTestClient(t, "http://127.0.0.1", "synthetic", nil)
	ctx, finish := c.nativeAdmissionContext(remoteAdmissionGuard(store.Get()), map[string]any{"type": "desktop.session.send"})
	defer finish()
	if err := desktopcompanion.CheckRemoteAdmission(ctx); err != nil {
		t.Fatalf("legacy transport rejected: %v", err)
	}
	local, done := c.nativeAdmissionContext(context.Background(), map[string]any{"type": "desktop.session.send"})
	defer done()
	c.publishPairState(PairStatus{}, "", true)
	if err := desktopcompanion.CheckRemoteAdmission(local); err != nil {
		t.Fatal("local task was affected")
	}
}
