package monitor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Decode the immutable receipt independently of the implementation's added
// fields. An older source must reach the missing timing assertion, not fail to build.
type pgRunSlotTimingReceipt struct {
	Complete             bool                   `json:"complete"`
	Samples              int                    `json:"samples"`
	Reason               string                 `json:"reason"`
	SourceFailure        *pgSampleSourceFailure `json:"source_failure"`
	TransportObservation *struct {
		pgTransportReceiptProof
		RunSlotWaitMs                   *int64 `json:"run_slot_wait_ms"`
		RunSlotGrantedRemainingBudgetMs *int64 `json:"run_slot_granted_remaining_budget_ms"`
	} `json:"transport_observation"`
}

// Delaying the caller after the actual grant is deliberately different from
// retaining all four slots. Both can leave the same budget at PG entry.
type pgRunSlotTimingSignal struct {
	*scheduledPgSampleFixture
	entryDelay time.Duration
}

// Keep the real probe, durable cadence and receipt path after the controlled delay.
func (self *pgRunSlotTimingSignal) Run(ctx context.Context, settings SignalSettings) (Alerts, error) {
	time.Sleep(self.entryDelay)
	return self.scheduledPgSampleFixture.Run(ctx, settings)
}

// Only SSH is synthetic. It either supplies all twelve frames after the SQL's
// mandatory waits or obeys the inherited owner; it never claims a native child.
func pgRunSlotTimingFixture(t *testing.T) (*pgRunSlotTimingSignal, *int) {
	t.Helper()
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		t.Fatal("unexpected transport callback")
		return "", "", nil
	})
	env.cfg.pgQuerySampleUntil = time.Time{}
	env.cfg.pgQuerySampleContinuous = true
	env.now = time.Now
	calls := new(int)
	env.runner.(*runner).runSSH = func(ctx context.Context, _ []string, _ string) (string, string, error) {
		*calls++
		started := time.Now()
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(22 * time.Second):
			return pgSampleTestEncode(t, pgSampleTestFrames(started)), "", nil
		}
	}
	return &pgRunSlotTimingSignal{scheduledPgSampleFixture: &scheduledPgSampleFixture{Signal: NewPgQuerySampleSignal(), env: env}}, calls
}

// Use the real four-slot pool and queue-inclusive owner, without changing its
// grants. synctest.Wait proves the request is queued before the release clock.
func pgRunSlotTimingTurn(t *testing.T, ctx context.Context, slots *runSlotPool, signal *pgRunSlotTimingSignal, queueDelay time.Duration) time.Duration {
	t.Helper()
	var releases []func()
	if queueDelay > 0 {
		for range runLoopMaxConcurrentSignals {
			release, err := slots.acquire(ctx, false)
			if err != nil {
				t.Fatal("fixture could not retain the four existing grants")
			}
			releases = append(releases, release)
		}
	}
	defer func() {
		for _, release := range releases {
			if release != nil {
				release()
			}
		}
	}()
	settings := syntheticSettings(nil)
	settings.Now = time.Now
	settings.PGQuerySampleContinuous = true
	settings.StateDir = signal.env.cfg.stateDir
	started := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := runSignalInSlot(ctx, slots, signal, settings)
		done <- err
	}()
	synctest.Wait()
	if queueDelay > 0 {
		if slots.active != runLoopMaxConcurrentSignals || len(slots.waiting) != 1 {
			t.Fatal("fixture did not reach the actual saturated global queue")
		}
		time.Sleep(queueDelay)
		releases[0]()
		releases[0] = nil
	}
	if err := <-done; err != nil {
		t.Fatal("real PG receipt path failed", err)
	}
	return time.Since(started)
}

// Read the exact receipt selected by durable cadence, including after a second
// attempt. Neither an alert timestamp nor a shared SSH queue is a grant clock.
func pgRunSlotReadTiming(t *testing.T, env *probeEnv) pgRunSlotTimingReceipt {
	t.Helper()
	dir := filepath.Join(env.cfg.stateDir, "pg-query-sample")
	state, err := pgSampleReadCadence(dir, time.Now())
	if err != nil || !state.NextEligibleAt.Equal(time.Now().Add(pgQuerySampleCadence)) {
		t.Fatal("terminal sample lost its unchanged cadence floor", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "receipt-"+state.ReceiptSHA256+".json"))
	var receipt pgRunSlotTimingReceipt
	if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.TransportObservation == nil {
		t.Fatal("fixture lacks its actual immutable transport receipt")
	}
	for _, private := range []string{"secret-test-value", "192.0.2.1", "transport_nonce"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private source bytes entered the finite timing projection")
		}
	}
	if receipt.TransportObservation.Spawn == "started" || receipt.TransportObservation.Joined {
		t.Fatal("synthetic transport acquired native child authority")
	}
	return receipt
}

// Equal entry budgets do not identify the cause: only the actual request and
// dispatch owners can distinguish the queue from delay after an immediate grant.
func TestPgRunSlotTimingSeparatesQueueFromPostGrantDelay(t *testing.T) {
	for _, c := range []struct {
		name                   string
		queueDelay, entryDelay time.Duration
		waitMs, grantMs        int64
	}{
		{name: "four-held-slots", queueDelay: 21 * time.Second, waitMs: 21000, grantMs: 19000},
		{name: "immediate-grant-delayed-caller", entryDelay: 21 * time.Second, waitMs: 0, grantMs: 40000},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				signal, calls := pgRunSlotTimingFixture(t)
				signal.entryDelay = c.entryDelay
				elapsed := pgRunSlotTimingTurn(t, t.Context(), newRunSlotPool(runLoopMaxConcurrentSignals), signal, c.queueDelay)
				r := pgRunSlotReadTiming(t, signal.env)
				o := r.TransportObservation
				if elapsed != 40*time.Second || *calls != 1 || r.Complete || r.Samples != 0 || r.Reason != "deadline" || r.SourceFailure == nil || r.SourceFailure.Phase != "host_transport" || r.SourceFailure.Cause != observationErrorClassTimeout || !o.OwnerDeadlineExceeded || o.RemainingBudgetMs != 19000 {
					t.Fatal("fixture lost the real inherited owner deadline and incomplete receipt")
				}
				if o.RunSlotWaitMs == nil || o.RunSlotGrantedRemainingBudgetMs == nil {
					t.Fatal("equal PG entry budgets lost actual request-to-grant timing authority")
				}
				if *o.RunSlotWaitMs != c.waitMs || *o.RunSlotGrantedRemainingBudgetMs != c.grantMs {
					t.Fatal("post-grant delay was attributed to the global slot request")
				}
			})
		})
	}
}

// A healthy immediate grant and an earlier inherited owner retain their exact
// budgets. No observation may assume every caller received a fresh forty seconds.
func TestPgRunSlotTimingHealthyAndEarlierOwner(t *testing.T) {
	for _, c := range []struct {
		name                     string
		parentBudget, queueDelay time.Duration
		complete                 bool
		elapsed                  time.Duration
		waitMs, grantMs          int64
	}{
		{name: "healthy", complete: true, elapsed: 22 * time.Second, waitMs: 0, grantMs: 40000},
		{name: "earlier-owner", parentBudget: 30 * time.Second, queueDelay: 12 * time.Second, elapsed: 30 * time.Second, waitMs: 12000, grantMs: 18000},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if c.parentBudget != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, c.parentBudget)
					defer cancel()
				}
				signal, calls := pgRunSlotTimingFixture(t)
				elapsed := pgRunSlotTimingTurn(t, ctx, newRunSlotPool(runLoopMaxConcurrentSignals), signal, c.queueDelay)
				r := pgRunSlotReadTiming(t, signal.env)
				o := r.TransportObservation
				if elapsed != c.elapsed || *calls != 1 || r.Complete != c.complete || (c.complete && r.Samples != 12) || o.OwnerDeadlineExceeded == c.complete || o.RemainingBudgetMs != c.grantMs {
					t.Fatal("source completion or earlier owner changed")
				}
				if o.RunSlotWaitMs == nil || o.RunSlotGrantedRemainingBudgetMs == nil || *o.RunSlotWaitMs != c.waitMs || *o.RunSlotGrantedRemainingBudgetMs != c.grantMs {
					t.Fatal("actual grant lost its inherited budget or measured wait")
				}
			})
		})
	}
}

// A direct probe has no run-slot owner. Missing evidence stays unknown, even
// when its entry budget happens to equal that of an immediate scheduled grant.
func TestPgRunSlotTimingDirectProbeKeepsGrantUnknown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		signal, calls := pgRunSlotTimingFixture(t)
		if _, err := signal.Run(t.Context(), syntheticSettings(nil)); err != nil {
			t.Fatal(err)
		}
		r := pgRunSlotReadTiming(t, signal.env)
		o := r.TransportObservation
		if !r.Complete || r.Samples != 12 || *calls != 1 || o.RunSlotWaitMs != nil || o.RunSlotGrantedRemainingBudgetMs != nil {
			t.Fatal("direct probe invented a global slot grant")
		}
	})
}

// Cadence reuses the signal and pool, but each request owns fresh timing facts.
func TestPgRunSlotTimingDoesNotReusePreviousGrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		signal, calls := pgRunSlotTimingFixture(t)
		slots := newRunSlotPool(runLoopMaxConcurrentSignals)
		pgRunSlotTimingTurn(t, t.Context(), slots, signal, 0)
		first := pgRunSlotReadTiming(t, signal.env)
		if !first.Complete {
			t.Fatal("first ordinary turn did not complete")
		}
		time.Sleep(pgQuerySampleCadence)
		pgRunSlotTimingTurn(t, t.Context(), slots, signal, 21*time.Second)
		second := pgRunSlotReadTiming(t, signal.env)
		o := second.TransportObservation
		if *calls != 2 || second.Complete || slots.active != 0 || len(slots.waiting) != 0 {
			t.Fatal("recurrence changed source attempts or retained another owner's slot")
		}
		if o.RunSlotWaitMs == nil || o.RunSlotGrantedRemainingBudgetMs == nil || *o.RunSlotWaitMs != 21000 || *o.RunSlotGrantedRemainingBudgetMs != 19000 {
			t.Fatal("recurring PG request lost or reused its grant evidence")
		}
	})
}
