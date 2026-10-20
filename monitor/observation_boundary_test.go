package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func requireObservationBoundary(t *testing.T, err error, want string) {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{})
	signal := NewProviderCountSignal()
	whole := visibilityAlert(settings, signal, err)
	partial := alertFromFinding(settings, signal.Number(), signal.Key(), signal.Name(), cannotObserveFinding("synthetic-observation", err))
	for _, alert := range []Alert{whole, partial} {
		if !strings.Contains(alert.Observed, want) || alert.Class != "cannot-observe" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
			t.Fatal("owning execution boundary missing or changed alert contract")
		}
		requireAlertOmits(t, alert, "private-boundary-canary", "192.0.2.91", "synthetic-command-private", "exit status 255")
	}
	if whole.Identity() != visibilityAlert(settings, signal, errors.New("synthetic")).Identity() {
		t.Fatal("boundary changed alert identity")
	}
}

func TestObservationBoundaryGlobalQueueDeadlineIsPrecontact(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := newRunSlotPool(4)
		var release []func()
		for range 4 {
			f, err := pool.acquire(t.Context(), false)
			if err != nil {
				t.Fatal(err)
			}
			release = append(release, f)
		}
		defer func() {
			for _, f := range release {
				f()
			}
		}()
		signal := &pgRunSlotOrdinaryControl{Signal: NewProviderCountSignal(), t: t}
		_, err := runSignalInSlot(t.Context(), pool, signal, syntheticSettings(nil))
		if signal.called || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("queue fixture contacted source")
		}
		requireObservationBoundary(t, err, "execution_boundaries=global-admission/not-attempted")
		if pool.active != 4 || len(pool.waiting) != 0 {
			t.Fatal("queue deadline released another owner")
		}
	})
}

func TestObservationBoundaryHostQueueDeadlineIsPrecontact(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := newRunner(&monitorConfig{addressMode: addressModeOverlay})
		runner.remoteCommands = newHostCommandLimiter(1)
		release, err := runner.remoteCommands.acquire(t.Context(), "192.0.2.91")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		calls := 0
		runner.runSSH = func(context.Context, []string, string) (string, string, error) { calls++; return "", "", nil }
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
		defer cancel()
		_, err = runner.sshTimeout(ctx, &host{name: "synthetic", overlayIp: "192.0.2.91"}, "synthetic-command-private", "", time.Second)
		if calls != 0 || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("host fixture reached native runner")
		}
		requireObservationBoundary(t, err, "execution_boundaries=host-admission/not-attempted")
	})
}

func TestObservationBoundarySharedAcquireRetainsExistingProof(t *testing.T) {
	calls := 0
	cause := fmt.Errorf("private-boundary-canary: %w", context.DeadlineExceeded)
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: &sshAdmissionPhaseFixture{
		acquireCommand: func(context.Context, string) (func() error, error) { return nil, cause },
	}})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) { calls++; return "", "", nil }
	_, err := runner.shell(t.Context(), &host{name: "synthetic", overlayIp: "192.0.2.91"}, "synthetic-command-private")
	if calls != 0 || !errors.Is(err, cause) {
		t.Fatal("shared acquisition lost precontact cause")
	}
	requireObservationBoundary(t, err, "ssh_admission_failures=acquire")
	if strings.Contains(observationBoundaryDetails(err), "native-") {
		t.Fatal("acquisition invented native execution")
	}
}

func TestObservationBoundaryNativeStartFailureHasNoChild(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay})
	_, err := runner.shell(t.Context(), &host{name: "synthetic", overlayIp: "192.0.2.91"}, "synthetic-command-private")
	if err == nil {
		t.Fatal("empty executable path did not refuse Start")
	}
	requireObservationBoundary(t, err, "execution_boundaries=native-start/failed-no-child")
}

func TestObservationBoundaryNativeWait255DoesNotAssertRemoteContact(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexit 255\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay})
	_, err := runner.shell(t.Context(), &host{name: "synthetic", overlayIp: "192.0.2.91"}, "synthetic-command-private")
	if classifyObservationError(err) != observationErrorClassSSHExit255 {
		t.Fatal("native255 class was lost")
	}
	requireObservationBoundary(t, err, "execution_boundaries=native-wait/started-joined")
	if strings.Contains(observationBoundaryDetails(err), "remote") {
		t.Fatal("native Wait asserted remote contact")
	}
}

// The barrier controls cancellation after real Start. Err exposes the modeled
// deadline, without a timing sleep or a short real-world timeout as proof.
type observationBoundaryDeadlineContext struct{ context.Context }

// Prevent context's cancelCtx fast path from bypassing the modeled Err.
func (self observationBoundaryDeadlineContext) Value(any) any { return nil }
func (self observationBoundaryDeadlineContext) Err() error {
	if self.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

func TestObservationBoundaryNativeTimeoutPreservesJoinedProof(t *testing.T) {
	dir := t.TempDir()
	readyPath, blockPath := filepath.Join(dir, "ready"), filepath.Join(dir, "block")
	if err := syscall.Mkfifo(readyPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(blockPath, 0600); err != nil {
		t.Fatal(err)
	}
	ready, err := os.OpenFile(readyPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	script := "#!/bin/sh\nprintf x > \"$MONITOR_TEST_BOUNDARY_READY\"\nread ignored < \"$MONITOR_TEST_BOUNDARY_BLOCK\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("MONITOR_TEST_BOUNDARY_READY", readyPath)
	t.Setenv("MONITOR_TEST_BOUNDARY_BLOCK", blockPath)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Hour})
	done := make(chan error, 1)
	go func() {
		_, err := runner.shell(observationBoundaryDeadlineContext{Context: ctx}, &host{name: "synthetic", overlayIp: "192.0.2.91"}, "synthetic-command-private")
		done <- err
	}()
	childJoined := false
	defer func() {
		cancel()
		if !childJoined {
			<-done
		}
	}()
	readDone := make(chan error, 1)
	readJoined := false
	go func() {
		var marker [1]byte
		n, err := ready.Read(marker[:])
		if err == nil && (n != 1 || marker[0] != 'x') {
			err = errors.New("invalid synthetic barrier")
		}
		readDone <- err
	}()
	defer func() {
		ready.Close()
		if !readJoined {
			<-readDone
		}
	}()
	select {
	case err := <-readDone:
		readJoined = true
		if err != nil {
			t.Fatal("native process did not reach its barrier")
		}
	case <-done:
		childJoined = true
		t.Fatal("native process terminated before the barrier")
	}
	cancel()
	err = <-done
	childJoined = true
	if classifyObservationError(err) != observationErrorClassTimeout {
		t.Fatal("deadline class changed")
	}
	requireObservationBoundary(t, err, "execution_boundaries=native-wait/started-joined")
}

func TestObservationBoundaryJoinedBranchesRemainIndependentAndPrivate(t *testing.T) {
	err := errors.Join(
		&runSlotAdmissionError{cause: context.DeadlineExceeded},
		&observationBoundaryError{err: errors.New("private-boundary-canary 192.0.2.91 synthetic-command-private"), boundary: observationBoundaryHost},
		&observationBoundaryError{err: errors.New("private-boundary-canary"), boundary: observationBoundaryNativeWait},
		&sshAdmissionUnavailableError{err: context.DeadlineExceeded, phase: sshAdmissionFailureRelease},
	)
	requireObservationBoundary(t, err, "execution_boundaries=global-admission/not-attempted,host-admission/not-attempted,native-wait/started-joined")
	requireObservationBoundary(t, err, "ssh_admission_failures=release")
}

func TestObservationBoundaryUnknownCallbacksAndMalformedProofStayUnknown(t *testing.T) {
	if got := observationBoundaryDetails(errors.New("native-wait/started-joined private-boundary-canary")); got != "" {
		t.Fatal("text manufactured a typed boundary")
	}
	err := &observationBoundaryError{err: errors.New("private-boundary-canary"), boundary: observationBoundary(255)}
	if got := observationBoundaryDetails(err); got != " execution_boundaries=unknown/unknown" {
		t.Fatal("malformed marker became native proof")
	}
	for range 65 {
		err = &observationBoundaryError{err: err, boundary: observationBoundaryHost}
	}
	got := observationBoundaryDetails(err)
	if !strings.Contains(got, "unknown/unknown") || len(got) > 512 {
		t.Fatal("bounded traversal did not retain unknown")
	}
}

func TestObservationBoundarySuccessfulCommandKeepsNilError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay})
	_, err := runner.shell(t.Context(), &host{name: "synthetic", overlayIp: "192.0.2.91"}, "synthetic-command-private")
	if err != nil || observationBoundaryDetails(err) != "" {
		t.Fatal("successful command became failed observation")
	}
}

func TestObservationBoundaryNativeCanceledParentPreservesJoinedProof(t *testing.T) {
	dir := t.TempDir()
	readyPath, blockPath := filepath.Join(dir, "ready"), filepath.Join(dir, "block")
	if err := syscall.Mkfifo(readyPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(blockPath, 0600); err != nil {
		t.Fatal(err)
	}
	ready, err := os.OpenFile(readyPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	script := "#!/bin/sh\nprintf x > \"$MONITOR_TEST_BOUNDARY_READY\"\nread ignored < \"$MONITOR_TEST_BOUNDARY_BLOCK\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("MONITOR_TEST_BOUNDARY_READY", readyPath)
	t.Setenv("MONITOR_TEST_BOUNDARY_BLOCK", blockPath)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Hour})
	done := make(chan error, 1)
	go func() {
		_, err := runner.shell(ctx, &host{name: "synthetic", overlayIp: "192.0.2.91"}, "synthetic-command-private")
		done <- err
	}()
	childJoined := false
	defer func() {
		cancel()
		if !childJoined {
			<-done
		}
	}()
	readDone := make(chan error, 1)
	readJoined := false
	go func() {
		var marker [1]byte
		n, err := ready.Read(marker[:])
		if err == nil && (n != 1 || marker[0] != 'x') {
			err = errors.New("invalid synthetic barrier")
		}
		readDone <- err
	}()
	defer func() {
		ready.Close()
		if !readJoined {
			<-readDone
		}
	}()
	select {
	case err := <-readDone:
		readJoined = true
		if err != nil {
			t.Fatal("native process did not reach its barrier")
		}
	case <-done:
		childJoined = true
		t.Fatal("native process terminated before the barrier")
	}
	cancel()
	err = <-done
	childJoined = true
	if classifyObservationError(err) != observationErrorClassCanceled {
		t.Fatal("parent cancellation class changed")
	}
	requireObservationBoundary(t, err, "execution_boundaries=native-wait/started-joined")
}

func TestObservationBoundaryCustomCallbackCanceledParentStaysUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	acquires, releases, calls := 0, 0, 0
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, sharedSshAdmission: &sshAdmissionPhaseFixture{
		acquireCommand: func(context.Context, string) (func() error, error) {
			acquires++
			return func() error { releases++; return nil }, nil
		},
	}})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		calls++
		cancel()
		return "", "", fmt.Errorf("private-boundary-canary native-wait/started-joined: %w", context.Canceled)
	}
	_, err := runner.shell(ctx, &host{name: "synthetic", overlayIp: "192.0.2.91"}, "synthetic-command-private")
	if calls != 1 || acquires != 1 || releases != 1 || !errors.Is(err, context.Canceled) || classifyObservationError(err) != observationErrorClassCanceled {
		t.Fatal("callback cancellation lost its class or exact cleanup")
	}
	if observationBoundaryDetails(err) != "" {
		t.Fatal("custom callback supplied invented native proof")
	}
	requireObservationBoundary(t, err, "error_class="+observationErrorClassCanceled)
}

type observationBoundaryCountedError struct {
	calls *int
	child error
}

func (self *observationBoundaryCountedError) Error() string {
	return "synthetic private-boundary-canary"
}
func (self *observationBoundaryCountedError) Unwrap() error {
	(*self.calls)++
	return self.child
}

func TestObservationBoundaryBroadJoinRetainsUnknownAtInspectionCap(t *testing.T) {
	calls := 0
	children := make([]error, 10000)
	for i := range children {
		children[i] = &observationBoundaryCountedError{calls: &calls}
	}
	children[len(children)-1] = &observationBoundaryError{err: errors.New("private-boundary-canary"), boundary: observationBoundaryNativeWait}
	got := observationBoundaryDetails(errors.Join(children...))
	if got != " execution_boundaries=unknown/unknown" || calls > 63 {
		t.Fatal("broad join exceeded bounded proof or attributed an omitted branch")
	}
}

func TestObservationBoundaryCycleRetainsUnknownWithoutUnboundedWalk(t *testing.T) {
	calls := 0
	err := &observationBoundaryCountedError{calls: &calls}
	err.child = err
	got := observationBoundaryDetails(err)
	if got != " execution_boundaries=unknown/unknown" || calls > 64 {
		t.Fatal("cyclic error projection did not stop at its fixed bound")
	}
}

func TestObservationBoundaryDeepTreeDoesNotAttributeOmittedProof(t *testing.T) {
	calls := 0
	var err error = &observationBoundaryError{err: errors.New("private-boundary-canary"), boundary: observationBoundaryNativeWait}
	for range 10000 {
		err = &observationBoundaryCountedError{calls: &calls, child: err}
	}
	got := observationBoundaryDetails(err)
	if got != " execution_boundaries=unknown/unknown" || calls > 64 {
		t.Fatal("deep error projection traversed or attributed omitted proof")
	}
}
