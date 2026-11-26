//go:build sidecar_pending && sidecar_testhooks

package computet3

// Settlement stopped at every step, and a rebuild racing a batch (BAL-M12-04; R2-07's acceptance, DESIGN-COMPUTE §9.7,
// D-BAL-13), on the private stack's PostgreSQL and Redis through the ledger's barriers. A stop is the stopped
// goroutine's end at its barrier, runtime.Goexit, as a crashed process ends: what it held open rolls back with its
// connection, and nothing after the barrier runs.

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// Ends the first goroutine to reach the barrier named stop, once; the answer removes the hook and says whether it
// stopped one.
func computeT3StopAt(stop string) func() bool {
	var stopped atomic.Bool
	sidecar.SetTestHooks(&sidecar.TestHooks{Barrier: func(name string, key string) {
		if name == stop && stopped.CompareAndSwap(false, true) {
			runtime.Goexit()
		}
	}})
	return func() bool {
		sidecar.SetTestHooks(nil)
		return stopped.Load()
	}
}

// Runs step on a goroutine of its own and waits for it to end, by return or by a stop.
func computeT3RunStopped(step func()) {
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		step()
	}()
	group.Wait()
}

// The settlement's state the test compares with an uninterrupted run's.
type computeT3Settled struct {
	settled, remaining, drawn, dayAmount, dayCount, totalAmount int64
	projectionSettled, unsettled, holds, charged, reserved      int64
	rows                                                        int64
}

func (self *computeT3Ledger) settledState(lotId server.Id, opKey server.Id) *computeT3Settled {
	self.t3.t.Helper()
	t3 := self.t3
	now := time.Now()
	projection := self.read()
	return &computeT3Settled{
		settled:           self.account().SettledNanoUsd,
		remaining:         t3.queryInt(`SELECT remaining_nano_usd FROM compute_lot WHERE lot_id = $1`, lotId),
		drawn:             t3.queryInt(`SELECT coalesce(sum(amount_nano_usd), 0)::bigint FROM compute_charge_lot WHERE charge_id = $1`, opKey),
		dayAmount:         t3.queryInt(`SELECT coalesce(sum(amount_nano_usd), 0)::bigint FROM compute_charge_day WHERE network_id = $1`, self.networkId),
		dayCount:          t3.queryInt(`SELECT coalesce(sum(count), 0)::bigint FROM compute_charge_day WHERE network_id = $1`, self.networkId),
		totalAmount:       t3.queryInt(`SELECT coalesce(sum(amount_nano_usd), 0)::bigint FROM compute_charge_total WHERE network_id = $1`, self.networkId),
		projectionSettled: projection.SettledNanoUsd,
		unsettled:         projection.UnsettledNanoUsd,
		holds:             projection.HoldsNanoUsd,
		charged:           self.counter(self.agentId, "c", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now),
		reserved:          self.counter(self.agentId, "r", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now),
		rows:              t3.queryInt(`SELECT count(*) FROM compute_op WHERE network_id = $1`, self.networkId),
	}
}

// R2-07's acceptance: the producer, the broker and the worker each stopped on both sides of the outcome's commit, the
// hold's turn into an unsettled charge, compute_settle's commit and its drop. At each stop the liability is still
// counted, so a concurrent admission for more than what is left is refused. Then the cache is dropped, the worker
// restarted, the receipt delivered again, twice: the balance, the lot's draws, the budget counters and the day and
// lifetime totals equal an uninterrupted run's, the duplicate adding nothing.
func TestComputeSettlementSurvivesEveryStop(t *testing.T) {
	hold, charge := computeT3Usd(2.58), computeT3Usd(1)
	var want *computeT3Settled
	stops := []string{
		"", // the uninterrupted run, which the others are held to
		"compute_ledger.outcome.before", "compute_ledger.outcome.after",
		"compute_ledger.settle.before", "compute_ledger.settle.after",
		"compute_book.commit.before", "compute_book.commit.after",
		"compute_book.drop.before", "compute_book.drop.after",
	}
	for _, stop := range stops {
		runComputeT3(t, func(t3 *computeT3) {
			t := t3.t
			ledger := newComputeT3Ledger(t3)
			lotId := ledger.credit(model.ComputeLotKindPurchase, computeT3Usd(10), time.Now().Add(-time.Hour), nil)
			ledger.rebuildFromLedger()
			opKey := ledger.admit(hold).OpKey
			settle := func() *controller.ComputeSettleAnswer {
				answer, err := controller.ComputeSettle(t3.env, t3.ctx, ledger.settings, &controller.ComputeSettleRequest{
					NetworkId: ledger.networkId, OpKey: opKey, Path: model.ComputeHoldPathTee, Outcome: computeT3Outcome(charge), Budgets: ledger.budgets(),
				})
				if err != nil {
					t.Error(err)
				}
				return answer
			}
			book := func() {
				if _, err := controller.ComputeBook(t3.env, t3.ctx, ledger.settings, model.ComputeOpShard(ledger.settings, ledger.networkId)); err != nil {
					t.Error(err)
				}
			}
			restore := computeT3StopAt(stop)
			computeT3RunStopped(func() {
				settle()
				book()
			})
			if stopped := restore(); stopped != (stop != "") {
				t.Fatalf("stopping at %q stopped %v", stop, stopped)
			}

			// the liability stands: no admission takes more than what is left after the charge, the balance alone
			// deciding, with no budget line
			request := ledger.teeRequest(computeT3Usd(10) - charge + computeT3Usd(0.01))
			request.Lines = nil
			request.Budgets = ledger.budgets()
			if answer := ledger.holdCall(request); answer.Admitted {
				t.Errorf("stopped at %q, an admission took the room of a pending liability", stop)
				ledger.release(answer.OpKey)
			}

			// the cache dropped, the worker restarted, the receipt delivered again, then once more
			ledger.dropCache()
			settle()
			book()
			ledger.rebuildFromLedger()
			book()
			if duplicate := settle(); duplicate.State == controller.ComputeSettleSettled {
				t.Errorf("stopped at %q, a duplicate receipt settled again", stop)
			}
			book()
			got := ledger.settledState(lotId, opKey)
			if stop == "" {
				want = got
				if want.settled != computeT3Usd(9) || want.remaining != computeT3Usd(9) || want.drawn != charge || want.dayCount != 1 ||
					want.unsettled != 0 || want.holds != 0 || want.charged != charge || want.reserved != 0 || want.rows != 0 {
					t.Fatalf("the uninterrupted run: %+v", want)
				}
				return
			}
			if *got != *want {
				t.Errorf("stopped at %q: %+v, want %+v", stop, got, want)
			}
		})
	}
}

// A rebuild racing a batch counts it once, in either order: the booking stopped after its commit, before its drop,
// while a failover's rebuild reads the rows already applied; and a rebuild holding the fence from its snapshot to its
// script while a booking waits on it, the booking's drop then removing the charge the rebuild wrote.
func TestComputeRebuildRacingABatchCountsItOnce(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.credit(model.ComputeLotKindPurchase, computeT3Usd(10), time.Now().Add(-time.Hour), nil)
		ledger.rebuildFromLedger()
		ledger.call(computeT3Usd(2), computeT3Usd(1))

		// the booking commits and stops; the rebuild reads it booked
		shard := model.ComputeOpShard(ledger.settings, ledger.networkId)
		booked := make(chan struct{})
		drop := make(chan struct{})
		var once sync.Once
		sidecar.SetTestHooks(&sidecar.TestHooks{Barrier: func(name string, key string) {
			if name == "compute_book.commit.after" {
				once.Do(func() {
					close(booked)
					<-drop
				})
			}
		}})
		done := make(chan error, 1)
		go func() {
			_, err := controller.ComputeBook(t3.env, t3.ctx, ledger.settings, shard)
			done <- err
		}()
		<-booked
		if err := model.ComputeProjectionMarkRebuild(t3.env, t3.ctx, ledger.networkId); err != nil {
			t.Fatal(err)
		}
		if rebuilt, err := controller.ComputeRebuild(t3.env, t3.ctx, ledger.settings, ledger.networkId, ledger.budgets()); err != nil || !rebuilt {
			t.Fatalf("the rebuild: %v, %v", rebuilt, err)
		}
		if projection := ledger.read(); projection.UnsettledNanoUsd != 0 || projection.SettledNanoUsd != computeT3Usd(9) {
			t.Errorf("the rebuild of a booked batch: %+v", projection)
		}
		close(drop)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		sidecar.SetTestHooks(nil)
		if projection := ledger.read(); projection.UnsettledNanoUsd != 0 || projection.SettledNanoUsd != computeT3Usd(9) || projection.AvailableNanoUsd() != computeT3Usd(9) {
			t.Errorf("after the drop: %+v", projection)
		}

		// the rebuild holds the fence with the row settled in its snapshot; a booking waits on the fence, then drops
		ledger.call(computeT3Usd(2), computeT3Usd(1.5))
		snapshotTaken := make(chan struct{})
		write := make(chan struct{})
		var rebuildOnce sync.Once
		sidecar.SetTestHooks(&sidecar.TestHooks{Barrier: func(name string, key string) {
			if name == "compute_rebuild.script.before" {
				rebuildOnce.Do(func() {
					close(snapshotTaken)
					<-write
				})
			}
		}})
		defer sidecar.SetTestHooks(nil)
		if err := model.ComputeProjectionMarkRebuild(t3.env, t3.ctx, ledger.networkId); err != nil {
			t.Fatal(err)
		}
		rebuildDone := make(chan error, 1)
		go func() {
			_, err := controller.ComputeRebuild(t3.env, t3.ctx, ledger.settings, ledger.networkId, ledger.budgets())
			rebuildDone <- err
		}()
		<-snapshotTaken
		bookDone := make(chan error, 1)
		go func() {
			_, err := controller.ComputeBook(t3.env, t3.ctx, ledger.settings, shard)
			bookDone <- err
		}()
		// the booking cannot pass the fence until the rebuild commits, so whether it has reached it yet, it books after
		close(write)
		if err := <-rebuildDone; err != nil {
			t.Fatal(err)
		}
		if err := <-bookDone; err != nil {
			t.Fatal(err)
		}
		if projection := ledger.read(); projection.UnsettledNanoUsd != 0 || projection.SettledNanoUsd != computeT3Usd(7.5) || projection.AvailableNanoUsd() != computeT3Usd(7.5) {
			t.Errorf("after the rebuild and the booking: %+v", projection)
		}
		if account := ledger.account(); account.SettledNanoUsd != computeT3Usd(7.5) {
			t.Errorf("settled %d", account.SettledNanoUsd)
		}
	})
}
