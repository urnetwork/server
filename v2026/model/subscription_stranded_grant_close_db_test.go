package model

import (
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The pass's read returns the payers whose grants ended within the window and
// still hold an unsettled escrow, in network order after the position. A grant
// that ended with its escrow settled, one that ended without escrow and a
// current grant's escrow name no payer. The window excludes its lower bound and
// includes its upper one.
func TestReadStrandedGrantPayersFindsEndedGrantsThatStillHoldEscrow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Millisecond)
		mintedAt := now.Add(-30 * time.Hour)
		deadline := mintedAt.Add(DefaultContractExpiration)

		strandedA := newPayerDueCloseFixture(t, ctx, "synthetic-stranded-a")
		strandedA.addContracts(ctx, strandedA.endedBalanceId, mintedAt, 2, deadline, false)
		strandedB := newPayerDueCloseFixture(t, ctx, "synthetic-stranded-b")
		strandedB.addContracts(ctx, strandedB.endedBalanceId, mintedAt, 1, deadline, false)
		currentOnly := newPayerDueCloseFixture(t, ctx, "synthetic-current-only")
		currentOnly.addContracts(ctx, currentOnly.currentBalanceId, now.Add(-2*time.Hour), 2, now.Add(-time.Hour), false)
		settled := newPayerDueCloseFixture(t, ctx, "synthetic-settled")
		settledIds := settled.addContracts(ctx, settled.endedBalanceId, mintedAt, 2, deadline, false)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET settled=true, settle_time=$2, payout_byte_count=0
				WHERE contract_id=ANY($1)`, settledIds, deadline))
		})

		ends := map[server.Id]time.Time{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT balance_id, end_time FROM transfer_balance WHERE balance_id=ANY($1)`,
				[]server.Id{strandedA.endedBalanceId, strandedB.endedBalanceId, currentOnly.endedBalanceId, settled.endedBalanceId})
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var balanceId server.Id
					var endTime time.Time
					server.Raise(rows.Scan(&balanceId, &endTime))
					ends[balanceId] = endTime
				}
			})
		})
		lower := ends[strandedA.endedBalanceId]
		upper := ends[strandedA.endedBalanceId]
		for _, end := range ends {
			lower = server.MinTime(lower, end)
			upper = server.MaxTime(upper, end)
		}

		want := []server.Id{strandedA.payerNetworkId, strandedB.payerNetworkId}
		slices.SortFunc(want, server.Id.Cmp)
		read := func(endedAfter time.Time, endedBefore time.Time, after server.Id, limit int) []server.Id {
			payers, err := ReadStrandedGrantPayers(ctx, endedAfter, endedBefore, after, limit)
			if err != nil {
				t.Fatal(err)
			}
			return payers
		}
		if payers := read(lower.Add(-time.Minute), upper.Add(time.Minute), server.Id{}, 16); !slices.Equal(payers, want) {
			t.Fatal("the read did not return exactly the payers of ended grants that still hold escrow", payers, want)
		}
		// pages in network order
		if payers := read(lower.Add(-time.Minute), upper.Add(time.Minute), server.Id{}, 1); !slices.Equal(payers, want[:1]) {
			t.Fatal("the first page is not the first payer", payers)
		}
		if payers := read(lower.Add(-time.Minute), upper.Add(time.Minute), want[0], 16); !slices.Equal(payers, want[1:]) {
			t.Fatal("the page after the first payer is not the second", payers)
		}
		// the bounds
		endA := ends[strandedA.endedBalanceId]
		if payers := read(endA.Add(-time.Millisecond), endA, server.Id{}, 16); !slices.Contains(payers, strandedA.payerNetworkId) {
			t.Fatal("the upper bound excluded a grant that ended at it", payers)
		}
		if payers := read(endA, endA.Add(time.Millisecond), server.Id{}, 16); slices.Contains(payers, strandedA.payerNetworkId) {
			t.Fatal("the lower bound included a grant that ended at it", payers)
		}
		// the current grants end tomorrow; their escrow names no payer today
		if payers := read(now.Add(-time.Hour), now.Add(time.Hour), server.Id{}, 16); len(payers) != 0 {
			t.Fatal("a window with no ended grant named payers", payers)
		}
	})
}
