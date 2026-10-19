// Native SSH diagnostics must retain their fixed observation class without
// turning arbitrary remote stderr into transport or service-health evidence.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// An early native connection deadline must reach both visibility reducers even
// when the monitor's outer command deadline has not expired.
func TestSshNativeConnectTimeoutReachesVisibilityReducers(t *testing.T) {
	exit255 := syntheticProcessExit(t, 255)
	for _, testCase := range []struct {
		name    string
		address string
		stderr  string
		err     error
	}{
		{name: "ipv4", address: "192.0.2.91", stderr: "ssh: connect to host 192.0.2.91 port 22: Connection timed out\r\n", err: exit255},
		{name: "ipv6-wrapped-status", address: "2001:db8::91", stderr: "ssh: connect to host 2001:db8::91 port 2222: Connection timed out", err: fmt.Errorf("wrapped: %w", exit255)},
	} {
		runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Minute})
		runner.runSSH = func(context.Context, []string, string) (string, string, error) {
			return "", testCase.stderr, testCase.err
		}
		output, err := runner.shell(context.Background(), &host{name: "synthetic-private-ssh-target", overlayIp: testCase.address}, "true")
		if output != "" || !errors.Is(err, exit255) {
			t.Fatalf("%s: SSH wrapping lost stdout or native status", testCase.name)
		}
		requireNativeSshVisibility(t, testCase.name, err, observationErrorClassTimeout, testCase.address)
	}
}

// Missing, stale-target, partial, malformed and remote-command evidence must
// remain ambiguous; a timeout-shaped phrase is not enough by itself.
func TestSshNativeConnectTimeoutKeepsAmbiguousFailures(t *testing.T) {
	exit255 := syntheticProcessExit(t, 255)
	exit1 := syntheticProcessExit(t, 1)
	const nativeStderr = "ssh: connect to host 192.0.2.91 port 22: Connection timed out"
	for _, testCase := range []struct {
		name   string
		stdout string
		stderr string
		err    error
		want   string
	}{
		{name: "bare-status", err: exit255, want: observationErrorClassSSHExit255},
		{name: "remote-message", stderr: "Connection timed out", err: exit255, want: observationErrorClassSSHExit255},
		{name: "other-target", stderr: "ssh: connect to host 192.0.2.92 port 22: Connection timed out", err: exit255, want: observationErrorClassSSHExit255},
		{name: "embedded-message", stderr: "remote task: " + nativeStderr, err: exit255, want: observationErrorClassSSHExit255},
		{name: "multiple-lines", stderr: nativeStderr + "\nremote task detail", err: exit255, want: observationErrorClassSSHExit255},
		{name: "partial-message", stderr: "ssh: connect to host 192.0.2.91 port 22: Connection timed", err: exit255, want: observationErrorClassSSHExit255},
		{name: "missing-port", stderr: "ssh: connect to host 192.0.2.91 port : Connection timed out", err: exit255, want: observationErrorClassSSHExit255},
		{name: "zero-port", stderr: "ssh: connect to host 192.0.2.91 port 0: Connection timed out", err: exit255, want: observationErrorClassSSHExit255},
		{name: "oversized-port", stderr: "ssh: connect to host 192.0.2.91 port 65536: Connection timed out", err: exit255, want: observationErrorClassSSHExit255},
		{name: "remote-output", stdout: "synthetic remote result", stderr: nativeStderr, err: exit255, want: observationErrorClassSSHExit255},
		{name: "remote-non-255", stderr: nativeStderr, err: exit1, want: observationErrorClassCommandFailed},
		{name: "status-looking-error", stderr: nativeStderr, err: errors.New("exit status 255"), want: observationErrorClassCommandFailed},
		{name: "authentication", stderr: "Permission denied (publickey).\n" + nativeStderr, err: exit255, want: observationErrorClassAccessDenied},
		{name: "host-key", stderr: "Host key verification failed.", err: exit255, want: observationErrorClassSSHExit255},
	} {
		runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Minute})
		runner.runSSH = func(context.Context, []string, string) (string, string, error) {
			return testCase.stdout, testCase.stderr, testCase.err
		}
		output, err := runner.shell(context.Background(), &host{name: "synthetic-private-ssh-target", overlayIp: "192.0.2.91"}, "true")
		if output != testCase.stdout || !errors.Is(err, testCase.err) {
			t.Fatalf("%s: SSH wrapping lost stdout or native status", testCase.name)
		}
		requireNativeSshVisibility(t, testCase.name, err, testCase.want, "192.0.2.91", "192.0.2.92", "synthetic remote result", "remote task detail")
	}
}

// Neither a local exit nor a legacy source wrapper can invent the selected
// target and stderr boundary that the SSH runner actually observed.
func TestSshNativeConnectTimeoutRequiresOwnedExecution(t *testing.T) {
	exit255 := syntheticProcessExit(t, 255)
	localErr := fmt.Errorf("%w: ssh: connect to host 192.0.2.91 port 22: Connection timed out", exit255)
	if got := classifyObservationError(localErr); got != observationErrorClassCommandFailed {
		t.Fatalf("local process stderr became SSH deadline evidence: %s", got)
	}
	if got := classifyObservationError(&sshCommandError{err: localErr}); got != observationErrorClassSSHExit255 {
		t.Fatalf("wrapped text invented an owned connection deadline: %s", got)
	}
}

// Healthy execution and authoritative parent cancellation take precedence over
// any child stderr, including a complete native-looking diagnostic.
func TestSshNativeConnectTimeoutDoesNotManufactureFailure(t *testing.T) {
	exit255 := syntheticProcessExit(t, 255)
	const nativeStderr = "ssh: connect to host 192.0.2.91 port 22: Connection timed out"
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Minute})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		return "synthetic healthy observation", nativeStderr, nil
	}
	sshHost := &host{name: "synthetic-private-ssh-target", overlayIp: "192.0.2.91"}
	output, err := runner.shell(context.Background(), sshHost, "true")
	if err != nil || output != "synthetic healthy observation" {
		t.Fatal("healthy SSH stderr manufactured an observation failure")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		cancel()
		return "", nativeStderr, exit255
	}
	_, err = runner.shell(ctx, sshHost, "true")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("child stderr replaced authoritative parent cancellation")
	}
	requireNativeSshVisibility(t, "parent-canceled", err, observationErrorClassCanceled, "192.0.2.91")
}

// Exercise the real whole-signal and per-target reductions, including fixed
// rendering, stable identity and the unchanged warning/sustain contract.
func requireNativeSshVisibility(t *testing.T, name string, err error, want string, privateText ...string) {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{})
	signal := NewMigrationsSignal()
	wholeSignal := visibilityAlert(settings, signal, err)
	perTarget := alertFromFinding(settings, signal.Number(), signal.Key(), signal.Name(), cannotObserveFinding("synthetic-edge/observation", err))
	for _, alert := range []Alert{wholeSignal, perTarget} {
		if alert.Observed != "error_class="+want {
			t.Fatalf("%s: native SSH discriminator lost in visibility reducer: got %s, want error_class=%s", name, alert.Observed, want)
		}
		if alert.Class != "cannot-observe" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
			t.Fatalf("%s: SSH classification changed visibility severity or sustain", name)
		}
		requireAlertOmits(t, alert, append(privateText, "synthetic-private-ssh-target", "connect to host", "Connection timed out", "exit status 255")...)
	}
	genericErr := errors.New("exit status 1")
	genericWhole := visibilityAlert(settings, signal, genericErr)
	genericTarget := alertFromFinding(settings, signal.Number(), signal.Key(), signal.Name(), cannotObserveFinding("synthetic-edge/observation", genericErr))
	if wholeSignal.Identity() != genericWhole.Identity() || perTarget.Identity() != genericTarget.Identity() {
		t.Fatalf("%s: SSH classification changed visibility identity", name)
	}
}
