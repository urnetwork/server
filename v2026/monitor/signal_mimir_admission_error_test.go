// Mimir host failures must reach the shared fixed-output classifier without
// losing the error tree that owns lifecycle, admission and native status.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The source returns these failures through the real per-host check and alert
// conversion. The parent is live, so the wrapped deadline belongs to the
// observation rather than an abandoned test invocation.
func TestMimirAdmissionObservationFailurePreservesWrappedClasses(t *testing.T) {
	const privateMarker = "generated-private-observer-fragment"
	exit255 := syntheticProcessExit(t, 255)
	exit2 := syntheticProcessExit(t, 2)
	admissionErr := &sshAdmissionUnavailableError{err: errors.New(privateMarker)}
	now := time.Date(2033, 1, 2, 3, 4, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name string
		err  error
		want string
	}{
		{name: "wrapped-deadline", err: context.DeadlineExceeded, want: observationErrorClassTimeout},
		{name: "wrapped-cancellation", err: context.Canceled, want: observationErrorClassCanceled},
		{name: "typed-admission", err: admissionErr, want: observationErrorClassAdmissionUnavailable},
		{name: "joined-admission-deadline", err: errors.Join(admissionErr, context.DeadlineExceeded), want: observationErrorClassTimeout},
		{name: "native-ssh-255", err: &sshCommandError{err: exit255}, want: observationErrorClassSSHExit255},
		{name: "native-local-255", err: exit255, want: observationErrorClassCommandFailed},
		{name: "native-remote-2", err: &sshCommandError{err: exit2}, want: observationErrorClassCommandFailed},
		{name: "status-text-is-not-native-ssh", err: errors.New("exit status 255"), want: observationErrorClassCommandFailed},
		{name: "invalid-response", err: errors.New("invalid response"), want: observationErrorClassInvalidResponse},
		{name: "unknown-cause", err: errors.New(privateMarker), want: observationErrorClassUnclassified},
	} {
		wrappedErr := fmt.Errorf("%s: %w", privateMarker, testCase.err)
		if got := classifyObservationError(wrappedErr); got != testCase.want {
			t.Fatalf("%s fixture class=%s, want %s", testCase.name, got, testCase.want)
		}
		alerts := runMimirAdmissionSynthetic(t, NewMimirAdmissionSignal(), now,
			map[string]mimirAdmissionSyntheticResponse{
				"observer-source.example": {err: wrappedErr},
			},
		)
		alert := requireMimirAdmissionObservationFailure(t, alerts, "observer-source.example/mimir-admission")
		classField, _, _ := strings.Cut(alert.Observed, " ")
		if classField != "error_class="+testCase.want {
			t.Errorf("causal Mimir admission error class lost: %s got=%s want=%s", testCase.name, alert.Observed, testCase.want)
		}
		if len(alerts) != 1 {
			t.Errorf("%s source error invented a concrete admission verdict: alerts=%d", testCase.name, len(alerts))
		}
		requireAlertOmits(t, alert, privateMarker, "host observation failed")
	}
}

// A joined cause can carry both lifecycle authority and a separately owned
// admission failure. Fixed class selection must not destroy either branch.
func TestMimirAdmissionObservationFailureRetainsCauseTree(t *testing.T) {
	const privateMarker = "generated-private-joined-cause"
	admissionErr := &sshAdmissionUnavailableError{err: errors.New(privateMarker)}
	sourceErr := errors.Join(
		fmt.Errorf("%s: %w", privateMarker, context.DeadlineExceeded),
		fmt.Errorf("%s: %w", privateMarker, admissionErr),
	)
	probe := &mimirAdmissionProbe{}
	assessment := probe.observe(time.Date(2033, 2, 3, 4, 5, 0, 0, time.UTC), []mimirAdmissionHostResult{
		{host: &host{name: "observer-source.example"}, err: sourceErr},
	})
	if len(assessment.visibilityFailures) != 1 || assessment.directComplete || assessment.comparable {
		t.Fatal("failed source lost its unknown assessment")
	}
	failure := assessment.visibilityFailures[0]
	var retainedAdmission *sshAdmissionUnavailableError
	if !errors.Is(failure.err, context.DeadlineExceeded) ||
		!errors.As(failure.err, &retainedAdmission) || retainedAdmission != admissionErr {
		t.Error("causal Mimir admission cause tree lost: lifecycle and typed admission must remain reachable")
	}
	finding := cannotObserveFinding(failure.target, failure.err)
	if strings.Contains(finding.observed, privateMarker) || strings.Contains(finding.symptom, privateMarker) {
		t.Fatal("joined source error escaped the fixed-output boundary")
	}
}

// Missing child discovery, incomplete child reduction and malformed frames do
// not acquire a transport classification merely because errors are preserved.
func TestMimirAdmissionObservationFailureKeepsUnknownFramesUnknown(t *testing.T) {
	const privateMarker = "generated-private-unknown-field"
	now := time.Date(2033, 3, 4, 5, 6, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name   string
		output string
	}{
		{name: "no-identified-child", output: mimirAdmissionHostFixture(nil, true, 0, 0, 0)},
		{name: "unobservable-child", output: mimirAdmissionHostFixture([]string{
			mimirAdmissionInstanceFixture(mimirAdmissionFixtureOptions{port: mimirAdmissionSyntheticPort}),
		}, true, 0, 0, 0)},
		{name: "malformed-frame", output: privateMarker + " 1\n"},
	} {
		alerts := runMimirAdmissionSynthetic(t, NewMimirAdmissionSignal(), now,
			map[string]mimirAdmissionSyntheticResponse{
				"observer-source.example": {output: testCase.output},
			},
		)
		alert := requireMimirAdmissionObservationFailure(t, alerts, "observer-source.example/mimir-admission")
		if alert.Observed != "error_class="+observationErrorClassUnclassified || len(alerts) != 1 {
			t.Errorf("%s ambiguous frame acquired an unsupported verdict", testCase.name)
		}
		requireAlertOmits(t, alert, privateMarker, "unknown field", "no local Mimir child", "omitted its bounded observation")
	}
}

// Journal context can be unavailable or contain rejection-looking counts
// while exact child counters and headroom remain healthy.
func TestMimirAdmissionObservationFailureHealthyDirectCountersIgnoreJournalContext(t *testing.T) {
	now := time.Date(2033, 4, 5, 6, 7, 0, 0, time.UTC)
	for _, testCase := range []struct {
		name     string
		complete bool
		counts   [3]int64
	}{
		{name: "optional-journal-unavailable"},
		{name: "journal-rejections-with-flat-direct-counters", complete: true, counts: [3]int64{5, 7, 11}},
	} {
		output := mimirAdmissionHostFixture([]string{
			mimirAdmissionCompleteInstanceFixture(now.Add(-time.Hour).Unix(), 0, 123456, 3456),
		}, testCase.complete, testCase.counts[0], testCase.counts[1], testCase.counts[2])
		alerts := runMimirAdmissionSynthetic(t, NewMimirAdmissionSignal(), now,
			map[string]mimirAdmissionSyntheticResponse{
				"observer-source.example": {output: output},
			},
		)
		if len(alerts) != 0 {
			t.Errorf("%s invented an admission or visibility failure: alerts=%d", testCase.name, len(alerts))
		}
	}
}

// Positive direct counters retain both independent pages even while a sibling
// source fails. The failed sibling's class is additional visibility evidence.
func TestMimirAdmissionObservationFailurePreservesPositiveSibling(t *testing.T) {
	const privateMarker = "generated-private-sibling-cause"
	now := time.Date(2033, 5, 6, 7, 8, 0, 0, time.UTC)
	responses := mimirAdmissionCounterHostResponses(now.Add(-time.Hour).Unix(), 2, 3, 123456, 3456)
	responses["observer-source.example"] = mimirAdmissionSyntheticResponse{
		err: fmt.Errorf("%s: %w", privateMarker, context.DeadlineExceeded),
	}
	alerts := runMimirAdmissionSynthetic(t, NewMimirAdmissionSignal(), now, responses)
	for _, class := range []string{"mimir-series-limit", "mimir-ingestion-rate-limit"} {
		alert := requireAlertClass(t, alerts, class)
		if alert.Target != "mimir-fleet" || alert.Severity != SeverityPage || alert.Sustain != 1 {
			t.Errorf("%s lost its concrete page identity", class)
		}
		for _, required := range []string{"configured_hosts=2", "observable_hosts=1", "affected_instances=1", "direct_complete=false"} {
			if !strings.Contains(alert.Observed, required) {
				t.Errorf("%s lost partial-fleet evidence %q", class, required)
			}
		}
		requireAlertOmits(t, alert, privateMarker)
	}
	visibility := requireMimirAdmissionObservationFailure(t, alerts, "observer-source.example/mimir-admission")
	if visibility.Observed != "error_class="+observationErrorClassTimeout {
		t.Error("causal Mimir admission error class lost: positive sibling must not replace the failed source cause")
	}
	if len(alerts) != 3 {
		t.Errorf("partial-fleet result lost or invented a finding: alerts=%d", len(alerts))
	}
	requireAlertOmits(t, visibility, privateMarker)
}

// A failed host preserves prior incidents. Elapsed unknown time cannot count
// toward either two-hour quiet hold after complete observations return.
func TestMimirAdmissionObservationFailureRecoveryRequiresCompleteQuietWindow(t *testing.T) {
	signal := NewMimirAdmissionSignal()
	now := time.Date(2033, 6, 7, 8, 9, 0, 0, time.UTC)
	complete := mimirAdmissionCounterHostResponses(now.Add(-time.Hour).Unix(), 2, 3, 123456, 3456)
	initial := runMimirAdmissionSynthetic(t, signal, now, complete)
	requireAlertClass(t, initial, "mimir-series-limit")
	requireAlertClass(t, initial, "mimir-ingestion-rate-limit")
	unknown := runMimirAdmissionSynthetic(t, signal, now.Add(time.Minute),
		map[string]mimirAdmissionSyntheticResponse{
			"metrics-a.example": {err: fmt.Errorf("synthetic wrapper: %w", context.DeadlineExceeded)},
		},
	)
	requireAlertClass(t, unknown, "mimir-series-limit")
	requireAlertClass(t, unknown, "mimir-ingestion-rate-limit")
	visibility := requireMimirAdmissionObservationFailure(t, unknown, "metrics-a.example/mimir-admission")
	if visibility.Observed != "error_class="+observationErrorClassTimeout {
		t.Error("causal Mimir admission error class lost: retained incident must not erase the failed source cause")
	}
	restoredAt := now.Add(3 * time.Hour)
	restored := runMimirAdmissionSynthetic(t, signal, restoredAt, complete)
	requireAlertClass(t, restored, "mimir-series-limit")
	requireAlertClass(t, restored, "mimir-ingestion-rate-limit")
	if len(restored) != 2 {
		t.Errorf("corrected complete observation retained visibility failure: alerts=%d", len(restored))
	}
	if recovered := runMimirAdmissionSynthetic(t, signal, restoredAt.Add(mimirAdmissionQuietWindow), complete); len(recovered) != 0 {
		t.Errorf("complete comparable quiet window did not recover: alerts=%d", len(recovered))
	}
}

func requireMimirAdmissionObservationFailure(t *testing.T, alerts []Alert, target string) Alert {
	t.Helper()
	var found Alert
	count := 0
	for _, alert := range alerts {
		if alert.Class == "cannot-observe" && alert.Target == target {
			found = alert
			count++
		}
	}
	if count != 1 {
		t.Fatalf("host visibility findings=%d, want exactly one", count)
	}
	if found.SignalNumber != "11.20a" || found.SignalKey != "mimir-admission" ||
		found.SignalID != "monitor/visibility" || found.Severity != SeverityWarn ||
		found.Sustain != 2 || found.Frame != "" || found.Environment != "synthetic-environment" {
		t.Fatal("host failure changed its public signal, class, target or sustain identity")
	}
	return found
}
