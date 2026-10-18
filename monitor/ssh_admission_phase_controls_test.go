// Additive admission evidence must not convert local callback progress into
// native authority or collapse different joined failures into one attempt.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// Every possible typed phase set has one canonical finite representation.
func TestSshAdmissionPhaseSetsAreClosedAndOrdered(t *testing.T) {
	acquire := &sshAdmissionUnavailableError{err: errors.New("private-admission-canary"), phase: sshAdmissionFailureAcquire}
	release := &sshAdmissionUnavailableError{err: errors.New("private-admission-canary"), phase: sshAdmissionFailureRelease}
	unknown := &sshAdmissionUnavailableError{err: errors.New("private-admission-canary"), phase: sshAdmissionFailurePhase(255)}
	for _, testCase := range []struct {
		err    error
		phases string
	}{
		{err: acquire, phases: "acquire"},
		{err: release, phases: "release"},
		{err: unknown, phases: "unknown"},
		{err: errors.Join(release, acquire, release), phases: "acquire,release"},
		{err: errors.Join(unknown, acquire), phases: "acquire,unknown"},
		{err: errors.Join(unknown, release), phases: "release,unknown"},
		{err: fmt.Errorf("private-admission-canary: %w", errors.Join(unknown, release, acquire, unknown)), phases: "acquire,release,unknown"},
	} {
		requireSshAdmissionPhaseVisibility(t, testCase.err, observationErrorClassAdmissionUnavailable, testCase.phases)
	}
}

// A cleanup deadline retains the original native failure in the error tree.
// Lifecycle precedence remains unchanged and the marker never claims a spawn.
func TestSshAdmissionPhaseReleaseKeepsNativeFailure(t *testing.T) {
	nativeFailure := syntheticProcessExit(t, 255)
	admission := &sshAdmissionPhaseFixture{acquireCommand: func(context.Context, string) (func() error, error) {
		return func() error { return context.DeadlineExceeded }, nil
	}}
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: admission})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		return "", "private-admission-canary", nativeFailure
	}
	_, err := runner.shell(context.Background(), &host{name: "synthetic-private-gateway", overlayIp: "192.0.2.91"}, "true")
	if !errors.Is(err, nativeFailure) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("release phase discarded a native failure or cleanup deadline")
	}
	requireSshAdmissionPhaseVisibility(t, err, observationErrorClassTimeout, "release")
}

// A caller that reaches acquisition before cancellation keeps both the local
// phase and lifecycle class. Pre-canceled calls never invent admission evidence.
func TestSshAdmissionPhaseCancellationRetainsActualBoundary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	admission := &sshAdmissionPhaseFixture{acquireCommand: func(context.Context, string) (func() error, error) {
		calls++
		cancel()
		return nil, ctx.Err()
	}}
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: admission})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		t.Fatal("canceled acquisition invoked transport")
		return "", "", nil
	}
	target := &host{name: "synthetic-private-gateway", overlayIp: "192.0.2.91"}
	_, firstErr := runner.shell(ctx, target, "true")
	if calls != 1 || !errors.Is(firstErr, context.Canceled) {
		t.Fatal("cancellation fixture did not reach acquisition exactly once")
	}
	requireSshAdmissionPhaseVisibility(t, firstErr, observationErrorClassCanceled, "acquire")
	_, nextErr := runner.shell(ctx, target, "true")
	if calls != 1 || !errors.Is(nextErr, context.Canceled) {
		t.Fatal("pre-canceled control reached acquisition")
	}
	requireSshAdmissionPhaseVisibility(t, nextErr, observationErrorClassCanceled, "")
}
