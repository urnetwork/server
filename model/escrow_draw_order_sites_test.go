//go:build sidecar_pending

package model

// QUOTA-M4-01's sites on Postgres, under -tags sidecar_pending, since the escrow_draw_order gate
// is false unless the binary links MG6: a team_credit line ending before a data pack is drawn
// after it through escrow's sort, settlement's read and the deadline close, and returned to
// first; gate off, the same rows go as today's order sends them; the gate stays on under a live
// line; the large-free-grant spread never takes the line.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// One balance of a payer, written as its grant would be: revenue makes it paid, and grantKind
// nil leaves grant_kind NULL, as on older balances.
func testingInsertEscrowDrawBalance(
	t testing.TB,
	ctx context.Context,
	networkId server.Id,
	byteCount ByteCount,
	revenue NanoCents,
	grantKind *string,
	startTime time.Time,
	endTime time.Time,
) server.Id {
	t.Helper()
	balanceId := server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO transfer_balance (balance_id, network_id, start_time, end_time, start_balance_byte_count, net_revenue_nano_cents, balance_byte_count, pro, grant_kind)
				VALUES ($1, $2, $3, $4, $5, $6, $5, false, $7)
			`,
			balanceId, networkId, startTime, endTime, byteCount, revenue, grantKind,
		))
	})
	return balanceId
}

// A payer with a data pack ending in a year and a team_credit line ending in 30 days, FS-46's
// pair, each of byteCount.
type testingEscrowDrawPayer struct {
	escrowSelectionTestClients
	packId server.Id
	lineId server.Id
}

func newTestingEscrowDrawPayer(t testing.TB, ctx context.Context, byteCount ByteCount) *testingEscrowDrawPayer {
	clients := newEscrowSelectionTestClients(t, ctx)
	now := server.NowUtc()
	credit := string(GrantKindTeamCredit)
	payer := &testingEscrowDrawPayer{
		escrowSelectionTestClients: clients,
		// NULL grant_kind, as a data code's
		packId: testingInsertEscrowDrawBalance(t, ctx, clients.payerNetworkId, byteCount, 3_000_000, nil, now.Add(-time.Hour), now.AddDate(1, 0, 0)),
		lineId: testingInsertEscrowDrawBalance(t, ctx, clients.payerNetworkId, byteCount, 9_000_000, &credit, now.Add(-time.Hour), now.AddDate(0, 0, 30)),
	}
	return payer
}

// Each balance's escrow and payout for the contract.
func testingEscrowPayouts(t testing.TB, ctx context.Context, contractId server.Id) (escrowed map[server.Id]ByteCount, paid map[server.Id]ByteCount) {
	t.Helper()
	escrowed = map[server.Id]ByteCount{}
	paid = map[server.Id]ByteCount{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT balance_id, balance_byte_count, COALESCE(payout_byte_count, 0) FROM transfer_escrow WHERE contract_id = $1`, contractId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var balanceId server.Id
				var escrow, payout ByteCount
				server.Raise(rows.Scan(&balanceId, &escrow, &payout))
				escrowed[balanceId] = escrow
				paid[balanceId] = payout
			}
		})
	})
	return
}

// The flag reads false on a NULL grant_kind; the equality spelling would sort those after the
// line, which is why the package bans it.
func TestEscrowDrawCreditFlagReadsNullAsFalse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		credit := string(GrantKindTeamCredit)
		free := string(GrantKindFree)
		line := testingInsertEscrowDrawBalance(t, ctx, networkId, 100, 1, &credit, now.Add(-time.Hour), now.Add(time.Hour))
		legacy := testingInsertEscrowDrawBalance(t, ctx, networkId, 100, 0, nil, now.Add(-time.Hour), now.Add(3*time.Hour))
		grant := testingInsertEscrowDrawBalance(t, ctx, networkId, 100, 0, &free, now.Add(-time.Hour), now.Add(2*time.Hour))

		read := escrowBalanceRead{drawOrder: true}
		order := func(orderBy string) (ids []server.Id, credits []bool) {
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, read.sql()+orderBy, networkId, now)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						balance := &escrowTransferBalance{}
						server.Raise(read.scan(rows, balance))
						ids = append(ids, balance.balanceId)
						credits = append(credits, balance.isCredit)
					}
				})
			})
			return
		}
		ids, credits := order(` ORDER BY ` + escrowDrawCreditSql + `, end_time, start_time, balance_id`)
		connect.AssertEqual(t, ids, []server.Id{grant, legacy, line})
		connect.AssertEqual(t, credits, []bool{false, false, true})
		// the banned spelling: NULL sorts after true in an ascending order
		ids, _ = order(` ORDER BY grant_kind = 'team_credit', end_time, start_time, balance_id`)
		connect.AssertEqual(t, ids, []server.Id{grant, line, legacy})
	})
}

// Escrow's sort, through the legacy admission path that sorts in Go: on, the pack first; off,
// today's order by end time, the line first.
func TestEscrowDrawOrderEscrowTakesThePackFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, c := range []struct {
			drawOrder bool
			requested ByteCount
			pack      ByteCount
			line      ByteCount
		}{
			{drawOrder: true, requested: 1024, pack: 1024},
			{drawOrder: true, requested: 6000, pack: 4096, line: 1904},
			{drawOrder: false, requested: 1024, line: 1024},
			{drawOrder: false, requested: 6000, pack: 1904, line: 4096},
		} {
			func() {
				pop := Testing_SetEscrowDrawOrder(c.drawOrder)
				defer pop()
				connect.AssertEqual(t, EscrowDrawOrderEnabled(), c.drawOrder)
				payer := newTestingEscrowDrawPayer(t, ctx, 4096)
				escrow, err := createTransferEscrow(ctx, payer.payerNetworkId, payer.payerId, payer.providerNetworkId, payer.providerId, c.requested)
				if err != nil {
					t.Fatal(err)
				}
				escrowed, _ := testingEscrowPayouts(t, ctx, escrow.ContractId)
				connect.AssertEqual(t, escrowed[payer.packId], c.pack)
				connect.AssertEqual(t, escrowed[payer.lineId], c.line)
			}()
		}
	})
}

// A settlement keeps bytes in draw order and returns the rest in reverse: the line pays only
// past the pack, and a partial settlement returns all of the line's reservation first (E-b).
func TestEscrowDrawOrderSettlementReturnsToTheLineFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pop := Testing_SetEscrowDrawOrder(true)
		defer pop()
		for _, c := range []struct {
			used ByteCount
			pack ByteCount
			line ByteCount
		}{
			{used: 5000, pack: 4096, line: 904},
			{used: 3000, pack: 3000, line: 0},
		} {
			payer := newTestingEscrowDrawPayer(t, ctx, 4096)
			escrow, err := createTransferEscrow(ctx, payer.payerNetworkId, payer.payerId, payer.providerNetworkId, payer.providerId, 6000)
			if err != nil {
				t.Fatal(err)
			}
			connect.AssertEqual(t, nil, CloseContract(ctx, escrow.ContractId, payer.payerId, c.used, false))
			connect.AssertEqual(t, nil, CloseContract(ctx, escrow.ContractId, payer.providerId, c.used, false))
			_, closed := GetContractClose(ctx, escrow.ContractId)
			connect.AssertEqual(t, closed, true)
			_, paid := testingEscrowPayouts(t, ctx, escrow.ContractId)
			connect.AssertEqual(t, paid[payer.packId], c.pack)
			connect.AssertEqual(t, paid[payer.lineId], c.line)
		}
	})
}

// The deadline close, which the sites row did not name, keeps bytes on the pack before the line
// too (plan-QUOTA-draw-order-sites, FS-46's added clause); off, today's order keeps them on the
// line first.
func TestEscrowDrawOrderDeadlineCloseKeepsThePackFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, c := range []struct {
			drawOrder bool
			pack      ByteCount
			line      ByteCount
		}{
			{drawOrder: true, pack: 3000, line: 0},
			{drawOrder: false, pack: 1096, line: 1904},
		} {
			payer := newTestingEscrowDrawPayer(t, ctx, 4096)
			// escrow while on, so both orders close the same reservation
			var escrow *TransferEscrow
			func() {
				pop := Testing_SetEscrowDrawOrder(true)
				defer pop()
				var err error
				escrow, err = createTransferEscrow(ctx, payer.payerNetworkId, payer.payerId, payer.providerNetworkId, payer.providerId, 6000)
				if err != nil {
					t.Fatal(err)
				}
			}()
			func() {
				pop := Testing_SetEscrowDrawOrder(c.drawOrder)
				defer pop()
				connect.AssertEqual(t, nil, CloseContract(ctx, escrow.ContractId, payer.payerId, 3000, false))
				result, err := ReconcileContractAtDeadline(ctx, escrow.ContractId, server.NowUtc().Add(2*time.Hour))
				if err != nil {
					t.Fatal(err)
				}
				connect.AssertEqual(t, result.Charged, ByteCount(3000))
				_, paid := testingEscrowPayouts(t, ctx, escrow.ContractId)
				connect.AssertEqual(t, paid[payer.packId], c.pack)
				connect.AssertEqual(t, paid[payer.lineId], c.line)
			}()
		}
	})
}

// An explicit off in sidecar.yml does not turn the gate off while a Team's line is in window
// (E-a); once no line is, it does.
func TestEscrowDrawOrderGateStaysOnUnderALiveLine(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		pop := Testing_SetEscrowDrawOrder(false)
		defer pop()
		connect.AssertEqual(t, EscrowDrawOrderEnabled(), false)

		networkId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team (network_id) VALUES ($1)`, networkId))
		})
		now := server.NowUtc()
		credit := string(GrantKindTeamCredit)
		lineId := testingInsertEscrowDrawBalance(t, ctx, networkId, 1024, 1, &credit, now.Add(-time.Hour), now.Add(time.Hour))
		Testing_LookForEscrowDrawOrderLine()
		connect.AssertEqual(t, EscrowDrawOrderEnabled(), true)
		connect.AssertEqual(t, EscrowDrawOrderOpenForCredit(), false)

		// a line that has ended keeps nothing on
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET end_time = $2 WHERE balance_id = $1`, lineId, now.Add(-time.Minute)))
		})
		Testing_LookForEscrowDrawOrderLine()
		connect.AssertEqual(t, EscrowDrawOrderEnabled(), false)
	})
}

// The spread over large free grants never selects the line: preferred needs zero revenue, and
// the line carries revenue. A test, not a comment (QUOTA-M4-01).
func TestEscrowDrawOrderSpreadNeverSelectsTheLine(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		free := string(GrantKindFree)
		credit := string(GrantKindTeamCredit)
		large := ProberTransferBalanceTopUp
		first := testingInsertEscrowDrawBalance(t, ctx, networkId, large, 0, &free, now.Add(-time.Hour), now.Add(time.Hour))
		second := testingInsertEscrowDrawBalance(t, ctx, networkId, large, 0, &free, now.Add(-time.Hour), now.Add(2*time.Hour))
		line := testingInsertEscrowDrawBalance(t, ctx, networkId, 10*large, 1, &credit, now.Add(-time.Hour), now.Add(3*time.Hour))

		preferred := map[server.Id]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, redisGrantSpreadSelectionSql, networkId, now, proberGrantFirstCount, ProberTransferBalanceTopUp)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					balance := &escrowTransferBalance{}
					var internal, isPreferred bool
					server.Raise(rows.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount, &balance.startTime, &balance.endTime, &internal, &isPreferred))
					preferred[balance.balanceId] = isPreferred
				}
			})
			rows, err = conn.Query(ctx, proberGrantExtendedSql, networkId, now, proberGrantExtendedCount, 0, ProberTransferBalanceTopUp)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					balance := &escrowTransferBalance{}
					var isPreferred bool
					server.Raise(rows.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount, &balance.startTime, &balance.endTime, &isPreferred))
					if isPreferred && balance.balanceId == line {
						t.Errorf("the extended window prefers the line")
					}
				}
			})
		})
		connect.AssertEqual(t, len(preferred), 3)
		connect.AssertEqual(t, preferred[first], true)
		connect.AssertEqual(t, preferred[second], true)
		connect.AssertEqual(t, preferred[line], false)
	})
}

// The Redis admission census and the zero-byte anchor, the sites QUOTA-M4-02 keys by the flag: on,
// both take the pack before a line that ends first; off, today's order takes the line.
func TestEscrowDrawOrderCensusAndAnchorTakeThePackFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, drawOrder := range []bool{true, false} {
			func() {
				pop := Testing_SetEscrowDrawOrder(drawOrder)
				defer pop()
				payer := newTestingEscrowDrawPayer(t, ctx, 4096)
				first := payer.packId
				if !drawOrder {
					first = payer.lineId
				}

				escrow, err := CreateTransferEscrow(ctx, payer.payerNetworkId, payer.payerId, payer.providerNetworkId, payer.providerId, 1024)
				if err != nil {
					t.Fatal(err)
				}
				drainRedisContractCompensations(t, ctx)
				connect.AssertEqual(t, len(escrow.Balances), 1)
				connect.AssertEqual(t, escrow.Balances[0].BalanceId, first)

				anchor, err := CreateTransferEscrow(ctx, payer.payerNetworkId, payer.payerId, payer.providerNetworkId, payer.providerId, 0)
				if err != nil {
					t.Fatal(err)
				}
				escrowed, _ := testingEscrowPayouts(t, ctx, anchor.ContractId)
				_, anchored := escrowed[first]
				connect.AssertEqual(t, len(escrowed), 1)
				connect.AssertEqual(t, anchored, true)
			}()
		}
	})
}
