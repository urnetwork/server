// Short Redis leases shed repeated legacy grant contention before the durable
// transaction. They never authorize a debit or replace PostgreSQL ownership.
package model

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"github.com/urnetwork/server"
)

const legacySettlementAdmissionGrantLimit = 32
const legacySettlementAdmissionBudget = 100 * time.Millisecond
const legacySettlementAdmissionLease = 2 * time.Second

// The contract-first primary key bounds this lookup independently of all escrow
// history. Missing, oversized or unreadable membership uses normal PG ownership.
const legacySettlementAdmissionGrantsSql = `SELECT balance_id FROM transfer_escrow
 WHERE contract_id=$1 ORDER BY balance_id LIMIT 33`

// Each single-key script uses an explicit hash tag and the deadline pool. A
// malformed or non-expiring value is unknown, never evidence for a busy result.
const legacySettlementAdmissionAcquireLua = `
local current = redis.call('GET', KEYS[1])
if current then
  local ttl = redis.call('PTTL', KEYS[1])
  if string.len(current) == 35 and string.match(current, '^v1:%x+$') and ttl > 0 and ttl <= tonumber(ARGV[2]) then
    return 0
  end
  return 2
end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return 1`

const legacySettlementAdmissionReleaseLua = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`

// Only this token can release its acquired or ambiguously acquired keys. A lost
// release leaves at most the fixed lease lifetime, including partial acquisition.
type legacySettlementAdmission struct {
	token string
	keys  []string
}

// Every head revisit keeps its original PG attempt, including nonwaiting heads.
// A stream of fresh pre-transaction Redis owners must not exclude those rows.
type legacySettlementAdmissionHeadKey struct{}

// A page must reach the authoritative grant gate before honoring later hints
// for an observed escrow set. The transaction may observe newer membership;
// the progress guarantee assumes stable membership. Intent/contract contention
// does not satisfy this probe. The map is page-local and visit-bounded.
type legacySettlementAdmissionPageKey struct{}

type legacySettlementAdmissionPage struct {
	allowForwardHints bool
	probed            map[string]bool
}

type legacySettlementAdmissionAttempt struct {
	owner    *legacySettlementAdmission
	page     *legacySettlementAdmissionPage
	grantSet string
}

// Keep this key independent of packet authorization and escrow projections.
func legacySettlementAdmissionKey(balanceId server.Id) string {
	return "legacy_settlement_owner:{" + balanceId.String() + "}:v1"
}

// Optional metadata errors cannot become accounting failures or alter retries.
func legacySettlementAdmissionGrantIds(ctx context.Context, contractId server.Id) (balanceIds []server.Id) {
	defer func() {
		if recover() != nil {
			balanceIds = nil
		}
	}()
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, legacySettlementAdmissionGrantsSql, contractId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var balanceId server.Id
				server.Raise(rows.Scan(&balanceId))
				balanceIds = append(balanceIds, balanceId)
			}
		})
	}, server.OptNoRetry())
	if len(balanceIds) == 0 || len(balanceIds) > legacySettlementAdmissionGrantLimit {
		return nil
	}
	for index, balanceId := range balanceIds {
		if balanceId == (server.Id{}) || (index > 0 && balanceId == balanceIds[index-1]) {
			return nil
		}
	}
	return balanceIds
}

// Every head revisit bypasses hints; allocated waits can still join the real
// grant lock queue. Other callers admit all known grants or release their prefix;
// Redis errors, ambiguous replies and unknown metadata fall through to PG.
func tryLegacySettlementAdmission(ctx context.Context, contractId server.Id, wait *legacySettlementGrantWait) (*legacySettlementAdmissionAttempt, bool) {
	head, _ := ctx.Value(legacySettlementAdmissionHeadKey{}).(bool)
	if wait != nil || head {
		return nil, false
	}
	page, _ := ctx.Value(legacySettlementAdmissionPageKey{}).(*legacySettlementAdmissionPage)
	// A fresh page may reach EOF and restart without any head revisit. Keep
	// its original PG opportunities and avoid repeated lookup/lease churn.
	// Only a real continuation can use hints for its forward lane.
	if page != nil && !page.allowForwardHints {
		return nil, false
	}
	bounded, cancel := context.WithTimeout(ctx, legacySettlementAdmissionBudget)
	defer cancel()
	balanceIds := legacySettlementAdmissionGrantIds(bounded, contractId)
	if len(balanceIds) == 0 {
		return nil, false
	}
	key := make([]byte, 0, 16*len(balanceIds))
	for _, balanceId := range balanceIds {
		key = append(key, balanceId[:]...)
	}
	attempt := &legacySettlementAdmissionAttempt{page: page, grantSet: string(key)}
	owner, busy := acquireLegacySettlementAdmission(bounded, balanceIds)
	attempt.owner = owner
	// A direct single-contract caller, a first set encounter or an exhausted
	// page-local map always gets the existing PG opportunity.
	return attempt, busy && page != nil && page.probed[attempt.grantSet]
}

// Record only a returned transaction that completed or reached the real grant
// ownership gate. Failed/ambiguous transactions never authorize later shedding.
func (self *legacySettlementAdmissionAttempt) finish(ctx context.Context, transactionReturned, completed bool, busyGate legacySettlementBusyGate) {
	if self == nil {
		return
	}
	self.owner.release(ctx)
	if transactionReturned && (completed || busyGate == legacySettlementBusyGrantSet) && self.page != nil && len(self.page.probed) < LegacySettlementPageLimit {
		self.page.probed[self.grantSet] = true
	}
}

// Copy and sort the bounded grant set before acquiring any lease. There is no
// wait while holding a partial Redis set and no cross-slot script or transaction.
func acquireLegacySettlementAdmission(ctx context.Context, balanceIds []server.Id) (*legacySettlementAdmission, bool) {
	if len(balanceIds) == 0 || len(balanceIds) > legacySettlementAdmissionGrantLimit {
		return nil, false
	}
	ids := append([]server.Id(nil), balanceIds...)
	sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
	for index, balanceId := range ids {
		if balanceId == (server.Id{}) || (index > 0 && balanceId == ids[index-1]) {
			return nil, false
		}
	}
	id := server.NewId()
	owner := &legacySettlementAdmission{token: "v1:" + hex.EncodeToString(id[:])}
	busy := false
	err := server.RedisWithDeadline(ctx, func(r server.RedisClient) error {
		for _, balanceId := range ids {
			key := legacySettlementAdmissionKey(balanceId)
			// Include an ambiguous final SET in token-checked cleanup.
			owner.keys = append(owner.keys, key)
			value, err := r.Eval(ctx, legacySettlementAdmissionAcquireLua, []string{key}, owner.token, legacySettlementAdmissionLease.Milliseconds()).Int64()
			if err != nil {
				return err
			}
			if value == 0 {
				owner.keys = owner.keys[:len(owner.keys)-1]
				busy = true
				return nil
			}
			if value != 1 {
				owner.keys = owner.keys[:len(owner.keys)-1]
				return fmt.Errorf("legacy settlement admission metadata is unknown")
			}
		}
		return nil
	})
	if err == nil && !busy {
		return owner, false
	}
	owner.release(ctx)
	return nil, err == nil && busy
}

// Run only after the financial transaction has unwound and released its PG
// connection. No joined projection or stream callback extends the hint lease.
func (self *legacySettlementAdmission) release(ctx context.Context) {
	if self == nil || len(self.keys) == 0 {
		return
	}
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), legacySettlementAdmissionBudget)
	defer cancel()
	_ = server.RedisWithDeadline(bounded, func(r server.RedisClient) error {
		for _, key := range self.keys {
			if err := r.Eval(bounded, legacySettlementAdmissionReleaseLua, []string{key}, self.token).Err(); err != nil {
				return err
			}
		}
		return nil
	})
}
