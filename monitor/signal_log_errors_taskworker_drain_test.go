package monitor

// SIGNALS.md §12.1: exercise the rendered log signal without interpreting a
// bounded drain outcome as a terminal process or claim-ownership observation.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Keep source failures distinct from completed windows, including when a
// failed helper returned a partial drain sample.
func taskworkerDrainGuidanceSource(output string, sourceErr error) *syntheticSource {
	return &syntheticSource{localFn: func(_ string, args ...string) (string, error) {
		if len(args) > 1 && args[0] == "ls" {
			return "repo names synthetic-taskworker", nil
		}
		return output, sourceErr
	}}
}

func TestLogErrorsTaskworkerDrainGiveUpKeepsLaterOutcomeUnknown(t *testing.T) {
	for _, line := range []string{
		"[taskworker]drain gave up after 90.0s with 1 tasks still running (claims release per task max time)",
		"[taskworker]drain gave up",
		"[taskworker]drain gave up " + strings.Repeat("synthetic-partial-detail ", 20),
	} {
		alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(taskworkerDrainGuidanceSource(line, nil)))
		if err != nil {
			t.Fatal(err)
		}
		alert := requireAlertClass(t, alerts, "taskworker-drain-gave-up")
		if alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.Frame != "" ||
			alert.Identity() != "logs/taskworker-drain-gave-up|taskworker-drain-gave-up|taskworker|" ||
			alert.Observed != "rate=1/min class=taskworker-drain-gave-up target=taskworker" ||
			!strings.Contains(alert.Symptom, "threshold 1/min") {
			t.Fatalf("drain guidance changed warning identity, threshold or counting: %+v", alert)
		}
		// This assertion fails before the correction: the generated meaning
		// claimed a process kill and stranded claims from the gave-up line.
		if !strings.Contains(alert.Evidence, "Later final handback, process termination and claim outcomes remain unknown") ||
			strings.Contains(alert.Evidence, "the process was killed") ||
			strings.Contains(alert.Evidence, "every in-flight claim is leased") {
			t.Fatalf("drain sample acquired an unobserved terminal outcome: %s", alert.Evidence)
		}
		for _, want := range []string{
			"WaitFinalHandback provides an additional bounded grace",
			"does not distinguish a task still executing from pending finalization or handback",
			"missing or truncated sample",
			"missing, stale, partial or unavailable lifecycle evidence",
			"Do not release claims or restart a worker from this line alone",
			"same process and generation",
			"quiet or incomplete log window does not prove lifecycle recovery",
		} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Errorf("drain guidance omitted authority boundary %q", want)
			}
		}
		_, sample, ok := strings.Cut(alert.Evidence, "\nsample: ")
		if !ok || len(sample) > 200 || !strings.HasPrefix(sample, "[taskworker]drain gave up") ||
			(len(line) <= 200 && sample != line) {
			t.Fatalf("guidance changed bounded drain sample: %q", sample)
		}
	}
}

// Later completion is compatible with a genuine earlier give-up. The
// classifier does not correlate process/task lifecycles or suppress the line.
func TestLogErrorsTaskworkerDrainLaterHandbackDoesNotEraseGiveUp(t *testing.T) {
	line := "[taskworker]drain gave up after 90.0s with 1 tasks still running"
	tailer := newLogTailer("taskworker", nil)
	for _, logLine := range []string{
		line,
		"synthetic task finalized and ownership handed back after drain",
		"[taskworker]drain finished cleanly in 1.0s",
		line,
	} {
		tailer.classify(logLine)
	}
	finding := findingByClass(t, tailer.drainWindow(), "taskworker-drain-gave-up")
	alert := alertFromFinding(syntheticSettings(nil), "1.5", "log-errors", "Log error-class rates", finding)
	if finding.healthy || alert.Severity != SeverityWarn ||
		alert.Observed != "rate=2/min class=taskworker-drain-gave-up target=taskworker" ||
		!strings.Contains(alert.Evidence, "\nsample: "+line) ||
		!strings.Contains(alert.Verify, "A later handback does not erase the earlier drain give-up") {
		t.Fatalf("later handback erased or reinterpreted actual give-up evidence: %+v", alert)
	}
}

func TestLogErrorsTaskworkerDrainHealthyOutcomesDoNotManufactureGiveUp(t *testing.T) {
	for _, line := range []string{
		"",
		"[taskworker]drain finished cleanly in 1.0s",
		"[taskworker]drain finished after cancel in 2.0s (1 canceled and rescheduled)",
		"synthetic task finalized and ownership handed back after drain",
	} {
		alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(taskworkerDrainGuidanceSource(line, nil)))
		if err != nil {
			t.Fatal(err)
		}
		for _, alert := range alerts {
			if alert.Class == "taskworker-drain-gave-up" {
				t.Fatalf("healthy outcome manufactured a give-up warning: %q", line)
			}
		}
	}
}

func TestLogErrorsTaskworkerDrainUnavailableWindowDoesNotClaimOutcome(t *testing.T) {
	for _, partial := range []string{"", "[taskworker]drain gave up after 90.0s with 1 tasks still running"} {
		unavailable := errors.New("synthetic drain log source unavailable")
		alerts, err := NewLogErrorsSignal().Run(context.Background(), syntheticSettings(taskworkerDrainGuidanceSource(partial, unavailable)))
		if !errors.Is(err, unavailable) || len(alerts) != 0 {
			t.Fatalf("failed source became a completed drain or healthy observation: alerts=%+v error=%v", alerts, err)
		}
	}
}
