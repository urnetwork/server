// SIGNALS.md §1.7 keeps local admission failures distinct from native transport
// evidence while preserving the lifecycle class and existing visibility gate.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The injected boundary never executes SSH or mutates shared admission state.
type sshAdmissionPhaseFixture struct {
	acquireCommand func(context.Context, string) (func() error, error)
}

func (self *sshAdmissionPhaseFixture) acquire(ctx context.Context, host string) (func() error, error) {
	return self.acquireCommand(ctx, host)
}

// Exercise both real visibility reducers, including stable identity and privacy.
func requireSshAdmissionPhaseVisibility(t *testing.T, err error, errorClass, phases string) {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{})
	signal := NewProviderCountSignal()
	whole := visibilityAlert(settings, signal, err)
	partial := alertFromFinding(settings, signal.Number(), signal.Key(), signal.Name(), cannotObserveFinding("synthetic-observation", err))
	generic := errors.New("synthetic command failure")
	wantObserved := "error_class=" + errorClass
	if phases != "" {
		wantObserved += " ssh_admission_failures=" + phases
	}
	for _, alert := range []Alert{whole, partial} {
		if alert.Observed != wantObserved {
			t.Errorf("causal admission phase missing: got %q, want %q", alert.Observed, wantObserved)
		}
		if alert.Class != "cannot-observe" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
			t.Error("admission phase changed the visibility class, severity or sustain")
		}
		if strings.Contains(alert.Action, "A local admission refusal") {
			t.Error("admission guidance still treats every local failure as a precontact refusal")
		}
		requireAlertOmits(t, alert, "private-admission-canary", "192.0.2.91", "192.0.2.92", "synthetic-private-gateway", "synthetic source bytes", "ssh_spawn=")
	}
	if whole.Identity() != visibilityAlert(settings, signal, generic).Identity() ||
		partial.Identity() != alertFromFinding(settings, signal.Number(), signal.Key(), signal.Name(), cannotObserveFinding("synthetic-observation", generic)).Identity() {
		t.Error("admission phase changed the existing alert identity")
	}
}

// Acquisition can carry an authoritative deadline without invoking transport.
func TestSshAdmissionPhaseAcquireDeadlineReachesVisibility(t *testing.T) {
	calls := 0
	admission := &sshAdmissionPhaseFixture{acquireCommand: func(context.Context, string) (func() error, error) {
		return nil, fmt.Errorf("private-admission-canary: %w", context.DeadlineExceeded)
	}}
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: admission})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		calls++
		return "synthetic source bytes", "", nil
	}
	out, err := runner.shell(context.Background(), &host{name: "synthetic-private-gateway", overlayIp: "192.0.2.91"}, "true")
	if calls != 0 || out != "" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("acquisition fixture did not stop before transport with its original deadline")
	}
	requireSshAdmissionPhaseVisibility(t, err, observationErrorClassTimeout, "acquire")
}

// Release can fail after a callback returned; the callback is not native-spawn
// authority and its output cannot make the failed observation healthy.
func TestSshAdmissionPhaseReleaseDeadlineReachesVisibility(t *testing.T) {
	calls, releases := 0, 0
	admission := &sshAdmissionPhaseFixture{acquireCommand: func(context.Context, string) (func() error, error) {
		return func() error {
			releases++
			return fmt.Errorf("private-admission-canary: %w", context.DeadlineExceeded)
		}, nil
	}}
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: admission})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		calls++
		return "synthetic source bytes", "", nil
	}
	out, err := runner.shell(context.Background(), &host{name: "synthetic-private-gateway", overlayIp: "192.0.2.91"}, "true")
	if calls != 1 || releases != 1 || out != "synthetic source bytes" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("release fixture did not preserve its callback output and original cleanup deadline")
	}
	requireSshAdmissionPhaseVisibility(t, err, observationErrorClassTimeout, "release")
}

// A joined gateway error can contain different phases from different attempts.
// Both survive in canonical order; neither becomes a claim about every gateway.
func TestSshAdmissionPhaseMixedGatewaysRetainBothFailures(t *testing.T) {
	calls := 0
	admission := &sshAdmissionPhaseFixture{acquireCommand: func(_ context.Context, host string) (func() error, error) {
		if host == "synthetic-private-gateway-a" {
			return func() error { return fmt.Errorf("private-admission-canary: %w", context.DeadlineExceeded) }, nil
		}
		return nil, fmt.Errorf("private-admission-canary: %w", context.DeadlineExceeded)
	}}
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: admission})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		calls++
		return "synthetic source bytes", "", nil
	}
	hosts := []*host{
		{name: "synthetic-private-gateway-a", overlayIp: "192.0.2.91"},
		{name: "synthetic-private-gateway-b", overlayIp: "192.0.2.92"},
	}
	out, selected, err := shellFirstServiceGateway(context.Background(), runner, hosts, nil, "true")
	if calls != 1 || out != "" || selected != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("gateway fixture did not retain both failed attempts")
	}
	requireSshAdmissionPhaseVisibility(t, err, observationErrorClassTimeout, "acquire,release")
}

// Ordinary continuous probes have no owner deadline; the shared queue's own
// finite refusal is admission-unavailable rather than a lifecycle timeout.
func TestSshAdmissionPhasePlainQueueDeadlineKeepsAdmissionClass(t *testing.T) {
	admission := &sshAdmissionPhaseFixture{acquireCommand: func(ctx context.Context, _ string) (func() error, error) {
		if _, finite := ctx.Deadline(); finite {
			t.Fatal("ordinary SSH queue acquired a new command deadline")
		}
		return nil, errors.New("SSH admission queue deadline")
	}}
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: admission})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		t.Fatal("queue refusal invoked transport")
		return "", "", nil
	}
	_, err := runner.shell(context.Background(), &host{name: "synthetic-private-gateway", overlayIp: "192.0.2.91"}, "true")
	requireSshAdmissionPhaseVisibility(t, err, observationErrorClassAdmissionUnavailable, "acquire")
	for _, signal := range []Signal{NewMimirShutdownSignal(), NewJournalBufferSignal(), NewProviderCountSignal(), NewCardinalitySignal(), NewProxyDeviceTargetSignal()} {
		if signal.(*signalAdapter).runBudget() != 0 {
			t.Errorf("ordinary %s observation gained a queue-inclusive owner deadline", signal.Key())
		}
	}
}

// The phase is independent of lifecycle precedence, including cancellation and
// an unphased legacy wrapper. Generic text must never invent a typed boundary.
func TestSshAdmissionPhaseLifecycleAndUnknownControls(t *testing.T) {
	for _, testCase := range []struct {
		err        error
		errorClass string
		phases     string
	}{
		{err: &sshAdmissionUnavailableError{err: context.DeadlineExceeded}, errorClass: observationErrorClassTimeout, phases: "unknown"},
		{err: &sshAdmissionUnavailableError{err: context.Canceled}, errorClass: observationErrorClassCanceled, phases: "unknown"},
		{err: errors.Join(errors.New("private-admission-canary"), &sshAdmissionUnavailableError{err: errors.New("private-admission-canary")}), errorClass: observationErrorClassAdmissionUnavailable, phases: "unknown"},
		{err: fmt.Errorf("SSH admission acquire private-admission-canary: %w", context.DeadlineExceeded), errorClass: observationErrorClassTimeout},
		{err: context.Canceled, errorClass: observationErrorClassCanceled},
	} {
		requireSshAdmissionPhaseVisibility(t, testCase.err, testCase.errorClass, testCase.phases)
	}
}

// Successful acquisition, callback and release preserve the same command
// deadline and output. No marker is a new observation or a healthy Alert.
func TestSshAdmissionPhaseHealthyTransportIsUnchanged(t *testing.T) {
	releases := 0
	admission := &sshAdmissionPhaseFixture{acquireCommand: func(context.Context, string) (func() error, error) {
		return func() error { releases++; return nil }, nil
	}}
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Minute, sharedSshAdmission: admission})
	runner.runSSH = func(ctx context.Context, _ []string, _ string) (string, string, error) {
		deadline, finite := ctx.Deadline()
		if !finite || time.Until(deadline) <= 0 || time.Until(deadline) > time.Minute {
			t.Fatal("transport lost its existing finite command deadline")
		}
		return "synthetic source bytes", "", nil
	}
	out, err := runner.shell(context.Background(), &host{name: "synthetic-private-gateway", overlayIp: "192.0.2.91"}, "true")
	if err != nil || out != "synthetic source bytes" || releases != 1 {
		t.Fatal("phase evidence changed healthy transport output or release")
	}
}

// A successful later gateway keeps the established gateway-selection contract;
// phase markers only describe errors the caller actually returns.
func TestSshAdmissionPhaseHealthyGatewayFallbackIsUnchanged(t *testing.T) {
	admission := &sshAdmissionPhaseFixture{acquireCommand: func(_ context.Context, host string) (func() error, error) {
		if host == "synthetic-private-gateway-a" {
			return nil, fmt.Errorf("private-admission-canary: %w", context.DeadlineExceeded)
		}
		return func() error { return nil }, nil
	}}
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: admission})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		return "synthetic source bytes", "", nil
	}
	hosts := []*host{
		{name: "synthetic-private-gateway-a", overlayIp: "192.0.2.91"},
		{name: "synthetic-private-gateway-b", overlayIp: "192.0.2.92"},
	}
	out, selected, err := shellFirstServiceGateway(context.Background(), runner, hosts, nil, "true")
	if err != nil || out != "synthetic source bytes" || selected != hosts[1] {
		t.Fatal("admission evidence changed successful gateway fallback")
	}
}
