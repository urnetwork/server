package model

// Packet authorization reads only the expiring Redis count. Committed lifecycle
// events maintain an exact membership set; an owned background task repairs lost
// events from PostgreSQL. No financial lock or packet read waits for that repair.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

const (
	ContractHoleTtl             = 60 * time.Second
	ContractHoleRefreshInterval = ContractHoleTtl / 2
	contractHoleEventLifetime   = 5 * time.Second
	contractHoleRedisTimeout    = time.Second
	contractHoleSourceTimeout   = 5 * time.Second
	contractHoleMemberLimit     = 8192
	ContractHoleRefreshPageSize = 256
)

var contractHoleCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_contract_hole_operations_total",
	Help: "Redis contract-hole projection operations by bounded outcome",
}, []string{"outcome"})

func init() {
	for _, outcome := range []string{"read_positive", "read_zero", "read_missing", "read_error", "event_expired", "event_error",
		"event_create", "event_remove", "event_invalidate", "refresh_oversized", "refresh_published", "refresh_superseded", "refresh_error"} {
		contractHoleCounter.WithLabelValues(outcome)
	}
	prometheus.MustRegister(contractHoleCounter)
}

// Client identifiers, rather than network identifiers, name this unordered pair.
// Both directions and a self-pair share exactly one count and one membership set.
func contractHoleKeys(sourceClientId, destinationClientId server.Id) []string {
	if sourceClientId.Cmp(destinationClientId) > 0 {
		sourceClientId, destinationClientId = destinationClientId, sourceClientId
	}
	prefix := fmt.Sprintf("contract-hole:v1:{%s:%s}:", sourceClientId, destinationClientId)
	return []string{prefix + "count", prefix + "members", prefix + "closed", prefix + "publication", prefix + "oversized"}
}

// Missing, expired, malformed and unavailable projections deny. This function
// never warms a cache or calls PostgreSQL; only lifecycle owners and the task
// below may populate it. The resident owns its own recheck rate limiter.
func HasOpenContractHole(ctx context.Context, sourceClientId, destinationClientId server.Id) bool {
	ctx, cancel := context.WithTimeout(ctx, contractHoleRedisTimeout)
	defer cancel()
	var count int64
	err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		var err error
		count, err = client.Get(ctx, contractHoleKeys(sourceClientId, destinationClientId)[0]).Int64()
		return err
	})
	if err != nil {
		if errors.Is(err, server.RedisNil) {
			contractHoleCounter.WithLabelValues("read_missing").Inc()
		} else {
			contractHoleCounter.WithLabelValues("read_error").Inc()
		}
		return false
	}
	if count > 0 {
		contractHoleCounter.WithLabelValues("read_positive").Inc()
		return true
	}
	contractHoleCounter.WithLabelValues("read_zero").Inc()
	return false
}

// Tombstones outlive every admitted event. An old create cannot follow a close
// after its tombstone expires because the create's fixed deadline has expired.
// Mutation also cancels an in-flight source snapshot. Membership guards make
// duplicate and ambiguous Redis replies safe to replay within that deadline.
const contractHoleEventScript = `
local now = redis.call('TIME')
local nowms = tonumber(now[1])*1000 + math.floor(tonumber(now[2])/1000)
redis.call('DEL', KEYS[4])
redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', nowms)
if ARGV[1] == 'invalidate' then
    redis.call('DEL', KEYS[1], KEYS[2])
    return 0
end
if ARGV[1] == 'oversized' then
    redis.call('SET', KEYS[5], '1', 'PX', ARGV[3])
    redis.call('DEL', KEYS[1], KEYS[2])
    return 0
end
local before = redis.call('SCARD', KEYS[2])
if before > 0 then
    redis.call('SET', KEYS[1], before, 'KEEPTTL')
else
    redis.call('DEL', KEYS[1])
end
if ARGV[1] == 'create' then
    if not redis.call('GET', KEYS[5]) and not redis.call('ZSCORE', KEYS[3], ARGV[2]) then
        if before >= tonumber(ARGV[4]) and redis.call('SISMEMBER', KEYS[2], ARGV[2]) == 0 then
            redis.call('SET', KEYS[5], '1', 'PX', ARGV[3])
            redis.call('DEL', KEYS[1], KEYS[2])
            return 0
        end
        if redis.call('SADD', KEYS[2], ARGV[2]) == 1 then
            redis.call('INCR', KEYS[1])
        end
    end
else
    redis.call('ZADD', KEYS[3], nowms + tonumber(ARGV[3]), ARGV[2])
    redis.call('PEXPIRE', KEYS[3], ARGV[3])
    if redis.call('SREM', KEYS[2], ARGV[2]) == 1 then
        redis.call('DECR', KEYS[1])
    end
end
local count = redis.call('SCARD', KEYS[2])
if count == 0 then
    redis.call('DEL', KEYS[1], KEYS[2])
else
    redis.call('PEXPIRE', KEYS[1], ARGV[3])
    redis.call('PEXPIRE', KEYS[2], ARGV[3])
end
return count
`

// Registers while the existing transaction still owns the contract. The fixed
// lifetime includes commit and scheduling delay; an old callback is discarded,
// leaving bounded background repair rather than resurrecting stale permission.
func contractHoleEventInTx(ctx context.Context, tx server.PgTx, contractId, sourceClientId, destinationClientId server.Id, operation string) {
	deadline := time.Now().Add(contractHoleEventLifetime)
	server.AddTxPostCommit(tx, "contract-hole:"+contractId.String(), contractHoleEventPost(ctx, contractId, sourceClientId, destinationClientId, operation, deadline))
}

// The captured deadline, not the eventual execution time, owns event freshness.
func contractHoleEventPost(ctx context.Context, contractId, sourceClientId, destinationClientId server.Id, operation string, deadline time.Time) server.PostFunction {
	return func() any {
		if !time.Now().Before(deadline) {
			contractHoleCounter.WithLabelValues("event_expired").Inc()
			return nil
		}
		postCtx, cancel := context.WithDeadline(ctx, deadline)
		defer cancel()
		if err := applyContractHoleEvent(postCtx, contractId, sourceClientId, destinationClientId, operation); err != nil {
			contractHoleCounter.WithLabelValues("event_error").Inc()
		} else {
			contractHoleCounter.WithLabelValues("event_" + operation).Inc()
		}
		return nil
	}
}

// Executes one deduplicated lifecycle delta without any PostgreSQL operation.
func applyContractHoleEvent(ctx context.Context, contractId, sourceClientId, destinationClientId server.Id, operation string) error {
	ctx, cancel := context.WithTimeout(ctx, contractHoleRedisTimeout)
	defer cancel()
	return server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		return client.Eval(ctx, contractHoleEventScript, contractHoleKeys(sourceClientId, destinationClientId),
			operation, contractId.String(), ContractHoleTtl.Milliseconds(), contractHoleMemberLimit).Err()
	})
}

// A new token supersedes an older snapshot. Lifecycle mutations remove it, so
// a refresh that read before a committed close cannot publish after its delta.
const contractHoleBeginRefreshScript = `
redis.call('SET', KEYS[4], ARGV[1], 'PX', ARGV[2])
return 1
`

// Installs one complete snapshot, never a partial page. Rebuild missing/drifted
// counters from membership before applying deltas. Removed members receive the
// same replay fence as a live close; authoritative reopened members clear it.
const contractHolePublishScript = `
if redis.call('GET', KEYS[4]) ~= ARGV[1] then return 0 end
redis.call('DEL', KEYS[4], KEYS[5])
local now = redis.call('TIME')
local nowms = tonumber(now[1])*1000 + math.floor(tonumber(now[2])/1000)
redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', nowms)
local wanted = {}
for i = 3, #ARGV do wanted[ARGV[i]] = true end
local before = redis.call('SMEMBERS', KEYS[2])
redis.call('SET', KEYS[1], #before)
for _, member in ipairs(before) do
    if not wanted[member] then
        redis.call('SREM', KEYS[2], member)
        redis.call('DECR', KEYS[1])
        redis.call('ZADD', KEYS[3], nowms + tonumber(ARGV[2]), member)
    end
end
for member, _ in pairs(wanted) do
    redis.call('ZREM', KEYS[3], member)
    if redis.call('SADD', KEYS[2], member) == 1 then redis.call('INCR', KEYS[1]) end
end
if redis.call('SCARD', KEYS[2]) == 0 then
    redis.call('DEL', KEYS[1], KEYS[2])
else
    redis.call('PEXPIRE', KEYS[1], ARGV[2])
    redis.call('PEXPIRE', KEYS[2], ARGV[2])
end
redis.call('PEXPIRE', KEYS[3], ARGV[2])
return 1
`

// Keep both directional branches independently indexable and count self-pairs
// once. A checkpoint may resume the same contract and therefore remains open.
// A meaningful final party close revokes before deferred financial settlement;
// empty/null party metadata does not constitute a close.
const contractHoleMembersSql = `
SELECT contract_id FROM (
    SELECT contract_id FROM transfer_contract AS contract
    WHERE (CASE WHEN contract.outcome IS NULL THEN contract.dispute = false ELSE false END)
      AND source_id = $1 AND destination_id = $2
      AND NOT EXISTS (
          SELECT 1 FROM contract_close AS close WHERE close.contract_id = contract.contract_id
            AND (NOT COALESCE(close.checkpoint, false) AND COALESCE(close.party, '') <> '') OFFSET 0
      )
    UNION ALL
    SELECT contract_id FROM transfer_contract AS contract
    WHERE $1 <> $2
      AND (CASE WHEN contract.outcome IS NULL THEN contract.dispute = false ELSE false END)
      AND source_id = $2 AND destination_id = $1
      AND NOT EXISTS (
          SELECT 1 FROM contract_close AS close WHERE close.contract_id = contract.contract_id
            AND (NOT COALESCE(close.checkpoint, false) AND COALESCE(close.party, '') <> '') OFFSET 0
      )
) AS eligible LIMIT $3
`

// Background-only source read. A fixed member ceiling bounds Redis script work;
// an oversized pair loses its projection instead of receiving a partial count.
func refreshContractHole(ctx context.Context, sourceClientId, destinationClientId server.Id) (published bool, returnErr error) {
	ctx, cancel := context.WithTimeout(ctx, contractHoleSourceTimeout)
	defer cancel()
	keys := contractHoleKeys(sourceClientId, destinationClientId)
	token := server.NewId().String()
	if err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		return client.Eval(ctx, contractHoleBeginRefreshScript, keys, token, contractHoleSourceTimeout.Milliseconds()).Err()
	}); err != nil {
		return false, err
	}
	var contractIds []server.Id
	if recovered := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, contractHoleMembersSql, sourceClientId, destinationClientId, contractHoleMemberLimit+1)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var contractId server.Id
					server.Raise(rows.Scan(&contractId))
					contractIds = append(contractIds, contractId)
				}
			})
		}, server.OptNoRetry())
	}); recovered != nil {
		return false, fmt.Errorf("contract hole source read failed: %v", recovered)
	}
	if len(contractIds) > contractHoleMemberLimit {
		contractHoleCounter.WithLabelValues("refresh_oversized").Inc()
		return false, applyContractHoleEvent(ctx, server.Id{}, sourceClientId, destinationClientId, "oversized")
	}
	args := []any{token, ContractHoleTtl.Milliseconds()}
	for _, contractId := range contractIds {
		args = append(args, contractId.String())
	}
	err := server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
		result, err := client.Eval(ctx, contractHolePublishScript, keys, args...).Int()
		published = result == 1
		return err
	})
	if err == nil {
		if published {
			contractHoleCounter.WithLabelValues("refresh_published").Inc()
		} else {
			contractHoleCounter.WithLabelValues("refresh_superseded").Inc()
		}
	}
	return published, err
}

// The existing unresolved source-pair index supplies this keyset. Its included
// contract id is the final tie breaker when creation timestamps coincide.
type ContractHoleCursor struct {
	SourceClientId      server.Id `json:"source_client_id"`
	DestinationClientId server.Id `json:"destination_client_id"`
	CreateTime          time.Time `json:"create_time"`
	ContractId          server.Id `json:"contract_id"`
}

const contractHolePageSql = `
SELECT source_id, destination_id, create_time, contract_id
FROM transfer_contract
WHERE (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END)
  AND source_id IS NOT NULL
  AND (source_id, destination_id, create_time, contract_id) > ($1, $2, $3, $4)
ORDER BY source_id, destination_id, create_time, contract_id
LIMIT $5
`

// Advances a bounded indexed candidate page. Final-close candidates are retained
// here deliberately: their pair snapshot can revoke a lost lifecycle event.
// A complete fleet pass may exceed its nominal interval; expired keys then deny.
func RefreshContractHolesPage(ctx context.Context, cursor *ContractHoleCursor) (*ContractHoleCursor, int, error) {
	start := ContractHoleCursor{}
	if cursor != nil {
		start = *cursor
	}
	var positions []ContractHoleCursor
	if recovered := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, contractHolePageSql, start.SourceClientId, start.DestinationClientId,
				start.CreateTime, start.ContractId, ContractHoleRefreshPageSize)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					position := ContractHoleCursor{}
					server.Raise(rows.Scan(&position.SourceClientId, &position.DestinationClientId, &position.CreateTime, &position.ContractId))
					positions = append(positions, position)
				}
			})
		}, server.OptNoRetry())
	}); recovered != nil {
		return cursor, 0, fmt.Errorf("contract hole candidate read failed: %v", recovered)
	}
	seenPairs := map[string]bool{}
	progress := cursor
	for _, position := range positions {
		pair := contractHoleKeys(position.SourceClientId, position.DestinationClientId)[0]
		if seenPairs[pair] {
			progress = &position
			continue
		}
		seenPairs[pair] = true
		if _, err := refreshContractHole(ctx, position.SourceClientId, position.DestinationClientId); err != nil {
			contractHoleCounter.WithLabelValues("refresh_error").Inc()
			return progress, len(seenPairs), err
		}
		progress = &position
	}
	if len(positions) < ContractHoleRefreshPageSize {
		return nil, len(seenPairs), nil
	}
	return &positions[len(positions)-1], len(seenPairs), nil
}
