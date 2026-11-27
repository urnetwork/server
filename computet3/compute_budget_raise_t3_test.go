//go:build sidecar_pending

package computet3

// Kind 21's raise at the broker on the private stack's PostgreSQL and Redis, real Lua (BAL-M12-16; DESIGN-COMPUTE
// §9.7, §14.3, §16.3, §16.4): a raise approved once and retried adds nothing, its spend check run once; a second action
// under a used approval is spent; the raise lifts only its subject's cap, a hold past the default admitted for the
// raised agent and refused for another, and stands through a lost projection's rebuild; a raise of period run is kept
// for its use and lifts nothing at the broker; a raise past its expiry is pruned. The spend check is the route's
// (computeservice's ledger_budget_raise_test.go holds its refusals); here it is a stand-in that counts its calls.

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// A spend check that spends uses until notAfter, counting each time it is asked.
type computeT3RaiseSpend struct {
	uses     uint32
	notAfter time.Time
	asked    int
}

func (self *computeT3RaiseSpend) spend(now time.Time) (*controller.ComputeBudgetRaiseSpend, error) {
	self.asked += 1
	return &controller.ComputeBudgetRaiseSpend{Uses: self.uses, NotAfterMillis: self.notAfter.UnixMilli()}, nil
}

// A raise of subjectId's period to amount under approvalId with effectKey.
func (self *computeT3Ledger) raise(approvalId server.Id, effectKey server.Id, subjectId *server.Id, period model.ComputeBudgetPeriod, amount int64, spend *computeT3RaiseSpend) (*controller.ComputeBudgetRaiseAnswer, error) {
	return controller.ComputeBudgetRaiseTake(self.t3.env, self.t3.ctx, self.settings, &controller.ComputeBudgetRaiseRequest{
		NetworkId: self.networkId, ApprovalId: approvalId, EffectKey: effectKey, SubjectId: subjectId, Period: period,
		AmountNanoUsd: amount, Spend: spend.spend,
	})
}

// The raised cap subjectId's counters hold for today, 0 for none.
func (self *computeT3Ledger) raisedCap(subjectId server.Id) int64 {
	self.t3.t.Helper()
	field, err := model.ComputeBudgetRaiseField(model.ComputeBudgetPeriodDay, self.redisNow())
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return self.field(model.ComputeBudgetKey(self.networkId, subjectId), field)
}

// A raise approved once and retried adds nothing: the retry answers the first's row, its spend check never asked, one
// row standing; the subject's raised cap is the raise's, and a raise's script run again does not lower it.
func TestBudgetRaiseApprovedOnceRetryAddsNothing(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.credit(model.ComputeLotKindPurchase, computeT3Usd(100), time.Now().Add(-time.Hour), nil)
		ledger.rebuildFromLedger()
		agentId := ledger.agentId
		approvalId, effectKey := server.NewId(), server.NewId()
		spend := &computeT3RaiseSpend{uses: 1, notAfter: time.Now().Add(time.Hour)}
		first, err := ledger.raise(approvalId, effectKey, &agentId, model.ComputeBudgetPeriodDay, computeT3Usd(10), spend)
		if err != nil || first.Replayed || !first.AtBroker || first.Raise.ApprovalUse != 0 {
			t.Fatalf("the first: %+v %v", first, err)
		}
		again, err := ledger.raise(approvalId, effectKey, &agentId, model.ComputeBudgetPeriodDay, computeT3Usd(10), spend)
		if err != nil || !again.Replayed || again.Raise.ApprovalUse != first.Raise.ApprovalUse || !again.Raise.ExpireTime.Equal(first.Raise.ExpireTime) {
			t.Fatalf("the retry: %+v %v", again, err)
		}
		if spend.asked != 1 {
			t.Errorf("the spend check was asked %d times", spend.asked)
		}
		if rows := t3.queryInt(`SELECT count(*) FROM compute_budget_raise WHERE approval_id = $1`, approvalId); rows != 1 {
			t.Errorf("%d rows", rows)
		}
		if raised := ledger.raisedCap(agentId); raised != computeT3Usd(10) {
			t.Errorf("the raised cap %d", raised)
		}
		// the day's raise ends at the next UTC midnight, the decision's hour being earlier
		if want := model.ComputeUtcDay(first.Raise.CreateTime).AddDate(0, 0, 1); !first.Raise.ExpireTime.Equal(want) && !first.Raise.ExpireTime.Equal(spend.notAfter.UTC().Truncate(time.Millisecond)) {
			t.Errorf("the expiry %v", first.Raise.ExpireTime)
		}
	})
}

// A second action under an approval of one use finds it spent and writes nothing; a raise of another approval is its
// own.
func TestBudgetRaiseSpentUseRefused(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.credit(model.ComputeLotKindPurchase, computeT3Usd(100), time.Now().Add(-time.Hour), nil)
		ledger.rebuildFromLedger()
		agentId := ledger.agentId
		approvalId := server.NewId()
		spend := &computeT3RaiseSpend{uses: 1, notAfter: time.Now().Add(time.Hour)}
		if _, err := ledger.raise(approvalId, server.NewId(), &agentId, model.ComputeBudgetPeriodDay, computeT3Usd(10), spend); err != nil {
			t.Fatal(err)
		}
		_, err := ledger.raise(approvalId, server.NewId(), &agentId, model.ComputeBudgetPeriodMonth, computeT3Usd(90), spend)
		if !errors.Is(err, model.ErrComputeBudgetRaiseSpent) {
			t.Fatalf("a second action under a used approval: %v", err)
		}
		if rows := t3.queryInt(`SELECT count(*) FROM compute_budget_raise WHERE approval_id = $1`, approvalId); rows != 1 {
			t.Errorf("%d rows", rows)
		}
		if answer, err := ledger.raise(server.NewId(), server.NewId(), &agentId, model.ComputeBudgetPeriodMonth, computeT3Usd(90), spend); err != nil || answer.Replayed {
			t.Errorf("another approval: %+v %v", answer, err)
		}
	})
}

// A raise lifts only its subject's cap: with $4 of the agent's $5 day charged, a $2 call is refused budget; raised to
// $10 the agent's call is admitted while another agent's $5.50 call on the same device stays refused at its $5. The
// raise stands through a lost projection's rebuild.
func TestBudgetRaiseLiftsOnlyItsSubjectsCap(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.credit(model.ComputeLotKindPurchase, computeT3Usd(100), time.Now().Add(-time.Hour), nil)
		ledger.rebuildFromLedger()
		ledger.call(computeT3Usd(4), computeT3Usd(4))
		if refused := ledger.holdCall(ledger.teeRequest(computeT3Usd(2))); refused.Admitted || refused.Refusal.Reason != controller.ComputeRefusalBudget {
			t.Fatalf("past the agent's day: %+v", refused)
		}
		agentId := ledger.agentId
		spend := &computeT3RaiseSpend{uses: 1, notAfter: time.Now().Add(time.Hour)}
		if _, err := ledger.raise(server.NewId(), server.NewId(), &agentId, model.ComputeBudgetPeriodDay, computeT3Usd(10), spend); err != nil {
			t.Fatal(err)
		}
		admitted := ledger.admit(computeT3Usd(2))
		ledger.release(admitted.OpKey)
		// another agent of the same device keeps its default
		raisedAgent := ledger.agentId
		ledger.agentId = server.NewId()
		if refused := ledger.holdCall(ledger.teeRequest(computeT3Usd(5.5))); refused.Admitted || refused.Refusal.Reason != controller.ComputeRefusalBudget {
			t.Errorf("another agent past its default: %+v", refused)
		}
		ledger.agentId = raisedAgent
		// a lost cache comes back with the raise
		ledger.dropCache()
		ledger.rebuildFromLedger()
		if raised := ledger.raisedCap(raisedAgent); raised != computeT3Usd(10) {
			t.Errorf("after the rebuild the raised cap is %d", raised)
		}
		if after := ledger.holdCall(ledger.teeRequest(computeT3Usd(2))); !after.Admitted {
			t.Errorf("after the rebuild the raised agent's call: %+v", after.Refusal)
		}
	})
}

// A raise of period run is kept for its use and lifts nothing at the broker, which counts days and months: the agent's
// day stays at its default. So does a gateway grant's, which names no agent; and a raise past its expiry is pruned.
func TestRunRaiseActsOnlyOnTheDevice(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.credit(model.ComputeLotKindPurchase, computeT3Usd(100), time.Now().Add(-time.Hour), nil)
		ledger.rebuildFromLedger()
		ledger.call(computeT3Usd(4), computeT3Usd(4))
		agentId := ledger.agentId
		spend := &computeT3RaiseSpend{uses: 1, notAfter: time.Now().Add(time.Hour)}
		run, err := ledger.raise(server.NewId(), server.NewId(), &agentId, model.ComputeBudgetPeriodRun, computeT3Usd(20), spend)
		if err != nil || run.AtBroker || run.Raise.Period != model.ComputeBudgetPeriodRun {
			t.Fatalf("the run's raise: %+v %v", run, err)
		}
		grant, err := ledger.raise(server.NewId(), server.NewId(), nil, model.ComputeBudgetPeriodDay, computeT3Usd(20), spend)
		if err != nil || grant.AtBroker || grant.Raise.SubjectId != (server.Id{}) {
			t.Fatalf("the grant's raise: %+v %v", grant, err)
		}
		if raised := ledger.raisedCap(agentId); raised != 0 {
			t.Errorf("a device-only raise wrote a cap of %d", raised)
		}
		if refused := ledger.holdCall(ledger.teeRequest(computeT3Usd(2))); refused.Admitted || refused.Refusal.Reason != controller.ComputeRefusalBudget {
			t.Errorf("the agent's day after a run's raise: %+v", refused)
		}
		// the rows go once past their expiry: the run's at its decision's hour, the grant's day at the next UTC midnight
		if pruned, err := controller.ComputeBudgetRaisePrune(t3.env, t3.ctx, time.Now()); err != nil || pruned != 0 {
			t.Errorf("before any expiry, pruned %d, %v", pruned, err)
		}
		pruned, err := controller.ComputeBudgetRaisePrune(t3.env, t3.ctx, time.Now().Add(26*time.Hour))
		if err != nil || pruned != 2 {
			t.Errorf("pruned %d, %v", pruned, err)
		}
	})
}
