package monitor

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

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
// task family and moved the identifier into the unredacted alert frame.
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
	if !strings.Contains(alert.Markdown(), "<task-id>|private") {
		t.Fatalf("task error was not retained and redacted in its source cell: %s", alert.Markdown())
	}
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
		if err == nil {
			t.Fatal("canceled command-slot wait returned no error")
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
