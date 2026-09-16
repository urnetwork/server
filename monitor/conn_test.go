package monitor

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

func syntheticProcessExit(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", code)).Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
		t.Fatalf("synthetic process did not supply native exit status %d", code)
	}
	return err
}

func TestSSHNativeExitStatusPreservesVisibilityAndPrivacy(t *testing.T) {
	exit255 := syntheticProcessExit(t, 255)
	exit1 := syntheticProcessExit(t, 1)
	const privateText = "private-token address=192.0.2.91 task=aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	for _, testCase := range []struct {
		name   string
		err    error
		stderr string
		want   string
	}{
		{name: "native-ssh-255", err: exit255, stderr: "ssh: connect to host 192.0.2.91 port 22: No route to host", want: observationErrorClassSSHExit255},
		{name: "native-255-without-reason-is-not-localized", err: exit255, want: observationErrorClassSSHExit255},
		{name: "wrapped-native-255", err: fmt.Errorf("wrapped: %w", exit255), want: observationErrorClassSSHExit255},
		{name: "remote-command-1", err: exit1, want: observationErrorClassCommandFailed},
		{name: "remote-command-1-with-255-looking-text", err: exit1, stderr: "exit status 255", want: observationErrorClassCommandFailed},
		{name: "status-looking-error-is-not-native", err: errors.New("exit status 255"), want: observationErrorClassCommandFailed},
		{name: "authentication-keeps-specific-class", err: exit255, stderr: "Permission denied (publickey)", want: observationErrorClassAccessDenied},
		{name: "timeout-keeps-specific-class", err: exit255, stderr: "connection timeout", want: observationErrorClassTimeout},
		{name: "successful-command-with-hostile-stderr", stderr: "exit status 255"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := &monitorConfig{
				addressMode:       addressModeOverlay,
				sshConnectTimeout: time.Second, commandTimeout: time.Second,
			}
			runner := newRunner(cfg)
			runner.runSSH = func(context.Context, []string, string) (string, string, error) {
				return "synthetic observation", testCase.stderr + " " + privateText, testCase.err
			}
			output, err := runner.shell(context.Background(), &host{name: "edge", overlayIp: "192.0.2.91"}, "true")
			if output != "synthetic observation" {
				t.Fatal("SSH error wrapping discarded the existing stdout contract")
			}
			if testCase.want == "" {
				if err != nil {
					t.Fatal("successful command stderr manufactured an SSH failure")
				}
				return
			}
			if got := classifyObservationError(err); got != testCase.want {
				t.Fatalf("error_class=%s, want %s", got, testCase.want)
			}
			if !errors.Is(err, testCase.err) {
				t.Fatal("typed SSH wrapper lost the original execution error")
			}
			settings := syntheticSettings(&syntheticSource{})
			signal := NewMigrationsSignal()
			wholeSignal := visibilityAlert(settings, signal, err)
			perTarget := alertFromFinding(settings, signal.Number(), signal.Key(), signal.Name(), cannotObserveFinding("edge/observation", err))
			for _, alert := range []Alert{wholeSignal, perTarget} {
				if alert.Class != "cannot-observe" || alert.Severity != SeverityWarn ||
					alert.Sustain != 2 || alert.Observed != "error_class="+testCase.want {
					t.Fatal("SSH taxonomy changed or suppressed the existing visibility alert")
				}
				requireAlertOmits(t, alert, privateText, "192.0.2.91", "aaaaaaaa-aaaa", "No route to host", "exit status 255")
				if testCase.want == observationErrorClassSSHExit255 {
					for _, discriminator := range []string{"remote command", "independent targets", "observer route", "intended VPN-session evidence before attributing"} {
						if !strings.Contains(alert.Action, discriminator) {
							t.Fatalf("SSH exit 255 action lost the %q discriminator", discriminator)
						}
					}
				}
			}
			generic := visibilityAlert(settings, signal, errors.New("exit status 1"))
			if wholeSignal.Identity() != generic.Identity() || perTarget.Target != "edge/observation" {
				t.Fatal("SSH taxonomy changed per-signal or per-target visibility identity")
			}
			if testCase.want == observationErrorClassSSHExit255 &&
				(strings.Contains(wholeSignal.Mechanism, "VPN") || strings.Contains(wholeSignal.Mechanism, "local route")) {
				t.Fatal("SSH exit 255 alone attributed a workstation transport cause")
			}
		})
	}
}

func TestSSHParentCancellationOverridesChildExitStatus(t *testing.T) {
	exit255 := syntheticProcessExit(t, 255)
	for _, childErr := range []error{exit255, nil} {
		ctx, cancel := context.WithCancel(context.Background())
		runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Minute})
		runner.runSSH = func(context.Context, []string, string) (string, string, error) {
			cancel()
			return "partial observation", "private child stderr", childErr
		}
		output, err := runner.shell(ctx, &host{name: "edge", overlayIp: "192.0.2.91"}, "true")
		cancel()
		if !errors.Is(err, context.Canceled) || classifyObservationError(err) != observationErrorClassCanceled {
			t.Fatal("authoritative cancellation became an SSH or command failure")
		}
		if output != "partial observation" {
			t.Fatal("cancellation changed partial stdout ownership")
		}
		if len(runner.remoteCommands.hostSlots("192.0.2.91")) != 0 {
			t.Fatal("canceled SSH command retained its admission slot")
		}
	}
}

func TestSSHPreCanceledContextDoesNotInvokeChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay})
	runner.runSSH = func(context.Context, []string, string) (string, string, error) {
		t.Fatal("pre-canceled SSH command invoked its child")
		return "", "", nil
	}
	_, err := runner.shell(ctx, &host{name: "edge"}, "true")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("pre-canceled context was replaced by missing-address evidence")
	}
}

func TestSSHChildDeadlineRemainsTimeoutWithLiveParent(t *testing.T) {
	exit255 := syntheticProcessExit(t, 255)
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Nanosecond})
	runner.runSSH = func(ctx context.Context, _ []string, _ string) (string, string, error) {
		<-ctx.Done()
		return "", "private child stderr", exit255
	}
	_, err := runner.shell(context.Background(), &host{name: "edge", overlayIp: "192.0.2.91"}, "true")
	var unreachable *unreachableError
	if !errors.As(err, &unreachable) || classifyObservationError(err) != observationErrorClassTimeout {
		t.Fatal("per-command deadline lost the existing timeout semantics")
	}
}

// PostgreSQL text may contain every former line and pipe delimiter. The psql
// CSV contract must retain it in one cell instead of manufacturing rows.
func TestPostgreSQLRowsPreserveEmbeddedRecordAndFieldDelimiters(t *testing.T) {
	cfg := &monitorConfig{
		addressMode: addressModeOverlay,
		hosts: []*host{{
			name: "pg-1", overlayIp: "192.0.2.10", roles: []string{"pg-primary"},
		}},
		pgPort: 5432, pgUser: "monitor", pgDb: "synthetic",
		sshConnectTimeout: time.Second, commandTimeout: time.Second,
	}
	runner := newRunner(cfg)
	var remoteCmd string
	runner.runSSH = func(_ context.Context, args []string, _ string) (string, string, error) {
		remoteCmd = args[len(args)-1]
		return "\"alpha\nbeta\",left|right,\"quote\"\"cell\"\nsecond,,value\n", "", nil
	}

	rows, err := runner.pg(context.Background(), "SELECT synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(remoteCmd, " --csv -t ") || strings.Contains(remoteCmd, "-F'|'") {
		t.Fatalf("PostgreSQL command does not require structural CSV output: %s", remoteCmd)
	}
	if len(rows) != 2 {
		t.Fatalf("parsed rows = %d, want 2: %#v", len(rows), rows)
	}
	if got := rows[0].str(0); got != "alpha\nbeta" {
		t.Fatalf("embedded newline cell = %q", got)
	}
	if got := rows[0].str(1); got != "left|right" {
		t.Fatalf("embedded pipe cell = %q", got)
	}
	if got := rows[0].str(2); got != "quote\"cell" {
		t.Fatalf("embedded quote cell = %q", got)
	}
}

// Malformed or truncated output is unknown state, never a partial database
// observation that downstream probes may attribute to a real object.
func TestPostgreSQLRowsFailClosedOnMalformedOrTruncatedOutput(t *testing.T) {
	for _, output := range []string{
		"first,second\nthird\n",
		"\"unfinished,record\n",
		"complete,record\ntruncated,record",
	} {
		if _, err := parsePgRows(output); err == nil {
			t.Errorf("malformed PostgreSQL output parsed successfully: %q", output)
		}
	}
}

// The production incident put a newline, pipe, and durable identifier in one
// task error. Legacy delimiter splitting turned its continuation into a second
// task family and moved the identifier into the alert frame. The structured
// row must preserve the family while the renderer emits only a fixed class.
func TestPostgreSQLFramingDoesNotCreateTaskIdentifierFrame(t *testing.T) {
	const taskId = "01a07b25-2590-abcd-1234-56789abcdef0"
	cfg := &monitorConfig{
		env: "synthetic", addressMode: addressModeOverlay,
		hosts: []*host{{
			name: "pg-1", overlayIp: "192.0.2.10", roles: []string{"pg-primary"},
		}},
		pgPort: 5432, pgUser: "monitor", pgDb: "synthetic",
		sshConnectTimeout: time.Second, commandTimeout: time.Second,
	}
	runner := newRunner(cfg)
	runner.runSSH = func(_ context.Context, args []string, stdin string) (string, string, error) {
		remoteCmd := args[len(args)-1]
		switch {
		case strings.Contains(stdin, "UpdateClientLocations"):
			return "12\n", "", nil
		case strings.Contains(stdin, "WITH history AS"):
			return "", "", nil
		case strings.Contains(stdin, "WITH failures AS"):
			if strings.Contains(remoteCmd, " --csv -t ") {
				return "CloseExpiredContracts,1,0,1,7,-5,\"Timeout\nforce close contract " + taskId + "|private\",1800,1,deadline-timeout=1,18.4,200,8MB\n", "", nil
			}
			return "CloseExpiredContracts|1|0|1|7|-5|Timeout\nforce close contract " + taskId + "|private|1800|1|deadline-timeout=1|18.4|200|8MB\n", "", nil
		default:
			t.Fatalf("unexpected PostgreSQL query: %s", stdin)
			return "", "", nil
		}
	}

	findings, err := (taskCanaryProbe{}).check(context.Background(), &probeEnv{
		cfg: cfg, runner: runner, now: func() time.Time { return time.Unix(0, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	taskFindings := []finding{}
	for _, result := range findings {
		if result.class == "task-parked" && !result.healthy {
			taskFindings = append(taskFindings, result)
		}
	}
	if len(taskFindings) != 1 {
		t.Fatalf("task alert rows = %d, want 1: %#v", len(taskFindings), taskFindings)
	}
	if taskFindings[0].frame != "CloseExpiredContracts" {
		t.Fatalf("task alert frame = %q", taskFindings[0].frame)
	}
	alert := alertFromFinding(SignalSettings{
		Environment: "synthetic", Now: func() time.Time { return time.Unix(0, 0) },
	}, "1.2", "task-canaries", "Task canaries", taskFindings[0])
	requireAlertOmits(t, alert, taskId)
	if !strings.Contains(alert.Markdown(), "representative_error_class=deadline-timeout") {
		t.Fatalf("task error was not reduced to its fixed class: %s", alert.Markdown())
	}
	requireAlertOmits(t, alert, "force close contract", "|private")
}

// A top-level signal limit is insufficient because individual probes fan out
// internally. Every fresh probe environment must share the transport-level
// budget or their combined SSH handshakes can cross sshd MaxStartups.
func TestSSHCommandsSharePerHostLimitAcrossProbeEnvironments(t *testing.T) {
	settings := SignalSettings{
		SSHUser:     "monitor",
		AddressMode: AddressModeOverlay,
		Hosts: []HostSettings{
			{Name: "db", OverlayAddress: "192.0.2.10"},
			{Name: "edge", OverlayAddress: "192.0.2.11"},
		},
	}.withDefaults().withRuntime()

	newTestRunner := func() (*runner, *monitorConfig) {
		cfg := configFromSignalSettings(settings)
		return newRunner(cfg), cfg
	}
	runnerA, cfgA := newTestRunner()
	runnerB, cfgB := newTestRunner()
	if runnerA.remoteCommands != runnerB.remoteCommands {
		t.Fatal("fresh probe environments did not share their remote-command limiter")
	}

	started := make(chan struct{}, maxConcurrentRemoteCommandsPerHost)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseAll()
	blockingSSH := func(ctx context.Context, _ []string, _ string) (string, string, error) {
		started <- struct{}{}
		select {
		case <-release:
			return "ok", "", nil
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	runnerA.runSSH = blockingSSH
	runnerB.runSSH = blockingSSH

	var wait sync.WaitGroup
	for i := 0; i < maxConcurrentRemoteCommandsPerHost; i++ {
		selectedRunner := runnerA
		target := cfgA.hosts[0]
		if i%2 == 1 {
			selectedRunner = runnerB
			target = cfgB.hosts[0]
		}
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := selectedRunner.sshTimeout(context.Background(), target, "true", "", time.Minute); err != nil {
				t.Errorf("bounded SSH command failed: %v", err)
			}
		}()
	}
	for i := 0; i < maxConcurrentRemoteCommandsPerHost; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("timed out filling the shared per-host command budget")
		}
	}
	dbSlots := runnerA.remoteCommands.hostSlots(cfgA.hosts[0].overlayIp)
	if got := len(dbSlots); got != maxConcurrentRemoteCommandsPerHost {
		t.Fatalf("occupied db slots = %d, want %d", got, maxConcurrentRemoteCommandsPerHost)
	}

	// A fifth command for the same host waits at the transport boundary and
	// honors cancellation without invoking ssh or consuming a slot.
	blockedRunner, blockedCfg := newTestRunner()
	blockedCalled := make(chan struct{}, 1)
	blockedRunner.runSSH = func(context.Context, []string, string) (string, string, error) {
		blockedCalled <- struct{}{}
		return "", "", nil
	}
	blockedCtx, cancelBlocked := context.WithCancel(context.Background())
	blockedDone := make(chan error, 1)
	go func() {
		_, err := blockedRunner.sshTimeout(blockedCtx, blockedCfg.hosts[0], "true", "", time.Minute)
		blockedDone <- err
	}()
	cancelBlocked()
	select {
	case err := <-blockedDone:
		if !errors.Is(err, context.Canceled) || classifyObservationError(err) != observationErrorClassCanceled {
			t.Fatal("canceled command-slot wait lost authoritative cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled command-slot wait did not return")
	}
	select {
	case <-blockedCalled:
		t.Fatal("same-host command crossed a full transport budget")
	default:
	}
	if got := len(dbSlots); got != maxConcurrentRemoteCommandsPerHost {
		t.Fatalf("canceled wait changed occupied db slots to %d", got)
	}

	// Saturating one host does not consume another host's independent budget.
	otherRunner, otherCfg := newTestRunner()
	otherCalled := false
	otherRunner.runSSH = func(context.Context, []string, string) (string, string, error) {
		otherCalled = true
		return "ok", "", nil
	}
	if _, err := otherRunner.sshTimeout(context.Background(), otherCfg.hosts[1], "true", "", time.Minute); err != nil {
		t.Fatalf("other-host SSH command failed: %v", err)
	}
	if !otherCalled {
		t.Fatal("saturated db host serialized an unrelated edge host")
	}

	releaseAll()
	wait.Wait()
	if got := len(dbSlots); got != 0 {
		t.Fatalf("released db slots = %d, want 0", got)
	}
}
