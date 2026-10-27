// Public failure-share controls retain fixed observation diagnostics without
// changing ratio policy, availability holds, identity or parent retirement.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A live parent's source error may retain independent joined local markers.
// Injected error types test projection only; callbacks manufacture no native
// process or remote execution evidence of their own.
func TestContractFailureShareSourceErrorRetainsSafeDiagnostics(t *testing.T) {
	for _, test := range []struct {
		sourceErr error
		observed  string
	}{
		{
			sourceErr: errors.Join(
				&sshAdmissionUnavailableError{err: errors.New("private-observation-canary"), phase: sshAdmissionFailureRelease},
				&observationBoundaryError{err: &mimirShellNativeError{err: errors.New("private-observation-canary"), stderr: "private-stderr-canary"}, boundary: observationBoundaryNativeWait},
			),
			observed: "reason=source_unavailable error_class=observation-admission-unavailable ssh_admission_failures=release execution_boundaries=native-wait/started-joined",
		},
		{
			sourceErr: &sshAdmissionUnavailableError{err: fmt.Errorf("private-observation-canary: %w", context.DeadlineExceeded), phase: sshAdmissionFailureAcquire},
			observed:  "reason=source_unavailable error_class=observation-timeout ssh_admission_failures=acquire",
		},
		{
			sourceErr: &observationBoundaryError{err: fmt.Errorf("private-observation-canary: %w", context.DeadlineExceeded), boundary: observationBoundaryNativeStart},
			observed:  "reason=source_unavailable error_class=observation-timeout execution_boundaries=native-start/failed-no-child",
		},
		{
			sourceErr: fmt.Errorf("private-observation-canary: %w", context.Canceled),
			observed:  "reason=source_unavailable error_class=observation-canceled",
		},
		{
			sourceErr: errors.New("private-observation-canary"),
			observed:  "reason=source_unavailable error_class=observation-unclassified",
		},
	} {
		calls := 0
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls++
			return "private-output-canary", test.sourceErr
		}}
		settings := contractStageSettings(contractStageNow, source)
		if err := settings.Validate(); err != nil {
			t.Fatal("synthetic placement invalid")
		}
		observation := &signalClearHoldObservation{}
		ctx := context.WithValue(context.Background(), signalClearHoldContextKey{}, observation)
		alerts, err := NewContractFailureShareSignal().Run(ctx, settings)
		if err != nil || len(alerts) != 1 || calls != 1 {
			t.Fatal("source error changed observation or attempted sibling replay")
		}
		alert := alerts[0]
		prior := alertFromFinding(settings, "25.1", "contract-failure-share", "Returned contract failure share", contractFailureUnknown("synthetic", "source_unavailable"))
		if alert.Observed != test.observed || alert.Identity() != prior.Identity() || alert.Class != "contract-failure-share-unobservable" || alert.Severity != SeverityWarn || alert.Sustain != 2 || !alert.unobservable {
			t.Fatal("fixed diagnostic projection or visibility identity changed")
		}
		for _, canary := range []string{"private-observation-canary", "private-stderr-canary", "private-output-canary", "aabb0011", "api-a.example"} {
			if strings.Contains(alert.Markdown(), canary) {
				t.Fatal("private source data escaped observation projection")
			}
		}
		holds := observation.snapshot(false)
		if len(holds) != len(contractStageFixtureSlots) {
			t.Fatal("failed observation changed ratio clear holds")
		}
		targetBools := map[string]bool{}
		for _, hold := range holds {
			if hold.SignalId != contractFailureShareProbeId || hold.Class != "contract-failure-share" {
				t.Fatal("failed source held unrelated identity")
			}
			targetBools[hold.Target] = true
		}
		for _, slot := range contractStageFixtureSlots {
			if !targetBools["synthetic/"+slot.service] {
				t.Fatal("desired ratio acquired false absence credit")
			}
		}
	}
}

// Retiring the actual live owner publishes neither findings nor new clear holds,
// even when the returned source tree contains classifiable local evidence.
func TestContractFailureShareSourceDiagnosticsPreserveParentRetirement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		calls++
		cancel()
		return "private-output-canary", &observationBoundaryError{err: errors.New("private-observation-canary"), boundary: observationBoundaryNativeWait}
	}}
	settings := contractStageSettings(contractStageNow, source)
	if err := settings.Validate(); err != nil {
		t.Fatal("synthetic placement invalid")
	}
	observation := &signalClearHoldObservation{}
	ctx = context.WithValue(ctx, signalClearHoldContextKey{}, observation)
	alerts, err := NewContractFailureShareSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || calls != 1 || len(alerts) != 0 || len(observation.snapshot(false)) != 0 {
		t.Fatal("retired source published diagnostics, holds or repeated contact")
	}
}

// Successful complete history remains eligible for ordinary healthy recovery;
// a successful transport with malformed body retains its separate parse reason.
func TestContractFailureShareDiagnosticsKeepHealthyAndParsePolicy(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		calls := 0
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			calls++
			if malformed {
				return "private-malformed-canary", nil
			}
			return contractFailureFixtureJson(t, contractFailureFixture(contractStageNow, 0)), nil
		}}
		settings := contractStageSettings(contractStageNow, source)
		if err := settings.Validate(); err != nil {
			t.Fatal("synthetic placement invalid")
		}
		observation := &signalClearHoldObservation{}
		ctx := context.WithValue(context.Background(), signalClearHoldContextKey{}, observation)
		alerts, err := NewContractFailureShareSignal().Run(ctx, settings)
		if err != nil || calls != 1 {
			t.Fatal("diagnostic correction changed successful transport")
		}
		if malformed {
			if len(alerts) != 1 || alerts[0].Observed != "reason=invalid_response" || !alerts[0].unobservable || len(observation.snapshot(false)) != 4 || strings.Contains(alerts[0].Markdown(), "private-malformed-canary") {
				t.Fatal("parse refusal borrowed transport classification or leaked body")
			}
		} else if len(alerts) != 0 || len(observation.snapshot(false)) != 0 {
			t.Fatal("healthy complete history changed availability")
		}
	}
}
