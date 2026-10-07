package hubclient

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"
	"time"

	"salcara/bridge/internal/desktopcompanion"
	"salcara/bridge/internal/protocol"
)

type routeDesktop struct {
	st           desktopcompanion.NativeConnection
	sent         []string
	statusErr    error
	sendErr      error
	statusChecks int
}

func (d *routeDesktop) NativeStatus(context.Context) (desktopcompanion.NativeConnection, error) {
	d.statusChecks++
	return d.st, d.statusErr
}
func (d *routeDesktop) NativeList(context.Context) ([]protocol.SessionInfo, error) { return nil, nil }
func (d *routeDesktop) NativeOpen(context.Context, string) (protocol.SessionInfo, []protocol.Event, error) {
	return protocol.SessionInfo{}, nil, nil
}
func (d *routeDesktop) NativeSend(_ context.Context, key, text, op string) error {
	d.sent = append(d.sent, key+"|"+text+"|"+op)
	return d.sendErr
}

func liveDesktop(key string) desktopcompanion.NativeConnection {
	return desktopcompanion.NativeConnection{Active: true, ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), SessionKeys: []string{key}, Capabilities: desktopcompanion.NativeCapabilities{List: true, Read: true, Send: true}}
}

type pagedRouteDesktop struct {
	routeDesktop
	list   []protocol.SessionInfo
	limits []int
}

func (d *pagedRouteDesktop) NativeList(context.Context) ([]protocol.SessionInfo, error) {
	return d.list, nil
}
func (d *pagedRouteDesktop) NativeOpenPageLimited(_ context.Context, key, cursor string, limit int) (protocol.SessionInfo, []protocol.Event, string, error) {
	d.limits = append(d.limits, limit)
	return protocol.SessionInfo{SessionKey: key, Tool: "codex", ControlSurface: "desktop", Controllable: true, ControlExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, nil, "", nil
}

func TestNativeHistoryLimitDefaultsRemainCompatibleAndBounded(t *testing.T) {
	key := "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"
	c, a := surfaceClient(t)
	d := &pagedRouteDesktop{routeDesktop: routeDesktop{st: liveDesktop(key)}}
	c.o.Desktop = d
	for _, limit := range []int{0, 2, 10} {
		cmd := map[string]any{"type": "desktop.session.open", "sessionKey": key, "controlSurface": "desktop"}
		if limit != 0 {
			cmd["limit"] = limit
		}
		if _, err := c.Dispatch(context.Background(), cmd); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(d.limits) != "[10 2 10]" {
		t.Fatal("native default or small first page lost")
	}
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "desktop.session.open", "controlSurface": "desktop", "sessionKey": key, "limit": 11}); err == nil {
		t.Fatal("unbounded native read accepted")
	}
	if len(a.opened) != 0 {
		t.Fatal("native read reached CLI")
	}
}

func TestNativeDirectoryPagesTenAtATimeAndRejectsInvalidPositions(t *testing.T) {
	c, _ := surfaceClient(t)
	d := &pagedRouteDesktop{}
	for i := 0; i < 25; i++ {
		d.list = append(d.list, protocol.SessionInfo{SessionKey: fmt.Sprintf("codex:%d", i)})
	}
	c.o.Desktop = d
	cursor := ""
	for _, expected := range []int{10, 10, 5} {
		cmd := map[string]any{"type": "desktop.sessions.list", "controlSurface": "desktop", "limit": 10}
		if cursor != "" {
			cmd["cursor"] = cursor
		}
		result, err := c.Dispatch(context.Background(), cmd)
		if err != nil {
			t.Fatal(err)
		}
		page := result.(map[string]any)
		if len(page["sessions"].([]protocol.SessionInfo)) != expected {
			t.Fatal("incorrect native page")
		}
		cursor = page["nextCursor"].(string)
	}
	if cursor != "" {
		t.Fatal("last page still advertises more")
	}
	for _, bad := range []string{"10junk", " 10", "+10", "01", "-1", "26"} {
		cmd := map[string]any{"type": "desktop.sessions.list", "controlSurface": "desktop", "limit": 10, "cursor": base64.RawURLEncoding.EncodeToString([]byte(bad))}
		if _, err := c.Dispatch(context.Background(), cmd); err == nil {
			t.Fatal("invalid native position accepted")
		}
	}
}

func TestNativeDirectoryNewHeadDoesNotShiftTheNextPageBoundary(t *testing.T) {
	c, _ := surfaceClient(t)
	d := &pagedRouteDesktop{}
	for i := 0; i < 25; i++ {
		d.list = append(d.list, protocol.SessionInfo{SessionKey: fmt.Sprintf("codex:%d", i)})
	}
	c.o.Desktop = d
	first, err := c.Dispatch(context.Background(), map[string]any{"type": "desktop.sessions.list", "controlSurface": "desktop", "limit": 10})
	if err != nil {
		t.Fatal(err)
	}
	cursor := first.(map[string]any)["nextCursor"].(string)
	d.list = append([]protocol.SessionInfo{{SessionKey: "codex:new"}}, d.list...)
	second, err := c.Dispatch(context.Background(), map[string]any{"type": "desktop.sessions.list", "controlSurface": "desktop", "limit": 10, "cursor": cursor})
	if err != nil {
		t.Fatal(err)
	}
	page := second.(map[string]any)["sessions"].([]protocol.SessionInfo)
	if len(page) != 10 || page[0].SessionKey != "codex:10" || page[9].SessionKey != "codex:19" {
		t.Fatal("new head shifted the older page")
	}
}

type approvalDesktop struct {
	routeDesktop
	approvals []string
}

func (d *approvalDesktop) NativeRespondApproval(_ context.Context, key, id, decision, message, op string) error {
	d.approvals = append(d.approvals, key+"|"+id+"|"+decision+"|"+message+"|"+op)
	return nil
}

func TestNativeHookApprovalRouteNeverTouchesWorkerOrCreatesSessionPermissions(t *testing.T) {
	key := "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"
	approvalID := "11111111-2222-4333-8444-555555555555"
	op := "22222222-2222-4333-8444-555555555555"
	c, a := surfaceClient(t)
	d := &approvalDesktop{routeDesktop: routeDesktop{st: liveDesktop(key)}}
	d.st.Capabilities.Approval = true
	d.st.ApprovalTransport = desktopcompanion.NativeApprovalTransport
	c.o.Desktop = d
	cmd := map[string]any{"type": "desktop.approval.respond", "controlSurface": "desktop", "sessionKey": key, "approvalId": approvalID, "operationId": op, "decision": "allow"}
	if result, err := c.Dispatch(context.Background(), cmd); err != nil || result.(map[string]any)["accepted"] != true {
		t.Fatal(result, err)
	}
	if len(d.approvals) != 1 || len(a.sent)+len(a.started)+len(a.opened)+len(a.stopped) != 0 {
		t.Fatal("native approval reached background worker")
	}
	for _, field := range []string{"allow_session", "missing-transport", "answers", "bad-op", "no-scope"} {
		copy := map[string]any{}
		for k, v := range cmd {
			copy[k] = v
		}
		switch field {
		case "allow_session":
			copy["decision"] = "allow_session"
		case "missing-transport":
			d.st.ApprovalTransport = ""
		case "answers":
			copy["answers"] = map[string]any{}
		case "bad-op":
			copy["operationId"] = "bad"
		case "no-scope":
			copy["sessionKey"] = "codex:" + approvalID
		}
		if result, err := c.Dispatch(context.Background(), copy); err == nil || result != nil {
			t.Fatal(field, result, err)
		}
		d.st.ApprovalTransport = desktopcompanion.NativeApprovalTransport
	}
	if len(d.approvals) != 1 {
		t.Fatal("rejected request dispatched")
	}
}

func TestSendExecutorIsExplicitAndNeverChangesWithLease(t *testing.T) {
	key := "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"
	c, a := surfaceClient(t)
	d := &routeDesktop{}
	c.o.Desktop = d
	send := func(surface string) map[string]any {
		res, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": key, "text": "hi", "controlSurface": surface})
		if err != nil {
			t.Fatal(err)
		}
		return res.(map[string]any)
	}
	if send("cli")["via"] != "background" || len(a.sent) != 1 || len(d.sent) != 0 {
		t.Fatal("without live mode the Codex engine must continue the thread")
	}
	d.st = liveDesktop(key)
	if send("cli")["via"] != "background" || len(a.sent) != 2 || len(d.sent) != 0 || d.statusChecks != 0 {
		t.Fatal("explicit CLI must not be hijacked by an available desktop lease")
	}
	if send("desktop")["via"] != "desktop" || len(a.sent) != 2 || len(d.sent) != 1 {
		t.Fatal("explicit desktop must never reach the background engine")
	}
	d.st.SessionKeys = []string{"codex:11111111-2222-4333-8444-555555555555"}
	if result, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": key, "text": "hi", "controlSurface": "desktop"}); err == nil || result != nil || len(a.sent) != 2 || len(d.sent) != 1 {
		t.Fatal("expired/out-of-scope desktop must refuse rather than change executor")
	}
}

func TestExplicitDesktopUnavailableAndUnsupportedPayloadNeverReachBackground(t *testing.T) {
	key := "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"
	for _, condition := range []string{"absent", "status-error", "expired", "out-of-scope", "no-send-capability", "claude", "image", "malformed-image", "model", "effort", "interrupt", "approval", "bad-operation-id"} {
		t.Run(condition, func(t *testing.T) {
			c, a := surfaceClient(t)
			d := &routeDesktop{st: liveDesktop(key)}
			c.o.Desktop = d
			cmd := map[string]any{"type": "session.send", "sessionKey": key, "text": "task", "controlSurface": "desktop"}
			switch condition {
			case "absent":
				c.o.Desktop = nil
			case "status-error":
				d.statusErr = errors.New("desktop unavailable")
			case "expired":
				d.st.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
			case "out-of-scope":
				d.st.SessionKeys = []string{"codex:11111111-2222-4333-8444-555555555555"}
			case "no-send-capability":
				d.st.Capabilities.Send = false
			case "claude":
				cmd["sessionKey"] = "claude:" + key[6:]
			case "image":
				cmd["attachments"] = []any{"attachment"}
			case "malformed-image":
				cmd["attachments"] = "attachment"
			case "model":
				cmd["model"] = "gpt-selected"
			case "effort":
				cmd["effort"] = "high"
			case "interrupt":
				cmd["type"] = "session.interrupt"
			case "approval":
				cmd["type"] = "approval.respond"
			case "bad-operation-id":
				cmd["operationId"] = "not-a-uuid"
			}
			if result, err := c.Dispatch(context.Background(), cmd); err == nil || result != nil {
				t.Fatal("unsupported desktop request accepted", result, err)
			}
			if len(a.sent)+len(a.started)+len(a.opened)+len(a.stopped)+len(d.sent) != 0 {
				t.Fatal("rejected request reached an executor")
			}
		})
	}
}

func TestExplicitDesktopDispatchedUncertaintyIsNotRetriedOrBackgroundFallback(t *testing.T) {
	key := "codex:01a0ae56-e9f6-4933-9ad4-5e08cd7874e5"
	c, a := surfaceClient(t)
	d := &routeDesktop{st: liveDesktop(key), sendErr: desktopcompanion.ErrDesktopDeliveryUncertain}
	c.o.Desktop = d
	result, err := c.Dispatch(context.Background(), map[string]any{"type": "session.send", "sessionKey": key, "text": "task", "operationId": "11111111-2222-4333-8444-555555555555", "controlSurface": "desktop"})
	if !errors.Is(err, desktopcompanion.ErrDesktopDeliveryUncertain) || result != nil || len(d.sent) != 1 || len(a.sent) != 0 {
		t.Fatal(result, err, d.sent, a.sent)
	}
}
