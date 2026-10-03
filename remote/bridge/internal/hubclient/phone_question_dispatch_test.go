package hubclient

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"salcara/bridge/internal/agents"
	"salcara/bridge/internal/config"
)

// This optional responder only records local dispatch. It never starts a
// worker, sends a provider request, or changes the user's pending approvals.
type phoneQuestionDispatchAgent struct {
	*fakeAgent
	pending bool
	calls   []agents.ApprovalResponse
}

func (a *phoneQuestionDispatchAgent) RespondWithAnswers(id string, response agents.ApprovalResponse) bool {
	if id != "fixture-question" || !a.pending {
		return false
	}
	a.calls = append(a.calls, response)
	if response.Decision != "deny" && (response.Decision != "allow" || len(response.Answers) != 1 || len(response.Answers["fixture-id"]) != 1 || strings.TrimSpace(response.Answers["fixture-id"][0]) == "") {
		return false
	}
	a.pending = false
	return true
}

type phoneQuestionLegacyDispatchAgent struct {
	*fakeAgent
	pending           bool
	decision, message string
}

func (a *phoneQuestionLegacyDispatchAgent) Respond(id, decision, message string) bool {
	if id != "fixture-command" || !a.pending {
		return false
	}
	a.pending, a.decision, a.message = false, decision, message
	return true
}

func phoneQuestionDispatchFixture(t *testing.T) (*Client, *phoneQuestionDispatchAgent) {
	t.Helper()
	c := statusFixture(t, config.Config{})
	a := &phoneQuestionDispatchAgent{fakeAgent: &fakeAgent{id: "codex"}, pending: true}
	c.SetManager(&fakeManager{list: []agents.Agent{a}})
	return c, a
}

func phoneQuestionDispatchCommand(answers any) map[string]any {
	return map[string]any{"type": "approval.respond", "approvalId": "fixture-question", "decision": "allow", "message": "Human answer", "answers": answers, "controlSurface": "cli"}
}

func TestPhoneQuestionDispatchForwardsStructuredAnswersAndPreservesKeys(t *testing.T) {
	c, a := phoneQuestionDispatchFixture(t)
	answers := map[string]any{"fixture-id": []any{"Exact label with spaces "}}
	result, err := c.Dispatch(context.Background(), phoneQuestionDispatchCommand(answers))
	if err != nil || result == nil || a.pending || len(a.calls) != 1 {
		t.Fatalf("answer not delivered: result=%v err=%v pending=%v calls=%d", result, err, a.pending, len(a.calls))
	}
	got := a.calls[0]
	if got.Decision != "allow" || got.Message != "Human answer" || !reflect.DeepEqual(got.Answers, map[string][]string{"fixture-id": {"Exact label with spaces "}}) {
		t.Fatalf("answers changed: %+v", got)
	}
	if _, err := c.Dispatch(context.Background(), phoneQuestionDispatchCommand(answers)); err == nil || len(a.calls) != 1 {
		t.Fatal("resolved answer was accepted twice")
	}
}

func TestPhoneQuestionDispatchInvalidPayloadDoesNotReachOrConsumeRequest(t *testing.T) {
	for name, answers := range map[string]any{
		"null object":       nil,
		"scalar":            "answer",
		"array not object":  []any{"answer"},
		"value not array":   map[string]any{"fixture-id": "answer"},
		"numeric value":     map[string]any{"fixture-id": []any{1}},
		"null array":        map[string]any{"fixture-id": nil},
		"null member":       map[string]any{"fixture-id": []any{nil}},
		"mixed null member": map[string]any{"fixture-id": []any{"answer", nil}},
		"nested object":     map[string]any{"fixture-id": []any{map[string]any{"value": "answer"}}},
		"too large":         map[string]any{"fixture-id": []any{strings.Repeat("x", (64<<10)+1)}},
	} {
		t.Run(name, func(t *testing.T) {
			c, a := phoneQuestionDispatchFixture(t)
			if _, err := c.Dispatch(context.Background(), phoneQuestionDispatchCommand(answers)); err == nil {
				t.Fatal("malformed answers accepted")
			}
			if !a.pending || len(a.calls) != 0 {
				t.Fatalf("malformed payload reached/consumed registry: pending=%v calls=%d", a.pending, len(a.calls))
			}
			if _, err := c.Dispatch(context.Background(), phoneQuestionDispatchCommand(map[string]any{"fixture-id": []any{"corrected"}})); err != nil || a.pending {
				t.Fatalf("correction could not answer original request: %v", err)
			}
		})
	}
}

func TestPhoneQuestionDispatchRejectedAnswerKeepsOriginalPendingForCorrection(t *testing.T) {
	for name, answers := range map[string]any{
		"missing answer":       map[string]any{},
		"unknown id":           map[string]any{"foreign-id": []any{"value"}},
		"empty required value": map[string]any{"fixture-id": []any{""}},
	} {
		t.Run(name, func(t *testing.T) {
			c, a := phoneQuestionDispatchFixture(t)
			if _, err := c.Dispatch(context.Background(), phoneQuestionDispatchCommand(answers)); err == nil {
				t.Fatal("semantically invalid answer accepted")
			}
			if !a.pending || len(a.calls) != 1 {
				t.Fatal("validation rejection consumed request")
			}
			if _, err := c.Dispatch(context.Background(), phoneQuestionDispatchCommand(map[string]any{"fixture-id": []any{"corrected"}})); err != nil || a.pending || len(a.calls) != 2 {
				t.Fatalf("correction not delivered: %v", err)
			}
		})
	}
}

func TestPhoneQuestionDispatchLegacyApprovalAllowDenyRemainCompatible(t *testing.T) {
	for _, decision := range []string{"allow", "allow_session", "deny"} {
		t.Run(decision, func(t *testing.T) {
			c := statusFixture(t, config.Config{})
			a := &phoneQuestionLegacyDispatchAgent{fakeAgent: &fakeAgent{id: "codex"}, pending: true}
			c.SetManager(&fakeManager{list: []agents.Agent{a}})
			if _, err := c.Dispatch(context.Background(), map[string]any{"type": "approval.respond", "approvalId": "fixture-command", "decision": decision, "message": "Human feedback", "controlSurface": "cli"}); err != nil {
				t.Fatal(err)
			}
			if a.pending || a.decision != decision || a.message != "Human feedback" {
				t.Fatal("old approval contract changed")
			}
		})
	}
}

func TestPhoneQuestionDispatchCancellationNeedsNoFakeAnswers(t *testing.T) {
	c, a := phoneQuestionDispatchFixture(t)
	if _, err := c.Dispatch(context.Background(), map[string]any{"type": "approval.respond", "approvalId": "fixture-question", "decision": "deny", "controlSurface": "cli"}); err != nil {
		t.Fatal(err)
	}
	if a.pending || len(a.calls) != 1 || a.calls[0].Answers != nil || a.calls[0].Decision != "deny" {
		t.Fatal("cancellation fabricated answers")
	}
}
