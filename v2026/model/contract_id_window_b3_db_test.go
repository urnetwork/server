package model

// The two contract reads that had no contract id bound (C1 readiness,
// 2026-11-17, B3): the removed client's open contracts, now bounded as every
// hot read of unexpired contracts is, and the stranded grant payers, whose
// escrow probe is now bounded by each grant's lifetime.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The removed client's read as it was before its bound.
func unboundedRemovedClientOpenContractsSql(t testing.TB) string {
	t.Helper()
	bound := unexpiredContractIdBoundSql("contract_id") + ` AND`
	if strings.Count(removedClientOpenContractsSql, bound) != 2 {
		t.Fatalf("the removed client's read has no bound on each arm: %s", removedClientOpenContractsSql)
	}
	return strings.ReplaceAll(removedClientOpenContractsSql, bound, ``)
}

// The removed client's read returns every open contract the unbounded read
// returned that the floor admits, in each direction, and none minted before
// the floor: those are past their deadline, and closing them belongs to the
// force-close sweep, as when a full queue drops the read.
func TestRemovedClientOpenContractsLeaveContractsBeforeTheFloorToTheSweep(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if server.IsContractPartitioned(ctx) {
			t.Skip("plain contract tables only")
		}
		ids := newUnexpiredContractReadIds()
		insertUnexpiredContractReadFixture(t, ctx, ids)
		floor := server.ContractIdFloor(server.NowUtc().Add(-unexpiredContractIdAge))
		server.Db(ctx, func(conn server.PgConn) {
			// the fixture's source and destination, so each arm returns rows
			for _, clientId := range []server.Id{ids.sourceId, ids.destinationId} {
				rows := unexpiredContractReadRows(t, ctx, conn, unexpiredContractDaysAt(server.NowUtc()).render(removedClientOpenContractsSql), clientId, clientRemovedOpenContractLimit)
				unboundedRows := unexpiredContractReadRows(t, ctx, conn, unboundedRemovedClientOpenContractsSql(t), clientId, clientRemovedOpenContractLimit)
				admitted, before := []string{}, []string{}
				for _, row := range unboundedRows {
					if server.RequireParseId(strings.Split(row, "|")[0]).Cmp(floor) < 0 {
						before = append(before, row)
					} else {
						admitted = append(admitted, row)
					}
				}
				slices.Sort(rows)
				slices.Sort(admitted)
				if !slices.Equal(rows, admitted) {
					t.Fatalf("the removed client's read returns %v, want the unbounded read's %v from the floor", rows, admitted)
				}
				// the open plain and companion contracts of the fixture's two
				// offsets before the floor, and of its five from it
				if len(before) != 4 || len(admitted) != 10 {
					t.Fatalf("the unbounded read returns %d contracts before the floor and %d from it, want 4 and 10", len(before), len(admitted))
				}
			}
		})
	})
}

// A stranded grant's payer, its grant over [start, end), and an escrow on it
// for a contract minted at each of the times.
func insertStrandedGrantBoundFixture(t testing.TB, ctx context.Context, start time.Time, end time.Time, minted []time.Time, settled bool) (payerNetworkId server.Id) {
	t.Helper()
	payerNetworkId = server.NewId()
	balanceId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_balance (balance_id, network_id, start_time, end_time, start_balance_byte_count, net_revenue_nano_cents, balance_byte_count, pro)
			VALUES ($1, $2, $3, $4, 1000000, 0, 1000000, false)
		`, balanceId, payerNetworkId, start, end))
		for _, mintedAt := range minted {
			contractId := contractIdMintedAt(mintedAt)
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO transfer_contract (
					contract_id, source_network_id, source_id, destination_network_id, destination_id,
					transfer_byte_count, payer_network_id, create_time, expiration_time
				)
				VALUES ($1, $2, $3, $4, $5, 100, $2, $6, $7)
			`, contractId, payerNetworkId, server.NewId(), server.NewId(), server.NewId(), mintedAt, mintedAt.Add(DefaultContractExpiration)))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count, settled, settle_time, payout_byte_count)
				VALUES ($1, $2, 100, $3, CASE WHEN $3 THEN $4::timestamp END, CASE WHEN $3 THEN 0 END)
			`, contractId, balanceId, settled, mintedAt.Add(DefaultContractExpiration)))
		}
	})
	return
}

// The stranded grant read as it was before its bound: each probe reads the
// grant's balance whole.
func unboundedStrandedGrantPayersSql(t testing.TB) string {
	t.Helper()
	bound := `
				AND e.contract_id >= ` + ContractIdCreatedFromSql("b.start_time") + ` AND e.contract_id < ` + contractDaysUpperSql
	if strings.Count(strandedGrantPayersSql, bound) != 1 {
		t.Fatalf("the stranded grant read has no escrow bound: %s", strandedGrantPayersSql)
	}
	return strings.Replace(strandedGrantPayersSql, bound, ``, 1)
}

// On the plain tables the bounded read returns the payers the unbounded read
// returned, for grants of a day, a month and a year that ended in the window,
// with escrows minted from their first minute to their last, a grant whose
// escrow settled, and a current grant.
func TestStrandedGrantPayersReadMatchesTheUnboundedRead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if server.IsContractPartitioned(ctx) {
			t.Skip("plain contract tables only")
		}
		now := server.NowUtc().Truncate(time.Millisecond)
		end := now.Add(-2 * time.Hour)
		want := []server.Id{}
		for _, length := range []time.Duration{24 * time.Hour, 30 * 24 * time.Hour, 365 * 24 * time.Hour} {
			start := end.Add(-length)
			for _, mintedAt := range []time.Time{start, start.Add(time.Minute), start.Add(length / 2), end.Add(-time.Minute)} {
				// a grant for each escrow, so each payer has one
				want = append(want, insertStrandedGrantBoundFixture(t, ctx, start, end, []time.Time{mintedAt}, false))
			}
		}
		insertStrandedGrantBoundFixture(t, ctx, end.Add(-24*time.Hour), end, []time.Time{end.Add(-time.Hour)}, true)
		insertStrandedGrantBoundFixture(t, ctx, now.Add(-time.Hour), now.Add(23*time.Hour), []time.Time{now.Add(-time.Minute)}, false)
		slices.SortFunc(want, server.Id.Cmp)

		endedAfter, endedBefore := end.Add(-time.Minute), end.Add(time.Minute)
		payers, err := ReadStrandedGrantPayers(ctx, endedAfter, endedBefore, server.Id{}, 1000)
		if err != nil || !slices.Equal(payers, want) {
			t.Fatalf("the read returns %v (%v), want every stranded grant's payer %v", payers, err, want)
		}
		args := []any{endedAfter, endedBefore, server.Id{}, 1000}
		server.Db(ctx, func(conn server.PgConn) {
			rows := unexpiredContractReadRows(t, ctx, conn, strandedGrantEscrowDays(endedBefore).render(strandedGrantPayersSql), args...)
			unboundedRows := unexpiredContractReadRows(t, ctx, conn, unboundedStrandedGrantPayersSql(t), args...)
			if !slices.Equal(rows, unboundedRows) || len(rows) != len(want) {
				t.Fatalf("the bounded read returns %v, and the unbounded read %v", rows, unboundedRows)
			}
		})
	})
}

// The relations and indexes a plan reads, each once, sorted: scan nodes by
// relation, and every node that names an index, bitmap index scans included.
func contractPlanReads(t testing.TB, raw []byte) []string {
	t.Helper()
	var plans []map[string]any
	server.Raise(json.Unmarshal(raw, &plans))
	reads := map[string]bool{}
	var inspect func(node map[string]any)
	inspect = func(node map[string]any) {
		if relation, ok := node["Relation Name"].(string); ok {
			reads["relation "+relation] = true
		}
		if index, ok := node["Index Name"].(string); ok {
			reads["index "+index] = true
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				inspect(child.(map[string]any))
			}
		}
	}
	inspect(plans[0]["Plan"].(map[string]any))
	sorted := []string{}
	for read := range reads {
		sorted = append(sorted, read)
	}
	slices.Sort(sorted)
	return sorted
}

// With Main's proportions, a window of daily grants of which one in a hundred
// still holds an unsettled escrow among settled ones, the bounded read probes
// the escrows as the unbounded read does: the window's grants from the end
// time index, and each grant's unsettled escrows from the unsettled escrow
// index, one probe per grant, in a custom plan and in a generic one. It
// returns the same payers.
func TestStrandedGrantPayersReadKeepsTheUnboundedReadsIndexes(t *testing.T) {
	if testing.Short() {
		t.Skip("seeds 50k grants; skipped in -short")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if server.IsContractPartitioned(ctx) {
			t.Skip("plain contract tables only")
		}
		end := server.NowUtc().Truncate(time.Millisecond).Add(-2 * time.Hour)
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				WITH grant_window AS (
					INSERT INTO transfer_balance (balance_id, network_id, start_time, end_time, start_balance_byte_count, net_revenue_nano_cents, balance_byte_count, pro)
					SELECT gen_random_uuid(), gen_random_uuid(), $1::timestamp - interval '24 hours', $1::timestamp - (n % 3600) * interval '1 millisecond', 1000000, 0, 1000000, false
					FROM generate_series(1, 50000) AS n
					RETURNING balance_id, start_time
				)
				INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count, settled, settle_time, payout_byte_count)
				SELECT
					(lpad(to_hex(floor(extract(epoch FROM grant_window.start_time + n * interval '1 hour') * 1000)::bigint), 12, '0') ||
						substr(md5(grant_window.balance_id::text || n::text), 1, 20))::uuid,
					grant_window.balance_id, 100, escrow.settled,
					CASE WHEN escrow.settled THEN grant_window.start_time + interval '3 hours' END,
					CASE WHEN escrow.settled THEN 0 END
				FROM grant_window
				CROSS JOIN generate_series(1, 3) AS n
				CROSS JOIN LATERAL (SELECT NOT (n = 1 AND abs(hashtext(grant_window.balance_id::text)) % 100 = 0) AS settled) AS escrow
			`, end))
		}, server.OptNoRetry())
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_balance, transfer_escrow`))
		}, server.OptReadWrite(), server.OptNoRetry())

		endedAfter, endedBefore := end.Add(-time.Minute), end
		bounded := strandedGrantEscrowDays(endedBefore).render(strandedGrantPayersSql)
		args := []any{endedAfter, endedBefore, server.Id{}, 100000}
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		rows := unexpiredContractReadRows(t, ctx, conn, bounded, args...)
		unboundedRows := unexpiredContractReadRows(t, ctx, conn, unboundedStrandedGrantPayersSql(t), args...)
		if !slices.Equal(rows, unboundedRows) || len(rows) < 100 {
			t.Fatalf("the bounded read returns %d payers, and the unbounded read %d", len(rows), len(unboundedRows))
		}
		for _, mode := range []string{"force_custom_plan", "force_generic_plan"} {
			reads := map[string][]string{}
			for name, sql := range map[string]string{"bounded": bounded, "unbounded": unboundedStrandedGrantPayersSql(t)} {
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				server.RaisePgResult(tx.Exec(ctx, `SET LOCAL plan_cache_mode = `+mode))
				var raw []byte
				server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+sql, args...).Scan(&raw))
				server.Raise(tx.Rollback(ctx))
				reads[name] = contractPlanReads(t, raw)
			}
			if !slices.Equal(reads["bounded"], reads["unbounded"]) || !slices.Contains(reads["bounded"], "index transfer_escrow_unsettled_balance_contract") {
				t.Fatalf("(%s) the bounded read reads %v, and the unbounded read %v", mode, reads["bounded"], reads["unbounded"])
			}
			t.Logf("(%s) %d payers; %v", mode, len(rows), reads["bounded"])
		}
	})
}

// The transfer_escrow partitions a stranded grant read plans and executes, by
// EXPLAIN ANALYZE in a custom plan with one probe per grant, as on Main.
func strandedGrantEscrowPartitions(t testing.TB, ctx context.Context, ranges map[string]contractPartitionRange, sql string, args ...any) (planned []string, executed []string) {
	t.Helper()
	conn := acquireContractLifecycleTestConnection(t, ctx)
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	server.Raise(err)
	defer tx.Rollback(context.WithoutCancel(ctx))
	for _, setting := range []string{`SET LOCAL plan_cache_mode = force_custom_plan`, `SET LOCAL enable_hashjoin = off`, `SET LOCAL enable_mergejoin = off`} {
		server.RaisePgResult(tx.Exec(ctx, setting))
	}
	var raw []byte
	server.Raise(tx.QueryRow(ctx, `EXPLAIN (ANALYZE, TIMING OFF, FORMAT JSON) `+sql, args...).Scan(&raw))
	var plans []map[string]any
	server.Raise(json.Unmarshal(raw, &plans))
	var inspect func(node map[string]any)
	inspect = func(node map[string]any) {
		if relation, ok := node["Relation Name"].(string); ok {
			if partition, ok := ranges[relation]; ok && partition.table == "transfer_escrow" {
				planned = append(planned, relation)
				if loops, _ := node["Actual Loops"].(float64); 0 < loops {
					executed = append(executed, relation)
				}
			}
		}
		if children, ok := node["Plans"].([]any); ok {
			for _, child := range children {
				inspect(child.(map[string]any))
			}
		}
	}
	inspect(plans[0]["Plan"].(map[string]any))
	slices.Sort(planned)
	slices.Sort(executed)
	return
}

// On partitioned contract tables the stranded grant read's escrow probe reads
// only the partitions of the grant's lifetime: the days after the window's
// end are not planned, and a probe executes no partition before the grant's
// start. The grant is in the second day after the cutover day, so the legacy
// partition, the days before the grant's and the day after it are outside it.
// The read without its bound, as it ran before, plans every partition and
// probes those before the grant's: the root cause, which the test proves
// against the read's own statement.
func TestStrandedGrantPayersProbeOnlyTheGrantsDaysWhenPartitioned(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if !server.IsContractPartitioned(ctx) {
			server.Testing_PartitionContracts(ctx)
		}
		ranges := contractPartitionRanges(t, ctx)
		grantDay := server.ContractPartitionDayStart(server.NowUtc()).AddDate(0, 0, 2)
		start, end := grantDay.Add(time.Hour), grantDay.Add(20*time.Hour)
		payerNetworkId := insertStrandedGrantBoundFixture(t, ctx, start, end, []time.Time{start.Add(time.Hour), end.Add(-time.Hour)}, false)

		endedAfter, endedBefore := end.Add(-time.Minute), end
		payers, err := ReadStrandedGrantPayers(ctx, endedAfter, endedBefore, server.Id{}, 16)
		if err != nil || !slices.Equal(payers, []server.Id{payerNetworkId}) {
			t.Fatalf("the read returns %v (%v), want the stranded grant's payer", payers, err)
		}

		// the grant's lifetime: from its start less the margin to the end of
		// the window's days
		lifetime := contractDays{
			lower: server.ContractIdFloor(start.Add(-ContractIdCreateTimeMargin)),
			upper: strandedGrantEscrowDays(endedBefore).upper,
		}
		args := []any{endedAfter, endedBefore, server.Id{}, 16}
		planned, executed := strandedGrantEscrowPartitions(t, ctx, ranges, strandedGrantEscrowDays(endedBefore).render(strandedGrantPayersSql), args...)
		for _, relation := range planned {
			if ranges[relation].lower.Cmp(lifetime.upper) >= 0 {
				t.Fatalf("the read plans %s, after the window's days: %v", relation, planned)
			}
		}
		for _, relation := range executed {
			if !ranges[relation].overlaps(lifetime) {
				t.Fatalf("a probe reads %s, outside the grant's lifetime: %v", relation, executed)
			}
		}
		if len(executed) == 0 {
			t.Fatal("no probe read the grant's day")
		}

		unboundedPlanned, unboundedExecuted := strandedGrantEscrowPartitions(t, ctx, ranges, unboundedStrandedGrantPayersSql(t), args...)
		outside := 0
		for _, relation := range unboundedExecuted {
			if !ranges[relation].overlaps(lifetime) {
				outside += 1
			}
		}
		if len(unboundedPlanned) <= len(planned) || outside == 0 {
			t.Fatalf("without its bound the read plans %v and probes %v; the test no longer sees the root cause", unboundedPlanned, unboundedExecuted)
		}
		t.Logf("planned %v, probed %v; without the bound planned %v, probed %v", planned, executed, unboundedPlanned, unboundedExecuted)
	})
}
