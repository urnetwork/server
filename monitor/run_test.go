package monitor

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type blockingLoopSignal struct {
	number  string
	active  *atomic.Int64
	maximum *atomic.Int64
	started chan<- struct{}
	release <-chan struct{}
}

type steppedAlertSignal struct {
	started chan<- struct{}
	release <-chan struct{}
}

type runLoopStreamingSource struct {
	*syntheticSource
	started chan<- []string
}

func (s *runLoopStreamingSource) StreamLocal(ctx context.Context, name string, args ...string) (*exec.Cmd, io.ReadCloser, error) {
	if name != "warpctl" {
		return nil, nil, fmt.Errorf("stream command = %q, want warpctl", name)
	}
	s.started <- append([]string(nil), args...)
	return fakeStream("exec sleep 3600")(ctx)
}

func (s *steppedAlertSignal) Number() string         { return "synthetic-stepped" }
func (s *steppedAlertSignal) Key() string            { return "synthetic-stepped" }
func (s *steppedAlertSignal) ID() string             { return "synthetic/stepped" }
func (s *steppedAlertSignal) Name() string           { return "synthetic stepped signal" }
func (s *steppedAlertSignal) Cadence() time.Duration { return time.Millisecond }
func (s *steppedAlertSignal) Run(ctx context.Context, _ SignalSettings) (Alerts, error) {
	s.started <- struct{}{}
	select {
	case <-s.release:
		return Alerts{{
			SignalID: "synthetic/stepped",
			Class:    "broken",
			Target:   "target",
			Sustain:  2,
		}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *blockingLoopSignal) Number() string         { return s.number }
func (s *blockingLoopSignal) Key() string            { return "loop-" + s.number }
func (s *blockingLoopSignal) ID() string             { return "synthetic/loop-" + s.number }
func (s *blockingLoopSignal) Name() string           { return "synthetic loop signal " + s.number }
func (s *blockingLoopSignal) Cadence() time.Duration { return time.Hour }

func (s *blockingLoopSignal) Run(ctx context.Context, _ SignalSettings) (Alerts, error) {
	active := s.active.Add(1)
	defer s.active.Add(-1)
	for maximum := s.maximum.Load(); active > maximum; maximum = s.maximum.Load() {
		if s.maximum.CompareAndSwap(maximum, active) {
			break
		}
	}
	s.started <- struct{}{}
	select {
	case <-s.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestRunLoopStartsStandingLogTailers(t *testing.T) {
	started := make(chan []string, 1)
	source := &runLoopStreamingSource{
		syntheticSource: &syntheticSource{localFn: func(name string, args ...string) (string, error) {
			if name != "warpctl" || strings.Join(args, " ") != "ls services synthetic" {
				return "", fmt.Errorf("unexpected local command %s %s", name, strings.Join(args, " "))
			}
			return "repo names synthetic-taskworker", nil
		}},
		started: started,
	}
	monitor := NewWithSignals(syntheticSettings(source), NewLogErrorsSignal())
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() {
		runErr <- monitor.RunLoop(ctx, func(context.Context, Signal, Alerts) error { return nil })
	}()

	select {
	case args := <-started:
		if got, want := strings.Join(args, " "), "logs synthetic taskworker --since=1s -f"; got != want {
			t.Fatalf("standing log command = %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not start the standing taskworker log tailer")
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("RunLoop returned an error after cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not stop its standing log tailer")
	}
}

func TestRunLoopBoundsConcurrentSignalExecutions(t *testing.T) {
	const signalCount = runLoopMaxConcurrentSignals * 3

	var active atomic.Int64
	var maximum atomic.Int64
	started := make(chan struct{}, signalCount)
	release := make(chan struct{})
	signals := make([]Signal, 0, signalCount)
	for i := range signalCount {
		signals = append(signals, &blockingLoopSignal{
			number:  fmt.Sprintf("synthetic-%d", i),
			active:  &active,
			maximum: &maximum,
			started: started,
			release: release,
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	allHandled := make(chan struct{})
	var handled atomic.Int64
	runErr := make(chan error, 1)
	go func() {
		runErr <- NewWithSignals(SignalSettings{}, signals...).RunLoop(
			ctx,
			func(context.Context, Signal, Alerts) error {
				if handled.Add(1) == signalCount {
					close(allHandled)
				}
				return nil
			},
		)
	}()

	for range runLoopMaxConcurrentSignals {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for the bounded startup wave")
		}
	}
	if got := active.Load(); got != runLoopMaxConcurrentSignals {
		t.Fatalf("active signal count = %d, want %d", got, runLoopMaxConcurrentSignals)
	}
	select {
	case <-started:
		t.Fatalf("another signal started while all %d execution slots were occupied", runLoopMaxConcurrentSignals)
	default:
	}

	close(release)
	select {
	case <-allHandled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for every initial signal execution")
	}
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("RunLoop returned an error after parent cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not stop after parent cancellation")
	}
	if got := maximum.Load(); got != runLoopMaxConcurrentSignals {
		t.Fatalf("maximum concurrent signal count = %d, want %d", got, runLoopMaxConcurrentSignals)
	}
}

func TestRunLoopDoesNotAlertForShutdownCancellation(t *testing.T) {
	var active atomic.Int64
	var maximum atomic.Int64
	started := make(chan struct{}, 1)
	signal := &blockingLoopSignal{
		number:  "synthetic-shutdown",
		active:  &active,
		maximum: &maximum,
		started: started,
		release: make(chan struct{}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	var handled atomic.Int64
	runErr := make(chan error, 1)
	go func() {
		runErr <- NewWithSignals(SignalSettings{}, signal).RunLoop(
			ctx,
			func(context.Context, Signal, Alerts) error {
				handled.Add(1)
				return nil
			},
		)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the signal to start")
	}
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("RunLoop returned an error after parent cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not stop after parent cancellation")
	}
	if got := handled.Load(); got != 0 {
		t.Fatalf("shutdown cancellation invoked the alert handler %d time(s), want 0", got)
	}
}

func TestRunLoopRequiresConsecutiveTicksBeforeHandlingSustainedAlert(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	signal := &steppedAlertSignal{started: started, release: release}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handled := make(chan Alerts, 2)
	runErr := make(chan error, 1)
	go func() {
		runErr <- NewWithSignals(SignalSettings{}, signal).RunLoop(
			ctx,
			func(_ context.Context, _ Signal, alerts Alerts) error {
				handled <- alerts
				return nil
			},
		)
	}()

	for tick := 1; tick <= 2; tick++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for tick %d", tick)
		}
		release <- struct{}{}
		select {
		case alerts := <-handled:
			want := 0
			if tick == 2 {
				want = 1
			}
			if len(alerts) != want {
				t.Fatalf("tick %d handled %d alert(s), want %d", tick, len(alerts), want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for tick %d handler", tick)
		}
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("RunLoop returned an error after parent cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not stop after parent cancellation")
	}
}

func TestCadenceAlertGateResetsStreakAfterHealthyTick(t *testing.T) {
	gate := newCadenceAlertGate()
	signal := &steppedAlertSignal{}
	alert := Alert{SignalID: signal.ID(), Class: "broken", Target: "target", Sustain: 2}

	if got := gate.filter(signal, Alerts{alert}); len(got) != 0 {
		t.Fatalf("first broken tick returned %d alert(s), want 0", len(got))
	}
	if got := gate.filter(signal, nil); len(got) != 0 {
		t.Fatalf("healthy tick returned %d alert(s), want 0", len(got))
	}
	if got := gate.filter(signal, Alerts{alert}); len(got) != 0 {
		t.Fatalf("broken tick after reset returned %d alert(s), want 0", len(got))
	}
	if got := gate.filter(signal, Alerts{alert}); len(got) != 1 {
		t.Fatalf("second consecutive broken tick returned %d alert(s), want 1", len(got))
	}
}
