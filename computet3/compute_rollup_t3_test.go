//go:build sidecar_pending

package computet3

// compute_rollup_day on the private stack's PostgreSQL (BAL-M12-05; DESIGN-COMPUTE §9.4, §9.13, §14.5, D-BAL-6): the
// closed day's day rows re-derived from their charges and a hand-edited one caught, a hand-edited total caught, the
// day rows past 400 days moved into compute_charge_pruned with the totals still equal, the partitions ahead bounded in
// UTC, the partitions past 90 days dropped with the kept operation rows that name their charges and the charges' lot
// draws, a pass resumed at its cursor, and each alarm's counter raised once for each difference a pass finds.

import (
	"strconv"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/core"
)

// The rollup's env: the test's database and Redis, a fake clock at now, and metrics the test reads.
type computeT3Rollup struct {
	t3       *computeT3
	env      operator.Env
	clock    *sidecartest.FakeClock
	metrics  *sidecartest.FakeMetrics
	settings *model.ComputeLedgerSettings
}

func newComputeT3Rollup(t3 *computeT3, now time.Time) *computeT3Rollup {
	clock := sidecartest.NewFakeClock(now)
	metrics := sidecartest.NewFakeMetrics(nil)
	env, err := core.NewEnv(&sidecar.ServiceHost{Redis: t3.redis, Clock: clock, Metrics: metrics}, &core.Grants{Db: true})
	if err != nil {
		t3.t.Fatal(err)
	}
	return &computeT3Rollup{t3: t3, env: env, clock: clock, metrics: metrics, settings: model.DefaultComputeLedgerSettings()}
}

// One pass from cursor, failing the test on its error.
func (self *computeT3Rollup) pass(cursor *controller.ComputeRollupCursor) *controller.ComputeRollupResult {
	self.t3.t.Helper()
	result, err := controller.ComputeRollupDay(self.env, self.t3.ctx, self.settings, cursor)
	if err != nil {
		self.t3.t.Fatal(err)
	}
	if result.Cursor.Phase != controller.ComputeRollupPhaseDone {
		self.t3.t.Fatalf("the pass stopped at %+v", result.Cursor)
	}
	return result
}

// The alarms counted so far, by the check's kind.
func (self *computeT3Rollup) alarms(kind string) float64 {
	return self.metrics.Value("urnetwork_compute_alarms_total", kind)
}

// The mismatches of one check in a result.
func computeT3Mismatches(result *controller.ComputeRollupResult, check controller.ComputeRollupCheck) []*controller.ComputeRollupMismatch {
	mismatches := []*controller.ComputeRollupMismatch{}
	for _, mismatch := range result.Mismatches {
		if mismatch.Check == check {
			mismatches = append(mismatches, mismatch)
		}
	}
	return mismatches
}

// A network with a lot and, booked, two calls and a search that started yesterday at noon UTC, and one call today:
// the day rows the pass re-derives.
func computeT3RollupLedger(t3 *computeT3) *computeT3Ledger {
	t3.t.Helper()
	ledger := newComputeT3Ledger(t3)
	now := time.Now().UTC()
	yesterday := model.ComputeUtcDay(now).AddDate(0, 0, -1).Add(12 * time.Hour)
	ledger.credit(model.ComputeLotKindPurchase, computeT3Usd(20), now.Add(-72*time.Hour), nil)
	ledger.settledRow(model.ComputeChargeKindInference, computeT3Usd(0.40), yesterday)
	ledger.settledRow(model.ComputeChargeKindInference, computeT3Usd(0.25), yesterday.Add(time.Minute))
	ledger.settledRow(model.ComputeChargeKindWebSearch, computeT3Usd(0.012), yesterday.Truncate(time.Minute))
	ledger.settledRow(model.ComputeChargeKindInference, computeT3Usd(0.10), now.Add(-time.Minute))
	if result := ledger.book(); result.BookedCount != 4 {
		t3.t.Fatalf("the booking: %+v", result)
	}
	return ledger
}

// A day row edited by hand is caught by the next pass, by its network and kind, once; the rows as compute_settle
// wrote them are not. The edit also leaves the totals apart from the day rows, which the totals check names too.
func TestComputeRollupDayCatchesAHandEditedDayRow(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := computeT3RollupLedger(t3)
		rollup := newComputeT3Rollup(t3, time.Now().UTC())
		result := rollup.pass(nil)
		if len(result.Mismatches) != 0 || rollup.alarms(controller.ComputeAlarmRollupDay) != 0 {
			t.Fatalf("the booked rows differ: %+v", result.Mismatches)
		}
		if result.NetworkCount != 1 || !result.Cursor.Day.Equal(model.ComputeUtcDay(time.Now().UTC()).AddDate(0, 0, -1)) {
			t.Fatalf("the pass: %+v", result)
		}
		yesterday := model.ComputeUtcDay(time.Now().UTC()).AddDate(0, 0, -1)
		t3.mustExec(`UPDATE compute_charge_day SET output_tokens = output_tokens + 7 WHERE network_id = $1 AND kind = $2 AND day = $3::date`,
			ledger.networkId, int16(model.ComputeChargeKindInference), yesterday.Format(time.DateOnly))
		result = rollup.pass(nil)
		days := computeT3Mismatches(result, controller.ComputeRollupCheckDay)
		if len(days) != 1 || days[0].NetworkId != ledger.networkId || days[0].Kind != model.ComputeChargeKindInference || !days[0].Day.Equal(yesterday) {
			t.Fatalf("the edited day row: %+v", days)
		}
		totals := computeT3Mismatches(result, controller.ComputeRollupCheckTotal)
		if len(totals) != 1 || totals[0].Kind != model.ComputeChargeKindInference {
			t.Fatalf("the totals beside the edited day row: %+v", totals)
		}
		if rollup.alarms(controller.ComputeAlarmRollupDay) != 1 || rollup.alarms(controller.ComputeAlarmRollupTotal) != 1 {
			t.Errorf("the alarms: day %v, total %v", rollup.alarms(controller.ComputeAlarmRollupDay), rollup.alarms(controller.ComputeAlarmRollupTotal))
		}
		// a row the charges do not give, and one they give that is gone, are each a difference of their kind
		t3.mustExec(`UPDATE compute_charge_day SET output_tokens = output_tokens - 7 WHERE network_id = $1 AND kind = $2 AND day = $3::date`,
			ledger.networkId, int16(model.ComputeChargeKindInference), yesterday.Format(time.DateOnly))
		t3.mustExec(`DELETE FROM compute_charge_day WHERE network_id = $1 AND kind = $2 AND day = $3::date`,
			ledger.networkId, int16(model.ComputeChargeKindWebSearch), yesterday.Format(time.DateOnly))
		result = rollup.pass(nil)
		if days := computeT3Mismatches(result, controller.ComputeRollupCheckDay); len(days) != 1 || days[0].Kind != model.ComputeChargeKindWebSearch {
			t.Fatalf("the deleted search row: %+v", days)
		}
		// today, the open day, is never re-derived: its row edited by hand is no day difference
		t3.mustExec(`INSERT INTO compute_charge_day (network_id, day, kind, sidecar_id, provider, input_tokens, output_tokens,
				cached_tokens, seconds, egress_bytes, searches, amount_nano_usd, refunded_nano_usd, count)
			VALUES ($1, $2::date, $3, $4, 1, 0, 0, 0, 0, 0, 0, 5, 0, 1)`,
			ledger.networkId, model.ComputeUtcDay(time.Now().UTC()).Format(time.DateOnly), int16(model.ComputeChargeKindEnclaveRun), server.NewId())
		result = rollup.pass(nil)
		for _, mismatch := range computeT3Mismatches(result, controller.ComputeRollupCheckDay) {
			if mismatch.Kind == model.ComputeChargeKindEnclaveRun {
				t.Errorf("today's row was re-derived: %+v", mismatch)
			}
		}
	})
}

// A lifetime total edited by hand is caught by the totals check alone, by its network and kind, once a pass.
func TestComputeTotalsCheckCatchesAHandEditedTotal(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := computeT3RollupLedger(t3)
		rollup := newComputeT3Rollup(t3, time.Now().UTC())
		if result := rollup.pass(nil); len(result.Mismatches) != 0 {
			t.Fatalf("the booked totals differ: %+v", result.Mismatches)
		}
		t3.mustExec(`UPDATE compute_charge_total SET count = count + 1 WHERE network_id = $1 AND kind = $2`,
			ledger.networkId, int16(model.ComputeChargeKindWebSearch))
		result := rollup.pass(nil)
		if len(computeT3Mismatches(result, controller.ComputeRollupCheckDay)) != 0 {
			t.Errorf("a total's edit read as a day row's: %+v", result.Mismatches)
		}
		totals := computeT3Mismatches(result, controller.ComputeRollupCheckTotal)
		if len(totals) != 1 || totals[0].NetworkId != ledger.networkId || totals[0].Kind != model.ComputeChargeKindWebSearch {
			t.Fatalf("the edited total: %+v", totals)
		}
		// a unit apart while nothing of the kind was pruned is a difference too
		t3.mustExec(`UPDATE compute_charge_total SET count = count - 1 WHERE network_id = $1 AND kind = $2`,
			ledger.networkId, int16(model.ComputeChargeKindWebSearch))
		t3.mustExec(`UPDATE compute_charge_total SET input_tokens = input_tokens + 1 WHERE network_id = $1 AND kind = $2`,
			ledger.networkId, int16(model.ComputeChargeKindInference))
		result = rollup.pass(nil)
		totals = computeT3Mismatches(result, controller.ComputeRollupCheckTotal)
		if len(totals) != 1 || totals[0].Kind != model.ComputeChargeKindInference {
			t.Fatalf("the edited units: %+v", totals)
		}
		if rollup.alarms(controller.ComputeAlarmRollupTotal) != 2 || rollup.alarms(controller.ComputeAlarmRollupDay) != 0 {
			t.Errorf("the alarms: total %v, day %v", rollup.alarms(controller.ComputeAlarmRollupTotal), rollup.alarms(controller.ComputeAlarmRollupDay))
		}
	})
}

// Day rows past 400 days move into compute_charge_pruned, summed per network and kind, in the statement that deletes
// them, and the totals still equal the day rows plus the pruned sums; the rows within 400 days stay, and a second
// pass moves nothing more.
func TestComputePruneKeepsTheTotalsEqual(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		ledger := computeT3RollupLedger(t3)
		now := time.Now().UTC()
		rollup := newComputeT3Rollup(t3, now)
		today := model.ComputeUtcDay(now)
		// two old days of enclave time, as compute_settle would have booked them, and one inside the 400 days
		dayRow := func(day time.Time, seconds int64, amount int64) {
			t3.mustExec(`INSERT INTO compute_charge_day (network_id, day, kind, sidecar_id, provider, enclave_id, input_tokens,
					output_tokens, cached_tokens, seconds, egress_bytes, searches, amount_nano_usd, refunded_nano_usd, count)
				VALUES ($1, $2::date, $3, $4, 2, $5, 0, 0, 0, $6, 0, 0, $7, 0, 1)`,
				ledger.networkId, day.Format(time.DateOnly), int16(model.ComputeChargeKindEnclaveRun), ledger.sidecarId,
				ledger.agentId, seconds, amount)
			t3.mustExec(`
				INSERT INTO compute_charge_total (network_id, kind, provider, enclave_id, input_tokens, output_tokens, cached_tokens,
					seconds, egress_bytes, searches, amount_nano_usd, refunded_nano_usd, count)
				VALUES ($1, $2, 2, $3, 0, 0, 0, $4, 0, 0, $5, 0, 1)
				ON CONFLICT (network_id, kind, member_id, catalog_id, provider, enclave_id) DO UPDATE SET
					seconds = compute_charge_total.seconds + EXCLUDED.seconds,
					amount_nano_usd = compute_charge_total.amount_nano_usd + EXCLUDED.amount_nano_usd,
					count = compute_charge_total.count + 1`,
				ledger.networkId, int16(model.ComputeChargeKindEnclaveRun), ledger.agentId, seconds, amount)
		}
		dayRow(today.AddDate(0, 0, -420), 600, computeT3Usd(0.30))
		dayRow(today.AddDate(0, 0, -401), 600, computeT3Usd(0.30))
		dayRow(today.AddDate(0, 0, -399), 300, computeT3Usd(0.15))
		result := rollup.pass(nil)
		if len(result.Mismatches) != 0 {
			t.Fatalf("the pass: %+v", result.Mismatches)
		}
		if result.PrunedRowCount != 2 {
			t.Errorf("%d day rows moved", result.PrunedRowCount)
		}
		if t3.queryInt(`SELECT amount_nano_usd FROM compute_charge_pruned WHERE network_id = $1 AND kind = $2`, ledger.networkId, int16(model.ComputeChargeKindEnclaveRun)) != computeT3Usd(0.60) ||
			t3.queryInt(`SELECT count FROM compute_charge_pruned WHERE network_id = $1 AND kind = $2`, ledger.networkId, int16(model.ComputeChargeKindEnclaveRun)) != 2 {
			t.Error("the pruned sums are not the moved rows'")
		}
		if t3.queryInt(`SELECT count(*) FROM compute_charge_day WHERE network_id = $1 AND day < $2::date`, ledger.networkId, today.AddDate(0, 0, -400).Format(time.DateOnly)) != 0 ||
			t3.queryInt(`SELECT count(*) FROM compute_charge_day WHERE network_id = $1 AND kind = $2`, ledger.networkId, int16(model.ComputeChargeKindEnclaveRun)) != 1 {
			t.Error("the day rows past and within 400 days")
		}
		// totals = day rows + pruned, for the kind pruned: the amount and the count
		total := t3.queryInt(`SELECT amount_nano_usd FROM compute_charge_total WHERE network_id = $1 AND kind = $2`, ledger.networkId, int16(model.ComputeChargeKindEnclaveRun))
		day := t3.queryInt(`SELECT sum(amount_nano_usd)::bigint FROM compute_charge_day WHERE network_id = $1 AND kind = $2`, ledger.networkId, int16(model.ComputeChargeKindEnclaveRun))
		if total != day+computeT3Usd(0.60) {
			t.Errorf("total %d, day rows %d and pruned %d", total, day, computeT3Usd(0.60))
		}
		if again := rollup.pass(nil); again.PrunedRowCount != 0 || len(again.Mismatches) != 0 {
			t.Errorf("a second pass: %+v", again)
		}
	})
}

// The partitions for today and the 7 days ahead stand after a pass begun west of UTC, each bounded at UTC midnights:
// a charge at a day's first instant and one at its last both land in that day's partition.
func TestComputePartitionsMadeAheadInUtc(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		// a pass at an hour that is still yesterday in UTC-8
		now := time.Now().UTC()
		west := now.In(time.FixedZone("UTC-8", -8*60*60))
		rollup := newComputeT3Rollup(t3, west)
		result := rollup.pass(nil)
		today := model.ComputeUtcDay(now)
		if len(result.PartitionDays) != 8 || !result.PartitionDays[0].Equal(today) || !result.PartitionDays[7].Equal(today.AddDate(0, 0, 7)) {
			t.Fatalf("the days ensured: %v", result.PartitionDays)
		}
		for ahead := 0; ahead <= 7; ahead += 1 {
			name := model.ComputeChargePartitionName(today.AddDate(0, 0, ahead))
			if t3.queryInt(`SELECT count(*) FROM pg_class WHERE relname = $1`, name) != 1 {
				t.Errorf("no partition %s", name)
			}
		}
		day := today.AddDate(0, 0, 3)
		insert := func(start time.Time) server.Id {
			chargeId := server.NewId()
			t3.mustExec(`INSERT INTO compute_charge (charge_id, network_id, kind, subject_ref, sidecar_id, provider, units,
					amount_nano_usd, cost_nano_usd, absorbed_nano_usd, start_time, end_time, settle_time)
				VALUES ($1, $2, 1, $3, $4, 1, '{}'::jsonb, 1, 1, 0, $5, $5, $5)`,
				chargeId, server.NewId(), server.NewId(), server.NewId(), start)
			return chargeId
		}
		first := insert(day)
		last := insert(day.AddDate(0, 0, 1).Add(-time.Microsecond))
		for _, chargeId := range []server.Id{first, last} {
			if t3.queryInt(`SELECT (tableoid::regclass::text = $2)::int FROM compute_charge WHERE charge_id = $1`, chargeId, model.ComputeChargePartitionName(day)) != 1 {
				t.Errorf("the charge %s is not in %s's partition", chargeId, day.Format(time.DateOnly))
			}
		}
		// a pass again makes nothing new, and fails on none that stands
		if again := rollup.pass(nil); len(again.PartitionDays) != 8 {
			t.Errorf("a second pass: %v", again.PartitionDays)
		}
	})
}

// A charge partition whose day ended 90 days ago drops, with its charges' lot draws and the kept operation rows that
// name its charges: an adjustment's and a search's by their own charges, a refund's by the charge it returns. A
// partition inside the 90 days, its draws and its kept rows stay.
func TestComputeChargesDroppedAtNinetyDaysWithTheirRefundRows(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		now := time.Now().UTC()
		rollup := newComputeT3Rollup(t3, now)
		today := model.ComputeUtcDay(now)
		old, recent := today.AddDate(0, 0, -92), today.AddDate(0, 0, -89)
		if err := model.ComputeChargePartitionsEnsure(rollup.env, t3.ctx, []time.Time{old, recent}); err != nil {
			t.Fatal(err)
		}
		networkId := server.NewId()
		charge := func(kind model.ComputeChargeKind, start time.Time) server.Id {
			chargeId := server.NewId()
			t3.mustExec(`INSERT INTO compute_charge (charge_id, network_id, kind, subject_ref, sidecar_id, provider, units,
					amount_nano_usd, cost_nano_usd, absorbed_nano_usd, start_time, end_time, settle_time)
				VALUES ($1, $2, $3, $4, $5, 1, '{}'::jsonb, 1000, 800, 0, $6, $6, $6)`,
				chargeId, networkId, int16(kind), server.NewId(), server.NewId(), start)
			t3.mustExec(`INSERT INTO compute_charge_lot (charge_id, draw_index, lot_id, amount_nano_usd) VALUES ($1, 0, $2, 1000)`, chargeId, server.NewId())
			return chargeId
		}
		keptRow := func(opKey server.Id, kind model.ComputeChargeKind, subjectRef server.Id, amount int64, start time.Time) {
			outcome := `{"units":{},"amount_nano_usd":` + strconv.FormatInt(amount, 10) +
				`,"cost_nano_usd":0,"absorbed_nano_usd":0,"end_time":"2026-01-02T00:00:00Z","settle_time":"2026-01-02T00:00:00Z"}`
			t3.mustExec(`INSERT INTO compute_op (op_key, network_id, shard, state, kind, subject_ref, sidecar_id, provider,
					hold_nano_usd, reserve_nano_usd, start_time, outcome)
				VALUES ($1, $2, 0, $3, $4, $5, $6, 1, 0, 0, $7, $8::jsonb)`,
				opKey, networkId, int16(model.ComputeOpStateApplied), int16(kind), subjectRef, server.NewId(), start, outcome)
		}
		oldCall := charge(model.ComputeChargeKindInference, old.Add(time.Hour))
		oldAdjustment := charge(model.ComputeChargeKindAdjustment, old.Add(2*time.Hour))
		oldSearch := charge(model.ComputeChargeKindWebSearch, old.Add(3*time.Hour))
		recentAdjustment := charge(model.ComputeChargeKindAdjustment, recent.Add(time.Hour))
		// a refund of the old call, booked later; its own charge is in a partition the pass keeps
		refundKey := controller.ComputeOpKeyForRefund(oldCall)
		keptRow(refundKey, model.ComputeChargeKindInference, oldCall, -500, today.AddDate(0, 0, -10))
		keptRow(oldAdjustment, model.ComputeChargeKindAdjustment, server.NewId(), 1000, old.Add(2*time.Hour))
		keptRow(oldSearch, model.ComputeChargeKindWebSearch, server.NewId(), 1000, old.Add(3*time.Hour))
		keptRow(recentAdjustment, model.ComputeChargeKindAdjustment, server.NewId(), 1000, recent.Add(time.Hour))

		result := rollup.pass(nil)
		if len(result.Dropped) != 1 || !result.Dropped[0].Day.Equal(old) || result.Dropped[0].OpRowCount != 3 || result.Dropped[0].DrawCount != 3 {
			t.Fatalf("dropped: %+v", result.Dropped)
		}
		if t3.queryInt(`SELECT count(*) FROM pg_class WHERE relname = $1`, model.ComputeChargePartitionName(old)) != 0 {
			t.Error("the old partition stands")
		}
		if t3.queryInt(`SELECT count(*) FROM pg_class WHERE relname = $1`, model.ComputeChargePartitionName(recent)) != 1 {
			t.Error("the partition inside 90 days is gone")
		}
		for _, opKey := range []server.Id{refundKey, oldAdjustment, oldSearch} {
			if t3.queryInt(`SELECT count(*) FROM compute_op WHERE op_key = $1`, opKey) != 0 {
				t.Errorf("the kept row %s naming a dropped charge stands", opKey)
			}
		}
		if t3.queryInt(`SELECT count(*) FROM compute_op WHERE op_key = $1`, recentAdjustment) != 1 ||
			t3.queryInt(`SELECT count(*) FROM compute_charge_lot WHERE charge_id = $1`, recentAdjustment) != 1 {
			t.Error("the recent adjustment's row or draw is gone")
		}
		if t3.queryInt(`SELECT count(*) FROM compute_charge_lot WHERE charge_id = ANY($1::uuid[])`,
			[]string{oldCall.String(), oldAdjustment.String(), oldSearch.String()}) != 0 {
			t.Error("a dropped charge's draws stand")
		}
		if again := rollup.pass(nil); len(again.Dropped) != 0 {
			t.Errorf("a second pass dropped %+v", again.Dropped)
		}
	})
}

// A pass resumed at its cursor checks only the networks after it: a difference in a network before the cursor is not
// raised again, one after it is.
func TestComputeRollupResumesAtItsCursor(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		first := computeT3RollupLedger(t3)
		second := computeT3RollupLedger(t3)
		if second.networkId.Cmp(first.networkId) < 0 {
			first, second = second, first
		}
		for _, ledger := range []*computeT3Ledger{first, second} {
			t3.mustExec(`UPDATE compute_charge_total SET amount_nano_usd = amount_nano_usd + 1 WHERE network_id = $1 AND kind = $2`,
				ledger.networkId, int16(model.ComputeChargeKindInference))
		}
		rollup := newComputeT3Rollup(t3, time.Now().UTC())
		rollup.settings.RollupNetworkBatchCount = 1
		cursor := controller.NewComputeRollupCursor(time.Now().UTC())
		after := first.networkId
		cursor.After = &after
		result := rollup.pass(cursor)
		if len(result.Mismatches) != 1 || result.Mismatches[0].NetworkId != second.networkId || result.NetworkCount != 1 {
			t.Fatalf("the resumed pass: %+v", result)
		}
		if rollup.alarms(controller.ComputeAlarmRollupTotal) != 1 {
			t.Errorf("%v alarms", rollup.alarms(controller.ComputeAlarmRollupTotal))
		}
		// from the start, one batch a network, both are found
		if result := rollup.pass(nil); len(result.Mismatches) != 2 || result.NetworkCount != 2 {
			t.Fatalf("a pass from the start: %+v", result)
		}
	})
}
