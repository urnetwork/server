// Forced-clock barriers exercise the production native Start seam. Every
// executable is a private local fixture; no test invokes network SSH or SQL.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A single attempt owns this clock. Barriers publish each time change before
// the next production stage; no package-global clock or wall-clock sleep is used.
type pgStartGuardControlClock struct {
	stateLock sync.Mutex
	now       time.Time
}

func (self *pgStartGuardControlClock) read() time.Time {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.now
}

func (self *pgStartGuardControlClock) set(now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = now
}

// Nanosecond comparison refuses only a strict shortfall; equality, missing
// deadline, and non-PG transports do not acquire an invented reserve.
func TestPgSampleStartGuardExactLowerBoundControls(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
	defer cancel()
	deadline, _ := ctx.Deadline()
	for _, c := range []struct {
		name      string
		remaining time.Duration
		refuse    bool
	}{
		{name: "sixth-observation", remaining: 17518 * time.Millisecond, refuse: true},
		{name: "one-nanosecond-below", remaining: 22*time.Second - time.Nanosecond, refuse: true},
		{name: "exact-floor", remaining: 22 * time.Second},
		{name: "one-nanosecond-above", remaining: 22*time.Second + time.Nanosecond},
		{name: "fourth-observation", remaining: 23354 * time.Millisecond},
	} {
		observation := &pgSampleTransportObservation{
			startClock: func() time.Time { return deadline.Add(-c.remaining) },
		}
		err := observation.admitNativeStart(ctx)
		if errors.Is(err, errPgSampleStartBudget) != c.refuse {
			t.Fatalf("%s: native-start decision changed the strict mandatory-wait boundary", c.name)
		}
		proof := observation.receipt
		if c.refuse {
			if proof.StartRefusalRemainingBudgetNs == nil || *proof.StartRefusalRemainingBudgetNs != c.remaining.Nanoseconds() || proof.Phase != "ssh_start" || proof.Spawn != "not_attempted" {
				t.Fatalf("%s: local refusal lost its exact pre-Start remaining time", c.name)
			}
		} else if err != nil || proof.StartRefusalRemainingBudgetNs != nil {
			t.Fatalf("%s: admissible minimum acquired an arbitrary reserve or refusal fact", c.name)
		}
		if proof.SpawnRemainingBudgetMs != nil || proof.Joined || proof.NativeExitCode != nil || proof.OwnerDeadlineExceeded {
			t.Fatalf("%s: a pre-Start decision invented native execution or owner expiration", c.name)
		}
	}
	if err := (*pgSampleTransportObservation)(nil).admitNativeStart(ctx); err != nil {
		t.Fatal("ordinary non-PG transport acquired a sampling floor")
	}
	observation := &pgSampleTransportObservation{}
	if err := observation.admitNativeStart(context.Background()); err != nil || observation.receipt.StartRefusalRemainingBudgetNs != nil {
		t.Fatal("unknown deadline became a proved shortfall")
	}
	canceled, cancelNow := context.WithCancel(ctx)
	cancelNow()
	if err := observation.admitNativeStart(canceled); !errors.Is(err, context.Canceled) || errors.Is(err, errPgSampleStartBudget) {
		t.Fatal("preexisting cancellation was relabeled as a sampling-window diagnosis")
	}
	if pgSampleMinimumWaitBudget != 22*time.Second || pgQuerySampleBudget != 40*time.Second || pgQuerySampleCount != 12 || strings.Count(pgQuerySampleSQL("fixture"), "SELECT pg_sleep(2);") != 11 {
		t.Fatal("guard changed the fixed SQL work or queue-inclusive owner")
	}
}

// The actual exec.Cmd can only run this local test helper. A marker proves that
// native Start crossed the boundary; synthetic output proves the healthy path.
func TestPgSampleStartGuardNativeHelper(t *testing.T) {
	if os.Getenv("MONITOR_PG_START_GUARD_CHILD") != "1" {
		return
	}
	if err := os.WriteFile(os.Getenv("MONITOR_PG_START_GUARD_MARKER"), []byte("started\n"), 0600); err != nil {
		os.Exit(91)
	}
	var input struct {
		Nonce string `json:"transport_nonce"`
	}
	if json.NewDecoder(io.LimitReader(os.Stdin, 262145)).Decode(&input) != nil || len(input.Nonce) != 32 {
		os.Exit(92)
	}
	for _, phase := range []string{"remote_started", "database_identity"} {
		raw, _ := json.Marshal(map[string]any{"schema": 1, "nonce": input.Nonce, "phase": phase})
		fmt.Fprintln(os.Stderr, "monitor-pg-phase "+string(raw))
	}
	io.WriteString(os.Stdout, pgSampleTestEncode(t, pgSampleTestFrames(time.Now().UTC().Truncate(time.Second))))
	os.Exit(0)
}

// Select a private executable, assert PATH resolution, and retain no production
// credentials, resource roots or endpoints in the child fixture.
func pgStartGuardNativeFixture(t *testing.T) string {
	t.Helper()
	fixture := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := "#!/bin/sh\nexec " + shellSingleQuote(executable) + " -test.run '^TestPgSampleStartGuardNativeHelper$' -test.count=1\n"
	if err := os.WriteFile(filepath.Join(fixture, "ssh"), []byte(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(fixture, "native-started")
	t.Setenv("PATH", fixture+string(os.PathListSeparator)+os.Getenv("PATH"))
	resources := t.TempDir()
	t.Setenv("WARP_HOME", resources)
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_HOST", "pg-start-guard.example")
	for _, kind := range []string{"config", "site", "vault"} {
		directory := filepath.Join(resources, kind)
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("WARP_"+strings.ToUpper(kind)+"_HOME", directory)
	}
	t.Setenv("MONITOR_PG_START_GUARD_CHILD", "1")
	t.Setenv("MONITOR_PG_START_GUARD_MARKER", marker)
	if resolved, err := exec.LookPath("ssh"); err != nil || resolved != filepath.Join(fixture, "ssh") {
		t.Fatal("fixture did not exclusively select its private native child")
	}
	return marker
}

// Admission is already granted with enough time. A barrier then consumes time
// inside the native-runner entry. An earlier runner/admission-only check cannot
// catch this; only the final pre-Start guard prevents the marker child.
func TestPgSampleStartGuardRefusesAtNativeSeamAfterAdmission(t *testing.T) {
	marker := pgStartGuardNativeFixture(t)
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		t.Error("unexpected injected source callback")
		return "", "", errors.New("fixture callback")
	})
	env.cfg.pgQuerySampleUntil = time.Time{}
	env.cfg.pgQuerySampleContinuous = true
	runner := env.runner.(*runner)
	host := env.cfg.hostByRole("pg-primary")
	address := host.addr(env.cfg.addressMode)
	otherRelease, err := runner.remoteCommands.acquire(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	defer otherRelease()
	clock := &pgStartGuardControlClock{}
	entered := make(chan time.Time, 1)
	allowNativeEntry := make(chan struct{})
	var grants, releases, nativeEntries atomic.Int32
	env.cfg.sharedSshAdmission = &sshAdmissionPhaseFixture{acquireCommand: func(ctx context.Context, _ string) (func() error, error) {
		deadline, known := ctx.Deadline()
		if !known {
			return nil, errors.New("fixture owner missing")
		}
		clock.set(deadline.Add(-30 * time.Second))
		pgSampleTransportFromContext(ctx).startClock = clock.read
		grants.Add(1)
		return func() error { releases.Add(1); return nil }, nil
	}}
	runner.runSSH = func(ctx context.Context, args []string, stdin string) (string, string, error) {
		nativeEntries.Add(1)
		deadline, _ := ctx.Deadline()
		entered <- deadline
		select {
		case <-allowNativeEntry:
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
		return runSSHCommand(ctx, args, stdin)
	}
	owner, cancel := context.WithTimeout(t.Context(), pgQuerySampleBudget)
	defer cancel()
	// Preserve q7 dispatcher observations through transport construction and
	// the local refusal. These in-process stamps do not claim a real pool wait.
	owner = observePgSampleRunSlot(owner)
	dispatch := pgSampleTransportFromContext(owner)
	dispatch.runSlotRequested()
	dispatch.runSlotGranted(owner)
	dispatchProof := dispatch.receipt
	if dispatchProof.RunSlotWaitMs == nil || dispatchProof.RunSlotGrantedRemainingBudgetMs == nil {
		t.Fatal("fixture did not establish its dispatcher timing control")
	}
	type result struct {
		findings []finding
		err      error
	}
	done := make(chan result, 1)
	go func() {
		findings, err := (pgQuerySampleProbe{}).check(owner, env)
		done <- result{findings: findings, err: err}
	}()
	select {
	case deadline := <-entered:
		if grants.Load() != 1 || releases.Load() != 0 || len(runner.remoteCommands.hostSlots(address)) != maxConcurrentRemoteCommandsPerHost {
			t.Fatal("fixture did not reach the seam with both of its admission grants")
		}
		clock.set(deadline.Add(-17518 * time.Millisecond))
		close(allowNativeEntry)
	case <-time.After(10 * time.Second):
		t.Fatal("fixture never reached the native-entry barrier")
	}
	var terminal result
	select {
	case terminal = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pre-Start refusal did not return through admission cleanup")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("strictly insufficient original budget still started the native child")
	}
	if terminal.err != nil || len(terminal.findings) != 1 || terminal.findings[0].class != "pg-query-sample-unavailable" || terminal.findings[0].tier != tierWarn || terminal.findings[0].sustain != 1 || !strings.Contains(terminal.findings[0].observed, "reason=source-unavailable phase=local_start_budget cause=below_mandatory_waits") {
		t.Fatal("local refusal changed unknown visibility or invented a remote failure")
	}
	receipt, raw := pgTransportReadControlReceipt(t, env)
	proof := receipt.TransportObservation
	if receipt.Complete || receipt.Samples != 0 || proof.Phase != "ssh_start" || proof.Spawn != "not_attempted" || proof.RemotePhase != "unknown" || proof.SpawnRemainingBudgetMs != nil || proof.Joined || proof.NativeExitCode != nil || proof.OwnerDeadlineExceeded || proof.ProgressStatus != "unobserved" {
		t.Fatal("unstarted local refusal invented source, Start/Wait, or expired-owner facts")
	}
	var timing pgRunSlotTimingReceipt
	if json.Unmarshal(raw, &timing) != nil || timing.TransportObservation == nil {
		t.Fatal("local refusal lost its independently decoded timing receipt")
	}
	timingProof := timing.TransportObservation
	if timingProof.RunSlotWaitMs == nil || *timingProof.RunSlotWaitMs != *dispatchProof.RunSlotWaitMs || timingProof.RunSlotGrantedRemainingBudgetMs == nil || *timingProof.RunSlotGrantedRemainingBudgetMs != *dispatchProof.RunSlotGrantedRemainingBudgetMs {
		t.Fatal("local refusal erased or rewrote the inherited dispatcher timing")
	}
	var local struct {
		Transport struct {
			Remaining *int64 `json:"start_refusal_remaining_budget_ns"`
		} `json:"transport_observation"`
	}
	if json.Unmarshal(raw, &local) != nil || local.Transport.Remaining == nil || *local.Transport.Remaining != (17518*time.Millisecond).Nanoseconds() {
		t.Fatal("receipt lost the exact guard decision budget")
	}
	if releases.Load() != 1 || len(runner.remoteCommands.hostSlots(address)) != 1 || nativeEntries.Load() != 1 || owner.Err() != nil {
		t.Fatal("refusal leaked a grant, released another owner, renewed work, or waited for expiration")
	}
	state, err := pgSampleReadCadence(filepath.Join(env.cfg.stateDir, "pg-query-sample"), env.now())
	if err != nil || !state.NextEligibleAt.Equal(env.now().Add(pgQuerySampleCadence)) {
		t.Fatal("pre-Start refusal changed the spent-attempt terminal cadence")
	}
	findings, err := (pgQuerySampleProbe{}).check(t.Context(), env)
	if err != nil || len(findings) != 1 || !strings.Contains(findings[0].observed, "cadence-not-due") || grants.Load() != 1 || releases.Load() != 1 || nativeEntries.Load() != 1 {
		t.Fatal("pre-Start refusal caused a retry or changed admission counters")
	}
	_, _ = pgTransportReadControlReceipt(t, env)
}

// A feasible unmodified owner still reaches actual native Start/Wait and a
// complete twelve-snapshot projection. The new fields remain absent on success.
func TestPgSampleStartGuardHealthyNativeSamplePreserved(t *testing.T) {
	marker := pgStartGuardNativeFixture(t)
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		t.Fatal("healthy fixture bypassed native transport")
		return "", "", nil
	})
	env.runner.(*runner).runSSH = runSSHCommand
	if _, err := (pgQuerySampleProbe{}).check(t.Context(), env); err != nil {
		t.Fatal(err)
	}
	if value, err := os.ReadFile(marker); err != nil || string(value) != "started\n" {
		t.Fatal("healthy source did not reach its owned native child")
	}
	receipt, raw := pgTransportReadControlReceipt(t, env)
	proof := receipt.TransportObservation
	if !receipt.Complete || receipt.Samples != 12 || receipt.SourceFailure != nil || proof.Spawn != "started" || !proof.Joined || proof.NativeExitCode == nil || *proof.NativeExitCode != 0 || proof.RemotePhase != "database_identity" || proof.OwnerDeadlineExceeded || strings.Contains(string(raw), "start_refusal_remaining_budget_ns") {
		t.Fatal("minimum-only guard changed the healthy native sample or claimed refusal")
	}
	var timing pgRunSlotTimingReceipt
	if json.Unmarshal(raw, &timing) != nil || timing.TransportObservation == nil {
		t.Fatal("healthy source lost its independently decoded timing receipt")
	}
	if timing.TransportObservation.RunSlotWaitMs != nil || timing.TransportObservation.RunSlotGrantedRemainingBudgetMs != nil {
		t.Fatal("direct healthy probe invented dispatcher timing authority")
	}
}

// Cancellation after the budget check is still owned by exec.CommandContext;
// the guard neither detaches the child nor overrides its original context.
func TestPgSampleStartGuardCancellationBeforeNativeStartHasNoChild(t *testing.T) {
	marker := pgStartGuardNativeFixture(t)
	owner, cancel := context.WithTimeout(t.Context(), pgQuerySampleBudget)
	defer cancel()
	ctx, observation := observePgSampleTransport(owner)
	deadline, _ := owner.Deadline()
	observation.startClock = func() time.Time {
		cancel()
		return deadline.Add(-pgSampleMinimumWaitBudget)
	}
	_, _, err := runSSHCommand(ctx, nil, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("native Start lost cancellation between guard and process creation")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("canceled native Start launched its child")
	}
	proof := observation.receipt
	if proof.Spawn != "start_failed" || proof.Joined || proof.SpawnRemainingBudgetMs != nil || proof.StartRefusalRemainingBudgetNs != nil {
		t.Fatal("canceled native Start invented a process or a budget refusal")
	}
}

// Joined release failures retain their higher-priority local admission cause;
// the private refusal measurement can still describe the unstarted operation.
func TestPgSampleStartGuardDoesNotHideAdmissionReleaseFailure(t *testing.T) {
	failure := pgSampleTransportFailure(errors.Join(errPgSampleStartBudget, &sshAdmissionUnavailableError{
		err: errors.New("synthetic release failure"), phase: sshAdmissionFailureRelease,
	}))
	if failure.Phase != "host_transport" || failure.Cause != observationErrorClassAdmissionUnavailable {
		t.Fatal("budget refusal hid a shared admission release failure")
	}
}
