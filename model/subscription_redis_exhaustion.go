// A payer whose grants are all reserved is refused on every request, and the
// census that refuses it is the costliest path selection has. It reads the
// grants in up to three discovery queries, sends one reservation and probes
// the rest (subscription_redis_grant_probe.go), reserves whatever room a grant
// still has only to release it again in the refusal's compensation, and runs
// the request's recovery page. On 2026-10-26 the platform's own proxy network,
// with thirty-two grants, paid that on hundreds of requests a second, and the
// compensations of its refusals failed on the deadline pool that every
// reservation shares.
//
// After a census in this process refuses a payer as exhausted, the payer's
// requests for a short hold first try to prove the same refusal: the census's
// keyset read of the grants and one pipelined probe of each grant with
// credit. The proof reserves nothing, so the refusal needs no compensation,
// and only grants holding a recovery or pending marker join the request's
// recovery page, as they would after the census. When any probe is not exact,
// a grant could fund the whole request, the room left reaches the shrink
// floor, or the census could stop at a selection limit first, the proof
// decides nothing, the census runs as before, and the payer's requests go to
// the census for the rest of the hold.
//
// The proof is the census at one instant. Each probe reads its grant in the
// reservation script's plain state, where a reservation computes the same
// availability from the same keys: a whole reservation reserves only a grant
// with room for the whole request, and a partial one the room left, up to the
// request. A census whose reservations read each grant when the proof read it
// refuses every whole attempt, reserves each grant's room, stops below the
// shrink floor with that total and refuses with the proof's error, then
// releases the room in its compensation. Recovery discovery holds the same
// marked grants, in keyset order.
//
// The list of exhausted payers only chooses the proof and never refuses a
// request: a stale entry costs one read and one probe pipeline before the
// census, a missing one the census itself. A rerun after the request's own
// recovery page released bytes runs the census directly.
package model

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// How long a refusing census, or a proof, keeps its payer on the list.
const redisExhaustedPayerHold = 5 * time.Second

// The most payers the list holds at once.
const redisExhaustedPayerCapacity = 64 * 1024

// The most rows the census's spread windows read before its keyset pages
// (subscription_redis_selection.go).
const redisGrantWindowRows = proberGrantFirstCount + proberGrantExtendedCount

// The payers this process last found exhausted, each until a hold after its
// last refusing census or proof. A payer whose proof decided nothing stays
// with the census for its hold, so a state the proof cannot decide, such as
// grants near a selection limit or a failing probe read, costs at most one
// proof per hold. A full list drops its expired payers at most once per hold
// and otherwise adds none. Safe for concurrent use.
type redisExhaustedPayers struct {
	hold     time.Duration
	capacity int

	stateLock    sync.Mutex
	payerIdHolds map[server.Id]redisExhaustedPayerHoldState
	pruneTime    time.Time
}

type redisExhaustedPayerHoldState struct {
	until time.Time
	// the last proof decided nothing; the census runs until the hold ends
	unproven bool
}

var exhaustedRedisPayers = newRedisExhaustedPayers(redisExhaustedPayerHold, redisExhaustedPayerCapacity)

// A test environment reset starts the next environment with no payer listed.
func init() {
	server.OnReset(exhaustedRedisPayers.clear)
}

func newRedisExhaustedPayers(hold time.Duration, capacity int) *redisExhaustedPayers {
	return &redisExhaustedPayers{
		hold:         hold,
		capacity:     capacity,
		payerIdHolds: map[server.Id]redisExhaustedPayerHoldState{},
	}
}

// Whether the payer's next request tries the proof: its last refusal in this
// process is within the hold, and no proof since decided nothing.
func (self *redisExhaustedPayers) recent(payerNetworkId server.Id, now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	hold, ok := self.payerIdHolds[payerNetworkId]
	if ok && !now.Before(hold.until) {
		delete(self.payerIdHolds, payerNetworkId)
		return false
	}
	return ok && !hold.unproven
}

// Lists the payer until a hold after now, unless its last proof within the
// hold decided nothing.
func (self *redisExhaustedPayers) note(payerNetworkId server.Id, now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if hold, ok := self.payerIdHolds[payerNetworkId]; ok && hold.unproven && now.Before(hold.until) {
		return
	}
	self.holdWithLock(payerNetworkId, redisExhaustedPayerHoldState{until: now.Add(self.hold)}, now)
}

// Keeps the payer with the census until a hold after now.
func (self *redisExhaustedPayers) noteUnproven(payerNetworkId server.Id, now time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.holdWithLock(payerNetworkId, redisExhaustedPayerHoldState{until: now.Add(self.hold), unproven: true}, now)
}

func (self *redisExhaustedPayers) holdWithLock(payerNetworkId server.Id, hold redisExhaustedPayerHoldState, now time.Time) {
	if _, ok := self.payerIdHolds[payerNetworkId]; !ok && self.capacity <= len(self.payerIdHolds) {
		if now.Before(self.pruneTime.Add(self.hold)) {
			return
		}
		self.pruneTime = now
		for payerId, held := range self.payerIdHolds {
			if !now.Before(held.until) {
				delete(self.payerIdHolds, payerId)
			}
		}
		if self.capacity <= len(self.payerIdHolds) {
			return
		}
	}
	self.payerIdHolds[payerNetworkId] = hold
}

// Removes the payer, which a census just funded.
func (self *redisExhaustedPayers) forget(payerNetworkId server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.payerIdHolds, payerNetworkId)
}

func (self *redisExhaustedPayers) clear() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clear(self.payerIdHolds)
	self.pruneTime = time.Time{}
}

// Reads the payer's grants as the census's keyset pages read them and probes
// each grant with credit, one pipeline per page. Reports the rows read, the
// room the census would reserve, and whether the probes prove that the census
// refuses the request as exhausted. A proof notes the grants holding a marker
// for the request's recovery page, as a census notes a skipped grant, and
// nothing else: no grant is reserved.
func proveRedisGrantExhaustion(ctx context.Context, admission *redisContractAdmission, payerNetworkId server.Id,
	now time.Time, requested ByteCount, limits redisGrantSelectionLimits) (rows int, room ByteCount, proven bool) {
	// The census stops at its row limit once its windows and keyset pages
	// reach it, and at its selected limit once that many grants have room.
	maxRows := limits.MaxRows - redisGrantWindowRows - 1
	roomCount := 0
	markedIds := []server.Id{}
	// every grant of a page in one pipeline
	probe := redisGrantProbe{contractId: admission.contractId, active: true, width: redisGrantPageRows}
	var cursor *escrowTransferBalance
	for {
		sql := escrowTransferBalanceSql
		args := []any{payerNetworkId, now}
		if cursor == nil {
			sql += ` ORDER BY end_time,start_time,balance_id LIMIT $3`
			args = append(args, redisGrantPageRows)
		} else {
			sql += ` AND (end_time,start_time,balance_id)>($3,$4,$5) ORDER BY end_time,start_time,balance_id LIMIT $6`
			args = append(args, cursor.endTime, cursor.startTime, cursor.balanceId, redisGrantPageRows)
		}
		page := []*escrowTransferBalance{}
		readRedisGrants(ctx, func(query server.PgCanQuery) {
			result, err := query.Query(ctx, sql, args...)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					balance := &escrowTransferBalance{}
					server.Raise(result.Scan(&balance.balanceId, &balance.paid, &balance.balanceByteCount, &balance.startTime, &balance.endTime))
					page = append(page, balance)
				}
			})
		})
		rows += len(page)
		if maxRows < rows {
			return rows, room, false
		}
		probe.forget()
		probe.observe(ctx, page)
		for _, balance := range page {
			cursor = balance
			// the census never reserves a grant without durable credit
			if balance.balanceByteCount <= 0 {
				continue
			}
			verdict := probe.verdicts[balance.balanceId]
			// An inexact probe defers to the census. Room for the rest of the
			// request funds it, whole or in the keyset.
			if !verdict.exact || requested-room <= verdict.available {
				return rows, room, false
			}
			if 0 < verdict.available {
				room += verdict.available
				roomCount += 1
			}
			if !verdict.idle {
				markedIds = append(markedIds, balance.balanceId)
			}
		}
		if len(page) < redisGrantPageRows {
			break
		}
	}
	if limits.MaxSelected <= roomCount {
		return rows, room, false
	}
	if _, ok := grantTransferEscrowByteCount(requested, room); ok {
		return rows, room, false
	}
	for _, balanceId := range markedIds {
		admission.noteBalance(balanceId)
		admission.noteZeroReservation(balanceId)
	}
	return rows, room, true
}
