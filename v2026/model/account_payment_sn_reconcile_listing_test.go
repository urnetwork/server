// The stuck-row listing reads each later row's sweeps once, and returns
// exactly what the per-row listing it replaced returned.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The listing as it read through 2026-10-26: each stuck row without sweeps
// read every sweep of every later row of its network. Kept verbatim as the
// reference the listing must match row for row.
var snReconcileStuckReferenceSql = `SELECT p.payment_id,p.payment_plan_id,p.network_id,p.wallet_id,p.payout_byte_count,p.payout_nano_cents,
	p.subsidy_payout_nano_cents,p.reliability_subsidy_nano_cents,p.bonus_payout_nano_cents,p.attribution_review_required,
	p.create_time,p.canceled,p.cancel_time,p.circle_idempotency_key,p.payment_record,p.tx_hash,p.token_type,p.token_amount,
	p.payment_time,p.payment_receipt,pinned.wallet_address,payout.wallet_id,payout.wallet_address,payout.blockchain,
	COALESCE(payout.active,false),(SELECT COUNT(*) FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id),
	(SELECT MIN(c.close_time) FROM transfer_escrow_sweep s JOIN transfer_contract c ON c.contract_id=s.contract_id WHERE s.payment_id=p.payment_id),
	EXISTS (SELECT 1 FROM ` + fmt.Sprintf(snReconcileEarlySweepsSql, "p", "eb.bound") + `)
	OR EXISTS (SELECT 1 FROM account_payment_bonus b WHERE b.payment_id=p.payment_id AND b.origin_payment_id<>p.payment_id),
	CASE WHEN EXISTS (SELECT 1 FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id) THEN false ELSE EXISTS (
		SELECT 1 FROM account_payment later CROSS JOIN LATERAL (SELECT ` + fmt.Sprintf(snReconcileEarlyBoundSql, "later") + ` AS bound) later_bound
		JOIN transfer_escrow_sweep s ON s.payment_id=later.payment_id JOIN transfer_contract c ON c.contract_id=s.contract_id
		WHERE later.network_id=p.network_id AND later.create_time>COALESCE(p.cancel_time,p.create_time) AND c.close_time<p.create_time AND c.close_time<=later_bound.bound) END,
	CASE WHEN EXISTS (SELECT 1 FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id) THEN false ELSE EXISTS (
		SELECT 1 FROM account_payment later WHERE later.network_id=p.network_id
		AND later.create_time>COALESCE(p.cancel_time,p.create_time) AND later.create_time<=$1) END
	FROM account_payment p
	CROSS JOIN LATERAL (SELECT ` + fmt.Sprintf(snReconcileEarlyBoundSql, "p") + ` AS bound) eb
	LEFT JOIN account_wallet pinned ON pinned.wallet_id=p.wallet_id
	LEFT JOIN payout_wallet pw ON pw.network_id=p.network_id
	LEFT JOIN account_wallet payout ON payout.wallet_id=pw.wallet_id AND payout.network_id=p.network_id
	WHERE NOT p.completed AND NOT ` + legacyProviderPaymentReplacedSql("p.payment_id") + ` AND (
		(p.canceled AND (p.circle_idempotency_key IS NOT NULL OR p.payment_record IS NOT NULL OR p.tx_hash IS NOT NULL)) OR
		(NOT p.canceled AND p.circle_idempotency_key IS NOT NULL AND p.payment_record IS NULL AND p.tx_hash IS NULL
		AND NOT EXISTS (SELECT 1 FROM audit_account_payment a WHERE a.event_id=p.circle_idempotency_key)))
	ORDER BY p.create_time,p.payment_id`

// The listing as it read through 2026-10-27: each later row's earliest
// contract close was read once, over every sweep of the row. Kept verbatim as
// the reference the id-bounded listing must match row for row.
var snReconcileStuckEarliestCloseReferenceSql = `WITH stuck AS MATERIALIZED (
	SELECT p.* FROM account_payment p
	WHERE NOT p.completed AND NOT ` + legacyProviderPaymentReplacedSql("p.payment_id") + ` AND (
		(p.canceled AND (p.circle_idempotency_key IS NOT NULL OR p.payment_record IS NOT NULL OR p.tx_hash IS NOT NULL)) OR
		(NOT p.canceled AND p.circle_idempotency_key IS NOT NULL AND p.payment_record IS NULL AND p.tx_hash IS NULL
		AND NOT EXISTS (SELECT 1 FROM audit_account_payment a WHERE a.event_id=p.circle_idempotency_key)))
), later_rows AS MATERIALIZED (
	SELECT DISTINCT later.payment_id,later.payment_plan_id,later.network_id,later.create_time FROM stuck p
	JOIN account_payment later ON later.network_id=p.network_id AND later.create_time>COALESCE(p.cancel_time,p.create_time)
	WHERE NOT EXISTS (SELECT 1 FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id)
), later_close AS MATERIALIZED (
	SELECT later.network_id,later.create_time,later_bound.bound,
		(SELECT MIN(c.close_time) FROM transfer_escrow_sweep s JOIN transfer_contract c ON c.contract_id=s.contract_id WHERE s.payment_id=later.payment_id) AS earliest_close
	FROM later_rows later CROSS JOIN LATERAL (SELECT ` + fmt.Sprintf(snReconcileEarlyBoundSql, "later") + ` AS bound) later_bound
)
	SELECT p.payment_id,p.payment_plan_id,p.network_id,p.wallet_id,p.payout_byte_count,p.payout_nano_cents,
	p.subsidy_payout_nano_cents,p.reliability_subsidy_nano_cents,p.bonus_payout_nano_cents,p.attribution_review_required,
	p.create_time,p.canceled,p.cancel_time,p.circle_idempotency_key,p.payment_record,p.tx_hash,p.token_type,p.token_amount,
	p.payment_time,p.payment_receipt,pinned.wallet_address,payout.wallet_id,payout.wallet_address,payout.blockchain,
	COALESCE(payout.active,false),(SELECT COUNT(*) FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id),
	(SELECT MIN(c.close_time) FROM transfer_escrow_sweep s JOIN transfer_contract c ON c.contract_id=s.contract_id WHERE s.payment_id=p.payment_id),
	EXISTS (SELECT 1 FROM ` + fmt.Sprintf(snReconcileEarlySweepsSql, "p", "eb.bound") + `)
	OR EXISTS (SELECT 1 FROM account_payment_bonus b WHERE b.payment_id=p.payment_id AND b.origin_payment_id<>p.payment_id),
	CASE WHEN EXISTS (SELECT 1 FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id) THEN false ELSE EXISTS (
		SELECT 1 FROM later_close later WHERE later.network_id=p.network_id AND later.create_time>COALESCE(p.cancel_time,p.create_time)
		AND later.earliest_close<p.create_time AND later.earliest_close<=later.bound) END,
	CASE WHEN EXISTS (SELECT 1 FROM transfer_escrow_sweep s WHERE s.payment_id=p.payment_id) THEN false ELSE EXISTS (
		SELECT 1 FROM account_payment later WHERE later.network_id=p.network_id
		AND later.create_time>COALESCE(p.cancel_time,p.create_time) AND later.create_time<=$1) END
	FROM stuck p
	CROSS JOIN LATERAL (SELECT ` + fmt.Sprintf(snReconcileEarlyBoundSql, "p") + ` AS bound) eb
	LEFT JOIN account_wallet pinned ON pinned.wallet_id=p.wallet_id
	LEFT JOIN payout_wallet pw ON pw.network_id=p.network_id
	LEFT JOIN account_wallet payout ON payout.wallet_id=pw.wallet_id AND payout.network_id=p.network_id
	ORDER BY p.create_time,p.payment_id`

// The listing returns every column of every row exactly as both references do.
func snReconcileTestRequireReferenceListing(t testing.TB, ctx context.Context) {
	t.Helper()
	got := snReconcileTestListing(t, ctx, snReconcileStuckSql)
	for name, sql := range map[string]string{"per-row": snReconcileStuckReferenceSql, "earliest-close": snReconcileStuckEarliestCloseReferenceSql} {
		want := snReconcileTestListing(t, ctx, sql)
		if len(got) != len(want) {
			t.Fatalf("listing has %d rows, the %s reference %d", len(got), name, len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("row %d differs from the %s reference:\n got %s\nwant %s", i, name, got[i], want[i])
			}
		}
	}
}

// A5: each listed row's replan evidence as `sql` reads it.
func snReconcileTestReplanEvidence(t testing.TB, ctx context.Context, sql string) map[server.Id]bool {
	t.Helper()
	evidence := map[server.Id]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT l.payment_id,l.replan_evidence FROM (`+sql+`) l(payment_id,payment_plan_id,network_id,wallet_id,payout_byte_count,
			payout_nano_cents,subsidy_payout_nano_cents,reliability_subsidy_nano_cents,bonus_payout_nano_cents,attribution_review_required,create_time,canceled,
			cancel_time,circle_idempotency_key,payment_record,tx_hash,token_type,token_amount,payment_time,payment_receipt,pinned_wallet_address,payout_wallet_id,
			payout_wallet_address,payout_wallet_blockchain,payout_wallet_active,sweep_count,earliest_close,early_earnings,replan_evidence,replanned_before_fix)`,
			SnReconcileReplanFixTime)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				var replanEvidence bool
				server.Raise(result.Scan(&paymentId, &replanEvidence))
				evidence[paymentId] = replanEvidence
			}
		})
	})
	return evidence
}

// Every column of every listed row, in order, as text.
func snReconcileTestListing(t testing.TB, ctx context.Context, sql string) []string {
	t.Helper()
	rows := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, sql, SnReconcileReplanFixTime)
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

// The root of the plan EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) ran for the
// listing `sql`.
func snReconcileTestAnalyzedPlan(t testing.TB, ctx context.Context, sql string) map[string]any {
	t.Helper()
	var plan string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) `+sql, SnReconcileReplanFixTime).Scan(&plan))
	})
	var explained []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(plan), &explained); err != nil || len(explained) != 1 {
		t.Fatal("explain", err, plan)
	}
	return explained[0].Plan
}

// The listing `sql`'s plan as text, for a failure message.
func snReconcileTestPlanText(t testing.TB, ctx context.Context, sql string) string {
	t.Helper()
	lines := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF, TIMING OFF) `+sql, SnReconcileReplanFixTime)
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

// The rows a scan returned and the rows its filters removed, over every loop.
// EXPLAIN shows each per loop and rounded, so the total is exact when every
// loop of the scan reads the same number of rows.
func snReconcileTestRowsRead(node map[string]any) float64 {
	perLoop := 0.0
	for _, key := range []string{"Actual Rows", "Rows Removed by Filter", "Rows Removed by Index Recheck"} {
		value, _ := node[key].(float64)
		perLoop += value
	}
	loops, _ := node["Actual Loops"].(float64)
	return perLoop * loops
}

// Whether a plan node or any node below it reads transfer_contract.
func snReconcileTestReadsContracts(node map[string]any) bool {
	if node["Relation Name"] == "transfer_contract" {
		return true
	}
	children, _ := node["Plans"].([]any)
	for _, child := range children {
		if snReconcileTestReadsContracts(child.(map[string]any)) {
			return true
		}
	}
	return false
}

// The transfer_contract rows a plan read, over every loop and every scan of
// the table. Reading one contract per sweep was the listing's cost on main.
func snReconcileTestContractRowsRead(plan map[string]any) float64 {
	var visit func(node map[string]any) float64
	visit = func(node map[string]any) (read float64) {
		if node["Relation Name"] == "transfer_contract" {
			read += snReconcileTestRowsRead(node)
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			read += visit(child.(map[string]any))
		}
		return
	}
	return math.Round(visit(plan))
}

// The sweep rows a plan read to check their contracts, over every loop: the
// rows read by each scan of transfer_escrow_sweep whose subplan reads
// transfer_contract.
func snReconcileTestSweepRowsProbed(plan map[string]any) float64 {
	var visit func(node map[string]any) float64
	visit = func(node map[string]any) (probed float64) {
		children, _ := node["Plans"].([]any)
		if node["Relation Name"] == "transfer_escrow_sweep" {
			for _, value := range children {
				if child := value.(map[string]any); child["Parent Relationship"] == "SubPlan" && snReconcileTestReadsContracts(child) {
					probed += snReconcileTestRowsRead(node)
					break
				}
			}
		}
		for _, child := range children {
			probed += visit(child.(map[string]any))
		}
		return
	}
	return math.Round(visit(plan))
}

// A5, A10, A8, RC-1, RC-2 and RC-22: the listing returns every column of every
// row exactly as the per-row and earliest-close listings did, over each way a
// row is listed or left out and each way a later row bears on its evidence. A
// later row's earliest close lying between two stuck rows' creations is the
// case a shared read must keep apart. Every contract id is minted at the
// contract's creation, as connect.NewId mints it.
func TestSnReconcileStuckListingMatchesReference(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := 24 * time.Hour
		july := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		sept := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		closedAt := func(cohort *payoutTransitionCohort, at time.Time) server.Id {
			return cohort.insertMinted(t, ctx, at.Add(-time.Hour), &at, at, 10, UsdToNanoCents(1))
		}
		payment := func(cohort *payoutTransitionCohort, at time.Time, contracts ...server.Id) server.Id {
			return snReconcileTestPayment(t, ctx, cohort.network, nil, at, contracts...)
		}
		cancelWithRecord := func(paymentId server.Id, at time.Time) {
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=$2,payment_record='record-'||payment_id WHERE payment_id=$1`, paymentId, at)
		}
		cancelWithKey := func(paymentId server.Id, at time.Time) {
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=$2,circle_idempotency_key=$3 WHERE payment_id=$1`, paymentId, at, server.NewId())
		}
		// a hash and no cancel time: the later rows count from its creation
		cancelWithHash := func(paymentId server.Id) {
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,tx_hash='0xhash-'||payment_id WHERE payment_id=$1`, paymentId)
		}
		reserveKey := func(paymentId server.Id) server.Id {
			key := server.NewId()
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET circle_idempotency_key=$2 WHERE payment_id=$1`, paymentId, key)
			return key
		}

		// Before the fix: every later row also re-planned.
		a := newPayoutTransitionCohort(t, ctx)
		julyFirst := payment(a, july.Add(day))
		cancelWithRecord(julyFirst, july.Add(2*day))
		julySecond := payment(a, july.Add(10*day))
		cancelWithHash(julySecond)
		julyLater := payment(a, july.Add(20*day), closedAt(a, july.Add(5*day)), closedAt(a, july.Add(15*day)))
		snReconcileTestEpoch(t, ctx, julyLater, july.Add(30*day))

		// After the fix: one later row whose earliest close lies between two
		// stuck rows' creations is evidence for the second only.
		b := newPayoutTransitionCohort(t, ctx)
		first := payment(b, sept.Add(day))
		cancelWithRecord(first, sept.Add(2*day))
		second := payment(b, sept.Add(10*day))
		cancelWithHash(second)
		shared := payment(b, sept.Add(20*day), closedAt(b, sept.Add(5*day)), closedAt(b, sept.Add(15*day)))
		snReconcileTestEpoch(t, ctx, shared, sept.Add(30*day))

		// The later row's own epoch explains its sweep. The stuck row records an
		// epoch, so it raises no bound.
		c := newPayoutTransitionCohort(t, ctx)
		explained := payment(c, sept.Add(day))
		snReconcileTestEpoch(t, ctx, explained, sept.Add(-30*day))
		cancelWithKey(explained, sept.Add(2*day))
		explainedLater := payment(c, sept.Add(10*day), closedAt(c, sept.Add(-5*day)))
		snReconcileTestEpoch(t, ctx, explainedLater, sept.Add(-10*day))

		// A later row whose plan recorded no epoch: every sweep is early.
		d := newPayoutTransitionCohort(t, ctx)
		withoutEpoch := payment(d, sept.Add(day))
		snReconcileTestEpoch(t, ctx, withoutEpoch, sept.Add(-30*day))
		cancelWithRecord(withoutEpoch, sept.Add(2*day))
		payment(d, sept.Add(10*day), closedAt(d, sept.Add(-5*day)))

		// An earlier row without an epoch raises the later row's bound past its sweep.
		e := newPayoutTransitionCohort(t, ctx)
		raised := payment(e, sept.Add(day))
		snReconcileTestEpoch(t, ctx, raised, sept.Add(-30*day))
		cancelWithRecord(raised, sept.Add(2*day))
		payment(e, sept.Add(5*day))
		raisedLater := payment(e, sept.Add(10*day), closedAt(e, sept.Add(-5*day)))
		snReconcileTestEpoch(t, ctx, raisedLater, sept.Add(-20*day))

		// The later row's contract never closed, and another sweep has no contract.
		f := newPayoutTransitionCohort(t, ctx)
		unclosed := payment(f, sept.Add(day))
		snReconcileTestEpoch(t, ctx, unclosed, sept.Add(-30*day))
		cancelWithRecord(unclosed, sept.Add(2*day))
		unclosedLater := payment(f, sept.Add(10*day), f.insertMinted(t, ctx, sept.Add(-6*day), nil, sept.Add(-5*day), 10, UsdToNanoCents(1)))
		snReconcileTestExec(t, ctx, `INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time,payment_id)
			VALUES($1,$2,$3,$4,10,1,$5,$6)`, contractIdMintedAt(sept.Add(-6*day)), f.balance, f.network, f.client, sept.Add(-5*day), unclosedLater)

		// Only a row created before the cancellation, and a row of another
		// network, hold early sweeps.
		g, other := newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx)
		notLater := payment(g, sept.Add(day))
		snReconcileTestEpoch(t, ctx, notLater, sept.Add(-30*day))
		cancelWithRecord(notLater, sept.Add(20*day))
		payment(g, sept.Add(10*day), closedAt(g, sept.Add(-5*day)))
		payment(other, sept.Add(25*day), closedAt(other, sept.Add(-5*day)))

		// A row later than one stuck row and earlier than another is evidence for
		// the first only, though the shared read holds it for their network.
		n := newPayoutTransitionCohort(t, ctx)
		beforeHolder := payment(n, sept.Add(day))
		cancelWithRecord(beforeHolder, sept.Add(2*day))
		payment(n, sept.Add(5*day), closedAt(n, sept.Add(-5*day)))
		afterHolder := payment(n, sept.Add(10*day))
		cancelWithRecord(afterHolder, sept.Add(11*day))

		// Rows that hold sweeps of their own, RC-2 included, and a moved bonus.
		h := newPayoutTransitionCohort(t, ctx)
		withSweeps := payment(h, july.Add(3*day), closedAt(h, july.Add(day)))
		snReconcileTestEpoch(t, ctx, withSweeps, july)
		cancelWithKey(withSweeps, july.Add(4*day))
		unretained := payment(h, sept.Add(3*day), closedAt(h, sept.Add(2*day)))
		reserveKey(unretained)
		// the planner moves a correction before any submission
		bonusTarget := payment(h, sept.Add(4*day), closedAt(h, sept.Add(3*day)))
		bonusOrigin := payment(h, sept.Add(2*day))
		snReconcileTestMoveBonus(t, ctx, bonusOrigin, bonusTarget, UsdToNanoCents(2))
		cancelWithKey(bonusTarget, sept.Add(5*day))

		// RC-2 without sweeps reads the evidence too.
		k := newPayoutTransitionCohort(t, ctx)
		unretainedBare := payment(k, sept.Add(day))
		snReconcileTestEpoch(t, ctx, unretainedBare, sept.Add(-30*day))
		reserveKey(unretainedBare)
		payment(k, sept.Add(10*day), closedAt(k, sept.Add(-5*day)))

		// Out of scope: completed, safely canceled, journaled, replaced and in flight.
		m := newPayoutTransitionCohort(t, ctx)
		completed := payment(m, sept.Add(day))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET completed=true,complete_time=$2,circle_idempotency_key=$3 WHERE payment_id=$1`, completed, sept.Add(2*day), server.NewId())
		safelyCanceled := payment(m, sept.Add(2*day))
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=$2 WHERE payment_id=$1`, safelyCanceled, sept.Add(3*day))
		journaled := payment(m, sept.Add(3*day))
		journaledKey := reserveKey(journaled)
		snReconcileTestExec(t, ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$2,'circle_attempt_request','{}')`, journaledKey, journaled)
		replaced := payment(m, sept.Add(4*day))
		cancelWithKey(replaced, sept.Add(5*day))
		snReconcileTestExec(t, ctx, `INSERT INTO audit_account_payment(event_id,payment_id,event_type,event_details) VALUES($1,$1,$2,'{}')`, replaced, legacyProviderPaymentReplacedEventType)
		inFlight := payment(m, sept.Add(5*day))
		reserveKey(inFlight)
		snReconcileTestExec(t, ctx, `UPDATE account_payment SET payment_record='in-flight' WHERE payment_id=$1`, inFlight)

		snReconcileTestRequireReferenceListing(t, ctx)

		// The fixture reaches every branch it claims to.
		rows := snReconcileTestStuckById(t, ctx)
		type flags struct{ replanEvidence, replannedBeforeFix bool }
		for paymentId, expected := range map[server.Id]flags{
			julyFirst: {false, true}, julySecond: {true, true},
			first: {false, false}, second: {true, false},
			explained: {false, false}, withoutEpoch: {true, false}, raised: {true, false}, unclosed: {false, false}, notLater: {false, false},
			beforeHolder: {true, false}, afterHolder: {false, false},
			withSweeps: {false, false}, unretained: {false, false}, bonusTarget: {false, false}, unretainedBare: {true, false},
		} {
			row := rows[paymentId]
			if row == nil || (flags{row.ReplanEvidence, row.ReplannedBeforeFix}) != expected {
				t.Fatal("evidence", paymentId, row, expected)
			}
		}
		if rows[unretained].Kind != SnReconcileUnretainedKey || rows[unretainedBare].Kind != SnReconcileUnretainedKey || rows[second].Kind != SnReconcileCanceledWithEvidence ||
			!rows[bonusTarget].EarlyEarnings || rows[withSweeps].SweepCount != 1 {
			t.Fatal("kinds and earnings", rows[unretained], rows[unretainedBare], rows[second], rows[bonusTarget], rows[withSweeps])
		}
		for _, paymentId := range []server.Id{completed, safelyCanceled, journaled, replaced, inFlight, bonusOrigin} {
			if rows[paymentId] != nil {
				t.Fatal("out-of-scope row listed", paymentId)
			}
		}
	})
}

// A5, A10: the listing reads a later row's sweeps only below the contract id
// bound (snReconcileLaterSweepIdBoundSql), and returns every column of every
// row exactly as the per-row and earliest-close listings did, over each case
// the bound must keep: a sweep that closed at its later row's bound, from the
// epoch's start, before the stuck row was created; one that closed a
// millisecond after the bound; a contract that never closed and a sweep without
// a contract; a later row without an epoch, whose bound is 'infinity', with a
// sweep that closed a millisecond before the stuck row was created and one
// that closed when it was; a bound raised by an earlier row without an epoch;
// two later rows of which only the second holds the early sweep; and a
// contract whose id ran ahead of its close by a millisecond less than the
// margin.
func TestSnReconcileStuckListingIdBoundMatchesReference(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := 24 * time.Hour
		sept := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		created := sept.Add(day)
		bound := sept.Add(-10 * day)
		// A settled contract created an hour before it closed at `closed`, with
		// its id minted at `minted`.
		mintedAt := func(cohort *payoutTransitionCohort, minted time.Time, closed time.Time) server.Id {
			return cohort.insertWithId(t, ctx, contractIdMintedAt(minted), closed.Add(-time.Hour), &closed, closed, 10, UsdToNanoCents(1))
		}
		closedAt := func(cohort *payoutTransitionCohort, closed time.Time) server.Id {
			return mintedAt(cohort, closed.Add(-time.Hour), closed)
		}
		payment := func(cohort *payoutTransitionCohort, at time.Time, contracts ...server.Id) server.Id {
			return snReconcileTestPayment(t, ctx, cohort.network, nil, at, contracts...)
		}
		// A row created at `at` whose plan's epoch started at `epoch`.
		planned := func(cohort *payoutTransitionCohort, at time.Time, epoch time.Time, contracts ...server.Id) server.Id {
			paymentId := payment(cohort, at, contracts...)
			snReconcileTestEpoch(t, ctx, paymentId, epoch)
			return paymentId
		}
		// A stuck row without sweeps, created at `created` and canceled with a
		// record a day later. Its own epoch raises no later row's bound.
		stuck := func(cohort *payoutTransitionCohort) server.Id {
			paymentId := planned(cohort, created, sept.Add(-30*day))
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=$2,payment_record='record-'||payment_id WHERE payment_id=$1`, paymentId, created.Add(day))
			return paymentId
		}

		// The later row's epoch started at the bound, when its early sweep closed.
		a := newPayoutTransitionCohort(t, ctx)
		atBound := stuck(a)
		planned(a, sept.Add(10*day), bound, closedAt(a, bound), closedAt(a, bound.Add(5*day)))

		// Its sweep closed a millisecond after the bound.
		b := newPayoutTransitionCohort(t, ctx)
		afterBound := stuck(b)
		planned(b, sept.Add(10*day), bound, closedAt(b, bound.Add(time.Millisecond)), closedAt(b, bound.Add(5*day)))

		// The later row's contract never closed, and another sweep has no contract.
		c := newPayoutTransitionCohort(t, ctx)
		unclosed := stuck(c)
		unclosedLater := planned(c, sept.Add(10*day), bound, c.insertMinted(t, ctx, bound.Add(-2*day), nil, bound.Add(-day), 10, UsdToNanoCents(1)))
		snReconcileTestExec(t, ctx, `INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time,payment_id)
			VALUES($1,$2,$3,$4,10,1,$5,$6)`, contractIdMintedAt(bound.Add(-2*day)), c.balance, c.network, c.client, bound.Add(-day), unclosedLater)

		// The later row's plan recorded no epoch, so its bound is 'infinity' and
		// the stuck row's creation bounds the read.
		d, dAt := newPayoutTransitionCohort(t, ctx), newPayoutTransitionCohort(t, ctx)
		withoutEpoch := stuck(d)
		payment(d, sept.Add(10*day), closedAt(d, created.Add(-time.Millisecond)))
		withoutEpochAtCreation := stuck(dAt)
		payment(dAt, sept.Add(10*day), closedAt(dAt, created))

		// An earlier row without an epoch raises the later row's bound from its
		// epoch's start to that row's creation, when the sweep closed.
		e := newPayoutTransitionCohort(t, ctx)
		raised := stuck(e)
		payment(e, sept.Add(-3*day))
		planned(e, sept.Add(10*day), sept.Add(-20*day), closedAt(e, sept.Add(-3*day)))

		// Of two later rows only the second holds the early sweep. The first holds
		// one created within a day after its bound, so read, that closed after it.
		g := newPayoutTransitionCohort(t, ctx)
		secondLater := stuck(g)
		planned(g, sept.Add(10*day), bound, closedAt(g, bound.Add(2*time.Hour)), closedAt(g, bound.Add(5*day)))
		planned(g, sept.Add(20*day), sept.Add(10*day), closedAt(g, bound.Add(12*time.Hour)), closedAt(g, sept.Add(15*day)))

		// The early sweep's contract id ran a millisecond less than the margin
		// ahead of its close, at the bound.
		h := newPayoutTransitionCohort(t, ctx)
		ahead := stuck(h)
		planned(h, sept.Add(10*day), bound, mintedAt(h, bound.Add(snReconcileContractIdMargin-time.Millisecond), bound))

		snReconcileTestRequireReferenceListing(t, ctx)
		rows := snReconcileTestStuckById(t, ctx)
		for paymentId, want := range map[server.Id]bool{atBound: true, afterBound: false, unclosed: false, withoutEpoch: true,
			withoutEpochAtCreation: false, raised: true, secondLater: true, ahead: true} {
			if rows[paymentId] == nil || rows[paymentId].ReplanEvidence != want || rows[paymentId].ReplannedBeforeFix {
				t.Fatal("replan evidence", paymentId, rows[paymentId], want)
			}
		}
	})
}

// A5, A10: the documented boundary of the contract id bound. Each stuck row's
// later row holds one sweep that closed at the later row's bound, before the
// stuck row was created, so the listing reads ids below the floor of the
// bound plus the margin. A sweep whose contract id ran ahead of its close by
// the margin, or by more, is not read: the listing finds no evidence where
// both references find it. One that ran a millisecond less ahead is read.
func TestSnReconcileStuckListingIdBoundBoundary(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := 24 * time.Hour
		sept := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		bound := sept.Add(-10 * day)
		holder := func(ahead time.Duration) server.Id {
			cohort := newPayoutTransitionCohort(t, ctx)
			paymentId := snReconcileTestPayment(t, ctx, cohort.network, nil, sept.Add(day))
			snReconcileTestEpoch(t, ctx, paymentId, sept.Add(-30*day))
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=$2,payment_record='record-'||payment_id WHERE payment_id=$1`, paymentId, sept.Add(2*day))
			contractId := cohort.insertWithId(t, ctx, contractIdMintedAt(bound.Add(ahead)), bound.Add(-time.Hour), &bound, bound, 10, UsdToNanoCents(1))
			later := snReconcileTestPayment(t, ctx, cohort.network, nil, sept.Add(10*day), contractId)
			snReconcileTestEpoch(t, ctx, later, bound)
			return paymentId
		}
		inside := holder(snReconcileContractIdMargin - time.Millisecond)
		atMargin := holder(snReconcileContractIdMargin)
		beyond := holder(snReconcileContractIdMargin + time.Hour)

		for name, sql := range map[string]string{"per-row": snReconcileStuckReferenceSql, "earliest-close": snReconcileStuckEarliestCloseReferenceSql} {
			reference := snReconcileTestReplanEvidence(t, ctx, sql)
			if !reference[inside] || !reference[atMargin] || !reference[beyond] {
				t.Fatal("the "+name+" reference's evidence", reference[inside], reference[atMargin], reference[beyond])
			}
		}
		listed := snReconcileTestReplanEvidence(t, ctx, snReconcileStuckSql)
		if !listed[inside] || listed[atMargin] || listed[beyond] {
			t.Fatal("the listing's evidence across the boundary", listed[inside], listed[atMargin], listed[beyond])
		}
	})
}

// A5, A10: the bound is the id floor of the millisecond a margin after the
// earlier of the stuck row's creation and the later row's bound, in UTC
// whatever the session time zone. An 'infinity' bound or a NULL one leaves the
// stuck row's creation.
func TestSnReconcileLaterSweepIdBound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		created := time.Date(2026, 9, 1, 12, 30, 15, 987654000, time.UTC)
		earlier := created.Add(-10*24*time.Hour + 500*time.Microsecond)
		text := func(at time.Time) *string {
			value := at.Format("2006-01-02 15:04:05.999999")
			return &value
		}
		infinity := "infinity"
		for _, test := range []struct {
			bound *string
			want  time.Time
		}{
			{bound: text(earlier), want: earlier},
			{bound: text(created), want: created},
			{bound: text(created.Add(time.Hour)), want: created},
			{bound: &infinity, want: created},
			{bound: nil, want: created},
		} {
			for _, timeZone := range []string{"UTC", "America/Chicago"} {
				var bound server.Id
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL TIME ZONE '%s'`, timeZone)))
					server.Raise(tx.QueryRow(ctx, `SELECT `+snReconcileLaterSweepIdBoundSql+`
						FROM (SELECT $1::timestamp AS create_time) p, (SELECT $2::text::timestamp AS bound) later`, created, test.bound).Scan(&bound))
				})
				if want := ContractIdTimeBound(test.want.Add(snReconcileContractIdMargin)); bound != want {
					t.Fatalf("%s: bound %v gives %s, want %s", timeZone, test.bound, bound, want)
				}
			}
		}
	})
}

// A5, A10: the listing reads a later row's sweeps only below the contract id
// bound, so the size of the later row does not matter. Three stuck rows
// without sweeps, created an hour apart from `start` and canceled a day after
// it, share one later row whose plan's epoch started 20 days before `start`:
// its bound. 50,000 of its sweeps are of contracts created from two days
// after the bound, which closed after the bound and before any stuck row was
// created: on main most of a later row's sweeps lie between the two. Five more
// are of contracts created within a day after the bound, which also closed
// after it. None is evidence. 2,000 sweeps of other payments give the
// statistics main has, where a payment holds few of the sweeps. The listing
// reads the five sweeps and their contracts once per stuck row, 15 of each;
// the earliest-close listing read a contract for each of the 50,005.
func TestSnReconcileStuckListingReadsLaterSweepsBelowIdBound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := 24 * time.Hour
		f := newPayoutTransitionCohort(t, ctx)
		start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		epoch := start.Add(-20 * day)
		const stuckCount, inRangeCount, pastCount, otherCount = 3, 5, 50000, 2000
		for i := range stuckCount {
			paymentId := snReconcileTestPayment(t, ctx, f.network, nil, start.Add(time.Duration(i)*time.Hour))
			// an epoch of its own, so the stuck row raises no later bound
			snReconcileTestEpoch(t, ctx, paymentId, start.Add(-30*day))
			// canceled after every stuck row was created, so none is later than another
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=$2,payment_record='record-'||payment_id WHERE payment_id=$1`, paymentId, start.Add(day))
		}
		contracts := []server.Id{}
		for i := range inRangeCount {
			created := epoch.Add(time.Duration(i+1) * time.Hour)
			closed := created.Add(time.Hour)
			contracts = append(contracts, f.insertMinted(t, ctx, created, &closed, closed, 10, UsdToNanoCents(1)))
		}
		later := snReconcileTestPayment(t, ctx, f.network, nil, start.Add(2*day), contracts...)
		snReconcileTestEpoch(t, ctx, later, epoch)
		// One contract every 30 seconds, each minted at its creation and closed an
		// hour after it. The last closes about 14 hours before `start`.
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
			f.sourceNetwork, f.source, f.network, f.client, usage, epoch.Add(2*day), pastCount, f.balance, later)
		// one sweep each of other payments, with ids minted over the same days
		snReconcileTestExec(t, ctx, `INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time,payment_id)
			SELECT (lpad(to_hex(floor(extract(epoch FROM swept AT TIME ZONE 'UTC')*1000)::bigint),12,'0')||'ffff'||lpad(to_hex(n),16,'0'))::uuid,$1,$2,$3,10,1,swept,gen_random_uuid()
			FROM (SELECT n,$4::timestamp+n*interval '10 minutes' AS swept FROM generate_series(0,$5::int-1) n) other`,
			f.balance, f.network, f.client, epoch, otherCount)
		snReconcileTestExec(t, ctx, `ANALYZE transfer_contract,transfer_escrow_sweep,account_payment,subsidy_payment`)

		rows := snReconcileTestStuck(t, ctx)
		if len(rows) != stuckCount {
			t.Fatal("stuck rows", len(rows))
		}
		for _, row := range rows {
			if row.ReplanEvidence || row.ReplannedBeforeFix {
				t.Fatal("a stuck row has evidence", row)
			}
		}
		got, want := snReconcileTestListing(t, ctx, snReconcileStuckSql), snReconcileTestListing(t, ctx, snReconcileStuckEarliestCloseReferenceSql)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("the listing differs from the earliest-close reference:\n got %s\nwant %s", got, want)
		}
		if read := snReconcileTestContractRowsRead(snReconcileTestAnalyzedPlan(t, ctx, snReconcileStuckEarliestCloseReferenceSql)); read < inRangeCount+pastCount {
			t.Fatalf("the earliest-close listing read %v contract rows, fewer than the later row's %d sweeps", read, inRangeCount+pastCount)
		}
		plan := snReconcileTestAnalyzedPlan(t, ctx, snReconcileStuckSql)
		probed, read := snReconcileTestSweepRowsProbed(plan), snReconcileTestContractRowsRead(plan)
		if probed != stuckCount*inRangeCount || read != stuckCount*inRangeCount {
			t.Fatalf("the listing read %v sweep rows to check their contracts and %v contract rows, want %d of each:\n%s",
				probed, read, stuckCount*inRangeCount, snReconcileTestPlanText(t, ctx, snReconcileStuckSql))
		}
		t.Log("plan:\n" + snReconcileTestPlanText(t, ctx, snReconcileStuckSql))
	})
}
