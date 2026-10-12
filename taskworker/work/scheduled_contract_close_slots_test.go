// A process runs at most the configured number of queued closes of open
// contracts at once; a close without a slot waits, then defers.
package work

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestScheduledCloseParallelLimitOnlyLowersTheCompiledLimit(t *testing.T) {
	if limit := scheduledCloseParallelLimit(); limit != scheduledCloseParallel {
		t.Fatal("limit without config", limit)
	}
	cases := []struct {
		config string
		want   int
	}{
		{config: "scheduled_close_parallel: 1\n", want: 1},
		{config: "scheduled_close_parallel: 3\nsteal_concurrency: 1\n", want: 3},
		{config: "scheduled_close_parallel: 0\n", want: scheduledCloseParallel},
		{config: "scheduled_close_parallel: 5\n", want: scheduledCloseParallel},
		{config: "scheduled_close_parallel: two\n", want: scheduledCloseParallel},
		{config: "steal_concurrency: 1\n", want: scheduledCloseParallel},
	}
	for _, c := range cases {
		func() {
			defer server.Config.PushSimpleResource(contractCloseConcurrencyResourceName, []byte(c.config))()
			if limit := scheduledCloseParallelLimit(); limit != c.want {
				t.Errorf("config %q: limit = %d, want %d", c.config, limit, c.want)
			}
		}()
	}
}

// Signals each wait on a full limiter for the duration of a test.
func observeScheduledCloseSlotWaits(t testing.TB) <-chan struct{} {
	waits := make(chan struct{}, 64)
	scheduledCloseSlotWaiting = func() { waits <- struct{}{} }
	t.Cleanup(func() { scheduledCloseSlotWaiting = nil })
	return waits
}

// A waiter takes the slot its holder releases; a lowered limit holds it until
// enough slots free; a timeout or an ended context refuses it; a release is
// idempotent.
func TestScheduledCloseSlotsAdmitAtMostTheLimit(t *testing.T) {
	waits := observeScheduledCloseSlotWaits(t)
	slots := newScheduledCloseSlotLimiter()
	var limit atomic.Int64
	limit.Store(2)
	limitFunc := func() int { return int(limit.Load()) }
	never := make(chan time.Time)

	first, ok := slots.acquire(context.Background(), limitFunc, never)
	second, ok2 := slots.acquire(context.Background(), limitFunc, never)
	if !ok || !ok2 {
		t.Fatal("slots below the limit were refused")
	}
	limit.Store(1)
	admitted := make(chan func())
	go func() {
		release, ok := slots.acquire(context.Background(), limitFunc, never)
		if !ok {
			release = nil
		}
		admitted <- release
	}()
	<-waits
	first()
	first()
	// One slot is still held, at the lowered limit of one.
	<-waits
	second()
	third := <-admitted
	if third == nil {
		t.Fatal("the waiter was refused after both slots freed")
	}

	timeout := make(chan time.Time)
	refused := make(chan bool)
	go func() {
		_, ok := slots.acquire(context.Background(), limitFunc, timeout)
		refused <- !ok
	}()
	<-waits
	close(timeout)
	if !<-refused {
		t.Fatal("a timed-out waiter took a slot")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := slots.acquire(ctx, limitFunc, never); ok {
		t.Fatal("an ended context took a slot")
	}
	<-waits
	third()
	if active := slots.active.Value(); active != 0 {
		t.Fatal("slots leaked", active)
	}
}

// Two payers' queued closes are claimed together under a limit of one. The
// close that takes the slot holds it inside its owner batch; the other waits
// without admitting its owner, then closes once the slot frees. Every claim
// ran its batch at once before, whatever the closer's other limits.
func TestScheduledCloseOwnerBatchesWaitForAProcessSlot(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer server.Config.PushSimpleResource(contractCloseConcurrencyResourceName, []byte("scheduled_close_parallel: 1\n"))()
		waits := observeScheduledCloseSlotWaits(t)
		// The waiter's wait never ends on its own.
		slotTimeout := scheduledCloseSlotTimeout
		scheduledCloseSlotTimeout = func() <-chan time.Time { return nil }
		defer func() { scheduledCloseSlotTimeout = slotTimeout }()
		f := newScheduledCloseOwnerFixture(t.Context())
		payers := []*scheduledCloseOwnerPayer{
			f.newPayer(t, t.Context(), "synthetic-slot-payer-0"),
			f.newPayer(t, t.Context(), "synthetic-slot-payer-1"),
		}
		owners := map[server.PgOwnershipKey]int{}
		for index, payer := range payers {
			f.addLegacyContracts(t.Context(), payer, 1)
			owners[server.NewPgOwnershipKey("transfer_balance", payer.balanceId)] = index
		}
		held, release, overlapped := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var stateLock sync.Mutex
		admissions := []int{}
		released := false
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), func(event server.PgOwnershipEvent) {
			if event.Kind != server.PgOwnershipAdmitted || event.TransactionScoped {
				return
			}
			for _, key := range event.Keys {
				index, found := owners[key]
				if !found {
					continue
				}
				first, overlap := false, false
				func() {
					stateLock.Lock()
					defer stateLock.Unlock()
					first = len(admissions) == 0
					overlap = len(admissions) == 1 && !released
					admissions = append(admissions, index)
				}()
				if first {
					close(held)
					<-release
				}
				if overlap {
					close(overlapped)
				}
				return
			}
		})
		publishScheduledCloses(t, ctx)
		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		done := make(chan struct{})
		var finished, retried, posts []server.Id
		var err error
		go func() {
			defer close(done)
			finished, retried, posts, err = worker.EvalTasks(2)
		}()
		<-held
		select {
		case <-waits:
		case <-overlapped:
			t.Error("a second owner batch started while the only slot was held")
		}
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			released = true
		}()
		close(release)
		<-done
		if err != nil || len(finished) != 2 || len(retried)+len(posts) != 0 {
			t.Fatal("slot-bound closes did not finish", finished, retried, posts, err)
		}
		for _, payer := range payers {
			requireScheduledCloseOwnerSettled(t, ctx, payer, 1)
		}
		if active := scheduledCloseSlots.active.Value(); active != 0 {
			t.Fatal("a close kept its slot", active)
		}
	})
}

// A close that finds no slot before its wait ends defers like a held owner:
// its owner is never admitted and its deadline is unchanged.
func TestScheduledCloseWithoutASlotDefers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer server.Config.PushSimpleResource(contractCloseConcurrencyResourceName, []byte("scheduled_close_parallel: 1\n"))()
		waits := observeScheduledCloseSlotWaits(t)
		timeout := make(chan time.Time)
		slotTimeout := scheduledCloseSlotTimeout
		scheduledCloseSlotTimeout = func() <-chan time.Time { return timeout }
		defer func() { scheduledCloseSlotTimeout = slotTimeout }()
		f := newScheduledCloseOwnerFixture(t.Context())
		payer := f.newPayer(t, t.Context(), "synthetic-no-slot-payer")
		f.addLegacyContracts(t.Context(), payer, 1)
		events := newScheduledCloseOwnerEvents(payer)
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), events.observe)
		deadline := publishScheduledCloses(t, ctx)
		// Another close holds the only slot.
		holder, ok := scheduledCloseSlots.acquire(t.Context(), scheduledCloseParallelLimit, make(chan time.Time))
		if !ok {
			t.Fatal("the holder found no slot")
		}
		defer holder()
		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		done := make(chan struct{})
		var finished, retried, posts []server.Id
		var err error
		go func() {
			defer close(done)
			finished, retried, posts, err = worker.EvalTasks(1)
		}()
		<-waits
		close(timeout)
		<-done
		if err != nil || len(finished) != 1 || len(retried)+len(posts) != 0 {
			t.Fatal("a close without a slot failed instead of deferring", finished, retried, posts, err)
		}
		if admitted, waiting, refused := events.counts(payer.balanceId); admitted != 0 || waiting != 0 || refused != 0 {
			t.Fatal("a close without a slot reached its owner", admitted, waiting, refused)
		}
		requireScheduledCloseOwnerSettled(t, ctx, payer, 0)
		result := readScheduledCloseResults(t, ctx)
		if len(result) != 1 || result[0].DeferredUntil == nil || result[0].RetryAt == nil || !result[0].RetryAt.Equal(deadline) {
			t.Fatal("a close without a slot did not defer with its deadline", result)
		}
		if queued := readQueuedScheduledCloseContracts(t, ctx); len(queued) != 1 {
			t.Fatal("the deferred close lost its successor", len(queued))
		}
	})
}
