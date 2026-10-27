// The stuck-row listing reads each later row's sweeps once, and returns
// exactly what the per-row listing it replaced returned.
package model

import (
	"context"
	"encoding/json"
	"fmt"
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

// The sweep rows a listing joined to their contracts, over every loop: the
// sweep rows of each join whose inputs also read transfer_contract, counted at
// the innermost such join. That join is the listing's cost on main, one
// contract read per sweep. A subplan or CTE is a query of its own, so a scan
// of sweeps alone, such as the hashed check whether a row holds sweeps that a
// small table invites, is not counted.
func snReconcileTestSweepsJoinedToContracts(t testing.TB, ctx context.Context, sql string) (joined float64) {
	t.Helper()
	var plan string
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) `+sql, SnReconcileReplanFixTime).Scan(&plan))
	})
	var explained []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(plan), &explained); err != nil || len(explained) != 1 {
		t.Fatal("explain", err, plan)
	}
	// the sweep rows of the subtree not yet counted, and whether it reads contracts
	var visit func(node map[string]any) (float64, bool)
	visit = func(node map[string]any) (sweeps float64, contracts bool) {
		switch node["Relation Name"] {
		case "transfer_escrow_sweep":
			sweeps = node["Actual Rows"].(float64) * node["Actual Loops"].(float64)
		case "transfer_contract":
			contracts = true
		}
		children, _ := node["Plans"].([]any)
		for _, value := range children {
			child := value.(map[string]any)
			childSweeps, childContracts := visit(child)
			if relationship := child["Parent Relationship"]; relationship == "SubPlan" || relationship == "InitPlan" {
				continue
			}
			sweeps += childSweeps
			contracts = contracts || childContracts
		}
		nodeType, _ := node["Node Type"].(string)
		if contracts && (nodeType == "Nested Loop" || strings.HasSuffix(nodeType, " Join")) {
			joined += sweeps
			sweeps = 0
		}
		return
	}
	visit(explained[0].Plan)
	return
}

// A5, A10, A8, RC-1, RC-2 and RC-22: the listing returns every column of every
// row exactly as the per-row listing did, over each way a row is listed or
// left out and each way a later row bears on its evidence. A later row's
// earliest close lying between two stuck rows' creations is the case the
// shared read must keep apart.
func TestSnReconcileStuckListingMatchesReference(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := 24 * time.Hour
		july := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
		sept := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		closedAt := func(cohort *payoutTransitionCohort, at time.Time) server.Id {
			return cohort.insert(t, ctx, at.Add(-time.Hour), &at, at, 10, UsdToNanoCents(1))
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
		unclosedLater := payment(f, sept.Add(10*day), f.insert(t, ctx, sept.Add(-6*day), nil, sept.Add(-5*day), 10, UsdToNanoCents(1)))
		snReconcileTestExec(t, ctx, `INSERT INTO transfer_escrow_sweep(contract_id,balance_id,network_id,destination_id,payout_byte_count,payout_net_revenue_nano_cents,sweep_time,payment_id)
			VALUES($1,$2,$3,$4,10,1,$5,$6)`, server.NewId(), f.balance, f.network, f.client, sept.Add(-5*day), unclosedLater)

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

		got, want := snReconcileTestListing(t, ctx, snReconcileStuckSql), snReconcileTestListing(t, ctx, snReconcileStuckReferenceSql)
		if len(got) != len(want) {
			t.Fatalf("listing has %d rows, the reference %d", len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("row %d differs:\n got %s\nwant %s", i, got[i], want[i])
			}
		}

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

// A5, A10: the listing reads each later row's sweeps once, however many stuck
// rows its network holds. Six stuck rows without sweeps share one later row of
// 60 sweeps that closed before any of them was created, all after the later
// row's own epoch started, so none is evidence: the per-row listing joined
// every one of those sweeps to its contract again for each stuck row.
func TestSnReconcileStuckListingReadsLaterSweepsOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		f := newPayoutTransitionCohort(t, ctx)
		start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
		const stuckCount, sweepCount = 6, 60
		for i := range stuckCount {
			paymentId := snReconcileTestPayment(t, ctx, f.network, nil, start.Add(time.Duration(i)*time.Hour))
			// an epoch of its own, so the stuck row raises no later bound
			snReconcileTestEpoch(t, ctx, paymentId, start.Add(-30*24*time.Hour))
			snReconcileTestExec(t, ctx, `UPDATE account_payment SET canceled=true,cancel_time=create_time,payment_record='record-'||payment_id WHERE payment_id=$1`, paymentId)
		}
		contracts := []server.Id{}
		for i := range sweepCount {
			closed := start.Add(-5*24*time.Hour + time.Duration(i)*time.Minute)
			contracts = append(contracts, f.insert(t, ctx, closed.Add(-time.Hour), &closed, closed, 10, UsdToNanoCents(1)))
		}
		later := snReconcileTestPayment(t, ctx, f.network, nil, start.Add(48*time.Hour), contracts...)
		snReconcileTestEpoch(t, ctx, later, start.Add(-10*24*time.Hour))

		rows := snReconcileTestStuck(t, ctx)
		if len(rows) != stuckCount {
			t.Fatal("stuck rows", len(rows))
		}
		for _, row := range rows {
			if row.ReplanEvidence || row.ReplannedBeforeFix {
				t.Fatal("a stuck row has evidence", row)
			}
		}
		plan := func(options string) string {
			lines := []string{}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `EXPLAIN (`+options+`) `+snReconcileStuckSql, SnReconcileReplanFixTime)
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
		if joined := snReconcileTestSweepsJoinedToContracts(t, ctx, snReconcileStuckSql); joined != sweepCount {
			t.Fatalf("the listing joined %v sweep rows to their contracts for one later row of %d:\n%s", joined, sweepCount, plan("ANALYZE, COSTS OFF, TIMING OFF"))
		}
		t.Log("plan:\n" + plan("COSTS OFF"))
	})
}
