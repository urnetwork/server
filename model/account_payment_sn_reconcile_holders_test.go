// The early holders read (§6, A2) reads only bounded sweeps, and returns
// exactly what the read it replaced returned.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The holders read as it was through 2026-10-27: every sweep of every row was
// read with its contract. Kept verbatim as the reference the read must match
// row for row.
var snReconcileHoldersReferenceSql = `SELECT p.payment_id,p.payment_plan_id,p.network_id,
	CASE WHEN p.canceled THEN 'safely_canceled' WHEN p.tx_hash IS NOT NULL THEN 'on_chain' WHEN p.payment_record IS NOT NULL THEN 'submitted'
		WHEN p.circle_idempotency_key IS NOT NULL THEN 'reserved' ELSE 'planned' END,
	p.payout_nano_cents,p.payout_byte_count,p.subsidy_payout_nano_cents,p.reliability_subsidy_nano_cents,early.revenue,early.bytes,bonus.amount
	FROM account_payment p
	CROSS JOIN LATERAL (SELECT ` + fmt.Sprintf(snReconcileEarlyBoundSql, "p") + ` AS bound) eb
	CROSS JOIN LATERAL (SELECT COUNT(*) n,COALESCE(SUM(s.payout_net_revenue_nano_cents),0)::bigint revenue,COALESCE(SUM(s.payout_byte_count),0)::bigint bytes
		FROM ` + fmt.Sprintf(snReconcileEarlySweepsSql, "p", "eb.bound") + `) early
	CROSS JOIN LATERAL (SELECT COUNT(*) n,COALESCE(SUM(b.amount_nano_cents),0)::bigint amount FROM account_payment_bonus b
		WHERE b.payment_id=p.payment_id AND b.origin_payment_id<>p.payment_id) bonus
	WHERE NOT p.completed AND (
		(p.canceled AND p.circle_idempotency_key IS NULL AND p.payment_record IS NULL AND p.tx_hash IS NULL) OR
		(NOT p.canceled AND NOT (p.circle_idempotency_key IS NOT NULL AND p.payment_record IS NULL AND p.tx_hash IS NULL
		AND NOT EXISTS (SELECT 1 FROM audit_account_payment a WHERE a.event_id=p.circle_idempotency_key))))
	AND 0<early.n+bonus.n
	ORDER BY p.network_id,p.create_time,p.payment_id`

// Every column of every holder row `sql` lists, in order, as text.
func snReconcileTestHoldersListing(t testing.TB, ctx context.Context, sql string) []string {
	t.Helper()
	rows := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, sql)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				values, err := result.Values()
				server.Raise(err)
				columns := make([]string, len(values))
				for i, value := range values {
					columns[i] = fmt.Sprintf("%v", value)
				}
				rows = append(rows, strings.Join(columns, "|"))
			}
		})
	})
	return rows
}

// The holders read lists every column of every row exactly as the reference.
func snReconcileTestRequireHoldersReference(t testing.TB, ctx context.Context) {
	t.Helper()
	got, want := snReconcileTestHoldersListing(t, ctx, snReconcileHoldersSql), snReconcileTestHoldersListing(t, ctx, snReconcileHoldersReferenceSql)
	if len(got) != len(want) {
		t.Fatalf("the holders read lists %d rows, the reference %d:\n got %s\nwant %s", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("holder %d differs from the reference:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}

// Each listed row's early revenue as `sql` reads it.
func snReconcileTestHoldersEarlyRevenue(t testing.TB, ctx context.Context, sql string) map[server.Id]NanoCents {
	t.Helper()
	revenue := map[server.Id]NanoCents{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT h.payment_id,h.early_revenue FROM (`+sql+`) h(payment_id,payment_plan_id,network_id,state,
			payout_nano_cents,payout_byte_count,subsidy_payout_nano_cents,reliability_subsidy_nano_cents,early_revenue,early_bytes,early_bonus)`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				var early NanoCents
				server.Raise(result.Scan(&paymentId, &early))
				revenue[paymentId] = early
			}
		})
	})
	return revenue
}

// The root of the plan EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) ran for the
// holders read `sql`.
func snReconcileTestHoldersPlan(t testing.TB, ctx context.Context, sql string) map[string]any {
	t.Helper()
	var plan string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+sql).Scan(&plan))
	})
	var explained []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(plan), &explained); err != nil || len(explained) != 1 {
		t.Fatal("explain", err, plan)
	}
	return explained[0].Plan
}

// The holders read `sql`'s plan as text, for a failure message.
func snReconcileTestHoldersPlanText(t testing.TB, ctx context.Context, sql string) string {
	t.Helper()
	lines := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) `+sql)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var line string
				server.Raise(result.Scan(&line))
				lines = append(lines, line)
			}
		})
	})
	return strings.Join(lines, "\n")
}

// A contract with an outcome and no close time, as legacy settlement left
// some, created an hour before `swept` with its id minted at `minted`. The
// usage guard now refuses one, so it is suspended in this test database only.
func snReconcileTestUnclosedSettled(t testing.TB, ctx context.Context, cohort *payoutTransitionCohort, minted time.Time, swept time.Time, revenue NanoCents) server.Id {
	t.Helper()
	contractId := contractIdMintedAt(minted)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_contract DISABLE TRIGGER transfer_contract_usage_guard`))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
			(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,usage_origin_is_source)
			VALUES($1,$2,$3,$4,$5,10,$6,NULL,'settled',true)`, contractId, cohort.sourceNetwork, cohort.source, cohort.network, cohort.client, swept.Add(-time.Hour)))
		server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_contract ENABLE TRIGGER transfer_contract_usage_guard`))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow_sweep
			(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
			VALUES($1,$2,$3,$4,10,$5,$6)`, contractId, cohort.balance, cohort.network, cohort.client, revenue, swept))
	})
	return contractId
}

// A sweep of an open contract, which has neither an outcome nor a close time.
func snReconcileTestOpenSwept(t testing.TB, ctx context.Context, cohort *payoutTransitionCohort, minted time.Time, swept time.Time, revenue NanoCents) server.Id {
	t.Helper()
	return cohort.insertWithId(t, ctx, contractIdMintedAt(minted), swept.Add(-time.Hour), nil, swept, 10, revenue)
}

// A sweep whose contract `contractId` does not exist.
func snReconcileTestOrphanSweep(t testing.TB, ctx context.Context, cohort *payoutTransitionCohort, contractId server.Id, swept time.Time, revenue NanoCents) server.Id {
	t.Helper()
	snReconcileTestExec(t, ctx, `INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
		VALUES($1,$2,$3,$4,10,$5,$6)`, contractId, cohort.balance, cohort.network, cohort.client, revenue, swept)
	return contractId
}

// The floor below which the holders read finds a sweep without its contract,
// when it runs now: snReconcileSweepContractIdFloorSql.
func snReconcileTestSweepContractFloor() time.Time {
	floor := server.NowUtc().Add(-server.ContractRetention)
	if floor.Before(snReconcileSweepContractRetainedTime) {
		return snReconcileSweepContractRetainedTime
	}
	return floor
}

// A2 (§6): the holders read lists every column of every row exactly as the
// reference, over each way a row's sweeps are read:
//   - a bound of 'infinity', where every sweep is early: one closed, one
//     without a close time, one without a contract;
//   - a bound from the epoch's start, with a sweep that closed at it, one that
//     closed after it within the id bound, and one past the id bound;
//   - a bound raised by an earlier row without an epoch;
//   - past the id bound, a contract with an outcome and no close time, an open
//     contract, and one without a close time whose two sweeps are early in two
//     networks' rows;
//   - within it, a contract without a close time, counted once;
//   - an id a millisecond short of the margin ahead of its close;
//   - a bound before the contract floor, with a sweep without its contract
//     between the id bound and the floor;
//   - a completed row holding a sweep without a close time, a bonus moved
//     from another row, and rows without early sweeps.
//
// Every contract id is minted at the contract's creation unless the case says
// otherwise.
func TestSnReconcileHoldersMatchReference(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := 24 * time.Hour
		now := server.NowUtc().Truncate(time.Hour)
		bound := now.Add(-20 * day)
		floor := snReconcileTestSweepContractFloor()
		// A settled contract closed at `closed`, created an hour before it.
		closedAt := func(cohort *payoutTransitionCohort, closed time.Time, revenue NanoCents) server.Id {
			return cohort.insertMinted(t, ctx, closed.Add(-time.Hour), &closed, closed, 10, revenue)
		}
		payment := func(cohort *payoutTransitionCohort, at time.Time, contracts ...server.Id) server.Id {
			return snReconcileTestPayment(t, ctx, cohort.network, nil, at, contracts...)
		}
		// A planned row created at `at` whose plan's epoch started at `epoch`.
		planned := func(cohort *payoutTransitionCohort, at time.Time, epoch time.Time, contracts ...server.Id) server.Id {
			paymentId := payment(cohort, at, contracts...)
			snReconcileTestEpoch(t, ctx, paymentId, epoch)
			return paymentId
		}

		// 'infinity': the plan recorded no epoch.
		a := newPayoutTransitionCohort(t, ctx)
		infinite := payment(a, now.Add(-day), closedAt(a, now.Add(-2*day), UsdToNanoCents(1)),
			snReconcileTestUnclosedSettled(t, ctx, a, now.Add(-3*day), now.Add(-3*day), UsdToNanoCents(2)),
			snReconcileTestOrphanSweep(t, ctx, a, contractIdMintedAt(now.Add(-4*day)), now.Add(-4*day), UsdToNanoCents(4)))

		// The epoch's start: a sweep closed at it is early, one closed after it,
		// within the id bound or past it, is not.
		b := newPayoutTransitionCohort(t, ctx)
		atBound := planned(b, now.Add(-day), bound, closedAt(b, bound, UsdToNanoCents(1)),
			closedAt(b, bound.Add(2*time.Hour), UsdToNanoCents(2)), closedAt(b, bound.Add(5*day), UsdToNanoCents(4)))

		// An earlier row without an epoch raises the bound past the sweep.
		c := newPayoutTransitionCohort(t, ctx)
		payment(c, bound.Add(3*day))
		raised := planned(c, now.Add(-day), bound, closedAt(c, bound.Add(2*day), UsdToNanoCents(1)), closedAt(c, bound.Add(5*day), UsdToNanoCents(2)))

		// Past the id bound: a settled contract without a close time and an open
		// contract are early.
		d := newPayoutTransitionCohort(t, ctx)
		unknownPast := planned(d, now.Add(-day), bound, snReconcileTestUnclosedSettled(t, ctx, d, bound.Add(3*day), bound.Add(3*day), UsdToNanoCents(1)),
			snReconcileTestOpenSwept(t, ctx, d, bound.Add(4*day), bound.Add(4*day), UsdToNanoCents(2)), closedAt(d, bound.Add(5*day), UsdToNanoCents(4)))

		// One contract without a close time with a sweep in each of two networks.
		e, e2 := newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx)
		shared := snReconcileTestUnclosedSettled(t, ctx, e, bound.Add(3*day), bound.Add(3*day), UsdToNanoCents(1))
		snReconcileTestExec(t, ctx, `INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time)
			VALUES($1,$2,$3,$4,10,$5,$6)`, shared, e2.balance, e2.network, e2.client, UsdToNanoCents(2), bound.Add(3*day))
		sharedFirst, sharedSecond := planned(e, now.Add(-day), bound), planned(e2, now.Add(-day), bound)
		snReconcileTestExec(t, ctx, `UPDATE transfer_escrow_sweep SET payment_id=CASE WHEN network_id=$2 THEN $3::uuid ELSE $4::uuid END WHERE contract_id=$1`,
			shared, e.network, sharedFirst, sharedSecond)

		// Within the id bound: a contract without a close time counts once.
		g := newPayoutTransitionCohort(t, ctx)
		unknownWithin := planned(g, now.Add(-day), bound, snReconcileTestUnclosedSettled(t, ctx, g, bound.Add(-2*day), bound.Add(-day), UsdToNanoCents(1)))

		// The id ran a millisecond short of the margin ahead of its close at the bound.
		h := newPayoutTransitionCohort(t, ctx)
		ahead := planned(h, now.Add(-day), bound, h.insertWithId(t, ctx, contractIdMintedAt(bound.Add(snReconcileContractIdMargin-time.Millisecond)),
			bound.Add(-time.Hour), &bound, bound, 10, UsdToNanoCents(1)))

		// A bound before the contract floor: below the floor a sweep may lack its
		// contract, and that sweep is early.
		k := newPayoutTransitionCohort(t, ctx)
		old := floor.Add(-60 * day)
		belowFloor := planned(k, now.Add(-day), old, snReconcileTestOrphanSweep(t, ctx, k, contractIdMintedAt(floor.Add(-10*day)), floor.Add(-10*day), UsdToNanoCents(1)),
			closedAt(k, floor.Add(-5*day), UsdToNanoCents(2)), closedAt(k, floor.Add(5*day), UsdToNanoCents(4)))

		// Not listed: a completed row, rows without early sweeps or any sweep; and
		// a bonus moved from another row is listed.
		m := newPayoutTransitionCohort(t, ctx)
		completed := planned(m, now.Add(-day), bound, snReconcileTestUnclosedSettled(t, ctx, m, bound.Add(3*day), bound.Add(3*day), UsdToNanoCents(1)))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET completed=true,complete_time=$2 WHERE payment_id=$1`, completed, now)
		lateOnly := planned(m, now.Add(-day), bound, closedAt(m, bound.Add(5*day), UsdToNanoCents(1)))
		empty := planned(m, now.Add(-day), bound)
		bonusTarget := planned(m, now.Add(-day), bound, closedAt(m, bound.Add(6*day), UsdToNanoCents(1)))
		bonusOrigin := planned(m, now.Add(-2*day), bound)
		snReconcileTestMoveBonus(t, ctx, bonusOrigin, bonusTarget, UsdToNanoCents(8))

		snReconcileTestRequireHoldersReference(t, ctx)
		// The fixture reaches every branch it claims to.
		early := snReconcileTestHoldersEarlyRevenue(t, ctx, snReconcileHoldersSql)
		for paymentId, want := range map[server.Id]NanoCents{infinite: UsdToNanoCents(7), atBound: UsdToNanoCents(1), raised: UsdToNanoCents(1),
			unknownPast: UsdToNanoCents(3), sharedFirst: UsdToNanoCents(1), sharedSecond: UsdToNanoCents(2), unknownWithin: UsdToNanoCents(1),
			ahead: UsdToNanoCents(1), belowFloor: UsdToNanoCents(1), bonusTarget: 0} {
			if got, listed := early[paymentId]; !listed || got != want {
				t.Fatal("early revenue", paymentId, listed, got, want)
			}
		}
		for _, paymentId := range []server.Id{completed, lateOnly, empty, bonusOrigin} {
			if _, listed := early[paymentId]; listed {
				t.Fatal("a row without early earnings is listed", paymentId)
			}
		}
	})
}

// A2 (§6): the documented boundaries of the holders read. Past a row's id
// bound it reads only the contracts without a close time, so it does not find
// a sweep there that the reference finds early, where:
//   - the sweep has no contract, which retention has not left since the
//     contract floor (snReconcileSweepContractRetainedTime);
//   - its contract closed at the bound with an id the margin ahead of its
//     close, which no contract's id runs (snReconcileLaterSweepIdBoundSql);
//   - its contract is disputed without a close time, which the dispute never
//     leaves.
//
// A contract id a millisecond short of the margin ahead is still read.
func TestSnReconcileHoldersBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := 24 * time.Hour
		now := server.NowUtc().Truncate(time.Hour)
		bound := now.Add(-20 * day)
		// A planned row whose plan's epoch started at the bound, holding one sweep.
		holder := func(sweep func(cohort *payoutTransitionCohort) server.Id) server.Id {
			cohort := newPayoutTransitionCohort(t, ctx)
			paymentId := snReconcileTestPayment(t, ctx, cohort.network, nil, now.Add(-day), sweep(cohort))
			snReconcileTestEpoch(t, ctx, paymentId, bound)
			return paymentId
		}
		orphan := holder(func(cohort *payoutTransitionCohort) server.Id {
			return snReconcileTestOrphanSweep(t, ctx, cohort, contractIdMintedAt(bound.Add(3*day)), bound.Add(3*day), UsdToNanoCents(1))
		})
		ahead := func(by time.Duration) func(cohort *payoutTransitionCohort) server.Id {
			return func(cohort *payoutTransitionCohort) server.Id {
				return cohort.insertWithId(t, ctx, contractIdMintedAt(bound.Add(by)), bound.Add(-time.Hour), &bound, bound, 10, UsdToNanoCents(1))
			}
		}
		atMargin := holder(ahead(snReconcileContractIdMargin))
		inside := holder(ahead(snReconcileContractIdMargin - time.Millisecond))
		disputed := holder(func(cohort *payoutTransitionCohort) server.Id {
			contractId := cohort.insertWithId(t, ctx, contractIdMintedAt(bound.Add(3*day)), bound.Add(3*day), nil, bound.Add(3*day), 10, UsdToNanoCents(1))
			snReconcileTestExec(t, ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, contractId)
			return contractId
		})

		reference, listed := snReconcileTestHoldersEarlyRevenue(t, ctx, snReconcileHoldersReferenceSql), snReconcileTestHoldersEarlyRevenue(t, ctx, snReconcileHoldersSql)
		for _, paymentId := range []server.Id{orphan, atMargin, inside, disputed} {
			if reference[paymentId] != UsdToNanoCents(1) {
				t.Fatal("the reference does not find the early sweep", paymentId, reference)
			}
		}
		if listed[inside] != UsdToNanoCents(1) || len(listed) != 1 {
			t.Fatal("the holders read across its boundaries", listed, inside)
		}
	})
}

// A2 (§6): the holders read reads a row's sweeps only below its id bound,
// with the sweeps of contracts without a close time. A planned row whose
// plan's epoch started 20 days ago holds 50,000 sweeps of contracts created
// from two days after its epoch started, each closed an hour after its
// creation. Five more are of contracts created within the day after the
// epoch started, which closed after it. Three settled contracts without a
// close time and one open contract, created later, hold its four early
// sweeps. 2,000 sweeps of other payments give the statistics main has, where
// a payment holds few of the sweeps. The read checks the five sweeps against
// their contracts and reads the four contracts without a close time, 9
// contract rows in all; the reference read a contract for each of the 50,009.
func TestSnReconcileHoldersReadsBoundedSweeps(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := 24 * time.Hour
		f := newPayoutTransitionCohort(t, ctx)
		now := server.NowUtc().Truncate(time.Hour)
		epoch := now.Add(-20 * day)
		const inRangeCount, pastCount, unknownCount, otherCount = 5, 50000, 4, 2000
		contracts := []server.Id{}
		for i := range inRangeCount {
			created := epoch.Add(time.Duration(i+1) * time.Hour)
			closed := created.Add(time.Hour)
			contracts = append(contracts, f.insertMinted(t, ctx, created, &closed, closed, 10, UsdToNanoCents(1)))
		}
		for i := range unknownCount - 1 {
			at := epoch.Add(3*day + time.Duration(i)*time.Hour)
			contracts = append(contracts, snReconcileTestUnclosedSettled(t, ctx, f, at, at, UsdToNanoCents(2)))
		}
		contracts = append(contracts, snReconcileTestOpenSwept(t, ctx, f, epoch.Add(4*day), epoch.Add(4*day), UsdToNanoCents(2)))
		holder := snReconcileTestPayment(t, ctx, f.network, nil, now.Add(-day), contracts...)
		snReconcileTestEpoch(t, ctx, holder, epoch)
		// One contract every 30 seconds, each minted at its creation and closed an
		// hour after it. The last closes about 14 hours before now.
		usage := &contractUsageSnapshot{Version: 1, ByteCount: 10, Providers: []contractProviderUsage{{ClientId: f.client, NetworkId: f.network, ByteCount: 10}}}
		snReconcileTestExec(t, ctx, `WITH minted AS (
				INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,close_time,outcome,provider_usage,usage_origin_is_source)
				SELECT (lpad(to_hex(floor(extract(epoch FROM created AT TIME ZONE 'UTC')*1000)::bigint),12,'0')||lpad(to_hex(n),20,'0'))::uuid,
					$1,$2,$3,$4,10,created,created+interval '1 hour','settled',$5,true
				FROM (SELECT n,$6::timestamp+n*interval '30 seconds' AS created FROM generate_series(0,$7::int-1) n) contract
				RETURNING contract_id,close_time
			)
			INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time,payment_id)
			SELECT contract_id,$8,$3,$4,10,1,close_time,$9 FROM minted`,
			f.sourceNetwork, f.source, f.network, f.client, usage, epoch.Add(2*day), pastCount, f.balance, holder)
		// one sweep each of other payments, with ids minted over the same days
		snReconcileTestExec(t, ctx, `INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time,payment_id)
			SELECT (lpad(to_hex(floor(extract(epoch FROM swept AT TIME ZONE 'UTC')*1000)::bigint),12,'0')||'ffff'||lpad(to_hex(n),16,'0'))::uuid,$1,$2,$3,10,1,swept,gen_random_uuid()
			FROM (SELECT n,$4::timestamp+n*interval '10 minutes' AS swept FROM generate_series(0,$5::int-1) n) other`,
			f.balance, f.network, f.client, epoch, otherCount)
		snReconcileTestExec(t, ctx, `ANALYZE transfer_contract,transfer_escrow_sweep,account_payment,subsidy_payment,account_payment_bonus`)

		snReconcileTestRequireHoldersReference(t, ctx)
		if early := snReconcileTestHoldersEarlyRevenue(t, ctx, snReconcileHoldersSql); len(early) != 1 || early[holder] != UsdToNanoCents(2)*unknownCount {
			t.Fatal("early revenue", early)
		}
		if read := snReconcileTestContractRowsRead(snReconcileTestHoldersPlan(t, ctx, snReconcileHoldersReferenceSql)); read < inRangeCount+pastCount+unknownCount {
			t.Fatalf("the reference read %v contract rows, fewer than the row's %d sweeps", read, inRangeCount+pastCount+unknownCount)
		}
		plan := snReconcileTestHoldersPlan(t, ctx, snReconcileHoldersSql)
		probed, read := snReconcileTestSweepRowsProbed(plan), snReconcileTestContractRowsRead(plan)
		if probed != inRangeCount || read != inRangeCount+unknownCount {
			t.Fatalf("the holders read checked %v sweep rows against their contracts and read %v contract rows, want %d and %d:\n%s",
				probed, read, inRangeCount, inRangeCount+unknownCount, snReconcileTestHoldersPlanText(t, ctx, snReconcileHoldersSql))
		}
		t.Log("plan:\n" + snReconcileTestHoldersPlanText(t, ctx, snReconcileHoldersSql))
	})
}
