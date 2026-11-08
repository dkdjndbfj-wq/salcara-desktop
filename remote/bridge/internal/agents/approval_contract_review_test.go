package agents

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"salcara/bridge/internal/protocol"
)

// The generated native v2 contract declares availableDecisions optional and
// nullable. Null is absence, not an explicit empty list of allowed actions.
func TestReviewCodexNullableDecisionsMatchAbsent(t *testing.T) {
	absent := cxRequestOptions(nil)
	null := cxRequestOptions(json.RawMessage(`null`))
	if !reflect.DeepEqual(null, absent) {
		t.Fatalf("native nullable options do not match omitted options: null=%+v omitted=%+v", null, absent)
	}
}

func TestReviewApprovalTimeoutNeverDependsOnOfferedDeny(t *testing.T) {
	for _, mode := range []string{"allow_only", "unknown_version", "request_cancelled", "source_closed"} {
		t.Run(mode, func(t *testing.T) {
			a := newApprovals()
			source, request := make(chan struct{}), make(chan struct{})
			options := &protocol.ApprovalOptions{Version: 1, Decisions: []string{"allow"}}
			if mode == "unknown_version" {
				options.Version = 99
			}
			p, _ := a.addValidatedRequestForLifetimes("fixture:s", "fixture", "ref", protocol.Event{ApprovalOptions: options}, nil, source, request)
			a.mu.Lock()
			p.expiresAt = time.Now().Add(-time.Second)
			a.mu.Unlock()
			if mode == "request_cancelled" {
				close(request)
			}
			if mode == "source_closed" {
				close(source)
			}
			done := make(chan approvalAnswer, 1)
			go func() { done <- a.wait(p, nil) }()
			select {
			case answer := <-done:
				if answer.decision != "deny" || a.countFor("fixture:s") != 0 {
					t.Fatal("expiry granted access or retained pending request")
				}
			case <-time.After(time.Second):
				t.Fatal("expiry blocked waiting for an unavailable response")
			}
		})
	}
}
