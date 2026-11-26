//go:build sidecar_pending

package computet3

// compute_op_recover on the private stack's PostgreSQL and Redis (BAL-M12-04; DESIGN-COMPUTE §9.7, §14.5): a row open a
// day released at no charge, its hold dropped and its provider counted for reconciliation; rows past the TEE call's 10
// minutes finished through their kind's source, settled where it finished, released where it was never served, left
// open where it cannot say; a row younger than that untouched; and void rows' holds dropped again before their rows
// are deleted.

import (
	"context"
	"sync"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// The test binary's inference source: an answer per operation key, wait for any other.
type computeT3RecoverSource struct {
	stateLock sync.Mutex
	answers   map[server.Id]*controller.ComputeRecoverAnswer
	asked     map[server.Id]int
}

var computeT3Source = &computeT3RecoverSource{answers: map[server.Id]*controller.ComputeRecoverAnswer{}, asked: map[server.Id]int{}}

func init() {
	controller.RegisterComputeRecoverSource(model.ComputeChargeKindInference, computeT3Source)
}

func (self *computeT3RecoverSource) Recover(env operator.Env, ctx context.Context, op *model.ComputeOp) (*controller.ComputeRecoverAnswer, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.asked[op.OpKey] += 1
	if answer, ok := self.answers[op.OpKey]; ok {
		return answer, nil
	}
	return &controller.ComputeRecoverAnswer{State: controller.ComputeRecoverWait}, nil
}

func (self *computeT3RecoverSource) answer(opKey server.Id, answer *controller.ComputeRecoverAnswer) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answers[opKey] = answer
}

func (self *computeT3RecoverSource) askedCount(opKey server.Id) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.asked[opKey]
}

// Moves an operation's start back by age, as if it had been open that long.
func (self *computeT3Ledger) age(opKey server.Id, age time.Duration) {
	self.t3.t.Helper()
	self.t3.mustExec(`UPDATE compute_op SET start_time = start_time - ($2::bigint * interval '1 millisecond') WHERE op_key = $1`, opKey, age.Milliseconds())
}

func (self *computeT3Ledger) recover() *controller.ComputeRecoverResult {
	self.t3.t.Helper()
	result, err := controller.ComputeRecover(self.t3.env, self.t3.ctx, self.settings)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return result
}

// A row open a day is released at no charge whatever its source would say, its hold dropped, its provider counted.
func TestComputeOpenRowReleasedAfterADay(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.rebuild(1, computeT3Usd(10), computeT3Usd(5), 0, model.ComputeAccountStateActive)
		stale := ledger.admit(computeT3Usd(2.58))
		ledger.age(stale.OpKey, 24*time.Hour+time.Minute)
		computeT3Source.answer(stale.OpKey, &controller.ComputeRecoverAnswer{State: controller.ComputeRecoverFinished, Path: model.ComputeHoldPathTee, Outcome: computeT3Outcome(computeT3Usd(1))})
		result := ledger.recover()
		if result.ExpiredProviderCounts[ledger.providerId] != 1 {
			t.Errorf("the run: %+v", result)
		}
		if op := ledger.op(stale.OpKey); op == nil || op.State != model.ComputeOpStateVoid || op.Outcome != nil {
			t.Errorf("the day-old row: %+v", op)
		}
		if projection := ledger.read(); projection.HoldsNanoUsd != 0 || projection.UnsettledNanoUsd != 0 {
			t.Errorf("holds %d, unsettled %d after the release", projection.HoldsNanoUsd, projection.UnsettledNanoUsd)
		}
		if computeT3Source.askedCount(stale.OpKey) != 0 {
			t.Error("the source was asked of a row past a day")
		}
	})
}

// Rows past the TEE call's 10 minutes go to their kind's source: settled where it finished, released where it was never
// served, left open where it cannot say; a younger row is not asked of.
func TestComputeRecoverFinishesRowsThroughTheirSource(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.rebuild(1, computeT3Usd(20), computeT3Usd(5), 0, model.ComputeAccountStateActive)
		// four calls of $1 within the agent's $5 day
		finished := ledger.admit(computeT3Usd(1))
		unknown := ledger.admit(computeT3Usd(1))
		waiting := ledger.admit(computeT3Usd(1))
		young := ledger.admit(computeT3Usd(1))
		for _, answer := range []*controller.ComputeHoldAnswer{finished, unknown, waiting} {
			ledger.age(answer.OpKey, 11*time.Minute)
		}
		computeT3Source.answer(finished.OpKey, &controller.ComputeRecoverAnswer{State: controller.ComputeRecoverFinished, Path: model.ComputeHoldPathTee, Outcome: computeT3Outcome(computeT3Usd(0.75))})
		computeT3Source.answer(unknown.OpKey, &controller.ComputeRecoverAnswer{State: controller.ComputeRecoverUnknown})
		result := ledger.recover()
		if result.SettledCount != 1 || result.ReleasedCount != 1 || result.WaitingCount != 1 {
			t.Errorf("the run: %+v", result)
		}
		if op := ledger.op(finished.OpKey); op.State != model.ComputeOpStateSettled || op.Outcome.AmountNanoUsd != computeT3Usd(0.75) {
			t.Errorf("the finished row: %+v", op)
		}
		if op := ledger.op(unknown.OpKey); op.State != model.ComputeOpStateVoid {
			t.Errorf("the unknown row: %+v", op)
		}
		if op := ledger.op(waiting.OpKey); op.State != model.ComputeOpStateOpen {
			t.Errorf("the waiting row: %+v", op)
		}
		if computeT3Source.askedCount(young.OpKey) != 0 {
			t.Error("a row younger than 10 minutes was asked of")
		}
		// the finished call's charge and the two open calls' holds stand; the released one's room is back
		if projection := ledger.read(); projection.UnsettledNanoUsd != computeT3Usd(0.75) || projection.HoldsNanoUsd != computeT3Usd(2) {
			t.Errorf("unsettled %d, holds %d", projection.UnsettledNanoUsd, projection.HoldsNanoUsd)
		}
	})
}

// A void row's hold is dropped again before the row is deleted, past the memo's window: a release whose script never
// reached Redis leaves no room held. A void row inside the window stays.
func TestComputeRecoverDropsVoidRowsHoldsBeforeDeleting(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.rebuild(1, computeT3Usd(10), computeT3Usd(5), 0, model.ComputeAccountStateActive)
		missed := ledger.admit(computeT3Usd(2))
		recent := ledger.admit(computeT3Usd(1))
		// the release moved the rows, and its script did not reach Redis: the holds stand
		if _, err := model.ComputeOpVoid(t3.env, t3.ctx, ledger.networkId, missed.OpKey); err != nil {
			t.Fatal(err)
		}
		if _, err := model.ComputeOpVoid(t3.env, t3.ctx, ledger.networkId, recent.OpKey); err != nil {
			t.Fatal(err)
		}
		ledger.age(missed.OpKey, ledger.settings.ProjectionMemoTtl+time.Minute)
		if projection := ledger.read(); projection.HoldsNanoUsd != computeT3Usd(3) {
			t.Fatalf("holds %d before the run", projection.HoldsNanoUsd)
		}
		result := ledger.recover()
		if result.VoidDeletedCount != 1 {
			t.Errorf("the run: %+v", result)
		}
		if ledger.op(missed.OpKey) != nil {
			t.Error("the old void row stands")
		}
		if op := ledger.op(recent.OpKey); op == nil || op.State != model.ComputeOpStateVoid {
			t.Errorf("the recent void row: %+v", op)
		}
		if projection := ledger.read(); projection.HoldsNanoUsd != computeT3Usd(1) {
			t.Errorf("holds %d after the run: only the recent void row's should stand until its turn", projection.HoldsNanoUsd)
		}
	})
}
