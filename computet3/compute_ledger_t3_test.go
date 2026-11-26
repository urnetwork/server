//go:build sidecar_pending

package computet3

// The ledger's calls on the private stack's PostgreSQL and Redis, running the real Lua (BAL-M12-03; DESIGN-COMPUTE §9.7,
// D-BAL-13, D-BAL-14): an admission's hold, row and confirm; a row PostgreSQL refuses voiding its hold and answering
// capacity; a settlement delivered twice charging once; a release after a settlement changing nothing; a budget
// refusal naming its line; a settled write claiming its key once; and no row, outcome or projection field holding a
// planted prompt. The admission stopped between its hold and its row is in compute_ledger_stop_t3_test.go, under
// sidecar_testhooks.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// One network's ledger under test: its projection's helpers and a sidecar with an agent.
type computeT3Ledger struct {
	*computeT3Network
	agentId    server.Id
	catalogId  string
	providerId int16
}

func newComputeT3Ledger(t3 *computeT3) *computeT3Ledger {
	return &computeT3Ledger{
		computeT3Network: newComputeT3Network(t3),
		agentId:          server.NewId(),
		catalogId:        "chutes-qwen3.8-27b-tee-high",
		providerId:       1,
	}
}

// §16.5's default lines for the ledger's agent on its sidecar.
func (self *computeT3Ledger) lines() []*model.ComputeBudgetLine {
	self.t3.t.Helper()
	agentId := self.agentId
	lines, err := model.ComputeDefaultBudgetLines(self.settings, &model.ComputeBudgetCaller{SidecarId: self.sidecarId, AgentId: &agentId})
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return lines
}

// A TEE call's request of hold, its own request id.
func (self *computeT3Ledger) teeRequest(hold int64) *controller.ComputeHoldRequest {
	agentId := self.agentId
	catalogId := self.catalogId
	version := int64(3)
	priceRead := int64(1764000000)
	providerRef := "instance-" + server.NewId().String()
	return &controller.ComputeHoldRequest{
		NetworkId: self.networkId, Kind: model.ComputeChargeKindInference, Path: model.ComputeHoldPathTee,
		SubjectRef: server.NewId(), SidecarId: self.sidecarId, AgentId: &agentId,
		CatalogId: &catalogId, CatalogVersion: &version, Provider: self.providerId, PriceRead: &priceRead, ProviderRef: &providerRef,
		HoldNanoUsd: hold, ReserveNanoUsd: hold, Lines: self.lines(),
	}
}

func (self *computeT3Ledger) holdCall(request *controller.ComputeHoldRequest) *controller.ComputeHoldAnswer {
	self.t3.t.Helper()
	answer, err := controller.ComputeHold(self.t3.env, self.t3.ctx, self.settings, request)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return answer
}

// An admitted TEE call of hold.
func (self *computeT3Ledger) admit(hold int64) *controller.ComputeHoldAnswer {
	self.t3.t.Helper()
	answer := self.holdCall(self.teeRequest(hold))
	if !answer.Admitted {
		self.t3.t.Fatalf("a call of %d was not admitted: %+v", hold, answer.Refusal)
	}
	return answer
}

// An outcome of amount, its usage tokens and times.
func computeT3Outcome(amount int64) *controller.ComputeOutcome {
	now := time.Now().UTC()
	return &controller.ComputeOutcome{
		Units:         model.ComputeUnits{InputTokens: 1200, OutputTokens: 300, CachedTokens: 40},
		AmountNanoUsd: amount,
		CostNanoUsd:   amount * 5 / 6,
		EndTime:       now,
		SettleTime:    now,
	}
}

func (self *computeT3Ledger) settleCall(opKey server.Id, amount int64) *controller.ComputeSettleAnswer {
	self.t3.t.Helper()
	answer, err := controller.ComputeSettle(self.t3.env, self.t3.ctx, self.settings, &controller.ComputeSettleRequest{
		NetworkId: self.networkId, OpKey: opKey, Path: model.ComputeHoldPathTee, Outcome: computeT3Outcome(amount),
	})
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return answer
}

func (self *computeT3Ledger) release(opKey server.Id) controller.ComputeReleaseState {
	self.t3.t.Helper()
	state, err := controller.ComputeRelease(self.t3.env, self.t3.ctx, self.settings, self.networkId, opKey)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return state
}

// The operation's row, nil where it is gone.
func (self *computeT3Ledger) op(opKey server.Id) *model.ComputeOp {
	self.t3.t.Helper()
	op, err := model.ComputeOpGet(self.t3.env, self.t3.ctx, self.networkId, opKey)
	if errors.Is(err, model.ErrComputeOpNotFound) {
		return nil
	}
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return op
}

// The unconfirmed set's members.
func (self *computeT3Ledger) unconfirmed() []string {
	self.t3.t.Helper()
	reply, err := self.t3.redis.RunScript(self.t3.ctx, computeT3UnconfirmedScript, []string{model.ComputeProjectionKey(self.networkId) + ":u"}, nil)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	members := []string{}
	for _, element := range reply.Replies {
		members = append(members, string(element.Bulk))
	}
	return members
}

// An admitted call has its row committed open, its start the hold's Redis TIME and its shard the network's; its hold
// confirmed, out of the unconfirmed set, its reservation moved into its counters; and the projection's holds carry it.
func TestComputeHoldAdmitsAndCommitsItsRow(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.rebuild(1, computeT3Usd(10), computeT3Usd(5), 0, model.ComputeAccountStateActive)
		before := ledger.redisNow()
		answer := ledger.admit(computeT3Usd(2.58))
		after := ledger.redisNow()
		op := ledger.op(answer.OpKey)
		if op == nil || op.State != model.ComputeOpStateOpen || op.HoldNanoUsd != computeT3Usd(2.58) || op.ReserveNanoUsd != computeT3Usd(2.58) {
			t.Fatalf("the row: %+v", op)
		}
		if op.StartTime.Before(before.Truncate(time.Millisecond)) || after.Before(op.StartTime) || !op.StartTime.Equal(answer.StartTime) {
			t.Errorf("the row starts at %v, the answer at %v, the hold between %v and %v", op.StartTime, answer.StartTime, before, after)
		}
		if op.Shard != model.ComputeOpShard(ledger.settings, ledger.networkId) || op.ProviderRef == nil || *op.CatalogId != ledger.catalogId {
			t.Errorf("the row's shard %d, provider ref %v, catalog %v", op.Shard, op.ProviderRef, op.CatalogId)
		}
		if members := ledger.unconfirmed(); len(members) != 0 {
			t.Errorf("unconfirmed after the confirm: %v", members)
		}
		if projection := ledger.read(); projection.HoldsNanoUsd != computeT3Usd(2.58) || projection.AvailableNanoUsd() != computeT3Usd(7.42) {
			t.Errorf("holds %d, available %d", projection.HoldsNanoUsd, projection.AvailableNanoUsd())
		}
		if reserved := ledger.counter(ledger.agentId, "r", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, op.StartTime); reserved != computeT3Usd(2.58) {
			t.Errorf("the agent's day reserves %d", reserved)
		}
	})
}

// An Env whose Db refuses every call, over the test's real Redis: PostgreSQL that cannot take a row.
type computeT3RefusingDbEnv struct {
	operator.Env
}

func (self *computeT3RefusingDbEnv) Db() (operator.Db, error) {
	return &computeT3RefusingDb{}, nil
}

type computeT3RefusingDb struct{}

var errComputeT3DbRefused = errors.New("the database refuses")

func (self *computeT3RefusingDb) Db(ctx context.Context, callback func(conn operator.DbConn) error, options ...operator.DbOption) error {
	return errComputeT3DbRefused
}

func (self *computeT3RefusingDb) Tx(ctx context.Context, callback func(tx operator.DbTx) error, options ...operator.DbOption) error {
	return errComputeT3DbRefused
}

// A row PostgreSQL refuses: the call answers capacity with a retry, its hold is voided at once in the real script, its
// reservation gone, nothing unconfirmed left, and the room it held is the next call's.
func TestComputeRowRefusedAnswersCapacity(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.rebuild(1, computeT3Usd(3), computeT3Usd(5), 0, model.ComputeAccountStateActive)
		refusing := &computeT3RefusingDbEnv{Env: t3.env}
		answer, err := controller.ComputeHold(refusing, t3.ctx, ledger.settings, ledger.teeRequest(computeT3Usd(2.58)))
		if err != nil {
			t.Fatal(err)
		}
		if answer.Admitted || answer.Refusal == nil || answer.Refusal.Reason != controller.ComputeRefusalCapacity || answer.Refusal.RetryAfter != ledger.settings.CapacityRetry {
			t.Fatalf("answered %+v", answer)
		}
		if projection := ledger.read(); projection.HoldsNanoUsd != 0 || projection.AvailableNanoUsd() != computeT3Usd(3) {
			t.Errorf("holds %d after the refusal", projection.HoldsNanoUsd)
		}
		if members := ledger.unconfirmed(); len(members) != 0 {
			t.Errorf("unconfirmed after the refusal: %v", members)
		}
		if ledger.op(answer.OpKey) != nil {
			t.Error("a row stands for the refused call")
		}
		if reserved := ledger.counter(ledger.agentId, "r", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, time.Now()); reserved != 0 {
			t.Errorf("the agent's day still reserves %d", reserved)
		}
		// the room is the next call's
		ledger.admit(computeT3Usd(2.58))
	})
}

// A settlement delivered twice charges once: the first commits its outcome and turns the hold into an unsettled
// charge; the second, a repeated receipt with another amount, meets the row settled and changes nothing, in the row,
// the projection or the budget counters.
func TestComputeSettleDeliveredTwiceChargesOnce(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.rebuild(1, computeT3Usd(10), computeT3Usd(5), 0, model.ComputeAccountStateActive)
		answer := ledger.admit(computeT3Usd(2.58))
		first := ledger.settleCall(answer.OpKey, computeT3Usd(0.40))
		if first.State != controller.ComputeSettleSettled || !first.Projected || first.Outcome.AmountNanoUsd != computeT3Usd(0.40) {
			t.Fatalf("the first settlement: %+v", first)
		}
		second := ledger.settleCall(answer.OpKey, computeT3Usd(0.90))
		if second.State != controller.ComputeSettleRepeat || second.Outcome.AmountNanoUsd != computeT3Usd(0.40) {
			t.Errorf("the second settlement: %+v", second)
		}
		op := ledger.op(answer.OpKey)
		if op.State != model.ComputeOpStateSettled || op.Outcome.AmountNanoUsd != computeT3Usd(0.40) || op.ProviderRef != nil {
			t.Errorf("the row after two deliveries: %+v, outcome %+v", op, op.Outcome)
		}
		projection := ledger.read()
		if projection.HoldsNanoUsd != 0 || projection.UnsettledNanoUsd != computeT3Usd(0.40) || projection.BalanceNanoUsd() != computeT3Usd(9.60) {
			t.Errorf("holds %d, unsettled %d, balance %d", projection.HoldsNanoUsd, projection.UnsettledNanoUsd, projection.BalanceNanoUsd())
		}
		charged := ledger.counter(ledger.agentId, "c", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, op.StartTime)
		reserved := ledger.counter(ledger.agentId, "r", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, op.StartTime)
		if charged != computeT3Usd(0.40) || reserved != 0 {
			t.Errorf("the agent's day charged %d and reserves %d", charged, reserved)
		}
	})
}

// A TEE call is held at its worst case, so a usage priced past its hold is charged the hold, the rest absorbed.
func TestComputeSettleNeverChargesPastTheHold(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.rebuild(1, computeT3Usd(10), computeT3Usd(5), 0, model.ComputeAccountStateActive)
		answer := ledger.admit(computeT3Usd(1))
		settled := ledger.settleCall(answer.OpKey, computeT3Usd(1.25))
		if settled.Outcome.AmountNanoUsd != computeT3Usd(1) || settled.Outcome.AbsorbedNanoUsd != computeT3Usd(0.25) {
			t.Errorf("a usage past the hold charged %d, absorbed %d", settled.Outcome.AmountNanoUsd, settled.Outcome.AbsorbedNanoUsd)
		}
		if projection := ledger.read(); projection.UnsettledNanoUsd != computeT3Usd(1) {
			t.Errorf("unsettled %d", projection.UnsettledNanoUsd)
		}
	})
}

// A release after a settlement changes nothing: the charge stands in the row and the projection. A release before it
// voids the row and the hold, and a settlement after that is answered released and charges nothing.
func TestComputeReleaseAfterSettleChangesNothing(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.rebuild(1, computeT3Usd(10), computeT3Usd(5), 0, model.ComputeAccountStateActive)
		settledCall := ledger.admit(computeT3Usd(2))
		ledger.settleCall(settledCall.OpKey, computeT3Usd(0.5))
		if state := ledger.release(settledCall.OpKey); state != controller.ComputeReleaseSettled {
			t.Errorf("a release after the settlement: %s", state)
		}
		if op := ledger.op(settledCall.OpKey); op.State != model.ComputeOpStateSettled {
			t.Errorf("the settled row is %s", op.State)
		}
		if projection := ledger.read(); projection.UnsettledNanoUsd != computeT3Usd(0.5) || projection.HoldsNanoUsd != 0 {
			t.Errorf("unsettled %d, holds %d after the release", projection.UnsettledNanoUsd, projection.HoldsNanoUsd)
		}

		releasedCall := ledger.admit(computeT3Usd(2))
		if state := ledger.release(releasedCall.OpKey); state != controller.ComputeReleaseReleased {
			t.Errorf("a release of an open call: %s", state)
		}
		if state := ledger.release(releasedCall.OpKey); state != controller.ComputeReleaseReleased {
			t.Errorf("a second release: %s", state)
		}
		if answer := ledger.settleCall(releasedCall.OpKey, computeT3Usd(0.7)); answer.State != controller.ComputeSettleReleased {
			t.Errorf("a settlement after the release: %+v", answer)
		}
		if op := ledger.op(releasedCall.OpKey); op.State != model.ComputeOpStateVoid || op.Outcome != nil {
			t.Errorf("the released row: %+v", op)
		}
		if projection := ledger.read(); projection.UnsettledNanoUsd != computeT3Usd(0.5) || projection.HoldsNanoUsd != 0 {
			t.Errorf("unsettled %d, holds %d after the second call", projection.UnsettledNanoUsd, projection.HoldsNanoUsd)
		}
		// a key never admitted settles and releases nothing
		if answer := ledger.settleCall(server.NewId(), computeT3Usd(1)); answer.State != controller.ComputeSettleGone {
			t.Errorf("a settlement of no row: %+v", answer)
		}
		if state := ledger.release(server.NewId()); state != controller.ComputeReleaseGone {
			t.Errorf("a release of no row: %s", state)
		}
	})
}

// A budget refusal names the line that refused: an agent at $4.90 of its $5 day refuses a 20-cent call on the agent's
// day line, whose budget and period the broker's reply names.
func TestComputeBudgetRefusalNamesItsLine(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		now := ledger.redisNow()
		ledger.rebuild(1, computeT3Usd(100), computeT3Usd(5), 0, model.ComputeAccountStateActive,
			&model.ComputeProjectionCounter{SubjectId: ledger.agentId, Budget: model.ComputeBudgetAgent, Period: model.ComputeBudgetPeriodDay, At: now, ChargedNanoUsd: computeT3Usd(4.90)})
		answer := ledger.holdCall(ledger.teeRequest(computeT3Usd(0.20)))
		if answer.Admitted || answer.Refusal == nil || answer.Refusal.Reason != controller.ComputeRefusalBudget {
			t.Fatalf("answered %+v", answer)
		}
		line := answer.Refusal.Line
		if line == nil || line.SubjectId != ledger.agentId || line.Budget != model.ComputeBudgetAgent || line.Period != model.ComputeBudgetPeriodDay {
			t.Errorf("the refusal names %+v", line)
		}
		reply := controller.ComputeRefusalAnswer(answer.Refusal)
		if reply.Code != "insufficient_quota" || reply.Reason != "budget" || reply.Budget != "agent" || reply.Period != "day" {
			t.Errorf("the broker replies %+v", reply)
		}
		if ledger.op(answer.OpKey) != nil {
			t.Error("a row stands for a refused call")
		}
		// ten cents fits
		ledger.admit(computeT3Usd(0.10))
	})
}

// A settled write's key claims its subject once: a refund written twice, the second a day later, is one row with the
// first write's start and amount; an egress hour is counted in the projection at once, as a settlement is.
func TestComputeWriteSettledClaimsItsKeyOnce(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.rebuild(1, computeT3Usd(10), computeT3Usd(5), 0, model.ComputeAccountStateActive)
		chargeId := server.NewId()
		first := time.Now().UTC().Truncate(time.Second)
		refund := func(at time.Time, amount int64) *controller.ComputeWriteAnswer {
			agentId := ledger.agentId
			answer, err := controller.ComputeWriteSettled(t3.env, t3.ctx, ledger.settings, &controller.ComputeSettledWrite{
				NetworkId: ledger.networkId, OpKey: controller.ComputeOpKeyForRefund(chargeId), Kind: model.ComputeChargeKindInference,
				SubjectRef: chargeId, SidecarId: ledger.sidecarId, AgentId: &agentId, Provider: ledger.providerId, StartTime: at,
				Outcome: &controller.ComputeOutcome{AmountNanoUsd: amount, EndTime: at, SettleTime: at},
			})
			if err != nil {
				t.Fatal(err)
			}
			return answer
		}
		written := refund(first, -computeT3Usd(0.40))
		again := refund(first.Add(24*time.Hour), -computeT3Usd(0.55))
		if !written.Written || again.Written || !again.StartTime.Equal(first) || again.Outcome.AmountNanoUsd != -computeT3Usd(0.40) {
			t.Errorf("the refund: %+v, its retry: %+v", written, again)
		}
		op := ledger.op(controller.ComputeOpKeyForRefund(chargeId))
		if op == nil || op.State != model.ComputeOpStateSettled || !op.StartTime.Equal(first) || op.SubjectRef != chargeId {
			t.Errorf("the refund's row: %+v", op)
		}
		// a refund reaches the projection through the booking's drop, never as an unsettled charge
		if projection := ledger.read(); projection.UnsettledNanoUsd != 0 {
			t.Errorf("unsettled %d after the refund", projection.UnsettledNanoUsd)
		}

		enclaveId := server.NewId()
		key, hourStart, err := controller.ComputeOpKeyForEgressHour(ledger.settings, enclaveId, 1, first)
		if err != nil {
			t.Fatal(err)
		}
		egress := func() *controller.ComputeWriteAnswer {
			answer, err := controller.ComputeWriteSettled(t3.env, t3.ctx, ledger.settings, &controller.ComputeSettledWrite{
				NetworkId: ledger.networkId, OpKey: key, Kind: model.ComputeChargeKindEnclaveHostEgress, SubjectRef: enclaveId,
				SidecarId: ledger.sidecarId, EnclaveId: &enclaveId, Provider: 4, StartTime: hourStart,
				Outcome: &controller.ComputeOutcome{Units: model.ComputeUnits{EgressBytes: 3 * 1024 * 1024 * 1024}, AmountNanoUsd: computeT3Usd(0.27), EndTime: first, SettleTime: first},
			})
			if err != nil {
				t.Fatal(err)
			}
			return answer
		}
		if !egress().Written || egress().Written {
			t.Error("the egress hour was not written once")
		}
		if projection := ledger.read(); projection.UnsettledNanoUsd != computeT3Usd(0.27) {
			t.Errorf("unsettled %d after the egress hour", projection.UnsettledNanoUsd)
		}
	})
}

// No row, outcome or projection field holds content (R-BAL-6): a planted prompt marker as a call's catalog id or
// provider reference is refused before any hold, leaving no trace; and after admissions, settlements, a release and a
// settled write on the same network, the marker is in no column of compute_op, nor in any field or value of the
// network's Redis keys. The ledger's calls write no log line.
func TestComputeRowsHoldNoContent(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.rebuild(1, computeT3Usd(10), computeT3Usd(5), 0, model.ComputeAccountStateActive)
		marker := "prompt-marker-7f3a: please summarize my unread mail"
		planted := ledger.teeRequest(computeT3Usd(1))
		planted.CatalogId = &marker
		if _, err := controller.ComputeHold(t3.env, t3.ctx, ledger.settings, planted); !errors.Is(err, model.ErrComputeOpArgs) {
			t.Errorf("a prompt as the catalog id: %v", err)
		}
		planted = ledger.teeRequest(computeT3Usd(1))
		planted.ProviderRef = &marker
		if _, err := controller.ComputeHold(t3.env, t3.ctx, ledger.settings, planted); !errors.Is(err, model.ErrComputeOpArgs) {
			t.Errorf("a prompt as the provider reference: %v", err)
		}
		markerToken := "prompt-marker-7f3a"
		settled := ledger.admit(computeT3Usd(2))
		ledger.settleCall(settled.OpKey, computeT3Usd(0.3))
		released := ledger.admit(computeT3Usd(1))
		ledger.release(released.OpKey)
		ledger.admit(computeT3Usd(1))

		rows := t3.queryInt(`SELECT count(*) FROM compute_op WHERE network_id = $1 AND row_to_json(compute_op)::text LIKE '%' || $2 || '%'`, ledger.networkId, "summarize")
		if rows != 0 {
			t.Errorf("%d rows hold the marker", rows)
		}
		if t3.queryInt(`SELECT count(*) FROM compute_op WHERE network_id = $1`, ledger.networkId) != 3 {
			t.Error("the network's rows are not the three admitted")
		}
		for _, key := range []string{model.ComputeProjectionKey(ledger.networkId), model.ComputeBudgetKey(ledger.networkId, ledger.agentId), model.ComputeBudgetKey(ledger.networkId, ledger.sidecarId)} {
			fields, err := t3.redis.HashGetAll(t3.ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			for field, value := range fields {
				if strings.Contains(field, "summarize") || strings.Contains(string(value), "summarize") ||
					strings.Contains(field, markerToken) || strings.Contains(string(value), markerToken) {
					t.Errorf("%s holds the marker in %s", key, field)
				}
			}
		}
	})
}
