// Attempt-local timing controls use private state, injected clocks and existing
// native fixtures. None selects production destinations or changes source policy.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type pgLocalTimingFixture struct {
	env    *probeEnv
	ctx    context.Context
	cancel context.CancelFunc
	clock  *pgStartGuardControlClock
	base   time.Time
	marker string
}

// Exercise the real sampler and native guard with the existing private fake
// SSH selection. Request/grant callbacks here are synthetic; the separate real
// run-slot controls own scheduler authority. No production destination is used.
func newPgLocalTimingFixture(t *testing.T, budget time.Duration) *pgLocalTimingFixture {
	t.Helper()
	marker := pgStartGuardNativeFixture(t)
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		t.Error("unexpected source fixture callback")
		return "", "", errors.New("synthetic source callback")
	})
	env.cfg.pgQuerySampleUntil = time.Time{}
	env.cfg.pgQuerySampleContinuous = true
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	t.Cleanup(cancel)
	deadline, _ := ctx.Deadline()
	base := deadline.Add(-budget)
	clock := &pgStartGuardControlClock{}
	clock.set(base)
	ctx = observePgSampleRunSlot(ctx)
	request := pgSampleTransportFromContext(ctx)
	request.localClock = clock.read
	request.runSlotRequested()
	request.runSlotGranted(ctx)
	env.runner.(*runner).runSSH = func(ctx context.Context, args []string, stdin string) (string, string, error) {
		pgSampleTransportFromContext(ctx).startClock = clock.read
		return runSSHCommand(ctx, args, stdin)
	}
	return &pgLocalTimingFixture{env: env, ctx: ctx, cancel: cancel, clock: clock, base: base, marker: marker}
}

func readPgLocalTiming(t *testing.T, env *probeEnv) (pgTransportControlReceipt, map[string]json.RawMessage) {
	t.Helper()
	control, raw := pgTransportReadControlReceipt(t, env)
	var wire struct {
		Transport struct {
			Timing map[string]json.RawMessage `json:"local_phase_timing"`
		} `json:"transport_observation"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal("local timing receipt is not valid JSON")
	}
	if wire.Transport.Timing == nil {
		t.Fatal("receipt omitted its local timing block")
	}
	return control, wire.Transport.Timing
}

func requirePgLocalSpan(t *testing.T, timing map[string]json.RawMessage, phase string, elapsed time.Duration) {
	t.Helper()
	var span *struct {
		Entered  *int64 `json:"entered_ns"`
		Returned *int64 `json:"returned_ns"`
	}
	if json.Unmarshal(timing[phase], &span) != nil || span == nil || span.Entered == nil || span.Returned == nil {
		t.Fatalf("local timing omitted owning %s span", phase)
	}
	if *span.Entered < 0 || *span.Returned < *span.Entered || time.Duration(*span.Returned-*span.Entered) != elapsed {
		t.Fatalf("local timing misattributed %s elapsed interval", phase)
	}
}

func requirePgLocalRefusal(t *testing.T, fixture *pgLocalTimingFixture, control pgTransportControlReceipt) {
	t.Helper()
	proof := control.TransportObservation
	if control.Complete || control.Samples != 0 || control.SourceFailure == nil || control.SourceFailure.Phase != "local_start_budget" || control.SourceFailure.Cause != "below_mandatory_waits" || proof.Spawn != "not_attempted" || proof.Joined || proof.NativeExitCode != nil || proof.OwnerDeadlineExceeded {
		t.Fatal("controlled elapsed time did not preserve the original no-start refusal")
	}
	if _, err := os.Stat(fixture.marker); !os.IsNotExist(err) {
		t.Fatal("refused timing fixture started a native child")
	}
}

func runPgLocalTiming(t *testing.T, fixture *pgLocalTimingFixture, probe pgQuerySampleProbe) (pgTransportControlReceipt, map[string]json.RawMessage) {
	t.Helper()
	if _, err := probe.check(fixture.ctx, fixture.env); err != nil {
		t.Fatal("timing fixture failed before retaining a source receipt")
	}
	return readPgLocalTiming(t, fixture.env)
}

func TestPgLocalTimingSettingsDelayIsAttributed(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	deadline, _ := fixture.ctx.Deadline()
	fixture.env.cfg.routerGenerationCheck = func(ctx context.Context) (bool, error) {
		if inherited, _ := ctx.Deadline(); inherited != deadline {
			t.Fatal("timing changed the original settings-check owner")
		}
		fixture.clock.set(fixture.base.Add(23 * time.Second))
		return true, nil
	}
	control, timing := runPgLocalTiming(t, fixture, pgQuerySampleProbe{})
	requirePgLocalRefusal(t, fixture, control)
	// This final assertion must fail on the original owning call without marks.
	requirePgLocalSpan(t, timing, "settings_generation_check", 23*time.Second)
	requirePgLocalSpan(t, timing, "cadence_publication", 0)
}

func TestPgLocalTimingPublicationDelayIsAttributed(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	syncCalls := 0
	probe := pgQuerySampleProbe{syncAttemptFile: func(*os.File) error {
		syncCalls++
		if syncCalls == 1 {
			fixture.clock.set(fixture.base.Add(23 * time.Second))
		}
		return nil
	}}
	control, timing := runPgLocalTiming(t, fixture, probe)
	requirePgLocalRefusal(t, fixture, control)
	if syncCalls < 3 {
		t.Fatal("timing fixture did not enter the actual durable cadence path")
	}
	requirePgLocalSpan(t, timing, "cadence_publication", 23*time.Second)
	requirePgLocalSpan(t, timing, "cadence_admission", 23*time.Second)
}

func TestPgLocalTimingSharedDelayIsAttributed(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	releases := 0
	fixture.env.cfg.sharedSshAdmission = &sshAdmissionPhaseFixture{acquireCommand: func(context.Context, string) (func() error, error) {
		fixture.clock.set(fixture.base.Add(23 * time.Second))
		return func() error { releases++; return nil }, nil
	}}
	control, timing := runPgLocalTiming(t, fixture, pgQuerySampleProbe{})
	requirePgLocalRefusal(t, fixture, control)
	if releases != 1 {
		t.Fatal("timing changed joined/unstarted shared-grant release ownership")
	}
	requirePgLocalSpan(t, timing, "shared_admission", 23*time.Second)
	requirePgLocalSpan(t, timing, "host_admission", 0)
}

func TestPgLocalTimingCadenceLockWaitIsSeparateFromPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
		held, err := lockProviderState(fixture.ctx, fixture.env.cfg.stateDir, "pg-query-sample-cadence")
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if held != nil {
				_ = held.Close()
			}
		}()
		done := make(chan error, 1)
		go func() {
			_, err := (pgQuerySampleProbe{}).check(fixture.ctx, fixture.env)
			done <- err
		}()
		// No real sleep or queue-length polling: the bubble establishes that the
		// actual sampler is blocked behind this held private cadence lock.
		synctest.Wait()
		fixture.clock.set(fixture.base.Add(23 * time.Second))
		if err := held.Close(); err != nil {
			t.Fatal(err)
		}
		held = nil
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		control, timing := readPgLocalTiming(t, fixture.env)
		requirePgLocalRefusal(t, fixture, control)
		requirePgLocalSpan(t, timing, "cadence_lock", 23*time.Second)
		requirePgLocalSpan(t, timing, "cadence_publication", 0)
	})
}

func TestPgLocalTimingHostWaitIsSeparateFromSharedGrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
		runner := fixture.env.runner.(*runner)
		host := fixture.env.cfg.hostByRole("pg-primary")
		var held []func()
		for range maxConcurrentRemoteCommandsPerHost {
			release, err := runner.remoteCommands.acquire(fixture.ctx, host.addr(fixture.env.cfg.addressMode))
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, release)
		}
		defer func() {
			for _, release := range held {
				release()
			}
		}()
		fixture.env.cfg.sharedSshAdmission = &sshAdmissionPhaseFixture{acquireCommand: func(context.Context, string) (func() error, error) { return func() error { return nil }, nil }}
		done := make(chan error, 1)
		go func() {
			_, err := (pgQuerySampleProbe{}).check(fixture.ctx, fixture.env)
			done <- err
		}()
		synctest.Wait()
		fixture.clock.set(fixture.base.Add(23 * time.Second))
		held[0]()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		control, timing := readPgLocalTiming(t, fixture.env)
		requirePgLocalRefusal(t, fixture, control)
		requirePgLocalSpan(t, timing, "host_admission", 23*time.Second)
		requirePgLocalSpan(t, timing, "shared_admission", 0)
	})
}

func TestPgLocalTimingCommandReadyGapIsNotAnAdmissionDelay(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	fixture.env.runner.(*runner).runSSH = func(ctx context.Context, args []string, stdin string) (string, string, error) {
		fixture.clock.set(fixture.base.Add(23 * time.Second))
		pgSampleTransportFromContext(ctx).startClock = fixture.clock.read
		return runSSHCommand(ctx, args, stdin)
	}
	control, timing := runPgLocalTiming(t, fixture, pgQuerySampleProbe{})
	requirePgLocalRefusal(t, fixture, control)
	requirePgLocalSpan(t, timing, "host_admission", 0)
	requirePgLocalSpan(t, timing, "native_setup", 0)
	var ready int64
	var setup struct {
		Entered int64 `json:"entered_ns"`
	}
	if json.Unmarshal(timing["command_ready_ns"], &ready) != nil || json.Unmarshal(timing["native_setup"], &setup) != nil || setup.Entered-ready != int64(23*time.Second) {
		t.Fatal("command-ready preemption was hidden or charged to admission/setup")
	}
}

func TestPgLocalTimingHealthyNativeSamplePreservesLifecycle(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	control, timing := runPgLocalTiming(t, fixture, pgQuerySampleProbe{})
	proof := control.TransportObservation
	if !control.Complete || control.Samples != 12 || proof.Spawn != "started" || !proof.Joined || proof.NativeExitCode == nil || *proof.NativeExitCode != 0 || proof.OwnerDeadlineExceeded {
		t.Fatal("timing altered the existing private native healthy sample")
	}
	if _, err := os.Stat(fixture.marker); err != nil {
		t.Fatal("healthy timing fixture did not start its selected native helper")
	}
	requirePgLocalSpan(t, timing, "native_setup", 0)
	requirePgLocalSpan(t, timing, "final_guard", 0)
	if string(timing["origin"]) != `"run_slot_request"` || string(timing["clock_status"]) != `"ok"` || string(timing["global_granted_ns"]) != "0" {
		t.Fatal("healthy timing lost its controlled request/grant origin")
	}
}

func TestPgLocalTimingInjectedSuccessKeepsNativeAndUnenteredSpansUnknown(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	fixture.env.runner.(*runner).runSSH = func(context.Context, []string, string) (string, string, error) {
		return pgSampleTestEncode(t, pgSampleTestFrames(fixture.env.now())), "", nil
	}
	control, timing := runPgLocalTiming(t, fixture, pgQuerySampleProbe{})
	if !control.Complete || control.TransportObservation.Joined || control.TransportObservation.Spawn == "started" {
		t.Fatal("injected timing callback invented native lifecycle")
	}
	for _, field := range []string{"settings_generation_check", "shared_admission", "shared_granted_ns", "one_shot_marker", "native_setup", "final_guard"} {
		if string(timing[field]) != "null" {
			t.Fatalf("unentered %s timing became a measured success", field)
		}
	}
}

func TestPgLocalTimingSharedCancellationPreservesOwnerAndUnknownGrant(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	deadline, _ := fixture.ctx.Deadline()
	fixture.env.cfg.sharedSshAdmission = &sshAdmissionPhaseFixture{acquireCommand: func(ctx context.Context, _ string) (func() error, error) {
		if inherited, _ := ctx.Deadline(); inherited != deadline {
			t.Fatal("shared timing renewed the original owner")
		}
		fixture.clock.set(fixture.base.Add(time.Second))
		fixture.cancel()
		return nil, ctx.Err()
	}}
	control, timing := runPgLocalTiming(t, fixture, pgQuerySampleProbe{})
	if control.Complete || control.TransportObservation.Spawn != "not_attempted" || control.TransportObservation.Joined {
		t.Fatal("canceled shared acquisition invented native execution")
	}
	requirePgLocalSpan(t, timing, "shared_admission", time.Second)
	for _, field := range []string{"shared_granted_ns", "command_ready_ns", "native_setup", "final_guard"} {
		if string(timing[field]) != "null" {
			t.Fatalf("canceled acquisition invented %s authority", field)
		}
	}
}

func TestPgLocalTimingPreflightFailureCreatesNoAttemptReceipt(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	fixture.env.cfg.routerGenerationCheck = func(context.Context) (bool, error) {
		fixture.clock.set(fixture.base.Add(time.Second))
		return false, errors.New("synthetic private settings canary")
	}
	_, err := (pgQuerySampleProbe{}).check(fixture.ctx, fixture.env)
	var preflight *pgSamplePreflightError
	if !errors.As(err, &preflight) {
		t.Fatal("timing changed the existing preflight refusal")
	}
	paths, globErr := filepath.Glob(filepath.Join(fixture.env.cfg.stateDir, "pg-query-sample", "receipt-*.json"))
	if globErr != nil || len(paths) != 0 {
		t.Fatal("unsaved preflight timing invented a durable database attempt")
	}
	alert := pgSampleFailureAlert(syntheticSettings(nil), NewPgQuerySampleSignal(), err)
	if strings.Contains(alert.Observed, "local_phase_timing") || strings.Contains(alert.Observed, "private settings canary") {
		t.Fatal("preflight warning persisted unsaved timing or private settings")
	}
}

func TestPgLocalTimingRegressingClockDoesNotChangeHealthyVerdict(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	fixture.env.cfg.routerGenerationCheck = func(context.Context) (bool, error) {
		fixture.clock.set(fixture.base.Add(-time.Second))
		return true, nil
	}
	fixture.env.runner.(*runner).runSSH = func(context.Context, []string, string) (string, string, error) {
		return pgSampleTestEncode(t, pgSampleTestFrames(fixture.env.now())), "", nil
	}
	control, timing := runPgLocalTiming(t, fixture, pgQuerySampleProbe{})
	if !control.Complete || string(timing["clock_status"]) != `"invalid"` {
		t.Fatal("invalid timing became a source failure or a valid clock")
	}
	var span struct {
		Entered  *int64 `json:"entered_ns"`
		Returned *int64 `json:"returned_ns"`
	}
	if json.Unmarshal(timing["settings_generation_check"], &span) != nil || span.Entered == nil || *span.Entered != 0 || span.Returned != nil || string(timing["host_granted_ns"]) != "null" || string(timing["global_granted_ns"]) != "0" || string(timing["sampler_entry_ns"]) != "0" {
		t.Fatal("regressing clock was clamped into invented elapsed authority")
	}
}

func TestPgLocalTimingDirectAndFreshRecorderDoNotInventPriorPhases(t *testing.T) {
	clock := &pgStartGuardControlClock{}
	clock.set(time.Now())
	ctx := observePgSampleRunSlot(t.Context())
	pgSampleTransportFromContext(ctx).localClock = clock.read
	_, direct := observePgSampleTransport(ctx)
	if direct.receipt.LocalPhaseTiming.Origin != "sampler_observer_entry" || direct.receipt.LocalPhaseTiming.GlobalGrantedNs != nil {
		t.Fatal("direct observation fabricated global scheduling authority")
	}
	direct.beginLocalPhase(pgLocalSettingsGeneration)
	direct.endLocalPhase(pgLocalSettingsGeneration)
	nextCtx := observePgSampleRunSlot(t.Context())
	nextRequest := pgSampleTransportFromContext(nextCtx)
	nextRequest.localClock = clock.read
	nextRequest.runSlotRequested()
	nextRequest.runSlotGranted(nextCtx)
	nextRequest.beginLocalPhase(pgLocalSettingsGeneration)
	nextRequest.endLocalPhase(pgLocalSettingsGeneration)
	_, next := observePgSampleTransport(nextCtx)
	if next.receipt.LocalPhaseTiming.SettingsGenerationCheck != nil || next.receipt.LocalPhaseTiming.Origin != "run_slot_request" || next.receipt.LocalPhaseTiming.GlobalGrantedNs == nil {
		t.Fatal("fresh request reused prior spans or lost its own actual grant mark")
	}
	var legacy pgSampleTransportReceipt
	if json.Unmarshal([]byte(`{"phase":"unobserved","spawn":"unknown"}`), &legacy) != nil || legacy.LocalPhaseTiming != nil {
		t.Fatal("legacy receipt invented phase timing")
	}
}

func TestPgLocalTimingEarlierOwnerAndExactGuardRemainUnchanged(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, 7*time.Second)
	deadline, _ := fixture.ctx.Deadline()
	fixture.env.cfg.routerGenerationCheck = func(ctx context.Context) (bool, error) {
		if inherited, _ := ctx.Deadline(); inherited != deadline {
			t.Fatal("local timing renewed an earlier parent deadline")
		}
		return true, nil
	}
	control, timing := runPgLocalTiming(t, fixture, pgQuerySampleProbe{})
	requirePgLocalRefusal(t, fixture, control)
	requirePgLocalSpan(t, timing, "final_guard", 0)
	_, raw := pgTransportReadControlReceipt(t, fixture.env)
	var wire struct {
		Transport struct {
			Grant *int64 `json:"run_slot_granted_remaining_budget_ms"`
		} `json:"transport_observation"`
	}
	if json.Unmarshal(raw, &wire) != nil || wire.Transport.Grant == nil || *wire.Transport.Grant > 7000 {
		t.Fatal("timing fabricated a fresh forty-second grant")
	}
}

func TestPgLocalTimingFixedEncodingBoundAndNilObserver(t *testing.T) {
	var observation *pgSampleTransportObservation
	observation.beginLocalPhase(pgLocalNativeSetup)
	observation.endLocalPhase(pgLocalNativeSetup)
	observation.localPoint(pgLocalCommandReady)
	maximum := int64(9223372036854775807)
	span := &pgSampleLocalSpan{EnteredNs: &maximum, ReturnedNs: &maximum}
	timing := &pgSampleLocalTiming{Schema: 1, Origin: "run_slot_request", ClockStatus: "ok", GlobalGrantedNs: &maximum, SamplerEntryNs: &maximum, SettingsGenerationCheck: span, CadenceLock: span, CadenceAdmission: span, CadencePublication: span, OneShotMarker: span, SampleCommandConstruction: span, TransportEntryNs: &maximum, HostAdmission: span, HostGrantedNs: &maximum, SharedAdmission: span, SharedGrantedNs: &maximum, CommandReadyNs: &maximum, NativeSetup: span, FinalGuard: span}
	raw, err := json.MarshalIndent(timing, "", "  ")
	if err != nil || len(raw) > 4096 {
		t.Fatal("fixed timing projection exceeds its finite encoding bound")
	}
	receipt := pgQuerySampleReceipt{Schema: 1, TransportObservation: &pgSampleTransportReceipt{LocalPhaseTiming: timing}}
	public, private, err := pgSampleEncodeReceipts(&receipt)
	if err != nil || len(public)+len(private) > pgSampleReceiptBytes {
		t.Fatal("timing changed the existing receipt byte cap")
	}
}

// The optional diagnostic clock is independent of the authoritative start clock.
func TestPgLocalTimingDiagnosticClockDoesNotOwnNativeGuard(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	fixture.env.runner.(*runner).runSSH = func(ctx context.Context, args []string, stdin string) (string, string, error) {
		fixture.clock.set(fixture.base.Add(23 * time.Second))
		pgSampleTransportFromContext(ctx).startClock = func() time.Time { return fixture.base }
		return runSSHCommand(ctx, args, stdin)
	}
	control, timing := runPgLocalTiming(t, fixture, pgQuerySampleProbe{})
	proof := control.TransportObservation
	if !control.Complete || control.Samples != 12 || proof.Spawn != "started" || !proof.Joined || proof.NativeExitCode == nil || *proof.NativeExitCode != 0 {
		t.Fatal("diagnostic elapsed time replaced the native guard's authoritative clock")
	}
	requirePgLocalSpan(t, timing, "final_guard", 0)
	var guard struct {
		Entered int64 `json:"entered_ns"`
	}
	if json.Unmarshal(timing["final_guard"], &guard) != nil || guard.Entered != int64(23*time.Second) {
		t.Fatal("guard clock selection hid the independent diagnostic interval")
	}
}

// A nil clock uses the existing real monotonic timestamps, with no owner renewal.
func TestPgLocalTimingNilClockPreservesGrantBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), pgQuerySampleBudget)
		defer cancel()
		ctx = observePgSampleRunSlot(ctx)
		request := pgSampleTransportFromContext(ctx)
		if request.localClock != nil {
			t.Fatal("ordinary request acquired an injected diagnostic clock")
		}
		request.runSlotRequested()
		request.runSlotGranted(ctx)
		if request.receipt.RunSlotWaitMs == nil || *request.receipt.RunSlotWaitMs != 0 || request.receipt.RunSlotGrantedRemainingBudgetMs == nil || *request.receipt.RunSlotGrantedRemainingBudgetMs != 40000 {
			t.Fatal("nil diagnostic clock changed the original request/grant budget")
		}
		_, observation := observePgSampleTransport(ctx)
		if observation.localClock != nil || observation.receipt.RemainingBudgetMs != 40000 || observation.receipt.LocalPhaseTiming.GlobalGrantedNs == nil || *observation.receipt.LocalPhaseTiming.GlobalGrantedNs != 0 {
			t.Fatal("nil diagnostic clock changed inherited grant or entry timing")
		}
	})
}

// Ordinary SSH paths have no PG recorder and cannot emit its timing projection.
func TestPgLocalTimingNonPgTransportKeepsObserverAbsent(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	calls := 0
	fixture.env.runner.(*runner).runSSH = func(ctx context.Context, _ []string, _ string) (string, string, error) {
		calls++
		if pgSampleTransportFromContext(ctx) != nil {
			t.Fatal("ordinary SSH caller inherited a PG timing observer")
		}
		return "synthetic success", "", nil
	}
	output, err := fixture.env.runner.(*runner).ssh(t.Context(), fixture.env.cfg.hostByRole("pg-primary"), "synthetic command", "")
	if err != nil || calls != 1 || output != "synthetic success" {
		t.Fatal("nil PG observer changed the ordinary SSH path")
	}
	raw, err := json.Marshal(pgSampleTransportReceipt{Phase: "unobserved", Spawn: "unknown"})
	if err != nil || strings.Contains(string(raw), "local_phase_timing") {
		t.Fatal("absent timing serialized as measured ordinary transport")
	}
}

// One-shot publication uses its own span and does not invent continuous cadence.
func TestPgLocalTimingOneShotMarkerIsSeparateFromCadence(t *testing.T) {
	fixture := newPgLocalTimingFixture(t, pgQuerySampleBudget)
	fixture.env.cfg.pgQuerySampleContinuous = false
	fixture.env.cfg.pgQuerySampleUntil = fixture.env.now().Add(time.Hour)
	probe := pgQuerySampleProbe{syncAttemptFile: func(*os.File) error {
		fixture.clock.set(fixture.base.Add(23 * time.Second))
		return nil
	}}
	control, timing := runPgLocalTiming(t, fixture, probe)
	requirePgLocalRefusal(t, fixture, control)
	requirePgLocalSpan(t, timing, "one_shot_marker", 23*time.Second)
	for _, field := range []string{"cadence_lock", "cadence_admission", "cadence_publication"} {
		if string(timing[field]) != "null" {
			t.Fatalf("one-shot publication invented %s", field)
		}
	}
}
