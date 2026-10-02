package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// Explicitly queued ordinary work cannot push each recurring finite observer
// behind the whole fleet. Two priority grants then one ordinary grant also
// prevent a stream of finite work from starving ordinary probes.
func TestRunSlotsPrioritizeBoundedWorkWithoutStarvingOrdinary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newRunSlotPool(1)
		release, err := p.acquire(t.Context(), false)
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan string, 5)
		finish := make(chan struct{})
		for _, item := range []struct {
			name     string
			priority bool
		}{{"ordinary1", false}, {"ordinary2", false}, {"bounded1", true}, {"bounded2", true}, {"bounded3", true}} {
			go func() {
				release, err := p.acquire(t.Context(), item.priority)
				if err != nil {
					t.Error(err)
					return
				}
				got <- item.name
				<-finish
				release()
			}()
			synctest.Wait() // each contender is durably queued before the next
		}
		release()
		for _, want := range []string{"bounded1", "bounded2", "ordinary1", "bounded3", "ordinary2"} {
			if observed := <-got; observed != want {
				t.Errorf("admitted %s, want %s", observed, want)
			}
			finish <- struct{}{}
		}
		synctest.Wait()
		if p.active != 0 || len(p.waiting) != 0 {
			t.Fatal("terminal queue retained grants or waiters")
		}
	})
}

func TestRunSlotsCanceledWaiterCannotStealOrReleaseAnotherGrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newRunSlotPool(1)
		release, err := p.acquire(t.Context(), false)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := p.acquire(ctx, true); done <- err }()
		synctest.Wait()
		cancel()
		if !errors.Is(<-done, context.Canceled) || p.active != 1 || len(p.waiting) != 0 {
			t.Fatal("canceled waiter changed another owner's grant")
		}
		release()
		if p.active != 0 {
			t.Fatal("owned grant was not released")
		}
	})
}

// Preserve the actual PG identity/budget but fail before its Run method. This
// is precisely the boundary where there cannot yet be a database receipt.
func TestPgQuerySampleQueueDeadlineIsVisibleOnFirstCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newRunSlotPool(runLoopMaxConcurrentSignals)
		for range runLoopMaxConcurrentSignals {
			release, err := p.acquire(t.Context(), false)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
		}
		s := NewPgQuerySampleSignal()
		settings := syntheticSettings(nil)
		settings.Now = time.Now
		settings.PGQuerySampleContinuous = true
		settings.StateDir = t.TempDir()
		started := time.Now()
		_, err := runSignalInSlot(t.Context(), p, s, settings)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 40*time.Second {
			t.Fatal("shared-slot owner budget changed")
		}
		alert := visibilityAlert(settings, s, err)
		if alert.Sustain != 1 || !strings.Contains(alert.Observed, "phase=shared-slot-admission") || !strings.Contains(alert.Observed, "source_contact_attempted=false") || !strings.Contains(alert.Observed, "last_completed_at=unknown") {
			t.Fatal("precontact loss invented a source observation or hid its phase")
		}
		if got := newCadenceAlertGate().filter(s, Alerts{alert}); len(got) != 1 {
			t.Fatal("first missing PG sample was suppressed")
		}
		if entries, err := os.ReadDir(settings.StateDir); err != nil || len(entries) != 0 {
			t.Fatal("unadmitted source invented durable sample state")
		}
		ordinary := visibilityAlert(settings, &steppedAlertSignal{}, err)
		if ordinary.Sustain != 2 {
			t.Fatal("ordinary visibility sustain changed")
		}
	})
}

type scheduledPgSampleFixture struct {
	Signal
	env *probeEnv
}

func (s *scheduledPgSampleFixture) runBudget() time.Duration { return pgQuerySampleBudget }
func (s *scheduledPgSampleFixture) Run(ctx context.Context, settings SignalSettings) (Alerts, error) {
	findings, err := (pgQuerySampleProbe{}).check(ctx, s.env)
	var alerts Alerts
	for _, f := range findings {
		alerts = append(alerts, alertFromFinding(settings, s.Number(), s.Key(), s.Name(), f))
	}
	return alerts, err
}

// Run the real12-snapshot parser, cadence admission and immutable receipt path
// through the real loop. Only SSH data is supplied by the fixture. The first
// bulk wave must not beat it to every slot; after fifteen minutes a second
// queued sample must receive the next released slot ahead of old ordinary work.
func TestPgQuerySampleCollectsAcrossLoadedStartupAndRecurringCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var active, maximum atomic.Int64
		ordinaryStarted := make(chan struct{}, 12)
		ordinaryRelease := make(chan struct{})
		calls := 0
		env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
			calls++
			if (calls == 1 && active.Load() != 0) || active.Load() >= runLoopMaxConcurrentSignals {
				t.Error("sampler lost startup priority or exceeded shared capacity")
			}
			started := time.Now()
			time.Sleep(22 * time.Second)
			return pgSampleTestEncode(t, pgSampleTestFrames(started)), "", nil
		})
		env.cfg.pgQuerySampleUntil = time.Time{}
		env.cfg.pgQuerySampleContinuous = true
		env.now = time.Now
		sampler := &scheduledPgSampleFixture{Signal: NewPgQuerySampleSignal(), env: env}
		signals := make([]Signal, 0, 13)
		for i := range 12 {
			signals = append(signals, &blockingLoopSignal{number: fmt.Sprint(i), active: &active, maximum: &maximum, started: ordinaryStarted, release: ordinaryRelease})
		}
		signals = append(signals, sampler)
		settings := syntheticSettings(&syntheticSource{})
		settings.Now = time.Now
		settings.StateDir = env.cfg.stateDir
		settings.PGQuerySampleContinuous = true
		sampled := make(chan struct{}, 2)
		done := make(chan error, 1)
		start := time.Now()
		go func() {
			done <- NewWithSignals(settings, signals...).RunLoopWithOptions(ctx, RunLoopOptions{MinimumProbeCadence: 15 * time.Minute}, func(_ context.Context, s Signal, _ Alerts) error {
				if s.ID() == sampler.ID() {
					sampled <- struct{}{}
				}
				return nil
			})
		}()
		<-sampled
		if calls != 1 || time.Since(start) != 15*time.Minute+22*time.Second {
			t.Fatal("initial floor or finite sample duration changed")
		}
		for range runLoopMaxConcurrentSignals {
			<-ordinaryStarted
		}
		first, err := pgSampleReadCadence(filepath.Join(env.cfg.stateDir, "pg-query-sample"), time.Now())
		if err != nil || first.Outcome != "complete" || first.CompletedAt.IsZero() {
			t.Fatal("first scheduled turn did not retain a complete receipt", err)
		}
		time.Sleep(15*time.Minute + time.Second)
		if calls != 1 {
			t.Fatal("occupied slots were bypassed")
		}
		ordinaryRelease <- struct{}{}
		<-sampled
		second, err := pgSampleReadCadence(filepath.Join(env.cfg.stateDir, "pg-query-sample"), time.Now())
		if err != nil || calls != 2 || second.Outcome != "complete" || second.AttemptedAt.Before(first.NextEligibleAt) || second.ReceiptSHA256 == first.ReceiptSHA256 {
			t.Fatal("queued recurring sample lost its durable floor or fresh receipt", err)
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if maximum.Load() > runLoopMaxConcurrentSignals {
			t.Fatal("global execution cap increased")
		}
	})
}
