// SIGNALS.md §1.7 preserves returned source causes without changing the
// lifecycle verdict of a host-scoped signal whose parent has ended.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// Cancellation occurs inside the synchronous probe, before both admission
// causes return. No scheduler, process, real source or elapsed delay is involved.
func TestSignalContextErrorPreservesReturnedAdmissionCauses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acquireCause := errors.New("private-admission-canary-acquire")
	releaseCause := errors.New("private-admission-canary-release")
	acquire := &sshAdmissionUnavailableError{err: acquireCause, phase: sshAdmissionFailureAcquire}
	release := &sshAdmissionUnavailableError{err: releaseCause, phase: sshAdmissionFailureRelease}
	sourceErr := errors.Join(acquire, release)
	calls := 0
	probe := hostScopeSyntheticProbe{checkFn: func(probeCtx context.Context, env *probeEnv) ([]finding, error) {
		calls++
		if probeCtx.Err() != nil {
			t.Fatal("probe was already canceled before the synchronous source return")
		}
		if _, scoped := env.runner.(*hostScopeRunner); !scoped {
			t.Fatal("fixture did not enable host observation policy")
		}
		cancel()
		return []finding{{probeId: "synthetic/returned", class: "synthetic-source-finding", target: "synthetic-source"}}, sourceErr
	}}
	signal := &signalAdapter{number: "1.7", key: "synthetic-context", name: "Synthetic context", probe: probe}
	settings := syntheticSettings(&syntheticSource{})
	settings.disabledHosts = []HostSettings{{Name: "disabled.example.test"}}
	alerts, err := signal.Run(ctx, settings)
	if calls != 1 || len(alerts) != 0 || !errors.Is(err, context.Canceled) {
		t.Fatal("parent cancellation did not withhold the completed probe findings")
	}
	if !errors.Is(err, sourceErr) || !errors.Is(err, acquireCause) || !errors.Is(err, releaseCause) {
		t.Fatal("synchronously returned source causes were discarded")
	}
	var returned *sshAdmissionUnavailableError
	if !errors.As(err, &returned) || returned != acquire {
		t.Fatal("typed source cause is no longer reachable")
	}
	if err.Error() != context.Canceled.Error() {
		t.Fatal("parent lifecycle error text changed")
	}
	requireSshAdmissionPhaseVisibility(t, err, observationErrorClassCanceled, "acquire,release")
}

// A source deadline or specialized provider/SSH failure must remain reachable
// without overriding the parent verdict, generic identity or private renderer.
func TestSignalContextErrorParentVerdictWinsReturnedSourceCause(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		parentDeadline bool
		sourceErr      error
		sourceClass    string
		parentClass    string
	}{
		{name: "source deadline", sourceErr: context.DeadlineExceeded, parentClass: observationErrorClassCanceled},
		{name: "source cancellation", parentDeadline: true, sourceErr: context.Canceled, parentClass: observationErrorClassTimeout},
		{name: "provider", sourceErr: newProviderVisibilityError(providerAuthenticationClass, "synthetic-private-provider-canary"), sourceClass: providerAuthenticationClass, parentClass: observationErrorClassCanceled},
		{name: "SSH reset", sourceErr: errors.New("synthetic-private-gateway.example.test: kex_exchange_identification: synthetic-private-reset-canary"), sourceClass: "ssh-admission-reset", parentClass: observationErrorClassCanceled},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		if testCase.parentDeadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
		}
		calls := 0
		probe := hostScopeSyntheticProbe{checkFn: func(context.Context, *probeEnv) ([]finding, error) {
			calls++
			cancel()
			return []finding{{probeId: "synthetic/returned", class: "synthetic-source-finding", target: "synthetic-source"}}, testCase.sourceErr
		}}
		signal := &signalAdapter{number: "1.7", key: "synthetic-context", name: "Synthetic context", probe: probe}
		settings := syntheticSettings(&syntheticSource{})
		settings.disabledHosts = []HostSettings{{Name: "disabled.example.test"}}
		if testCase.sourceClass != "" && visibilityAlert(settings, signal, testCase.sourceErr).Class != testCase.sourceClass {
			t.Fatalf("%s: specialized source fixture was inactive", testCase.name)
		}
		alerts, err := signal.Run(ctx, settings)
		cancel()
		if calls != 1 || len(alerts) != 0 || !errors.Is(err, ctx.Err()) || !errors.Is(err, testCase.sourceErr) {
			t.Fatalf("%s: completed causes or withheld findings changed", testCase.name)
		}
		if err.Error() != ctx.Err().Error() {
			t.Fatalf("%s: private source text replaced the parent error", testCase.name)
		}
		for _, returned := range []error{err, fmt.Errorf("synthetic outer error: %w", err)} {
			alert := visibilityAlert(settings, signal, returned)
			if classifyObservationError(returned) != testCase.parentClass || alert.Class != "cannot-observe" || alert.Observed != "error_class="+testCase.parentClass {
				t.Fatalf("%s: source cause overrode the parent verdict", testCase.name)
			}
			if alert.Identity() != visibilityAlert(settings, signal, ctx.Err()).Identity() || alert.Severity != SeverityWarn || alert.Sustain != 2 {
				t.Fatalf("%s: cancellation changed visibility identity or severity", testCase.name)
			}
			requireAlertOmits(t, alert, "synthetic-private-provider-canary", "synthetic-private-gateway.example.test", "synthetic-private-reset-canary", "synthetic-source-finding")
		}
	}
}

// Without an additional source cause, callers keep exact context sentinels,
// including callers that compare the returned error directly.
func TestSignalContextErrorNilAndPlainSourceKeepContextSentinel(t *testing.T) {
	for _, testCase := range []struct {
		parentDeadline bool
		plainSource    bool
	}{
		{},
		{plainSource: true},
		{parentDeadline: true},
		{parentDeadline: true, plainSource: true},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		if testCase.parentDeadline {
			cancel()
			ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
		}
		probe := hostScopeSyntheticProbe{checkFn: func(context.Context, *probeEnv) ([]finding, error) {
			cancel()
			var sourceErr error
			if testCase.plainSource {
				sourceErr = ctx.Err()
			}
			return []finding{{probeId: "synthetic/returned", class: "synthetic-source-finding", target: "synthetic-source"}}, sourceErr
		}}
		signal := &signalAdapter{number: "1.7", key: "synthetic-context", name: "Synthetic context", probe: probe}
		settings := syntheticSettings(&syntheticSource{})
		settings.disabledHosts = []HostSettings{{Name: "disabled.example.test"}}
		alerts, err := signal.Run(ctx, settings)
		cancel()
		if err != ctx.Err() || len(alerts) != 0 {
			t.Fatal("plain cancellation lost its exact sentinel or withheld findings")
		}
	}
}

// A live parent still returns the probe's own error and specialized class.
func TestSignalContextErrorLiveParentPreservesProbeAuthority(t *testing.T) {
	sourceErr := newProviderVisibilityError(providerAuthenticationClass, "synthetic-private-provider-canary")
	probe := hostScopeSyntheticProbe{checkFn: func(context.Context, *probeEnv) ([]finding, error) {
		return nil, sourceErr
	}}
	signal := &signalAdapter{number: "1.7", key: "synthetic-context", name: "Synthetic context", probe: probe}
	settings := syntheticSettings(&syntheticSource{})
	settings.disabledHosts = []HostSettings{{Name: "disabled.example.test"}}
	alerts, err := signal.Run(context.Background(), settings)
	if err != sourceErr || len(alerts) != 0 || visibilityAlert(settings, signal, err).Class != providerAuthenticationClass {
		t.Fatal("live parent changed the probe's source-error authority")
	}
}
