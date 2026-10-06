package controller

// Virtual time pins the query budget and race boundary without depending on
// database timing or scheduler luck. The callback represents one transaction.

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestCompanionOriginWaitBoundsMissingLookupCount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		var attempts []time.Duration
		escrow, err := waitForCompanionOrigin(context.Background(), func() (*model.TransferEscrow, error) {
			attempts = append(attempts, time.Since(start))
			return nil, model.ErrMissingCompanionOrigin
		})
		if escrow != nil || !errors.Is(err, model.ErrMissingCompanionOrigin) {
			t.Fatalf("missing origin = (%v, %v)", escrow, err)
		}
		if len(attempts) > 9 {
			t.Fatalf("missing origin used %d lookups, want at most 9: %v", len(attempts), attempts)
		}
		if attempts[0] != 0 || attempts[1] != 100*time.Millisecond || attempts[len(attempts)-1] != 3*time.Second {
			t.Fatalf("immediate, first retry, or final deadline attempt changed: %v", attempts)
		}
		for i := 1; i < len(attempts); i++ {
			if delta := attempts[i] - attempts[i-1]; delta <= 0 || delta > 500*time.Millisecond {
				t.Fatalf("attempt interval %v exceeds the race-detection bound", delta)
			}
		}
	})
}

func TestCompanionOriginWaitFindsOriginAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		want := &model.TransferEscrow{}
		got, err := waitForCompanionOrigin(context.Background(), func() (*model.TransferEscrow, error) {
			if time.Since(start) < CompanionOriginWaitTimeout {
				return nil, model.ErrMissingCompanionOrigin
			}
			return want, nil
		})
		if err != nil || got != want || time.Since(start) != CompanionOriginWaitTimeout {
			t.Fatalf("deadline origin = (%v, %v) after %v", got, err, time.Since(start))
		}
	})
}

func TestCompanionOriginWaitPreservesImmediateAndTerminalResults(t *testing.T) {
	terminalErr := errors.New("synthetic terminal error")
	want := &model.TransferEscrow{}
	for _, test := range []struct {
		name   string
		escrow *model.TransferEscrow
		err    error
	}{
		{name: "existing origin", escrow: want},
		{name: "terminal failure", err: terminalErr},
	} {
		calls := 0
		got, err := waitForCompanionOrigin(context.Background(), func() (*model.TransferEscrow, error) {
			calls++
			return test.escrow, test.err
		})
		if got != test.escrow || err != test.err || calls != 1 {
			t.Fatalf("%s = (%v, %v), calls=%d", test.name, got, err, calls)
		}
	}
}

func TestCompanionOriginWaitCancellationDoesNotStartAnotherTransaction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		calls := 0
		result := make(chan error, 1)
		go func() {
			_, err := waitForCompanionOrigin(ctx, func() (*model.TransferEscrow, error) {
				calls++
				return nil, model.ErrMissingCompanionOrigin
			})
			result <- err
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("canceled wait err=%v, transactions=%d", err, calls)
		}
	})
}

func TestCompanionOriginWaitAlreadyCanceledDoesNotStartTransaction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	_, err := waitForCompanionOrigin(ctx, func() (*model.TransferEscrow, error) {
		calls++
		return nil, model.ErrMissingCompanionOrigin
	})
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("already canceled err=%v, transactions=%d", err, calls)
	}
}

func TestCompanionOriginWaitPreservesCommitDuringCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	want := &model.TransferEscrow{}
	calls := 0
	got, err := waitForCompanionOrigin(ctx, func() (*model.TransferEscrow, error) {
		calls++
		cancel()
		return want, nil
	})
	if err != nil || got != want || calls != 1 {
		t.Fatalf("committed transaction lost or retried: result=(%v, %v), calls=%d", got, err, calls)
	}
}

func TestCompanionOriginWaitSlowMissingQueryDoesNotExtendRetryWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		calls := 0
		_, err := waitForCompanionOrigin(context.Background(), func() (*model.TransferEscrow, error) {
			calls++
			time.Sleep(CompanionOriginWaitTimeout)
			return nil, model.ErrMissingCompanionOrigin
		})
		if !errors.Is(err, model.ErrMissingCompanionOrigin) || calls != 1 || time.Since(start) != CompanionOriginWaitTimeout {
			t.Fatalf("slow missing query err=%v, calls=%d, elapsed=%v", err, calls, time.Since(start))
		}
	})
}

func TestCompanionOriginWaitDoesNotLoseEventDuringLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		changes := connect.NewMonitorValue(uint64(0))
		updates := func() <-chan struct{} { _, update := changes.Get(); return update }
		calls := 0
		start := time.Now()
		want := &model.TransferEscrow{}
		got, err := waitForCompanionOrigin(context.Background(), func() (*model.TransferEscrow, error) {
			calls++
			if calls == 1 {
				// The read's snapshot misses an origin whose commit and event
				// arrive before the read returns. The armed event must survive.
				changes.Update(func(version uint64) uint64 { return version + 1 })
				return nil, model.ErrMissingCompanionOrigin
			}
			return want, nil
		}, updates)
		if got != want || err != nil || calls != 2 || time.Since(start) != 100*time.Millisecond {
			t.Fatalf("event during query lost: got=(%v, %v), calls=%d elapsed=%v", got, err, calls, time.Since(start))
		}
	})
}

func TestCompanionOriginWaitEventPreemptsSlowFallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		changes := connect.NewMonitorValue(uint64(0))
		updates := func() <-chan struct{} { _, update := changes.Get(); return update }
		start := time.Now()
		var ready atomic.Bool
		go func() {
			time.Sleep(250 * time.Millisecond)
			ready.Store(true)
			changes.Update(func(version uint64) uint64 { return version + 1 })
		}()
		calls := 0
		want := &model.TransferEscrow{}
		got, err := waitForCompanionOrigin(context.Background(), func() (*model.TransferEscrow, error) {
			calls++
			if ready.Load() {
				return want, nil
			}
			return nil, model.ErrMissingCompanionOrigin
		}, updates)
		if got != want || err != nil || calls != 3 || time.Since(start) != 250*time.Millisecond {
			t.Fatalf("event waited for fallback: got=(%v, %v), calls=%d elapsed=%v", got, err, calls, time.Since(start))
		}
	})
}

func TestCompanionOriginWaitMissingNotificationStillFindsOrigin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		want := &model.TransferEscrow{}
		got, err := waitForCompanionOrigin(context.Background(), func() (*model.TransferEscrow, error) {
			if time.Since(start) < 250*time.Millisecond {
				return nil, model.ErrMissingCompanionOrigin
			}
			return want, nil
		}, func() <-chan struct{} { return nil })
		if got != want || err != nil || time.Since(start) != 600*time.Millisecond {
			t.Fatalf("legacy or lost event not recovered: got=(%v, %v), elapsed=%v", got, err, time.Since(start))
		}
	})
}

func TestCompanionOriginWaitDuplicateEventsRemainRateLimited(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		changes := connect.NewMonitorValue(uint64(0))
		updates := func() <-chan struct{} { _, update := changes.Get(); return update }
		start := time.Now()
		calls := 0
		_, err := waitForCompanionOrigin(context.Background(), func() (*model.TransferEscrow, error) {
			calls++
			changes.Update(func(version uint64) uint64 { return version + 1 })
			return nil, model.ErrMissingCompanionOrigin
		}, updates)
		if !errors.Is(err, model.ErrMissingCompanionOrigin) || calls != 31 || time.Since(start) != 3*time.Second {
			t.Fatalf("duplicate events bypassed rate/deadline: err=%v calls=%d elapsed=%v", err, calls, time.Since(start))
		}
	})
}
