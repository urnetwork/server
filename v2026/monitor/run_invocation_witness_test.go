package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type invocationTestSignal struct {
	run func(context.Context) (Alerts, error)
}

func (*invocationTestSignal) Number() string         { return "1.1" }
func (*invocationTestSignal) Key() string            { return "invocation-control" }
func (*invocationTestSignal) ID() string             { return "synthetic/invocation-control" }
func (*invocationTestSignal) Name() string           { return "private-name-must-not-render" }
func (*invocationTestSignal) Cadence() time.Duration { return time.Hour }
func (self *invocationTestSignal) Run(ctx context.Context, _ SignalSettings) (Alerts, error) {
	return self.run(ctx)
}

func invocationControlWitness(t *testing.T, signal Signal, output io.Writer, diagnostics io.Writer) *RunInvocationWitness {
	t.Helper()
	w, err := newRunInvocationWitness(output, diagnostics, settingsWitnessTestIdentity(), []Signal{signal}, defaultRunInvocationLimits())
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func invocationControlRecords(t *testing.T, output *bytes.Buffer) []runInvocationRecord {
	t.Helper()
	records := []runInvocationRecord{}
	for _, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		var record runInvocationRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

// Actual scheduler dispatches persist healthy/empty and held-only returns even
// when their ordinary gated output is empty. Error text must stay private.
func TestInvocationWitnessSchedulerEmptyHeldAndError(t *testing.T) {
	for _, mode := range []string{"empty", "held", "error"} {
		t.Run(mode, func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			calls, handled := 0, 0
			private := errors.New("private-source-error-and-query")
			s := &invocationTestSignal{run: func(context.Context) (Alerts, error) {
				calls++
				if mode == "error" {
					return nil, private
				}
				if mode == "held" {
					return Alerts{{SignalID: "synthetic", Class: "broken", Target: "private-target", Sustain: 2}}, nil
				}
				return nil, nil
			}}
			w := invocationControlWitness(t, s, &output, &diagnostics)
			stop := errors.New("stop-handler")
			err := NewWithSignals(SignalSettings{}, s).RunExecutionLoop(context.Background(), RunLoopOptions{InvocationWitness: w}, func(_ context.Context, _ Signal, execution SignalExecution) error {
				handled++
				if mode == "held" && (len(execution.Alerts) != 0 || len(execution.HeldAlerts) != 1) {
					t.Error("held execution changed")
				}
				if execution.Failed != (mode == "error") {
					t.Error("source status changed")
				}
				return stop
			})
			if !errors.Is(err, stop) || calls != 1 || handled != 1 {
				t.Fatal("observer changed actual dispatch/handler outcome")
			}
			r := invocationControlRecords(t, &output)
			if len(r) != 2 || r[0].Event != "dispatch_begin" || r[1].Event != "run_return" || r[0].Invocation != r[1].Invocation || r[0].Sequence != 1 || r[1].Sequence != 2 || r[0].Identity != settingsWitnessTestIdentity() || r[0].RunId != r[1].RunId || r[0].SelectedKeysSha256 != r[1].SelectedKeysSha256 {
				t.Fatal("actual invocation pairing/identity lost")
			}
			want := "returned_nil"
			if mode == "error" {
				want = "returned_error"
			}
			if r[1].Result != want || diagnostics.Len() != 0 {
				t.Fatal("return class changed")
			}
			for _, text := range []string{private.Error(), "private-target", s.Name(), "healthy", "backend_complete"} {
				if strings.Contains(output.String(), text) {
					t.Fatal("private or unsupported assertion rendered")
				}
			}
		})
	}
}

type invocationFailWriter struct {
	calls  int
	failAt int
	short  bool
	output bytes.Buffer
}

func (self *invocationFailWriter) Write(b []byte) (int, error) {
	self.calls++
	if self.calls == self.failAt {
		if self.short {
			return len(b) - 1, nil
		}
		return 0, errors.New("private-sink-detail")
	}
	return self.output.Write(b)
}

// Exercise failures at both actual scheduler begin and return. Neither may
// skip the source/handler, alter errors or retry writes/diagnostics.
func TestInvocationWitnessRuntimeFailureDoesNotChangeScheduler(t *testing.T) {
	for _, at := range []int{1, 2} {
		for _, short := range []bool{false, true} {
			out := &invocationFailWriter{failAt: at, short: short}
			var diagnostic bytes.Buffer
			calls, handled := 0, 0
			sourceErr := errors.New("source-outcome")
			s := &invocationTestSignal{run: func(context.Context) (Alerts, error) { calls++; return nil, sourceErr }}
			w := invocationControlWitness(t, s, out, &diagnostic)
			stop := errors.New("handler-outcome")
			err := NewWithSignals(SignalSettings{}, s).RunExecutionLoop(context.Background(), RunLoopOptions{InvocationWitness: w}, func(_ context.Context, _ Signal, e SignalExecution) error {
				handled++
				if !e.Failed {
					t.Error("lost failed source")
				}
				return stop
			})
			if !errors.Is(err, stop) || calls != 1 || handled != 1 {
				t.Fatal("sink changed scheduler")
			}
			_, err = runSignalWithWitness(context.Background(), s, SignalSettings{}, w, "active", 0)
			if err != sourceErr || calls != 2 || out.calls != at || diagnostic.String() != runInvocationDiagnostic+"\n" {
				t.Fatal("sticky failure changed result, retried or leaked diagnostics")
			}
		}
	}
}

func TestInvocationWitnessBoundsClosedOwnerAndSequence(t *testing.T) {
	for _, bound := range []string{"record", "bytes", "count", "sequence", "invocation", "closed", "identity"} {
		var out, diag bytes.Buffer
		calls := 0
		s := &invocationTestSignal{run: func(context.Context) (Alerts, error) { calls++; return nil, nil }}
		if bound == "identity" {
			if _, err := newRunInvocationWitness(&out, &diag, settingsWitnessIdentity{}, []Signal{s}, defaultRunInvocationLimits()); err == nil {
				t.Fatal("invalid identity accepted")
			}
			continue
		}
		w := invocationControlWitness(t, s, &out, &diag)
		switch bound {
		case "record":
			w.limits.recordBytes = 1
		case "bytes":
			w.limits.totalBytes = 1
		case "count":
			w.limits.records = 1
		case "sequence":
			w.sequence = math.MaxUint64
		case "invocation":
			w.invocation = math.MaxUint64
		case "closed":
			w.Close()
		}
		if _, err := runSignalWithWitness(context.Background(), s, SignalSettings{}, w, "active", 0); err != nil || calls != 1 {
			t.Fatal("budget/closed sink changed invocation")
		}
		before := out.Len()
		runSignalWithWitness(context.Background(), s, SignalSettings{}, w, "active", 0)
		if out.Len() != before || calls != 2 {
			t.Fatal("unusable stream resumed")
		}
		if bound != "closed" && diag.String() != runInvocationDiagnostic+"\n" {
			t.Fatal("failure diagnostic not fixed and once")
		}
	}
	if defaultRunInvocationLimits() != (runInvocationLimits{4096, 512 * 1024 * 1024, 1000000}) {
		t.Fatal("admitted launch budget changed")
	}
}

// Cancellation on a queued admission proves no dispatch. A source that returns
// after cancel is recorded only as that return, while shutdown still omits handler.
func TestInvocationWitnessAdmissionCancellationAndShutdown(t *testing.T) {
	var out, diag bytes.Buffer
	calls := 0
	s := &invocationTestSignal{run: func(context.Context) (Alerts, error) { calls++; return nil, nil }}
	w := invocationControlWitness(t, s, &out, &diag)
	slots := newRunSlotPool(1)
	release, err := slots.acquire(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := runSignalInSlotWithWitness(ctx, slots, s, SignalSettings{}, w, 0); err == nil || calls != 0 || out.Len() != 0 {
		t.Fatal("unadmitted source was dispatched")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	s.run = func(context.Context) (Alerts, error) { calls++; cancel(); return nil, nil }
	handled := 0
	if err := NewWithSignals(SignalSettings{}, s).RunExecutionLoop(ctx, RunLoopOptions{InvocationWitness: w}, func(context.Context, Signal, SignalExecution) error { handled++; return nil }); err != nil || handled != 0 || calls != 1 {
		t.Fatal("shutdown behavior changed")
	}
	r := invocationControlRecords(t, &out)
	if len(r) != 2 || r[1].Result != "returned_nil_context_ended" {
		t.Fatal("ended parent became healthy or lost honest return")
	}
}

func TestInvocationWitnessDoesNotRecoverPanicOrJoinAsyncChild(t *testing.T) {
	var out, diag bytes.Buffer
	s := &invocationTestSignal{run: func(context.Context) (Alerts, error) { panic("original-panic") }}
	w := invocationControlWitness(t, s, &out, &diag)
	func() {
		defer func() {
			if recover() != "original-panic" {
				t.Error("observer changed panic")
			}
		}()
		runSignalWithWitness(context.Background(), s, SignalSettings{}, w, "active", 0)
	}()
	if r := invocationControlRecords(t, &out); len(r) != 1 || r[0].Event != "dispatch_begin" {
		t.Fatal("unwinding fabricated return")
	}
	out.Reset()
	release, joined := make(chan struct{}), make(chan struct{})
	go func() { <-release; close(joined) }()
	defer func() { close(release); <-joined }()
	s.run = func(context.Context) (Alerts, error) { return nil, nil }
	w = invocationControlWitness(t, s, &out, &diag)
	runSignalWithWitness(context.Background(), s, SignalSettings{}, w, "standing", 24*time.Hour)
	select {
	case <-joined:
		t.Fatal("test child unexpectedly joined")
	default:
	}
	r := invocationControlRecords(t, &out)
	if len(r) != 2 || r[1].Result != "returned_nil" || r[0].ConfiguredDelayNanoseconds != int64(time.Hour) {
		t.Fatal("return/standing scope changed")
	}
}

// Real loop manual ticks prove observer-on/off parity and post-handler cadence.
func TestInvocationWitnessCadenceAndDisabledParity(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var out, diag bytes.Buffer
		started := make(chan struct{}, 2)
		release := make(chan struct{})
		handled := make(chan struct{}, 2)
		ticks := make(chan cadenceFloorTick, 3)
		s := &invocationTestSignal{run: func(context.Context) (Alerts, error) { started <- struct{}{}; return nil, nil }}
		var w *RunInvocationWitness
		if enabled {
			w = invocationControlWitness(t, s, &out, &diag)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- NewWithSignals(SignalSettings{}, s).runExecutionLoop(ctx, RunLoopOptions{MinimumProbeCadence: 2 * time.Hour, InvocationWitness: w}, func(context.Context, Signal, SignalExecution) error { handled <- struct{}{}; <-release; return nil }, func(d time.Duration) runLoopTicker {
				tick := cadenceFloorTick{d, make(chan time.Time, 1)}
				ticks <- tick
				return &manualRunLoopTicker{c: tick.ticks}
			})
		}()
		first := receiveCadenceFloorValue(t, ticks)
		if first.cadence != 2*time.Hour {
			t.Fatal("startup floor changed")
		}
		select {
		case <-started:
			t.Fatal("dispatch before floor")
		default:
		}
		first.ticks <- time.Time{}
		receiveCadenceFloorValue(t, started)
		receiveCadenceFloorValue(t, handled)
		select {
		case <-ticks:
			t.Fatal("cadence began during handler")
		default:
		}
		release <- struct{}{}
		next := receiveCadenceFloorValue(t, ticks)
		if next.cadence != 2*time.Hour {
			t.Fatal("effective delay changed")
		}
		cancel()
		if err := receiveCadenceFloorValue(t, done); err != nil {
			t.Fatal(err)
		}
		if !enabled && (out.Len() != 0 || diag.Len() != 0) {
			t.Fatal("disabled witness performed I/O")
		}
		if enabled && len(invocationControlRecords(t, &out)) != 2 {
			t.Fatal("enabled loop lost empty return")
		}
	}
}

func TestInvocationWitnessConcurrentCallsRemainPaired(t *testing.T) {
	var out, diag bytes.Buffer
	s := &invocationTestSignal{run: func(context.Context) (Alerts, error) { return nil, nil }}
	w := invocationControlWitness(t, s, &out, &diag)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			runSignalWithWitness(context.Background(), s, SignalSettings{}, w, "active", 0)
		}()
	}
	wait.Wait()
	r := invocationControlRecords(t, &out)
	seen := map[uint64]int{}
	for i, e := range r {
		if e.Sequence != uint64(i+1) {
			t.Fatal("sequence lost")
		}
		seen[e.Invocation]++
	}
	if len(r) != 16 || len(seen) != 8 {
		t.Fatal("concurrent events lost")
	}
	for _, n := range seen {
		if n != 2 {
			t.Fatal("unpaired invocation")
		}
	}
}

// Refused paths cause no identity discovery or source execution.
func TestInvocationWitnessStartupRefusesReusedOrNonPrivatePath(t *testing.T) {
	private := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewRunInvocationWitness(ctx, filepath.Join(private, "w.jsonl"), SignalSettings{}, nil, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatal("ended startup accepted")
	}
	if _, err := NewRunInvocationWitness(context.Background(), private, SignalSettings{}, nil, io.Discard); err == nil {
		t.Fatal("existing destination accepted")
	}
}

// Reuse the already bound startup identity; this must not discover /proc or
// require a native unit. Real private-file ownership and permissions are tested.
func TestInvocationWitnessProtectedFileAndReusedIdentity(t *testing.T) {
	var diag bytes.Buffer
	s := &invocationTestSignal{run: func(context.Context) (Alerts, error) { return nil, nil }}
	settings := SignalSettings{settingsGenerationWitness: &settingsGenerationWitness{identity: settingsWitnessTestIdentity()}}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "w.jsonl")
	w, err := NewRunInvocationWitness(context.Background(), path, settings, []Signal{s}, &diag)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("private output permissions lost")
	}
	if _, err := NewRunInvocationWitness(context.Background(), path, settings, []Signal{s}, &diag); err == nil {
		t.Fatal("existing output reused")
	}
	runSignalWithWitness(context.Background(), s, SignalSettings{}, w, "active", 0)
	w.Close()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	runSignalWithWitness(context.Background(), s, SignalSettings{}, w, "active", 0)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("closed output changed")
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRunInvocationWitness(context.Background(), filepath.Join(dir, "other"), settings, []Signal{s}, &diag); err == nil {
		t.Fatal("nonprivate parent accepted")
	}
}

type invocationCloseErrorWriter struct{ bytes.Buffer }

func (*invocationCloseErrorWriter) Close() error { return errors.New("private-close-error") }
func TestInvocationWitnessCloseFailureCannotChangeSourceOutcome(t *testing.T) {
	var diag bytes.Buffer
	out := &invocationCloseErrorWriter{}
	calls := 0
	s := &invocationTestSignal{run: func(context.Context) (Alerts, error) { calls++; return nil, nil }}
	w := invocationControlWitness(t, s, out, &diag)
	if err := w.Close(); err != nil {
		t.Fatal("close failure escaped witness")
	}
	if _, err := runSignalWithWitness(context.Background(), s, SignalSettings{}, w, "active", 0); err != nil || calls != 1 || out.Len() != 0 || diag.String() != runInvocationDiagnostic+"\n" {
		t.Fatal("closed failed witness changed source or leaked error")
	}
}
