package protocol

// NormalizeTimelineEvent repairs only historical wire aliases and missing
// settlement metadata. It never fabricates a grant from an incomplete event.
// Readers must apply the same compatibility to already-retained Hub events.
func NormalizeTimelineEvent(e Event) Event {
	if e.Tool != "hermes" {
		return e
	}
	switch e.Type {
	case "tool":
		if e.Status == "completed" {
			e.Status = "done"
		}
	case "notice":
		if e.Level == "warning" {
			e.Level = "warn"
		}
		if e.Level == "" {
			e.Level = "info"
		}
	case "approval.resolved":
		if e.Decision == "" {
			e.Decision = "unknown"
		}
		if e.By == "" {
			e.By = "unknown"
		}
	}
	return e
}
