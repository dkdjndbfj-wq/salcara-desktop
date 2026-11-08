package usageledger

import "testing"

func TestAuditImageCompletionRequiresAnActualImageResult(t *testing.T) {
	for _, raw := range []string{
		`{"status":"completed","data":[]}`,
		`{"type":"response.completed","data":[]}`,
		`{"type":"image_generation.completed","status":"completed"}`,
	} {
		var collector Collector
		collector.ObserveImageJSON([]byte(raw))
		if collector.State() == "completed" || collector.Final() {
			t.Fatalf("non-image terminal envelope was accepted: %s state=%s final=%v", raw, collector.State(), collector.Final())
		}
	}
}
