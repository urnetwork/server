// A close alone reads each grant's room as the escrow read before 2026-11-26
// computed it in SQL, and charges exactly the same.
package model

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The escrow read a close alone sent before 2026-11-26: each row's room
// computed in the statement by a sum correlated to the row. Kept as the
// control whose rooms readDeadlineEscrowRoomsInTx must reproduce.
var deadlineEscrowRowsCorrelatedSql = fmt.Sprintf(`SELECT e.balance_id,e.balance_byte_count,COALESCE(b.start_balance_byte_count,0),
	COALESCE(b.net_revenue_nano_cents,0),b.network_id,
	GREATEST(0,COALESCE(b.balance_byte_count,0)::numeric-COALESCE((SELECT sum(debit_byte_count)
	FROM transfer_debit_journal j WHERE j.balance_id=e.balance_id AND NOT j.applied),0))::bigint,
	e.settled,e.payout_byte_count,e.redis_reserved,
	(SELECT debit_byte_count FROM transfer_debit_journal j WHERE j.balance_id=e.balance_id AND j.contract_id=e.contract_id)
	FROM unnest(ARRAY[$1::uuid]) AS requested(contract_id)
	CROSS JOIN LATERAL (SELECT contract_id,balance_id,balance_byte_count,settled,payout_byte_count,redis_reserved
		FROM transfer_escrow WHERE contract_id=requested.contract_id ORDER BY balance_id LIMIT %d) AS e
	LEFT JOIN transfer_balance b USING(balance_id)
	ORDER BY COALESCE(%s,false) DESC,b.end_time NULLS LAST,e.balance_id`, deadlineEscrowRowLimit+1, transferBalanceFundsFirstSqlOf("b"))

// A contract's escrow rows by a statement of the control's shape, its sixth
// column the room.
func readDeadlineEscrowsBySqlInTx(ctx context.Context, tx server.PgTx, sql string, contractId server.Id) []contractDeadlineEscrow {
	escrows := []contractDeadlineEscrow{}
	rows, err := tx.Query(ctx, sql, contractId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var escrow contractDeadlineEscrow
			server.Raise(rows.Scan(&escrow.balanceId, &escrow.amount, &escrow.start, &escrow.revenue, &escrow.networkId,
				&escrow.available, &escrow.settled, &escrow.priorPayout, &escrow.redisReserved, &escrow.journal))
			escrows = append(escrows, escrow)
		}
	})
	return escrows
}

// A close alone's read: the rows and then each grant's room.
func readDeadlineEscrowsWithRoomsInTx(ctx context.Context, tx server.PgTx, contractId server.Id) []contractDeadlineEscrow {
	escrows := readDeadlineEscrowRowsInTx(ctx, tx, contractId)
	readDeadlineEscrowRoomsInTx(ctx, tx, escrows)
	return escrows
}

// Every field a close decides with, in order; the balance is the new read's
// input and not part of the control.
func deadlineEscrowsText(escrows []contractDeadlineEscrow) string {
	optional := func(value *ByteCount) string {
		if value == nil {
			return "null"
		}
		return fmt.Sprint(*value)
	}
	lines := []string{}
	for _, escrow := range escrows {
		networkId := "null"
		if escrow.networkId != nil {
			networkId = escrow.networkId.String()
		}
		lines = append(lines, fmt.Sprintf("%s amount=%d start=%d revenue=%d network=%s room=%d settled=%t prior=%s redis=%t journal=%s",
			escrow.balanceId, escrow.amount, escrow.start, escrow.revenue, networkId, escrow.available,
			escrow.settled, optional(escrow.priorPayout), escrow.redisReserved, optional(escrow.journal)))
	}
	return strings.Join(lines, "\n")
}

// A payout map in balance order.
func deadlineEscrowPayoutsText(escrows []contractDeadlineEscrow, payouts map[server.Id]sweepPayout) string {
	lines := []string{}
	for _, escrow := range escrows {
		if payout, ok := payouts[escrow.balanceId]; ok {
			lines = append(lines, fmt.Sprintf("%s %+v", escrow.balanceId, payout))
		}
	}
	return strings.Join(lines, "\n")
}

// Several contracts over grants that cover every branch of the room: pending
// and applied debits, a pending sum below, at and above the balance, a sum
// past the largest bigint, a zero and a negative balance, a grant that does
// not exist (with a pending debit of its own), the contract's own pending
// debit, a settled escrow and a Redis-reserved one, a grant two contracts
// share and a paid Pro grant that funds first. For each contract the rows and
// rooms of a close alone equal the control's in order, and so do the payouts
// the close computes from them at several reported usages.
func TestDeadlineEscrowRoomsEqualTheCorrelatedRead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		payerNetworkId := server.NewId()
		otherNetworkId := server.NewId()

		type grant struct {
			networkId server.Id
			balance   ByteCount
			paidPro   bool
			end       time.Duration
		}
		grantIds := map[string]server.Id{}
		grants := map[string]grant{
			"pending_and_applied": {payerNetworkId, 1000, false, 1 * time.Hour},
			"untouched":           {payerNetworkId, 50, false, 2 * time.Hour},
			"over":                {payerNetworkId, 100, false, 3 * time.Hour},
			"exact":               {payerNetworkId, 400, false, 4 * time.Hour},
			"past_bigint":         {payerNetworkId, 5000, false, 5 * time.Hour},
			"zero":                {payerNetworkId, 0, false, 6 * time.Hour},
			"negative":            {payerNetworkId, -300, false, 7 * time.Hour},
			"applied_only":        {payerNetworkId, 600, false, 8 * time.Hour},
			"own_debit":           {payerNetworkId, 900, false, 9 * time.Hour},
			"paid_pro":            {payerNetworkId, 700, true, 10 * time.Hour},
			"other_network":       {otherNetworkId, 800, false, 30 * time.Minute},
		}
		for name := range grants {
			grantIds[name] = server.NewId()
		}
		grantIds["missing"] = server.NewId()

		contracts := map[string]server.Id{}
		for _, name := range []string{"several", "saturation", "edges", "flags", "none"} {
			contracts[name] = server.NewId()
		}
		type escrow struct {
			grant    string
			amount   ByteCount
			settled  bool
			payout   *ByteCount
			reserved bool
		}
		seventy := ByteCount(70)
		escrows := map[string][]escrow{
			"several":    {{grant: "pending_and_applied", amount: 300}, {grant: "untouched", amount: 40}, {grant: "missing", amount: 25}},
			"saturation": {{grant: "over", amount: 90}, {grant: "exact", amount: 90}, {grant: "past_bigint", amount: 90}, {grant: "pending_and_applied", amount: 90}},
			"edges":      {{grant: "zero", amount: 10}, {grant: "negative", amount: 10}, {grant: "applied_only", amount: 10}, {grant: "own_debit", amount: 500}, {grant: "paid_pro", amount: 200}, {grant: "other_network", amount: 30}},
			"flags":      {{grant: "pending_and_applied", amount: 100, settled: true, payout: &seventy}, {grant: "untouched", amount: 20, reserved: true}, {grant: "paid_pro", amount: 60, reserved: true}},
		}
		type debit struct {
			grant    string
			contract string
			amount   ByteCount
			applied  bool
		}
		debits := []debit{
			{grant: "pending_and_applied", amount: 100},
			{grant: "pending_and_applied", amount: 200},
			{grant: "pending_and_applied", amount: 500, applied: true},
			{grant: "over", amount: 250},
			{grant: "exact", amount: 150},
			{grant: "exact", amount: 250},
			{grant: "past_bigint", amount: 1 << 62},
			{grant: "past_bigint", amount: 1 << 62},
			{grant: "negative", amount: 50},
			{grant: "applied_only", amount: 300, applied: true},
			{grant: "own_debit", contract: "edges", amount: 120},
			{grant: "own_debit", amount: 80},
			{grant: "missing", amount: 35},
			{grant: "paid_pro", amount: 10},
			{grant: "untouched", contract: "flags", amount: 0},
		}

		server.Tx(ctx, func(tx server.PgTx) {
			for name, g := range grants {
				// a paid Pro grant funds first (transferBalanceFundsFirstSqlOf)
				revenue := NanoCents(0)
				var grantKind *string
				if g.paidPro {
					revenue = 1000
					kind := string(GrantKindPro)
					grantKind = &kind
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_balance
					(balance_id,network_id,start_time,end_time,start_balance_byte_count,net_revenue_nano_cents,balance_byte_count,pro,grant_kind)
					VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
					grantIds[name], g.networkId, now.Add(-time.Hour), now.Add(g.end), max(g.balance, 1000),
					revenue, g.balance, g.paidPro, grantKind))
			}
			for name, rows := range escrows {
				for _, row := range rows {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow
						(contract_id,balance_id,balance_byte_count,settled,payout_byte_count,redis_reserved)
						VALUES ($1,$2,$3,$4,$5,$6)`,
						contracts[name], grantIds[row.grant], row.amount, row.settled, row.payout, row.reserved))
				}
			}
			for _, row := range debits {
				contractId := server.NewId()
				if row.contract != "" {
					contractId = contracts[row.contract]
				}
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal
					(contract_id,balance_id,debit_byte_count,shard,applied) VALUES ($1,$2,$3,$4,$5)`,
					contractId, grantIds[row.grant], row.amount, transferDebitShard(grantIds[row.grant]), row.applied))
			}
		})

		wantRooms := map[string]ByteCount{
			"pending_and_applied": 700, "untouched": 50, "missing": 0, "over": 0, "exact": 0, "past_bigint": 0,
			"zero": 0, "negative": 0, "applied_only": 600, "own_debit": 700, "paid_pro": 690, "other_network": 800,
		}
		for name, contractId := range contracts {
			var control, read []contractDeadlineEscrow
			server.Tx(ctx, func(tx server.PgTx) {
				control = readDeadlineEscrowsBySqlInTx(ctx, tx, deadlineEscrowRowsCorrelatedSql, contractId)
				read = readDeadlineEscrowsWithRoomsInTx(ctx, tx, contractId)
			}, server.TxReadCommitted)
			if len(control) != len(escrows[name]) {
				t.Fatalf("%s: the control read %d escrow rows, want %d", name, len(control), len(escrows[name]))
			}
			if want, got := deadlineEscrowsText(control), deadlineEscrowsText(read); want != got {
				t.Fatalf("%s: a close alone's escrow rows and rooms differ from the correlated read's\nwant:\n%s\ngot:\n%s", name, want, got)
			}
			for _, escrow := range read {
				for grantName, grantId := range grantIds {
					if grantId == escrow.balanceId && escrow.available != wantRooms[grantName] {
						t.Fatalf("%s: grant %s has room %d, want %d", name, grantName, escrow.available, wantRooms[grantName])
					}
				}
			}
			for _, used := range []ByteCount{0, 1, 89, 250, 700, 2000, math.MaxInt64} {
				for _, payerId := range []server.Id{payerNetworkId, otherNetworkId} {
					var controlAdjustments, readAdjustments []string
					controlPayouts := deadlineEscrowPayouts(used, control, payerId, true, &controlAdjustments)
					readPayouts := deadlineEscrowPayouts(used, read, payerId, true, &readAdjustments)
					if want, got := deadlineEscrowPayoutsText(control, controlPayouts), deadlineEscrowPayoutsText(read, readPayouts); want != got ||
						strings.Join(controlAdjustments, ",") != strings.Join(readAdjustments, ",") {
						t.Fatalf("%s used=%d: the close's payouts differ from the correlated read's\nwant:\n%s %v\ngot:\n%s %v",
							name, used, want, controlAdjustments, got, readAdjustments)
					}
				}
			}
		}
		// the funds-first grant leads the edges contract, as the control orders it
		var edges []contractDeadlineEscrow
		server.Tx(ctx, func(tx server.PgTx) {
			edges = readDeadlineEscrowsWithRoomsInTx(ctx, tx, contracts["edges"])
		}, server.TxReadCommitted)
		if len(edges) == 0 || edges[0].balanceId != grantIds["paid_pro"] {
			t.Fatal("the paid Pro grant does not fund the edges contract first")
		}
	})
}
