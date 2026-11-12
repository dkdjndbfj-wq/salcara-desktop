package protocol

import "testing"

func TestHermesLegacyTimelineNormalizationDoesNotInventGrant(t *testing.T) {
	for _, tc := range []struct {
		in   Event
		want Event
	}{
		{Event{Tool: "hermes", Type: "tool", Status: "completed"}, Event{Tool: "hermes", Type: "tool", Status: "done"}},
		{Event{Tool: "hermes", Type: "notice", Level: "warning", Text: "recover"}, Event{Tool: "hermes", Type: "notice", Level: "warn", Text: "recover"}},
		{Event{Tool: "hermes", Type: "notice", Text: "status"}, Event{Tool: "hermes", Type: "notice", Level: "info", Text: "status"}},
		{Event{Tool: "hermes", Type: "approval.resolved"}, Event{Tool: "hermes", Type: "approval.resolved", Decision: "unknown", By: "unknown"}},
	} {
		got := NormalizeTimelineEvent(tc.in)
		if got.Status != tc.want.Status || got.Level != tc.want.Level || got.Decision != tc.want.Decision || got.By != tc.want.By || got.Text != tc.want.Text {
			t.Fatal(got, tc.want)
		}
	}
	for _, in := range []Event{{Tool: "codex", Type: "approval.resolved"}, {Tool: "foreign", Type: "tool", Status: "completed"}, {Tool: "hermes", Type: "turn", Status: "completed"}, {Tool: "hermes", Type: "approval.resolved", Decision: "future", By: "alien"}, {Tool: "hermes", Type: "notice", Level: "future"}} {
		got := NormalizeTimelineEvent(in)
		if got.Status != in.Status || got.Level != in.Level || got.Decision != in.Decision || got.By != in.By {
			t.Fatal("broadened compatibility", in, got)
		}
	}
}
