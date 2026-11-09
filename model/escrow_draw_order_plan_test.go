//go:build sidecar_pending

package model

// QUOTA-M4-02: the keyset sites on MG6's team_credit index, under -tags sidecar_pending, which
// links MG6. Gate on, each page is one index scan with no sort and pages in escrowDrawLess's
// order; gate off, today's text plans on today's index as before (FS-55); the exhaustion proof
// refuses as the census does with a line among the balances.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

const escrowDrawIndexName = "transfer_balance_active_network_credit_end_start_id"

// The plan of one query with sequential and bitmap scans off, as CODESTYLE's compatibility rule
// plans the running release's shapes: whether it sorts, and the indexes it reads.
func testingExplainEscrowDrawPlan(t testing.TB, ctx context.Context, sql string, args ...any) (sorted bool, indexNames []string) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
		server.RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_bitmapscan = off`))
		var raw []byte
		server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+sql, args...).Scan(&raw))
		var plans []map[string]any
		server.Raise(json.Unmarshal(raw, &plans))
		var walk func(node map[string]any)
		walk = func(node map[string]any) {
			switch node["Node Type"] {
			case "Sort", "Incremental Sort":
				sorted = true
			}
			if name, ok := node["Index Name"].(string); ok {
				indexNames = append(indexNames, name)
			}
			children, _ := node["Plans"].([]any)
			for _, child := range children {
				walk(child.(map[string]any))
			}
		}
		for _, plan := range plans {
			walk(plan["Plan"].(map[string]any))
		}
	})
	return
}

// count balances of one payer, every fifth a line, with ties at every key, and ANALYZE.
func testingInsertEscrowDrawPage(t testing.TB, ctx context.Context, networkId server.Id, count int) []*escrowTransferBalance {
	t.Helper()
	now := server.NowUtc().Truncate(time.Second)
	credit := string(GrantKindTeamCredit)
	free := string(GrantKindFree)
	insert := `
		INSERT INTO transfer_balance (balance_id, network_id, start_time, end_time, start_balance_byte_count, net_revenue_nano_cents, balance_byte_count, pro, grant_kind)
		VALUES ($1, $2, $3, $4, 1024, $5, 1024, false, $6)
	`
	balances := []*escrowTransferBalance{}
	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for index := range count {
				var grantKind *string
				switch index % 5 {
				case 0:
					grantKind = &credit
				case 1:
					grantKind = &free
				}
				balance := &escrowTransferBalance{
					balanceId: server.NewId(),
					isCredit:  index%5 == 0,
					// three end times and two start times, so every key ties somewhere
					endTime:   now.Add(time.Duration(1+index%3) * time.Hour),
					startTime: now.Add(-time.Duration(1+index%2) * time.Hour),
				}
				batch.Queue(insert, balance.balanceId, networkId, balance.startTime, balance.endTime, NanoCents(index%2), grantKind)
				balances = append(balances, balance)
			}
			// other payers, so the payer is a slice of the index
			for range 4 {
				other := server.NewId()
				for index := range 50 {
					batch.Queue(insert, server.NewId(), other, now.Add(-time.Hour), now.Add(time.Duration(1+index)*time.Minute), NanoCents(0), nil)
				}
			}
		})
	})
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_balance`))
	})
	return balances
}

// Gate on: the census's first page and keyset page, the zero escrow priority's page and the
// zero-byte anchor each read MG6's index with no sort. Gate off: today's text reads today's index
// with no sort, unchanged under the migrated schema (FS-55).
func TestEscrowDrawOrderPagesAreOneIndexScan(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		balances := testingInsertEscrowDrawPage(t, ctx, networkId, 200)
		now := server.NowUtc()
		cursor := balances[7]

		on := escrowBalanceRead{drawOrder: true}
		off := escrowBalanceRead{drawOrder: false}
		type page struct {
			name  string
			sql   string
			args  []any
			index string
		}
		pages := []page{}
		for _, read := range []escrowBalanceRead{on, off} {
			index := "transfer_balance_active_network_end_start_id"
			if read.drawOrder {
				index = escrowDrawIndexName
			}
			first, firstArgs := read.censusPage(networkId, now, nil, redisGrantPageRows)
			next, nextArgs := read.censusPage(networkId, now, cursor, redisGrantPageRows)
			pages = append(pages,
				page{name: fmt.Sprintf("census first page, draw order %t", read.drawOrder), sql: first, args: firstArgs, index: index},
				page{name: fmt.Sprintf("census keyset page, draw order %t", read.drawOrder), sql: next, args: nextArgs, index: index},
			)
		}
		pages = append(pages,
			page{name: "zero escrow priority, draw order", sql: on.sql() + escrowDrawOrderBySql + ` LIMIT $3`, args: []any{networkId, now, redisGrantPageRows}, index: escrowDrawIndexName},
			page{name: "zero-byte anchor, draw order", sql: on.sql() + escrowDrawOrderBySql + ` LIMIT 1`, args: []any{networkId, now}, index: escrowDrawIndexName},
		)
		for _, p := range pages {
			sorted, indexNames := testingExplainEscrowDrawPlan(t, ctx, p.sql, p.args...)
			if sorted {
				t.Errorf("%s sorts: %v", p.name, indexNames)
			}
			if !slices.Equal(indexNames, []string{p.index}) {
				t.Errorf("%s reads %v, want %s alone", p.name, indexNames, p.index)
			}
		}
	})
}

// 200 balances paged 64 at a time come out in exactly the comparator's order, and gate off in
// today's order by end time, start time and balance id.
func TestEscrowDrawOrderPagesFollowTheComparator(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		balances := testingInsertEscrowDrawPage(t, ctx, networkId, 200)
		now := server.NowUtc()
		for _, read := range []escrowBalanceRead{{drawOrder: true}, {drawOrder: false}} {
			paged := []server.Id{}
			var cursor *escrowTransferBalance
			for {
				sql, args := read.censusPage(networkId, now, cursor, redisGrantPageRows)
				page := []*escrowTransferBalance{}
				server.Db(ctx, func(conn server.PgConn) {
					rows, err := conn.Query(ctx, sql, args...)
					server.WithPgResult(rows, err, func() {
						for rows.Next() {
							balance := &escrowTransferBalance{}
							server.Raise(read.scan(rows, balance))
							page = append(page, balance)
						}
					})
				})
				for _, balance := range page {
					paged = append(paged, balance.balanceId)
					cursor = balance
				}
				if len(page) < redisGrantPageRows {
					break
				}
			}
			want := slices.Clone(balances)
			if !read.drawOrder {
				// today's order knows no flag
				want = []*escrowTransferBalance{}
				for _, balance := range balances {
					want = append(want, &escrowTransferBalance{endTime: balance.endTime, startTime: balance.startTime, balanceId: balance.balanceId})
				}
			}
			slices.SortFunc(want, escrowDrawCompare)
			wantIds := []server.Id{}
			for _, balance := range want {
				wantIds = append(wantIds, balance.balanceId)
			}
			connect.AssertEqual(t, len(paged), 200)
			if !slices.Equal(paged, wantIds) {
				t.Fatalf("draw order %t: pages leave the comparator's order", read.drawOrder)
			}
		}
	})
}

// With the gate on and a line among the payer's balances, the exhaustion proof decides as the
// census does: a held line is refused by both, with the proof; room only on the line funds the
// request from the line, reached last, and the proof leaves it to the census.
func TestEscrowDrawOrderExhaustionProofPagesAsTheCensus(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		withRedisExhaustedPayersForTest(t)
		pop := Testing_SetEscrowDrawOrder(true)
		defer pop()
		connect.AssertEqual(t, EscrowDrawOrderEnabled(), true)
		// the payer's line, after its held grants, with room
		line := func(room ByteCount) func(t testing.TB, ctx context.Context, ids []server.Id) {
			return func(t testing.TB, ctx context.Context, ids []server.Id) {
				var networkId server.Id
				var startTime, endTime time.Time
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT network_id, start_time, end_time FROM transfer_balance WHERE balance_id = $1`, ids[0]).Scan(&networkId, &startTime, &endTime))
				})
				for _, id := range ids {
					redisGrantProbeToken(t, ctx, id, 1000, 1000)
				}
				credit := string(GrantKindTeamCredit)
				// the line ends first, so only the flag puts it last
				lineId := testingInsertEscrowDrawBalance(t, ctx, networkId, 1000, 1, &credit, startTime, endTime.Add(-time.Minute))
				if held := 1000 - room; 0 < held {
					redisGrantProbeToken(t, ctx, lineId, 1000, held)
				}
			}
		}
		for _, scenario := range []redisExhaustionScenario{
			{name: "a held line among held grants", grants: 4, credit: 1000, request: 512, proven: true, outcome: "refused", setup: line(0)},
			{name: "room only on the line", grants: 4, credit: 1000, request: 512, outcome: "funded", funded: map[int]ByteCount{-1: 512}, setup: line(600)},
		} {
			requireRedisExhaustionTwinsAgree(t, ctx, scenario)
		}
		if !strings.Contains(escrowDrawOrderBySql, escrowDrawCreditSql) {
			t.Fatal("the keyset order does not lead with the flag")
		}
	})
}
