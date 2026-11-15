// Exercises the shared §26 source-failure projection through all public
// constructors with synthetic callbacks and retained local healthy fixtures.
package monitor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The ten consumers share one read path but keep independent signal identities.
func stormDiagnosticSignals() []Signal {
	return []Signal{
		NewPgBouncerBacklogSignal(), NewServicesCpuSignal(), NewDbAcquireWaitSignal(), NewContractCreationsStuckSignal(),
		NewReservationReleaseSignal(), NewRedisPoolTimeoutsSignal(), NewRedisMainThreadSignal(), NewProxyHostCpuSignal(),
		NewContractReplyCollapseSignal(), NewHostedContractCapSignal(),
	}
}

// Typed causes and independent local markers survive the source wrapper,
// without promoting private diagnostics into a remote-cause or HTTP claim.
func TestStormSourceFailuresRetainFixedDiagnosticsAcrossAllProbes(t *testing.T) {
	privateErr := errors.New("private-storm-diagnostic https://secret.invalid/private?key=canary SQL SELECT private_payload")
	for _, variant := range []struct {
		name        string
		sourceErr   error
		wantStrings []string
	}{
		{name: "joined local boundaries", sourceErr: errors.Join(
			&sshAdmissionUnavailableError{err: privateErr, phase: sshAdmissionFailureAcquire},
			&sshAdmissionUnavailableError{err: privateErr, phase: sshAdmissionFailureRelease},
			&observationBoundaryError{err: context.DeadlineExceeded, boundary: observationBoundaryHost},
			&observationBoundaryError{err: privateErr, boundary: observationBoundaryNativeWait},
		), wantStrings: []string{"error_class=observation-timeout", "ssh_admission_failures=acquire,release", "execution_boundaries=host-admission/not-attempted,native-wait/started-joined"}},
		{name: "untyped source", sourceErr: privateErr, wantStrings: []string{"error_class=observation-unclassified"}},
	} {
		for _, signal := range stormDiagnosticSignals() {
			calls := 0
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				calls++
				return "private-storm-partial-body", variant.sourceErr
			}}
			alerts, err := signal.Run(context.Background(), stormMainSettings(stormSyntheticNow, source))
			if err != nil || len(alerts) != 1 || calls != 4 {
				t.Fatalf("%s %s: source failure projection changed gateway attempts or alert count", signal.Key(), variant.name)
			}
			alert := alerts[0]
			if alert.Class != signal.Key()+"-unobservable" || alert.Severity != SeverityWarn || alert.Sustain != 2 || !alert.unobservable || !strings.Contains(alert.Observed, "visibility_reason=source_unavailable gaps=none") {
				t.Fatalf("%s %s: source failure changed visibility identity or holds", signal.Key(), variant.name)
			}
			for _, want := range variant.wantStrings {
				if !strings.Contains(alert.Observed, want) {
					t.Fatalf("%s %s: source failure lost fixed diagnostic field", signal.Key(), variant.name)
				}
			}
			if strings.Contains(alert.Observed, "http_status=") || strings.Contains(alert.Observed, "backend_cause=") || (variant.name == "untyped source" && strings.Contains(alert.Observed, "execution_boundaries=")) {
				t.Fatalf("%s %s: unknown source invented status, cause or execution proof", signal.Key(), variant.name)
			}
			requireAlertOmits(t, alert, "private-storm-diagnostic", "secret.invalid", "key=canary", "SELECT private_payload", "private-storm-partial-body")
		}
	}
}

// A completed matrix with missing rows, malformed data and a complete healthy
// history remain different from a transport failure; none borrows its markers.
func TestStormSourceDiagnosticsKeepSuccessfulAndInvalidReadsSeparate(t *testing.T) {
	for _, signal := range stormDiagnosticSignals() {
		for _, response := range []string{
			"private-storm-malformed-body",
			`{"status":"success","data":{"resultType":"matrix","result":[]}}`,
		} {
			calls := 0
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { calls++; return response, nil }}
			alerts, err := signal.Run(context.Background(), stormMainSettings(stormSyntheticNow, source))
			if err != nil || len(alerts) == 0 || calls != 1 {
				t.Fatalf("%s: incomplete response lost visibility or changed query cost", signal.Key())
			}
			for _, alert := range alerts {
				if !alert.unobservable || strings.Contains(alert.Observed, "error_class=") || strings.Contains(alert.Observed, "visibility_reason=source_unavailable") {
					t.Fatalf("%s: source diagnostics leaked into successful transport or invented health", signal.Key())
				}
				requireAlertOmits(t, alert, "private-storm-malformed-body")
			}
		}
		cases := loadStormReplayFixture(t, signal.Key())
		alerts := runStormReplay(t, signal, cases["healthy"], nil)
		if len(alerts) != 0 {
			t.Fatalf("%s: complete healthy history changed", signal.Key())
		}
	}
}

// Owner retirement still wins over a source return: it cannot publish fixed
// diagnostics, partial measurements or private text after that owner ends.
func TestStormSourceDiagnosticsPreserveCanceledOwner(t *testing.T) {
	for _, signal := range stormDiagnosticSignals() {
		ctx, cancel := context.WithCancel(context.Background())
		privateErr := errors.New("private-storm-canceled-source")
		calls := 0
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls++
			cancel()
			return "private-storm-canceled-body", &observationBoundaryError{err: privateErr, boundary: observationBoundaryNativeWait}
		}}
		alerts, err := signal.Run(ctx, stormMainSettings(stormSyntheticNow, source))
		cancel()
		if !errors.Is(err, context.Canceled) || !errors.Is(err, privateErr) || len(alerts) != 0 || calls != 1 {
			t.Fatalf("%s: retired source owner lost precedence, cause or publication fence", signal.Key())
		}
		if strings.Contains(err.Error(), "private-storm-canceled") {
			t.Fatalf("%s: lifecycle error rendered private source text", signal.Key())
		}
	}
}
