package monitor

// The model control compares a real emitted annotation with this same finite
// fixture. The production reducer must count only the untouched panic record.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestLegacyRegistrationObservationPreservesPanicAndNovelCounts(t *testing.T) {
	body, err := os.ReadFile("testdata/legacy_registration_db_observation.json")
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	body, err = json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	annotation := "[synthetic-host][taskworker][synthetic-generation][cid:synthetic-correlation] [legacy_registration_db] " + string(body)
	tailer := newLogTailer("taskworker", nil)
	for _, cause := range []string{"live", "dispatch_budget", "discovery_budget", "deadline", "canceled", "other"} {
		variant := strings.ReplaceAll(annotation, `"cause":"deadline"`, `"cause":"`+cause+`"`)
		for range 100 {
			tailer.classify(variant)
		}
	}
	if len(tailer.classCounts) != 0 || len(tailer.novelCounts) != 0 {
		t.Fatal("finite annotation created an error-class or novel rate")
	}
	owner := "server/v2026/model.registerLegacyCloseOwnerPage"
	panicLine := panicOwnerTestLine(t, "context.deadlineExceededError=context deadline exceeded", "github.com/urnetwork/"+owner)
	for range 5 {
		tailer.classify(annotation)
		tailer.classify(panicLine)
	}
	if len(tailer.classCounts) != 2 || tailer.classCounts["panic"] != 5 || tailer.classCounts["panic\x00"+owner] != 5 || len(tailer.novelCounts) != 0 {
		t.Fatal("annotation changed aggregate/owner counts or introduced novelty")
	}
	observation := parsePanicLogObservation(panicLine)
	if observation.owner != owner || observation.errorType != "unknown" || observation.sqlstate != "unknown" {
		t.Fatal("untouched two-field panic payload lost its documented projection", observation)
	}
	pages := 0
	for _, finding := range tailer.drainWindow() {
		if finding.class != "panic" || finding.healthy {
			continue
		}
		alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", finding)
		if alert.Severity != SeverityPage || alert.Target != "taskworker" || !strings.Contains(alert.Observed, "rate=5/min") {
			t.Fatal("annotation changed existing panic thresholds or target")
		}
		pages++
	}
	if pages != 2 {
		t.Fatal("aggregate and owner pages were not both preserved")
	}
	// The prefix is not excluded: unexpected error-shaped data remains visible.
	tailer.classify("[legacy_registration_db] synthetic unrelated error")
	if len(tailer.novelCounts) != 1 {
		t.Fatal("diagnostic prefix suppressed unrelated or malformed errors")
	}
}
