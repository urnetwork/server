//go:build sidecar_pending

package computet3

// compute_settle's booking and the lost cache's rebuild on the private stack's PostgreSQL and Redis, real Lua
// (BAL-M12-04; DESIGN-COMPUTE §9.4, §9.7, §9.13, D-BAL-13, D-BAL-14): a batch booked in §9.4's draw order with its
// account and rollups, its rows applied and deleted but the kept ones, the projection dropped; a charge after UTC
// midnight counting to its start's day whatever the session's zone; the first charge day the earliest; a rebuild after
// a failover admitting only after it, the counters back; and seeded runs holding the ledger's properties. A rebuild
// racing a batch, and settlement stopped at every step, are in compute_book_stop_t3_test.go, under sidecar_testhooks.

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// §16.5's default lines for an operation's subjects, as a broker without CACL's resolver gives them.
func (self *computeT3Ledger) budgets() controller.ComputeBudgetLines {
	settings := self.settings
	return func(ctx context.Context, subjects *controller.ComputeBudgetSubjects) ([]*model.ComputeBudgetLine, error) {
		return model.ComputeDefaultBudgetLines(settings, &model.ComputeBudgetCaller{SidecarId: subjects.SidecarId, AgentId: subjects.AgentId})
	}
}

// Credits a lot of kind and amount, made at created, as BAL-M12-07's crediting will: the lot, and the account's settled
// and version raised by it.
func (self *computeT3Ledger) credit(kind model.ComputeLotKind, amount int64, created time.Time, expire *time.Time) server.Id {
	self.t3.t.Helper()
	lotId := server.NewId()
	market := model.ComputeMarketStripe
	if kind == model.ComputeLotKindPromotion {
		market = model.ComputeMarketPromotion
	}
	self.t3.mustExec(`
		INSERT INTO compute_lot (lot_id, network_id, kind, market, market_ref, amount_nano_usd, remaining_nano_usd, create_time, expire_time)
		VALUES ($1, $2, $3, $4, $5, $6, $6, $7, $8)
	`, lotId, self.networkId, int16(kind), int16(market), "ref_"+lotId.String(), amount, created.UTC(), expire)
	self.t3.mustExec(`
		INSERT INTO compute_account (network_id, settled_nano_usd, state, negative_floor_nano_usd, version, update_time)
		VALUES ($1, $2, $3, 0, 1, now())
		ON CONFLICT (network_id) DO UPDATE SET settled_nano_usd = compute_account.settled_nano_usd + $2, version = compute_account.version + 1
	`, self.networkId, amount, int16(model.ComputeAccountStateActive))
	return lotId
}

// Drops the projection, every key of the network's, and rebuilds it from the ledger as the broker would.
func (self *computeT3Ledger) rebuildFromLedger() {
	self.t3.t.Helper()
	self.dropCache()
	rebuilt, err := controller.ComputeRebuild(self.t3.env, self.t3.ctx, self.settings, self.networkId, self.budgets())
	if err != nil || !rebuilt {
		self.t3.t.Fatalf("the rebuild: %v, %v", rebuilt, err)
	}
}

// Deletes the network's projection, its unconfirmed set and its subjects' counters: a lost cache.
func (self *computeT3Ledger) dropCache() {
	self.t3.t.Helper()
	keys := []string{
		model.ComputeProjectionKey(self.networkId), model.ComputeProjectionKey(self.networkId) + ":u",
		model.ComputeBudgetKey(self.networkId, self.agentId), model.ComputeBudgetKey(self.networkId, self.sidecarId),
	}
	if err := self.t3.redis.Delete(self.t3.ctx, keys); err != nil {
		self.t3.t.Fatal(err)
	}
}

// One run of compute_settle for the network's shard.
func (self *computeT3Ledger) book() *controller.ComputeBookResult {
	self.t3.t.Helper()
	result, err := controller.ComputeBook(self.t3.env, self.t3.ctx, self.settings, model.ComputeOpShard(self.settings, self.networkId))
	if err != nil {
		self.t3.t.Fatal(err)
	}
	return result
}

// The network's account.
func (self *computeT3Ledger) account() *model.ComputeAccount {
	self.t3.t.Helper()
	account, err := model.ComputeAccountGet(self.t3.env, self.t3.ctx, self.networkId)
	if err != nil || account == nil {
		self.t3.t.Fatalf("the account: %v, %v", account, err)
	}
	return account
}

// Admits a call of hold with the ledger's budgets and settles it at amount.
func (self *computeT3Ledger) call(hold int64, amount int64) server.Id {
	self.t3.t.Helper()
	answer := self.admit(hold)
	if settled := self.settleCall(answer.OpKey, amount); settled.State != controller.ComputeSettleSettled {
		self.t3.t.Fatalf("the call did not settle: %+v", settled)
	}
	return answer.OpKey
}

// A settled row of kind written straight to compute_op, starting at start: what a path not built yet would settle.
func (self *computeT3Ledger) settledRow(kind model.ComputeChargeKind, amount int64, start time.Time) server.Id {
	self.t3.t.Helper()
	opKey := server.NewId()
	agentId := self.agentId
	catalogId := self.catalogId
	provider := self.providerId
	units := model.ComputeUnits{InputTokens: 1200, OutputTokens: 300}
	if kind == model.ComputeChargeKindWebSearch {
		catalogId, provider, units = "search/parallel/fast", 8, model.ComputeUnits{Searches: 1}
	}
	op := &model.ComputeOp{
		OpKey: opKey, NetworkId: self.networkId, State: model.ComputeOpStateSettled, Kind: kind, SubjectRef: server.NewId(),
		SidecarId: self.sidecarId, AgentId: &agentId, CatalogId: &catalogId, Provider: provider, StartTime: start,
		HoldNanoUsd: amount, ReserveNanoUsd: amount,
		Outcome: &model.ComputeOpOutcome{Units: units, AmountNanoUsd: amount, CostNanoUsd: amount * 5 / 6, EndTime: start, SettleTime: start},
	}
	if kind == model.ComputeChargeKindWebSearch {
		attempt := int16(1)
		op.Attempt = &attempt
	}
	if inserted, _, err := model.ComputeOpInsertSettled(self.t3.env, self.t3.ctx, self.settings, op); err != nil || !inserted {
		self.t3.t.Fatalf("the settled row: %v, %v", inserted, err)
	}
	return opKey
}

// A batch books in §9.4's draw order: a search, first, passes over the promotion to the subscription grant; the calls
// draw the promotion, then the grant. The account falls by every charge, its version rises once, its first charge day
// is today's UTC day; each charge is in its day and lifetime rows; the calls' rows are deleted and the search's kept;
// the projection's charges are dropped and its settled is the account's. A second run books nothing.
func TestComputeSettleBooksABatch(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		now := time.Now().UTC()
		expire := now.Add(10 * 24 * time.Hour)
		promotionId := ledger.credit(model.ComputeLotKindPromotion, computeT3Usd(2), now.Add(-time.Hour), &expire)
		grantId := ledger.credit(model.ComputeLotKindSubscription, computeT3Usd(3), now.Add(-2*time.Hour), nil)
		purchaseId := ledger.credit(model.ComputeLotKindPurchase, computeT3Usd(10), now.Add(-3*time.Hour), nil)
		ledger.rebuildFromLedger()
		version := ledger.account().Version

		searchKey := ledger.settledRow(model.ComputeChargeKindWebSearch, computeT3Usd(0.012), now.Add(-time.Minute).Truncate(time.Minute))
		first := ledger.call(computeT3Usd(2.58), computeT3Usd(1))
		second := ledger.call(computeT3Usd(2.58), computeT3Usd(2.50))
		third := ledger.call(computeT3Usd(1), computeT3Usd(0.25))

		result := ledger.book()
		if result.AppliedCount != 4 || result.BookedCount != 4 || result.NetworkCount != 1 {
			t.Errorf("the run: %+v", result)
		}
		draws := map[server.Id]int64{}
		for _, opKey := range []server.Id{searchKey, first, second, third} {
			for _, lotId := range []server.Id{promotionId, grantId, purchaseId} {
				draws[lotId] += t3.queryInt(`SELECT coalesce(sum(amount_nano_usd), 0)::bigint FROM compute_charge_lot WHERE charge_id = $1 AND lot_id = $2`, opKey, lotId)
			}
		}
		if t3.queryInt(`SELECT count(*) FROM compute_charge_lot WHERE charge_id = $1 AND lot_id = $2`, searchKey, promotionId) != 0 {
			t.Error("the search drew the promotion")
		}
		// the promotion's $2 by the first two calls; the grant's $0.012, $1.50 and $0.25; nothing from the purchase
		if draws[promotionId] != computeT3Usd(2) || draws[grantId] != computeT3Usd(0.012)+computeT3Usd(1.5)+computeT3Usd(0.25) || draws[purchaseId] != 0 {
			t.Errorf("drawn: promotion %d, grant %d, purchase %d", draws[promotionId], draws[grantId], draws[purchaseId])
		}
		remaining := t3.queryInt(`SELECT sum(remaining_nano_usd)::bigint FROM compute_lot WHERE network_id = $1`, ledger.networkId)
		spent := computeT3Usd(0.012) + computeT3Usd(1) + computeT3Usd(2.5) + computeT3Usd(0.25)
		account := ledger.account()
		if remaining != computeT3Usd(15)-spent || account.SettledNanoUsd != computeT3Usd(15)-spent || account.Version != version+1 {
			t.Errorf("remaining %d, settled %d, version %d after %d", remaining, account.SettledNanoUsd, account.Version, version)
		}
		if account.FirstChargeDay == nil || !account.FirstChargeDay.Equal(model.ComputeUtcDay(now.Add(-time.Minute))) {
			t.Errorf("the first charge day %v", account.FirstChargeDay)
		}
		// the calls' day row: three charges, their units added; the search's its own kind's
		if t3.queryInt(`SELECT count FROM compute_charge_day WHERE network_id = $1 AND kind = $2`, ledger.networkId, int16(model.ComputeChargeKindInference)) != 3 ||
			t3.queryInt(`SELECT amount_nano_usd FROM compute_charge_day WHERE network_id = $1 AND kind = $2`, ledger.networkId, int16(model.ComputeChargeKindInference)) != computeT3Usd(3.75) ||
			t3.queryInt(`SELECT input_tokens FROM compute_charge_day WHERE network_id = $1 AND kind = $2`, ledger.networkId, int16(model.ComputeChargeKindInference)) != 3*1200 {
			t.Error("the calls' day row is not three charges' sum")
		}
		if t3.queryInt(`SELECT searches FROM compute_charge_day WHERE network_id = $1 AND kind = $2`, ledger.networkId, int16(model.ComputeChargeKindWebSearch)) != 1 {
			t.Error("the search's day row")
		}
		if t3.queryInt(`SELECT sum(amount_nano_usd)::bigint FROM compute_charge_total WHERE network_id = $1`, ledger.networkId) != spent {
			t.Error("the lifetime totals are not the charges' sum")
		}
		// the calls' rows are deleted, the search's kept, applied
		for _, opKey := range []server.Id{first, second, third} {
			if ledger.op(opKey) != nil {
				t.Errorf("a booked call's row %s stands", opKey)
			}
		}
		if op := ledger.op(searchKey); op == nil || op.State != model.ComputeOpStateApplied {
			t.Errorf("the search's row: %+v", op)
		}
		projection := ledger.read()
		if projection.UnsettledNanoUsd != 0 || projection.HoldsNanoUsd != 0 || projection.SettledNanoUsd != account.SettledNanoUsd || projection.Version == "" {
			t.Errorf("the projection after the drop: %+v", projection)
		}
		// the agent's counters charged the three calls, reserving nothing
		if charged := ledger.counter(ledger.agentId, "c", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now); charged != computeT3Usd(3.75) {
			t.Errorf("the agent's day charged %d", charged)
		}
		// a second run books and drops nothing
		if again := ledger.book(); again.AppliedCount != 0 || again.RedroppedCount != 0 || ledger.account().Version != account.Version {
			t.Errorf("a second run: %+v", again)
		}
	})
}

// An Env whose every database session runs in a zone fourteen hours ahead of UTC, where a minute before UTC midnight
// is already the next day: each transaction sets it locally, and each plain call sets it and resets it after.
type computeT3ZoneEnv struct {
	operator.Env
}

func (self *computeT3ZoneEnv) Db() (operator.Db, error) {
	db, err := self.Env.Db()
	if err != nil {
		return nil, err
	}
	return &computeT3ZoneDb{db: db}, nil
}

type computeT3ZoneDb struct {
	db operator.Db
}

const computeT3Zone = "Pacific/Kiritimati"

func (self *computeT3ZoneDb) Db(ctx context.Context, callback func(conn operator.DbConn) error, options ...operator.DbOption) error {
	return self.db.Db(ctx, func(conn operator.DbConn) error {
		if _, err := conn.Exec(ctx, `SET TimeZone TO '`+computeT3Zone+`'`); err != nil {
			return err
		}
		defer conn.Exec(ctx, `RESET TimeZone`)
		return callback(conn)
	}, options...)
}

func (self *computeT3ZoneDb) Tx(ctx context.Context, callback func(tx operator.DbTx) error, options ...operator.DbOption) error {
	return self.db.Tx(ctx, func(tx operator.DbTx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL TimeZone TO '`+computeT3Zone+`'`); err != nil {
			return err
		}
		return callback(tx)
	}, options...)
}

// A charge starting thirty seconds before UTC midnight counts to that UTC day, its partition and its day row, though
// every session that books it runs fourteen hours ahead of UTC, where it is already the next day; the booking makes the
// partition of a day that has none.
func TestComputeChargeAfterMidnightCountsToItsStartDay(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.credit(model.ComputeLotKindPurchase, computeT3Usd(10), time.Now().Add(-48*time.Hour), nil)
		ledger.rebuildFromLedger()
		yesterday := model.ComputeUtcDay(time.Now()).Add(-24 * time.Hour)
		start := yesterday.Add(23*time.Hour + 59*time.Minute + 30*time.Second)
		opKey := ledger.settledRow(model.ComputeChargeKindInference, computeT3Usd(0.75), start)
		zoned := &computeT3ZoneEnv{Env: t3.env}
		if _, err := controller.ComputeBook(zoned, t3.ctx, ledger.settings, model.ComputeOpShard(ledger.settings, ledger.networkId)); err != nil {
			t.Fatal(err)
		}
		partition := ""
		zone := ""
		err := t3.db.Db(t3.ctx, func(conn operator.DbConn) error {
			rows, err := conn.Query(t3.ctx, `SELECT tableoid::regclass::text FROM compute_charge WHERE charge_id = $1`, opKey)
			if err != nil {
				return err
			}
			defer rows.Close()
			if !rows.Next() {
				return errors.New("no charge")
			}
			return rows.Scan(&partition)
		})
		if err != nil {
			t.Fatal(err)
		}
		// the zone the booking's sessions ran in
		zonedDb, _ := zoned.Db()
		zonedDb.Tx(t3.ctx, func(tx operator.DbTx) error {
			rows, err := tx.Query(t3.ctx, `SHOW TimeZone`)
			if err != nil {
				return err
			}
			defer rows.Close()
			if rows.Next() {
				return rows.Scan(&zone)
			}
			return nil
		})
		if zone != computeT3Zone {
			t.Fatalf("the booking's sessions ran in %q", zone)
		}
		if partition != model.ComputeChargePartitionName(yesterday) {
			t.Errorf("the charge is in %s, want %s", partition, model.ComputeChargePartitionName(yesterday))
		}
		if t3.queryInt(`SELECT count(*) FROM compute_charge_day WHERE network_id = $1 AND day = $2::date`, ledger.networkId, yesterday.Format("2006-01-02")) != 1 {
			t.Error("the day row is not yesterday's UTC day")
		}
		if account := ledger.account(); account.FirstChargeDay == nil || !account.FirstChargeDay.Equal(yesterday) {
			t.Errorf("the first charge day %v", account.FirstChargeDay)
		}
	})
}

// Charges settle out of start order, so the first charge day takes the earliest start day it has seen: today's, then
// two days back once a late charge books, and not today's again.
func TestComputeFirstChargeDayTakesTheEarliest(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.credit(model.ComputeLotKindPurchase, computeT3Usd(10), time.Now().Add(-72*time.Hour), nil)
		ledger.rebuildFromLedger()
		today := model.ComputeUtcDay(time.Now())
		ledger.call(computeT3Usd(1), computeT3Usd(0.10))
		ledger.book()
		if day := ledger.account().FirstChargeDay; day == nil || !day.Equal(today) {
			t.Errorf("after today's charge: %v", day)
		}
		twoDaysBack := today.Add(-48 * time.Hour)
		ledger.settledRow(model.ComputeChargeKindInference, computeT3Usd(0.10), twoDaysBack.Add(3*time.Hour))
		ledger.book()
		if day := ledger.account().FirstChargeDay; day == nil || !day.Equal(twoDaysBack) {
			t.Errorf("after the late charge: %v", day)
		}
		ledger.call(computeT3Usd(1), computeT3Usd(0.10))
		ledger.book()
		if day := ledger.account().FirstChargeDay; day == nil || !day.Equal(twoDaysBack) {
			t.Errorf("after another of today's: %v", day)
		}
	})
}

// A projection marked after its shard's failover admits nothing until it is rebuilt: a call without the broker's
// budget lines stays held with the one-second retry; one with them rebuilds under the fence and is admitted after. The
// rebuild brings back what the lost projection held: the open call's hold and reservation, the settled call's unsettled
// charge and its counter, the booked call's counter from its day row, and the account's settled balance. An absent
// projection rebuilds the same way.
func TestComputeRebuildAfterFailoverAdmitsOnlyAfter(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		ledger.credit(model.ComputeLotKindPurchase, computeT3Usd(10), time.Now().Add(-time.Hour), nil)
		ledger.rebuildFromLedger()
		ledger.call(computeT3Usd(1), computeT3Usd(0.50))
		ledger.book()
		ledger.call(computeT3Usd(2), computeT3Usd(1))
		open := ledger.admit(computeT3Usd(2.58))
		now := time.Now()
		before := ledger.read()
		charged := ledger.counter(ledger.agentId, "c", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now)
		reserved := ledger.counter(ledger.agentId, "r", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now)
		if charged != computeT3Usd(1.5) || reserved != computeT3Usd(2.58) {
			t.Fatalf("before: charged %d, reserved %d", charged, reserved)
		}

		if err := model.ComputeProjectionMarkRebuild(t3.env, t3.ctx, ledger.networkId); err != nil {
			t.Fatal(err)
		}
		unresolved := ledger.holdCall(ledger.teeRequest(computeT3Usd(0.10)))
		if unresolved.Admitted || unresolved.Refusal.Reason != controller.ComputeRefusalHeld || unresolved.Refusal.RetryAfter != ledger.settings.ProjectionRebuildRetry {
			t.Errorf("a call with no budget lines on a marked projection: %+v", unresolved)
		}
		// the counters were lost with the master's last writes
		ledger.dropCounters()
		request := ledger.teeRequest(computeT3Usd(0.10))
		request.Budgets = ledger.budgets()
		admitted := ledger.holdCall(request)
		if !admitted.Admitted {
			t.Fatalf("after the rebuild: %+v", admitted.Refusal)
		}
		after := ledger.read()
		if after.Marked || after.SettledNanoUsd != before.SettledNanoUsd || after.UnsettledNanoUsd != before.UnsettledNanoUsd ||
			after.HoldsNanoUsd != before.HoldsNanoUsd+computeT3Usd(0.10) {
			t.Errorf("before %+v, after %+v", before, after)
		}
		if got := ledger.counter(ledger.agentId, "c", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now); got != charged {
			t.Errorf("the agent's day charged %d after the rebuild, %d before", got, charged)
		}
		if got := ledger.counter(ledger.agentId, "r", model.ComputeBudgetAgent, model.ComputeBudgetPeriodDay, now); got != reserved+computeT3Usd(0.10) {
			t.Errorf("the agent's day reserves %d after the rebuild", got)
		}
		// the open call settles and books as if nothing happened
		ledger.settleCall(open.OpKey, computeT3Usd(2))
		ledger.dropCache()
		request = ledger.teeRequest(computeT3Usd(0.10))
		request.Budgets = ledger.budgets()
		if answer := ledger.holdCall(request); !answer.Admitted {
			t.Errorf("after an absent projection: %+v", answer.Refusal)
		}
		ledger.book()
		if account := ledger.account(); account.SettledNanoUsd != computeT3Usd(10)-computeT3Usd(0.5)-computeT3Usd(1)-computeT3Usd(2) {
			t.Errorf("settled %d", account.SettledNanoUsd)
		}
	})
}

// Deletes the network's counters alone.
func (self *computeT3Ledger) dropCounters() {
	self.t3.t.Helper()
	keys := []string{model.ComputeBudgetKey(self.networkId, self.agentId), model.ComputeBudgetKey(self.networkId, self.sidecarId)}
	if err := self.t3.redis.Delete(self.t3.ctx, keys); err != nil {
		self.t3.t.Fatal(err)
	}
}

// The ledger's properties over seeded runs of purchases, a promotion, calls held and settled or released, bookings
// and rebuilds in between: what was credited is what the lots have left, plus what charges drew, plus what lapsed; the
// account's settled is the lots' remaining less what no lot covered; a booking run again changes nothing; every hold
// is released once its call ends; and a balance calls hold against never goes below zero.
func TestComputeLedgerProperties(t *testing.T) {
	for _, seed := range []int64{1, 7, 42} {
		runComputeT3(t, func(t3 *computeT3) {
			t := t3.t
			random := rand.New(rand.NewSource(seed))
			ledger := newComputeT3Ledger(t3)
			now := time.Now().UTC()
			expire := now.Add(30 * 24 * time.Hour)
			ledger.credit(model.ComputeLotKindPromotion, computeT3Usd(2), now.Add(-time.Hour), &expire)
			for i := 0; i < 3; i += 1 {
				ledger.credit(model.ComputeLotKindPurchase, int64(1+random.Intn(10))*computeT3Usd(1), now.Add(-time.Duration(2+i)*time.Hour), nil)
			}
			ledger.rebuildFromLedger()
			opens := []server.Id{}
			for step := 0; step < 40; step += 1 {
				switch choice := random.Intn(10); {
				case choice < 5:
					hold := int64(1+random.Intn(150)) * computeT3Usd(0.01)
					answer := ledger.holdCall(ledger.teeRequest(hold))
					if answer.Admitted {
						opens = append(opens, answer.OpKey)
					}
				case choice < 8 && 0 < len(opens):
					index := random.Intn(len(opens))
					opKey := opens[index]
					opens = append(opens[:index], opens[index+1:]...)
					if random.Intn(4) == 0 {
						ledger.release(opKey)
						continue
					}
					op := ledger.op(opKey)
					ledger.settleCall(opKey, random.Int63n(op.HoldNanoUsd+1))
				case choice < 9:
					ledger.book()
				default:
					ledger.rebuildFromLedger()
				}
				if projection := ledger.read(); projection.Present && projection.AvailableNanoUsd() < 0 {
					t.Fatalf("seed %d step %d: available %d", seed, step, projection.AvailableNanoUsd())
				}
			}
			for _, opKey := range opens {
				ledger.settleCall(opKey, random.Int63n(ledger.op(opKey).HoldNanoUsd+1))
			}
			ledger.book()
			account := ledger.account()
			// settlement is idempotent: a run again books nothing
			if again := ledger.book(); again.AppliedCount != 0 || ledger.account().Version != account.Version {
				t.Errorf("seed %d: a second run %+v", seed, again)
			}
			credited := t3.queryInt(`SELECT sum(amount_nano_usd)::bigint FROM compute_lot WHERE network_id = $1`, ledger.networkId)
			remaining := t3.queryInt(`SELECT sum(remaining_nano_usd)::bigint FROM compute_lot WHERE network_id = $1`, ledger.networkId)
			lapsed := t3.queryInt(`SELECT coalesce(sum(lapsed_nano_usd), 0)::bigint FROM compute_lot WHERE network_id = $1`, ledger.networkId)
			drawn := t3.queryInt(`SELECT coalesce(sum(d.amount_nano_usd), 0)::bigint FROM compute_charge_lot d JOIN compute_lot l USING (lot_id) WHERE l.network_id = $1`, ledger.networkId)
			charged := t3.queryInt(`SELECT coalesce(sum(amount_nano_usd), 0)::bigint FROM compute_charge WHERE network_id = $1`, ledger.networkId)
			if credited != remaining+drawn+lapsed {
				t.Errorf("seed %d: credited %d, remaining %d, drawn %d, lapsed %d", seed, credited, remaining, drawn, lapsed)
			}
			if account.SettledNanoUsd != remaining-(charged-drawn) {
				t.Errorf("seed %d: settled %d, remaining %d, overrun %d", seed, account.SettledNanoUsd, remaining, charged-drawn)
			}
			if account.SettledNanoUsd < 0 {
				t.Errorf("seed %d: calls took settled below zero, %d", seed, account.SettledNanoUsd)
			}
			if t3.queryInt(`SELECT coalesce(sum(amount_nano_usd), 0)::bigint FROM compute_charge_day WHERE network_id = $1`, ledger.networkId) != charged {
				t.Errorf("seed %d: the day rows are not the charges' sum", seed)
			}
			projection := ledger.read()
			if projection.HoldsNanoUsd != 0 || projection.UnsettledNanoUsd != 0 || projection.SettledNanoUsd != account.SettledNanoUsd {
				t.Errorf("seed %d: the projection after every call ended: %+v", seed, projection)
			}
			if t3.queryInt(`SELECT count(*) FROM compute_op WHERE network_id = $1 AND state IN ($2, $3)`, ledger.networkId, int16(model.ComputeOpStateOpen), int16(model.ComputeOpStateSettled)) != 0 {
				t.Errorf("seed %d: rows left open or settled", seed)
			}
		})
	}
}

// A refund books as a negative charge of the kind it returns (§9.6): its amount back to the lots its charge drew, in
// reverse draw order, as negative draws; settled back up; its day row net of it, its size in refunded and no count
// added; and its row kept once booked, so a retry meets the claim.
func TestComputeRefundBooksBackToItsDraws(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := newComputeT3Ledger(t3)
		now := time.Now().UTC()
		expire := now.Add(10 * 24 * time.Hour)
		promotionId := ledger.credit(model.ComputeLotKindPromotion, computeT3Usd(1), now.Add(-time.Hour), &expire)
		purchaseId := ledger.credit(model.ComputeLotKindPurchase, computeT3Usd(10), now.Add(-2*time.Hour), nil)
		ledger.rebuildFromLedger()
		chargeId := ledger.call(computeT3Usd(2), computeT3Usd(1.5))
		ledger.book()
		settledAfterCharge := ledger.account().SettledNanoUsd
		agentId := ledger.agentId
		catalogId := ledger.catalogId
		write := func(at time.Time) *controller.ComputeWriteAnswer {
			answer, err := controller.ComputeWriteSettled(t3.env, t3.ctx, ledger.settings, &controller.ComputeSettledWrite{
				NetworkId: ledger.networkId, OpKey: controller.ComputeOpKeyForRefund(chargeId), Kind: model.ComputeChargeKindInference,
				SubjectRef: chargeId, SidecarId: ledger.sidecarId, AgentId: &agentId, CatalogId: &catalogId, Provider: ledger.providerId,
				StartTime: at, Outcome: &controller.ComputeOutcome{AmountNanoUsd: -computeT3Usd(1.5), EndTime: at, SettleTime: at},
			})
			if err != nil {
				t.Fatal(err)
			}
			return answer
		}
		write(now)
		ledger.book()
		// the charge drew the promotion's $1 then the purchase's $0.50; the refund gives the $0.50 back first
		refundKey := controller.ComputeOpKeyForRefund(chargeId)
		if t3.queryInt(`SELECT amount_nano_usd FROM compute_charge_lot WHERE charge_id = $1 AND draw_index = 0`, refundKey) != -computeT3Usd(0.5) ||
			t3.queryInt(`SELECT (lot_id = $2)::int FROM compute_charge_lot WHERE charge_id = $1 AND draw_index = 0`, refundKey, purchaseId) == 0 {
			t.Error("the refund's first draw is not the purchase's $0.50")
		}
		if t3.queryInt(`SELECT remaining_nano_usd FROM compute_lot WHERE lot_id = $1`, promotionId) != computeT3Usd(1) ||
			t3.queryInt(`SELECT remaining_nano_usd FROM compute_lot WHERE lot_id = $1`, purchaseId) != computeT3Usd(10) {
			t.Error("the lots are not back to what they held")
		}
		if account := ledger.account(); account.SettledNanoUsd != settledAfterCharge+computeT3Usd(1.5) {
			t.Errorf("settled %d after the refund, %d before", account.SettledNanoUsd, settledAfterCharge)
		}
		if t3.queryInt(`SELECT amount_nano_usd FROM compute_charge_day WHERE network_id = $1`, ledger.networkId) != 0 ||
			t3.queryInt(`SELECT refunded_nano_usd FROM compute_charge_day WHERE network_id = $1`, ledger.networkId) != computeT3Usd(1.5) ||
			t3.queryInt(`SELECT count FROM compute_charge_day WHERE network_id = $1`, ledger.networkId) != 1 {
			t.Error("the day row is not net of the refund with its size in refunded")
		}
		if op := ledger.op(refundKey); op == nil || op.State != model.ComputeOpStateApplied {
			t.Errorf("the refund's row: %+v", op)
		}
		// a retry the next day meets the claim and books nothing
		if again := write(now.Add(24 * time.Hour)); again.Written {
			t.Error("a retried refund was written again")
		}
		if result := ledger.book(); result.AppliedCount != 0 {
			t.Errorf("a retried refund booked: %+v", result)
		}
		if projection := ledger.read(); projection.SettledNanoUsd != ledger.account().SettledNanoUsd {
			t.Errorf("the projection's settled %d, the account's %d", projection.SettledNanoUsd, ledger.account().SettledNanoUsd)
		}
	})
}
