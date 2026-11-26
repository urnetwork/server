//go:build sidecar_pending && sidecar_testhooks

package computet3

// An admission stopped between its hold and its row (BAL-M12-03; DESIGN-COMPUTE §9.7's handoff, D-BAL-13), on the
// private stack's Redis and PostgreSQL through the ledger's barriers: the stopped call's room stays held while it is
// fresh, is dropped by the network's next script once 60 s have passed, as planted, and the call, woken, forwards
// nothing and leaves its row void.

import (
	"sync/atomic"
	"testing"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"
)

// Moves an unconfirmed hold's time ARGV[2] ms before Redis TIME, as if that long had passed. KEYS: the unconfirmed set.
// ARGV: op, age ms.
var computeT3AgeScript = &operator.RedisScript{
	Name: "compute-t3-age",
	Source: model.ComputeHoldPrelude() + `
redis.call('ZADD', KEYS[1], cmp_str(cmp_now() - cmp_int(ARGV[2], 'the age')), ARGV[1])
return {'aged'}
`,
}

func TestComputeStopBetweenHoldAndRowStrandsNoRoom(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.rebuild(1, computeT3Usd(3), computeT3Usd(5), 0, model.ComputeAccountStateActive)

		// the first call to pass its hold is stopped there, before its row
		var stopped atomic.Bool
		stoppedKeys := make(chan string, 1)
		wake := make(chan struct{})
		sidecar.SetTestHooks(&sidecar.TestHooks{Barrier: func(name string, key string) {
			if name == "compute_ledger.hold.after" && stopped.CompareAndSwap(false, true) {
				stoppedKeys <- key
				<-wake
			}
		}})
		defer sidecar.SetTestHooks(nil)
		answers := make(chan *controller.ComputeHoldAnswer, 1)
		errs := make(chan error, 1)
		go func() {
			answer, err := controller.ComputeHold(t3.env, t3.ctx, ledger.settings, ledger.teeRequest(computeT3Usd(2.58)))
			errs <- err
			answers <- answer
		}()
		stoppedKey := <-stoppedKeys
		if members := ledger.unconfirmed(); len(members) != 1 {
			t.Fatalf("the stopped call's hold is not the one unconfirmed: %v", members)
		}

		// while fresh, its room is held: a second call is answered held, not balance
		if answer := ledger.holdCall(ledger.teeRequest(computeT3Usd(2.58))); answer.Admitted || answer.Refusal.Reason != controller.ComputeRefusalHeld {
			t.Fatalf("a second call beside the stopped one: %+v", answer)
		}
		// 61 s pass for the stopped hold, as planted, and the network's next script drops it: a third call takes its room
		if _, err := t3.redis.RunScript(t3.ctx, computeT3AgeScript, []string{model.ComputeProjectionKey(ledger.networkId) + ":u"}, []string{ledger.unconfirmed()[0], "61000"}); err != nil {
			t.Fatal(err)
		}
		third := ledger.admit(computeT3Usd(2.58))

		// the stopped call wakes: its confirm finds its hold dropped, so it forwards nothing and its row goes to void
		close(wake)
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		answer := <-answers
		if answer.Admitted || answer.Refusal == nil || answer.Refusal.Reason != controller.ComputeRefusalCapacity || answer.OpKey.String() != stoppedKey {
			t.Errorf("the woken call answered %+v", answer)
		}
		if op := ledger.op(answer.OpKey); op == nil || op.State != model.ComputeOpStateVoid {
			t.Errorf("the woken call's row: %+v", op)
		}
		projection := ledger.read()
		if projection.HoldsNanoUsd != computeT3Usd(2.58) || projection.AvailableNanoUsd() != computeT3Usd(0.42) {
			t.Errorf("holds %d, available %d: only the third call's room should be held", projection.HoldsNanoUsd, projection.AvailableNanoUsd())
		}
		if op := ledger.op(third.OpKey); op == nil || op.State != model.ComputeOpStateOpen {
			t.Errorf("the third call's row: %+v", op)
		}
	})
}
