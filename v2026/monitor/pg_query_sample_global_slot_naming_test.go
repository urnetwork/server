package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPgSampleGlobalRunSlotNamingPreservesWrappedOwnerBoundary(t *testing.T) {
	err := fmt.Errorf("synthetic wrapper: %w", &runSlotAdmissionError{cause: context.DeadlineExceeded})
	alert := visibilityAlert(syntheticSettings(nil), NewPgQuerySampleSignal(), err)
	if alert.Class != "pg-query-sample-unavailable" || alert.Severity != SeverityWarn || alert.Sustain != 1 {
		t.Fatal("global slot naming changed the existing first-miss warning")
	}
	for _, want := range []string{
		"phase=global-run-slot-admission cause=not-admitted",
		"error_class=observation-timeout",
		"source_contact_attempted=false",
	} {
		if !strings.Contains(alert.Observed, want) {
			t.Fatalf("wrapped global admission lost %q", want)
		}
	}
	if strings.Contains(alert.Observed, "phase=shared-slot-admission") {
		t.Fatal("global signal admission retained an ambiguous shared SSH label")
	}
}

func TestPgSampleGlobalRunSlotNamingDoesNotRelabelSharedSshAcquire(t *testing.T) {
	err := fmt.Errorf("synthetic wrapper: %w", &sshAdmissionUnavailableError{err: context.DeadlineExceeded, phase: sshAdmissionFailureAcquire})
	alert := visibilityAlert(syntheticSettings(nil), NewPgQuerySampleSignal(), err)
	if !strings.Contains(alert.Observed, "phase=source-or-adapter cause=unknown") || !strings.Contains(alert.Observed, "source_contact_attempted=unknown") || strings.Contains(alert.Observed, "global-run-slot-admission") {
		t.Fatal("shared SSH acquisition invented a global-slot or no-contact fact")
	}
	other := visibilityAlert(syntheticSettings(nil), NewLegacySettlementsSignal(), err)
	if !strings.Contains(other.Observed, "ssh_admission_failures=acquire") || strings.Contains(other.Observed, "global-run-slot-admission") {
		t.Fatal("global naming changed the independent shared SSH acquire projection")
	}
}

func TestPgSampleGlobalRunSlotNamingUntypedFailureKeepsContactUnknown(t *testing.T) {
	err := errors.New("synthetic global-run-slot-admission-looking message")
	alert := visibilityAlert(syntheticSettings(nil), NewPgQuerySampleSignal(), err)
	if !strings.Contains(alert.Observed, "phase=source-or-adapter cause=unknown") || !strings.Contains(alert.Observed, "source_contact_attempted=unknown") || strings.Contains(alert.Observed, "phase=global-run-slot-admission") {
		t.Fatal("untyped wording fabricated typed global admission authority")
	}
}
