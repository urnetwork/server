// SIGNALS.md §§23.1–23.2: exercise both public H1+ signals with synthetic sources.
// Finite diagnostics preserve role ownership, privacy and lifecycle boundaries.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Exercise both public constructors: shared diagnostics cannot borrow the
// other carrier's protocol, service inventory, or returned error authority.
func h1PlusDiagnosticSignals() []struct {
	signal   Signal
	service  string
	protocol string
} {
	return []struct {
		signal   Signal
		service  string
		protocol string
	}{
		{NewConnectH1PlusSignal(), "connect", "urnetwork-framer/1"},
		{NewProxyH1PlusSignal(), "proxy", "urnetwork-framerxl/1"},
	}
}

// Check actual public role identity, one attempt, finite fields and private-data omission.
func requireH1PlusSourceDiagnostic(t *testing.T, sourceErr error, want []string, absent []string) {
	t.Helper()
	for _, role := range h1PlusDiagnosticSignals() {
		calls := 0
		settings := h1PlusTestSettings(t, h1PlusTestNow(), role.service, h1PlusTestFleet(h1PlusTestNow()))
		settings.Source = &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			calls++
			if !strings.Contains(command, "urnetwork_"+role.service+"_h1plus_") || !strings.Contains(command, role.protocol) {
				t.Fatalf("%s borrowed another carrier's source query", role.signal.Key())
			}
			return "private-h1plus-partial-body", sourceErr
		}}
		alerts, err := role.signal.Run(context.Background(), settings)
		if err != nil || calls != 1 || len(alerts) != 1 {
			t.Fatalf("%s changed failure outcome or single-attempt policy", role.signal.Key())
		}
		alert := alerts[0]
		if alert.Class != "h1plus-telemetry-incomplete" || alert.Severity != SeverityWarn || alert.Sustain != 2 || alert.SignalKey != role.signal.Key() || alert.Target != role.service+"-inventory" || alert.Frame != role.protocol {
			t.Fatalf("%s changed visibility identity, hold, or carrier", role.signal.Key())
		}
		for _, field := range append([]string{"visibility=unknown reason=bounded-source-unavailable"}, want...) {
			if !strings.Contains(alert.Observed, field) {
				t.Errorf("%s lost fixed diagnostic %s", role.signal.Key(), field)
			}
		}
		for _, field := range append([]string{"http_status=", "backend_cause="}, absent...) {
			if strings.Contains(alert.Observed, field) {
				t.Errorf("%s invented field %s", role.signal.Key(), field)
			}
		}
		requireAlertOmits(t, alert, "private-h1plus-partial-body", "private-h1plus-error", "secret.example", "credential-canary", "SELECT private_column")
	}
}

// Keep returned typed class and native-boundary facts without changing visibility.
func TestH1PlusSourceFailureRetainsTypedFiniteDiagnostics(t *testing.T) {
	err := &observationBoundaryError{err: fmt.Errorf("private-h1plus-error: %w", context.DeadlineExceeded), boundary: observationBoundaryNativeWait}
	requireH1PlusSourceDiagnostic(t, err, []string{"error_class=observation-timeout", "execution_boundaries=native-wait/started-joined"}, []string{"ssh_admission_failures="})
}

// Refuse execution authority from text resembling typed markers.
func TestH1PlusSourceFailureKeepsUntypedBoundaryUnknown(t *testing.T) {
	// Text can resemble a code-owned marker but cannot become proof.
	err := errors.New("private-h1plus-error https://secret.example/?key=credential-canary SELECT private_column native-wait/started-joined ssh_admission_failures=acquire")
	requireH1PlusSourceDiagnostic(t, err, []string{"error_class=observation-unclassified"}, []string{"execution_boundaries=", "ssh_admission_failures="})
}

// Retain independent joined markers without asserting a shared attempt or cause.
func TestH1PlusSourceFailurePreservesJoinedFiniteMarkers(t *testing.T) {
	privateErr := errors.New("private-h1plus-error credential-canary")
	err := errors.Join(
		&sshAdmissionUnavailableError{err: privateErr, phase: sshAdmissionFailureAcquire},
		&sshAdmissionUnavailableError{err: privateErr, phase: sshAdmissionFailureRelease},
		&observationBoundaryError{err: context.DeadlineExceeded, boundary: observationBoundaryHost},
		&observationBoundaryError{err: privateErr, boundary: observationBoundaryNativeWait},
	)
	requireH1PlusSourceDiagnostic(t, err, []string{"error_class=observation-timeout", "ssh_admission_failures=acquire,release", "execution_boundaries=host-admission/not-attempted,native-wait/started-joined"}, nil)
}

// Reduce malformed typed markers to unknown rather than invented native progress.
func TestH1PlusSourceFailureKeepsMalformedMarkersUnknown(t *testing.T) {
	err := &observationBoundaryError{
		err:      &sshAdmissionUnavailableError{err: errors.New("private-h1plus-error"), phase: sshAdmissionFailurePhase(255)},
		boundary: observationBoundary(255),
	}
	requireH1PlusSourceDiagnostic(t, err, []string{"error_class=observation-admission-unavailable", "ssh_admission_failures=unknown", "execution_boundaries=unknown/unknown"}, []string{"native-wait/", "not-attempted"})
}

// Keep malformed successful reads separate from source-failure diagnostics.
func TestH1PlusSourceDiagnosticsDoNotLabelMalformedSuccessfulRead(t *testing.T) {
	for _, role := range h1PlusDiagnosticSignals() {
		calls := 0
		settings := h1PlusTestSettings(t, h1PlusTestNow(), role.service, h1PlusTestFleet(h1PlusTestNow()))
		settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls++
			return "private-h1plus-malformed-body native-wait/started-joined", nil
		}}
		alerts, err := role.signal.Run(context.Background(), settings)
		if err != nil || calls != 1 || len(alerts) != 1 || alerts[0].Class != "h1plus-telemetry-incomplete" || !strings.Contains(alerts[0].Observed, "reason=invalid-or-incomplete-source-response") {
			t.Fatalf("%s changed malformed successful-read outcome", role.signal.Key())
		}
		for _, field := range []string{"error_class=", "execution_boundaries=", "ssh_admission_failures="} {
			if strings.Contains(alerts[0].Observed, field) {
				t.Errorf("%s borrowed source-failure fields into parsed response", role.signal.Key())
			}
		}
		requireAlertOmits(t, alerts[0], "private-h1plus-malformed-body", "native-wait/started-joined")
	}
}

// Preserve complete healthy observations independently for each carrier.
func TestH1PlusSourceDiagnosticsPreserveHealthyIndependentRoles(t *testing.T) {
	for _, role := range h1PlusDiagnosticSignals() {
		now := h1PlusTestNow()
		alerts, err := role.signal.Run(context.Background(), h1PlusTestSettings(t, now, role.service, h1PlusTestFleet(now)))
		if err != nil || len(alerts) != 0 {
			t.Fatalf("%s changed complete active carrier control", role.signal.Key())
		}
	}
}

// Retain lifecycle precedence and prevent late diagnostic publication.
func TestH1PlusSourceDiagnosticsCanceledOwnerPublishesNothing(t *testing.T) {
	for _, role := range h1PlusDiagnosticSignals() {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		settings := h1PlusTestSettings(t, h1PlusTestNow(), role.service, h1PlusTestFleet(h1PlusTestNow()))
		settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls++
			cancel()
			return "private-h1plus-canceled-body", &observationBoundaryError{err: errors.New("private-h1plus-error"), boundary: observationBoundaryNativeWait}
		}}
		alerts, err := role.signal.Run(ctx, settings)
		cancel()
		if !errors.Is(err, context.Canceled) || len(alerts) != 0 || calls != 1 {
			t.Fatalf("%s changed canceled owner or published late diagnostics", role.signal.Key())
		}
		if strings.Contains(err.Error(), "private-h1plus-") {
			t.Errorf("%s rendered private canceled source data", role.signal.Key())
		}
	}
}
