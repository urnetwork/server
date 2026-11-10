package model

import (
	"context"
	"slices"
	"time"

	"github.com/urnetwork/server"
)

// A payer's transfer balances in draw order (DESIGN.md 20.9's Draw order, D-QUOTA-9), for the draws
// sidecar billing makes: evs's bytes at a usage batch's apply (QUOTA-M2-05), which have no reservation,
// leases (QUOTA-M2-07) and relayed reservations at CreateContract (QUOTA-M2-08). The order is
// escrowDrawLess's (escrow_draw_order.go), the one order of a payer's balances: lexicographic on
// (credit, end time, start time, balance id), false first, so a team_credit line pays only after every
// other in-window balance. credit is the flag every loader selects, never the equality spelling, which
// would sort older balances' NULL grant_kind after the line. These draws are sidecar billing's own and
// read the flag whether or not escrow's sites follow the escrow_draw_order gate: until Team checkout
// writes the first team_credit balance (QUOTA-M6-02, after the gate is on) the flag is false on every
// balance and the order is escrow's.
//
// The read takes no balance lock: a debit after use draws what the read saw, so two batches applying
// at once may draw the same room, and the flush overdraws the first balance by the difference while a
// later one keeps room. Every byte still debits once; only which balance pays may differ (§20.10's
// debit after use, with overdraft).

// One of a payer's balances as a draw reads it.
type DrawOrderBalance struct {
	BalanceId server.Id
	StartTime time.Time
	EndTime   time.Time
	// A team_credit line, drawn after every other balance.
	Credit bool
	// balance_byte_count: the grant less every debit applied. It may read below zero.
	BalanceByteCount int64
	// Debits journaled against the balance and not yet applied, in service_debit_journal and
	// transfer_debit_journal.
	PendingByteCount int64
}

// What a draw may still take: the balance less its pending debits, never below zero.
func (self *DrawOrderBalance) RoomByteCount() int64 {
	return max(0, self.BalanceByteCount-self.PendingByteCount)
}

// The draw order as a comparison: escrowDrawLess's keys, the credit line last, then by end time,
// start time and balance id.
func compareDrawOrderBalances(a *DrawOrderBalance, b *DrawOrderBalance) int {
	return compareEscrowDrawKeys(a.drawKey(), b.drawKey())
}

type DrawOrderSettings struct {
	// The most in-window balances one network's draw reads. A network holds a few at once: its
	// grant, its subscription and its data packs.
	MaxBalanceCount int
}

func DefaultDrawOrderSettings() *DrawOrderSettings {
	return &DrawOrderSettings{
		MaxBalanceCount: 1000,
	}
}

var drawOrderSettings = DefaultDrawOrderSettings()

// The network's balances in window at now, in draw order, each with its pending debits. Depleted
// balances are included: past every balance's room a debit after use overdraws the last in-window one.
func LoadDrawOrderBalances(ctx context.Context, networkId server.Id, now time.Time) (balances []*DrawOrderBalance) {
	server.Db(ctx, func(conn server.PgConn) {
		balances = loadDrawOrderBalancesWithQuery(ctx, conn, networkId, now)
	})
	return
}

// LoadDrawOrderBalances in the caller's transaction, still with no balance lock.
func LoadDrawOrderBalancesInTx(ctx context.Context, tx server.PgTx, networkId server.Id, now time.Time) []*DrawOrderBalance {
	return loadDrawOrderBalancesWithQuery(ctx, tx, networkId, now)
}

func loadDrawOrderBalancesWithQuery(ctx context.Context, query server.PgCanQuery, networkId server.Id, now time.Time) []*DrawOrderBalance {
	// service_debit_journal is MG2's: a binary that does not link it has no service debits to count
	servicePending := `0`
	if server.SidecarMg2Linked() {
		servicePending = `COALESCE((
			SELECT SUM(service.debit_byte_count)
			FROM service_debit_journal service
			WHERE service.balance_id = balance.balance_id AND NOT service.applied
		), 0)`
	}
	balances := []*DrawOrderBalance{}
	result, err := query.Query(
		ctx,
		`
			SELECT
				balance.balance_id,
				balance.start_time,
				balance.end_time,
				(balance.grant_kind IS NOT DISTINCT FROM 'team_credit'),
				balance.balance_byte_count,
				`+servicePending+` + COALESCE((
					SELECT SUM(transfer.debit_byte_count)
					FROM transfer_debit_journal transfer
					WHERE transfer.balance_id = balance.balance_id AND NOT transfer.applied
				), 0)
			FROM transfer_balance balance
			WHERE
				balance.network_id = $1 AND
				balance.start_time <= $2 AND
				$2 < balance.end_time
			ORDER BY
				(balance.grant_kind IS NOT DISTINCT FROM 'team_credit'),
				balance.end_time,
				balance.start_time,
				balance.balance_id
			LIMIT $3
		`,
		networkId,
		now.UTC(),
		drawOrderSettings.MaxBalanceCount,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			balance := &DrawOrderBalance{}
			server.Raise(result.Scan(
				&balance.BalanceId,
				&balance.StartTime,
				&balance.EndTime,
				&balance.Credit,
				&balance.BalanceByteCount,
				&balance.PendingByteCount,
			))
			balances = append(balances, balance)
		}
	})
	// the comparator is the order, whatever the planner returned
	slices.SortStableFunc(balances, compareDrawOrderBalances)
	return balances
}
