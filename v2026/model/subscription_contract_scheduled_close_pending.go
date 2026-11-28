// An owner turn of deadline closes reads each grant's pending journal sum at
// most once for all of its members (subscription_contract_scheduled_close_owner.go).
package model

import (
	"context"
	"errors"
	"math"

	"github.com/urnetwork/server/v2026"
)

// One owner turn's pending journal sums by grant. The turn holds each grant's
// owner and, from its first member on the grant, the grant's row, so the
// flush, every other deadline close and every legacy debit of the grant wait
// for the turn; the turn counts its own journal debits as it writes them. An
// ordinary Redis settlement takes neither: it can append a pending debit to
// the grant at any time, and a per-member read saw that debit in the later
// members. check proves before commit that no such debit could have changed a
// decision the read sum bounded; otherwise the turn fails, and its members
// close alone, each with its own read, as before.
type deadlineTurnPending struct {
	locked map[server.Id]bool
	grants map[server.Id]*deadlineTurnPendingGrant
}

type deadlineTurnPendingGrant struct {
	// The pending sum read once in the turn, and the turn's own journal debits
	// on the grant after that read.
	read      ByteCount
	journaled ByteCount
	// The decisions the read bounded. Each positive count left at least slack
	// of its room, and capped is whether some room was below its need.
	decisions int
	slack     ByteCount
	capped    bool
}

func newDeadlineTurnPending() *deadlineTurnPending {
	return &deadlineTurnPending{locked: map[server.Id]bool{}, grants: map[server.Id]*deadlineTurnPendingGrant{}}
}

// The grant rows the turn's FOR UPDATE returned.
func (self *deadlineTurnPending) lock(balanceIds []server.Id) {
	for _, balanceId := range balanceIds {
		self.locked[balanceId] = true
	}
}

// How much of a valid escrow's positive need its grant's room allows. The
// room is a close alone's available (readDeadlineEscrowRoomsInTx): the
// balance less the pending sum, at least 0.
func (self *deadlineTurnPending) count(ctx context.Context, tx server.PgTx, escrow *contractDeadlineEscrow, need ByteCount) ByteCount {
	if escrow.balance <= 0 {
		// A pending sum is never negative, so no read can give room.
		return 0
	}
	if !self.locked[escrow.balanceId] {
		// The turn does not hold this row; read it each time, as before.
		return min(need, deadlinePendingRoom(escrow.balance, readDeadlinePendingDebit(ctx, tx, escrow.balanceId)))
	}
	grant := self.grants[escrow.balanceId]
	if grant == nil {
		grant = &deadlineTurnPendingGrant{read: readDeadlinePendingDebit(ctx, tx, escrow.balanceId), slack: math.MaxInt64}
		self.grants[escrow.balanceId] = grant
	}
	room := deadlinePendingRoom(escrow.balance, deadlinePendingSum(grant.read, grant.journaled))
	count := min(need, room)
	grant.decisions++
	if 0 < count {
		grant.slack = min(grant.slack, room-count)
	}
	if count < need {
		grant.capped = true
	}
	return count
}

// A journal debit the turn wrote; one written before the grant's read is in
// the read.
func (self *deadlineTurnPending) journaled(balanceId server.Id, debit ByteCount) {
	if grant := self.grants[balanceId]; grant != nil {
		grant.journaled = deadlinePendingSum(grant.journaled, max(0, debit))
	}
}

var errDeadlineTurnPendingChanged = errors.New("a grant's pending debits changed during its deadline owner turn")

// Reads again, before commit, each grant whose read bounded more than one
// decision. While the turn holds the grant only another writer's additions
// change the sum. A decision with count c and room r keeps c under any
// addition up to r-c, and a count of 0 (no room) stays 0, so a change within
// every slack leaves each decision the one the per-member read would have
// made at any point of the turn. A larger change, or a sum that fell under a
// capped decision, fails the turn.
func (self *deadlineTurnPending) check(ctx context.Context, tx server.PgTx) {
	for balanceId, grant := range self.grants {
		if grant.decisions < 2 {
			continue
		}
		expected := deadlinePendingSum(grant.read, grant.journaled)
		current := readDeadlinePendingDebit(ctx, tx, balanceId)
		switch {
		case current == expected:
		case expected < current && current < math.MaxInt64 && current-expected <= grant.slack:
		case current < expected && !grant.capped:
		default:
			server.Raise(errDeadlineTurnPendingChanged)
		}
	}
}

func readDeadlinePendingDebit(ctx context.Context, tx server.PgTx, balanceId server.Id) (pending ByteCount) {
	server.Raise(tx.QueryRow(ctx, deadlinePendingDebitSql, balanceId).Scan(&pending))
	return
}

// The sum of two counts that are not negative, saturated as the read is.
func deadlinePendingSum(a, b ByteCount) ByteCount {
	if math.MaxInt64-a < b {
		return math.MaxInt64
	}
	return a + b
}

// The balance less a pending sum that is not negative, at least 0.
func deadlinePendingRoom(balance, pending ByteCount) ByteCount {
	if balance <= pending {
		return 0
	}
	return balance - pending
}
