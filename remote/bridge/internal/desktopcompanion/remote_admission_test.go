package desktopcompanion

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestRemoteAdmissionRecheckedAfterNativeStatus(t *testing.T) {
	for _, approval := range []bool{false, true} {
		t.Run(map[bool]string{false: "send", true: "approval"}[approval], func(t *testing.T) {
			f := gatewayFixture(t)
			f.capabilities.Approval = true
			f.approvalTransport = NativeApprovalTransport
			var revoked atomic.Bool
			f.statusHook = func() { revoked.Store(true) }
			ctx := WithRemoteAdmissionGuard(context.Background(), func() error {
				if revoked.Load() {
					return errors.New("synthetic authorization changed")
				}
				return nil
			})
			var err error
			if approval {
				err = f.service.NativeRespondApproval(ctx, "codex:"+targetID, otherID, "allow", "", operationID)
			} else {
				err = f.service.NativeSend(ctx, "codex:"+targetID, "fixture", operationID)
			}
			if !errors.Is(err, ErrDesktopRequestUnsent) {
				t.Fatalf("must report provably unsent, got %v", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, call := range f.calls {
				if call["type"] != "status" {
					t.Fatal("native write escaped late guard")
				}
			}
		})
	}
}
