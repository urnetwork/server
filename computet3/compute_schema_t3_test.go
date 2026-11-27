//go:build sidecar_pending

package computet3

// MG-C1 on the private stack's PostgreSQL 18 (BAL-M12-01): every table, index and today's partition made, and the keys
// §9.4 rests on refusing what they exist to refuse.

import (
	"fmt"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
)

// The ledger's indexes, by name.
var computeT3LedgerIndexes = []string{
	"compute_lot_draw", "compute_charge_network", "compute_op_shard_state", "compute_op_state_start",
	"compute_auto_renew_retry", "compute_dispute_network", "compute_budget_raise_subject",
	"compute_promo_redemption_network", "compute_lot_lapse", "compute_account_update", "compute_account_review",
}

// The steps that add a column to a table an earlier step made, each with its table and column (BAL-M12-12's state_time,
// BAL-M12-11's store_ref).
var computeT3LedgerColumns = map[string][2]string{
	"compute_account_state_time":   {"compute_account", "state_time"},
	"compute_auto_renew_store_ref": {"compute_auto_renew", "store_ref"},
}

// Every table, index and partition is made; a second lot with one (market, market_ref) is refused; the day and total
// keys refuse a duplicate whose nullable columns are null; a charge is unique by (start_time, charge_id) and lands in
// its UTC day's partition whatever the session's zone, a day with no partition refusing it until one is made; an
// operation is unique by its key.
func TestComputeLedgerMigrationsApply(t *testing.T) {
	runComputeT3(t, func(t3 *computeT3) {
		t := t3.t
		// each step makes the table or index its id names: an index's kind i, or I on the partitioned table
		indexes := map[string]bool{}
		for _, index := range computeT3LedgerIndexes {
			indexes[index] = true
		}
		for _, step := range model.ComputeLedgerMigrations() {
			if step.Id == "compute_charge_today" {
				continue
			}
			if column, ok := computeT3LedgerColumns[step.Id]; ok {
				if t3.queryInt(`SELECT count(*) FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`, column[0], column[1]) != 1 {
					t.Errorf("%s did not add %s.%s", step.Id, column[0], column[1])
				}
				continue
			}
			kinds := "{r,p}"
			if indexes[step.Id] {
				kinds = "{i,I}"
			}
			if t3.queryInt(`SELECT count(*) FROM pg_class WHERE relname = $1 AND relkind::text = ANY($2::text[])`, step.Id, kinds) != 1 {
				t.Errorf("%s is not in the migrated database", step.Id)
			}
			delete(indexes, step.Id)
		}
		for index := range indexes {
			t.Errorf("no step makes the index %s", index)
		}
		// compute_charge is partitioned, and today's partition, in UTC, is attached to it
		if t3.queryInt(`SELECT count(*) FROM pg_partitioned_table JOIN pg_class ON pg_class.oid = partrelid WHERE relname = 'compute_charge'`) != 1 {
			t.Error("compute_charge is not partitioned")
		}
		today := time.Now().UTC()
		if t3.queryInt(`SELECT count(*) FROM pg_inherits JOIN pg_class child ON child.oid = inhrelid JOIN pg_class parent ON parent.oid = inhparent WHERE parent.relname = 'compute_charge' AND child.relname = $1`, model.ComputeChargePartitionName(today)) != 1 {
			// the migration ran at its own now(): a test past midnight UTC finds yesterday's, which still bounds a UTC day
			yesterday := today.AddDate(0, 0, -1)
			if t3.queryInt(`SELECT count(*) FROM pg_class WHERE relname = $1`, model.ComputeChargePartitionName(yesterday)) != 1 {
				t.Errorf("no partition of today's UTC day, %s", model.ComputeChargePartitionName(today))
			}
		}

		networkId := server.NewId()
		now := time.Now().UTC()

		// a lot is unique by (market, market_ref): a replayed webhook credits once
		insertLot := func(marketRef string) error {
			return t3.exec(`
				INSERT INTO compute_lot (lot_id, network_id, kind, market, market_ref, amount_nano_usd, remaining_nano_usd, create_time)
				VALUES ($1, $2, $3, $4, $5, $6, $6, $7)
			`, server.NewId(), networkId, int16(model.ComputeLotKindPurchase), int16(model.ComputeMarketStripe), marketRef,
				10*model.ComputeCreditNanoUsd, now)
		}
		if err := insertLot("pi_test_1"); err != nil {
			t.Fatal(err)
		}
		if err := insertLot("pi_test_1"); !computeT3SqlState(err, "23505") {
			t.Errorf("a second lot with one (market, market_ref): %v", err)
		}
		if err := insertLot("pi_test_2"); err != nil {
			t.Errorf("a lot with another reference: %v", err)
		}

		// the day rollup's key, its nullable columns null: NULLS NOT DISTINCT refuses the duplicate
		insertDay := func() error {
			return t3.exec(`
				INSERT INTO compute_charge_day (network_id, day, kind, sidecar_id, agent_id, catalog_id, member_id, provider, enclave_id,
					input_tokens, output_tokens, cached_tokens, seconds, egress_bytes, searches, amount_nano_usd, refunded_nano_usd, count)
				VALUES ($1, $2, $3, $4, NULL, NULL, NULL, 0, NULL, 0, 0, 0, 0, 0, 0, 0, 0, 0)
			`, networkId, now.Truncate(24*time.Hour), int16(model.ComputeChargeKindAdjustment), server.Id{})
		}
		if err := insertDay(); err != nil {
			t.Fatal(err)
		}
		if err := insertDay(); !computeT3SqlState(err, "23505") {
			t.Errorf("a second day row with null columns: %v", err)
		}
		// the lifetime rollup's key, likewise
		insertTotal := func() error {
			return t3.exec(`
				INSERT INTO compute_charge_total (network_id, kind, member_id, catalog_id, provider, enclave_id,
					input_tokens, output_tokens, cached_tokens, seconds, egress_bytes, searches, amount_nano_usd, refunded_nano_usd, count)
				VALUES ($1, $2, NULL, NULL, 0, NULL, 0, 0, 0, 0, 0, 0, 0, 0, 0)
			`, networkId, int16(model.ComputeChargeKindAdjustment))
		}
		if err := insertTotal(); err != nil {
			t.Fatal(err)
		}
		if err := insertTotal(); !computeT3SqlState(err, "23505") {
			t.Errorf("a second total row with null columns: %v", err)
		}
		// and an upsert adds to the one row, its units exactly
		for range 2 {
			t3.mustExec(`
				INSERT INTO compute_charge_total (network_id, kind, member_id, catalog_id, provider, enclave_id,
					input_tokens, output_tokens, cached_tokens, seconds, egress_bytes, searches, amount_nano_usd, refunded_nano_usd, count)
				VALUES ($1, $2, NULL, 'chutes-test-model-tee-high', 1, NULL, 1000, 10, 0, 0, 0, 0, 7, 0, 1)
				ON CONFLICT (network_id, kind, member_id, catalog_id, provider, enclave_id) DO UPDATE SET
					input_tokens = compute_charge_total.input_tokens + EXCLUDED.input_tokens,
					amount_nano_usd = compute_charge_total.amount_nano_usd + EXCLUDED.amount_nano_usd,
					count = compute_charge_total.count + EXCLUDED.count
			`, networkId, int16(model.ComputeChargeKindInference))
		}
		if tokens := t3.queryInt(`SELECT input_tokens FROM compute_charge_total WHERE network_id = $1 AND kind = $2`, networkId, int16(model.ComputeChargeKindInference)); tokens != 2000 {
			t.Errorf("the upserted total holds %d input tokens", tokens)
		}

		// a charge, unique by (start_time, charge_id), in its UTC day's partition
		insertCharge := func(chargeId server.Id, start time.Time) error {
			return t3.exec(`
				INSERT INTO compute_charge (charge_id, network_id, kind, subject_ref, sidecar_id, provider, units,
					amount_nano_usd, cost_nano_usd, absorbed_nano_usd, start_time, end_time, settle_time)
				VALUES ($1, $2, $3, $4, $5, 1, '{"input_tokens": 10}', 5, 4, 0, $6, $6, $6)
			`, chargeId, networkId, int16(model.ComputeChargeKindInference), server.NewId(), server.NewId(), start)
		}
		// the session's zone never moves a charge's day: 23:30 at UTC-8 on the 25th is the 26th in UTC
		chargeId := server.NewId()
		partitionDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, 3)
		start := partitionDay.Add(30 * time.Minute)
		if err := insertCharge(chargeId, start); !computeT3SqlState(err, "23514") {
			t.Errorf("a charge on a day with no partition: %v", err)
		}
		t3.mustExec(model.ComputeChargePartitionStatement(partitionDay))
		// a second make of the same day does nothing
		t3.mustExec(model.ComputeChargePartitionStatement(partitionDay.Add(12 * time.Hour)))
		err := t3.db.Db(t3.ctx, func(conn operator.DbConn) error {
			if _, err := conn.Exec(t3.ctx, `SET TIME ZONE 'America/Los_Angeles'`); err != nil {
				return err
			}
			// the 30 minutes past midnight UTC, written in the session's zone, where it is still the day before
			local := start.In(time.FixedZone("PST", -8*60*60)).Format("2006-01-02 15:04:05-08")
			_, err := conn.Exec(t3.ctx, fmt.Sprintf(`
				INSERT INTO compute_charge (charge_id, network_id, kind, subject_ref, sidecar_id, provider, units,
					amount_nano_usd, cost_nano_usd, absorbed_nano_usd, start_time, end_time, settle_time)
				VALUES ($1, $2, 1, $3, $4, 1, '{}', 5, 4, 0, '%s', '%s', '%s')
			`, local, local, local), chargeId, networkId, server.NewId(), server.NewId())
			if err != nil {
				return err
			}
			_, err = conn.Exec(t3.ctx, `RESET TIME ZONE`)
			return err
		}, operator.DbReadWrite, operator.DbRetryNone)
		if err != nil {
			t.Fatal(err)
		}
		if t3.queryInt(fmt.Sprintf(`SELECT count(*) FROM %s WHERE charge_id = $1`, model.ComputeChargePartitionName(partitionDay)), chargeId) != 1 {
			t.Error("the charge is not in its UTC day's partition")
		}
		if err := insertCharge(chargeId, start); !computeT3SqlState(err, "23505") {
			t.Errorf("a second charge with one (start_time, charge_id): %v", err)
		}

		// an operation is unique by its key
		insertOp := func(opKey server.Id) error {
			return t3.exec(`
				INSERT INTO compute_op (op_key, network_id, shard, state, kind, subject_ref, sidecar_id, provider,
					hold_nano_usd, reserve_nano_usd, start_time)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 1, 100, 100, $8)
			`, opKey, networkId, model.ComputeOpShard(model.DefaultComputeLedgerSettings(), networkId),
				int16(model.ComputeOpStateOpen), int16(model.ComputeChargeKindInference), server.NewId(), server.NewId(), now)
		}
		opKey := server.NewId()
		if err := insertOp(opKey); err != nil {
			t.Fatal(err)
		}
		if err := insertOp(opKey); !computeT3SqlState(err, "23505") {
			t.Errorf("a second operation with one key: %v", err)
		}

		// a redemption is unique per code and network, and a raise by its approval and use and by its effect key
		codeId := server.NewId()
		redeem := func() error {
			return t3.exec(`
				INSERT INTO compute_promo_redemption (redemption_id, code_id, network_id, lot_id, sidecar_id, redeem_time)
				VALUES ($1, $2, $3, $4, $5, $6)
			`, server.NewId(), codeId, networkId, server.NewId(), server.NewId(), now)
		}
		if err := redeem(); err != nil {
			t.Fatal(err)
		}
		if err := redeem(); !computeT3SqlState(err, "23505") {
			t.Errorf("a second redemption of one code in one network: %v", err)
		}
		approvalId := server.NewId()
		effectKey := server.NewId()
		raise := func(use int32, key server.Id) error {
			return t3.exec(`
				INSERT INTO compute_budget_raise (approval_id, approval_use, effect_key, network_id, subject_id, period,
					amount_nano_usd, expire_time, create_time)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
			`, approvalId, use, key, networkId, server.NewId(), int16(model.ComputeBudgetPeriodDay), 10*model.ComputeCreditNanoUsd, now)
		}
		if err := raise(1, effectKey); err != nil {
			t.Fatal(err)
		}
		if err := raise(1, server.NewId()); !computeT3SqlState(err, "23505") {
			t.Errorf("a second raise of one approval's use: %v", err)
		}
		if err := raise(2, effectKey); !computeT3SqlState(err, "23505") {
			t.Errorf("a second raise under one effect key: %v", err)
		}
	})
}
