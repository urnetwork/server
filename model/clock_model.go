package model

// The public transfer clock is a Redis projection of finalized contracts. The
// database remains the source used for the block-9 backfill; requests read one
// Redis key and never touch PostgreSQL.

import (
	"context"
	"strconv"
	"time"

	"github.com/urnetwork/server"
)

const ClockSinceBlock = 9

var ClockSinceTime = SubnetBlockGenesis.Add(
	time.Duration(ClockSinceBlock-1) * SubnetBlockDuration,
)

const clockTransferByteCountRedisKey = "clock.transfer_byte_count.block_9"

// Redis Lua numbers lose integer precision above 2^53. Compare normalized
// non-negative decimal strings by length and then lexically instead, while the
// actual counter continues to use Redis INCRBY's signed 64-bit integer.
const clockReconcileScript = `
local current = redis.call('GET', KEYS[1])
local candidate = ARGV[1]

local function normalize(value)
    value = string.gsub(value, '^0+', '')
    if value == '' then
        return '0'
    end
    return value
end

candidate = normalize(candidate)
if (not current) or (not string.match(current, '^%d+$')) then
    redis.call('SET', KEYS[1], candidate)
    return candidate
end

current = normalize(current)
if (#current < #candidate) or (#current == #candidate and current < candidate) then
    redis.call('SET', KEYS[1], candidate)
    return candidate
end
return current
`

type ClockResult struct {
	// A decimal string keeps the API exact for JavaScript clients after the
	// counter exceeds Number.MAX_SAFE_INTEGER.
	TotalTransferByteCount string `json:"total_transfer_byte_count"`
	SinceBlock             int    `json:"since_block"`
	SinceTime              string `json:"since_time"`
}

func clockResult(byteCount ByteCount) *ClockResult {
	return &ClockResult{
		TotalTransferByteCount: strconv.FormatInt(byteCount, 10),
		SinceBlock:             ClockSinceBlock,
		SinceTime:              ClockSinceTime.Format(time.RFC3339),
	}
}

// GetClock reads the public clock without touching PostgreSQL. ok is false
// before the initial backfill has created the counter, or if the stored value
// is malformed.
func GetClock(ctx context.Context) (result *ClockResult, ok bool) {
	var value string
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		value, err = r.Get(ctx, clockTransferByteCountRedisKey).Result()
		if err != nil {
			return
		}
		ok = true
	}, server.OptNoRetry())
	if !ok {
		return nil, false
	}
	byteCount, err := strconv.ParseInt(value, 10, 64)
	if err != nil || byteCount < 0 {
		return nil, false
	}
	return clockResult(byteCount), true
}

// AddClockTransferByteCount advances the clock once a contract has atomically
// claimed its final outcome. INCRBY is atomic across API replicas. It is
// deliberately not retried: a connection loss after Redis applied INCRBY is
// ambiguous, and blindly retrying could double count. A scalar counter cannot
// prove whether that contract was applied after an ambiguous failure.
func AddClockTransferByteCount(ctx context.Context, byteCount ByteCount) {
	if byteCount <= 0 {
		return
	}
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.IncrBy(ctx, clockTransferByteCountRedisKey, byteCount).Result()
		server.Raise(err)
	}, server.OptNoRetry())
}

// clockTransferPost detaches the Redis projection from request cancellation.
// Settlement is already committed when posts run, so losing the caller must
// not discard the clock update. Failures are contained by server.RunPosts and
// repaired by the backfill task.
func clockTransferPost(ctx context.Context, byteCount ByteCount) server.PostFunction {
	return func() any {
		clockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		AddClockTransferByteCount(clockCtx, byteCount)
		return nil
	}
}

// clockBackfillCandidate recomputes the retained block-9 aggregate. It does
// not try to decide whether individual contracts were already counted; a
// scalar Redis value contains no such membership information.
func clockBackfillCandidate(ctx context.Context) ByteCount {
	var candidate ByteCount
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT COALESCE(SUM(contract_close.used_transfer_byte_count), 0)
                FROM transfer_contract
                INNER JOIN contract_close ON
                    contract_close.contract_id = transfer_contract.contract_id AND
                    contract_close.party = $1
                WHERE
                    transfer_contract.outcome IS NOT NULL AND
                    $2 <= transfer_contract.close_time
            `,
			ContractPartyDestination,
			ClockSinceTime,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&candidate))
			}
		})
	})
	return candidate
}

func reconcileClockCandidate(ctx context.Context, candidate ByteCount) ByteCount {
	var reconciled string
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Eval(
			ctx,
			clockReconcileScript,
			[]string{clockTransferByteCountRedisKey},
			strconv.FormatInt(candidate, 10),
		).Text()
		server.Raise(err)
		reconciled = value
	}, server.OptNoRetry())

	byteCount, err := strconv.ParseInt(reconciled, 10, 64)
	server.Raise(err)
	return ByteCount(byteCount)
}

// BackfillClock performs a best-effort block-9 aggregate reconciliation. The
// destination close is the canonical transfer count, matching
// RollupTransferAuditEvents. The Lua max can fill an aggregate gap without
// overwriting a newer live value.
//
// Recomputing after the first reconcile narrows the initialization window by
// including contracts committed after the first SQL snapshot. PostgreSQL and
// Redis are not one atomic system, however, so the second pass is still only a
// best-effort aggregate reconciliation.
//
// This compares aggregates; it does not establish per-contract membership or
// repair an ambiguous increment exactly. Completed contract retention
// eventually removes source rows, so Redis persistence remains the lifetime
// source after retained history is gone.
func BackfillClock(ctx context.Context) ByteCount {
	reconcileClockCandidate(ctx, clockBackfillCandidate(ctx))
	return reconcileClockCandidate(ctx, clockBackfillCandidate(ctx))
}

// clockContractTransferByteCount reads the same canonical destination close
// used by BackfillClock. It is reserved for rare closure paths that do not
// already have the close rows in memory.
func clockContractTransferByteCount(ctx context.Context, contractId server.Id) ByteCount {
	var byteCount ByteCount
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT contract_close.used_transfer_byte_count
                FROM transfer_contract
                INNER JOIN contract_close ON
                    contract_close.contract_id = transfer_contract.contract_id AND
                    contract_close.party = $2
                WHERE
                    transfer_contract.contract_id = $1 AND
                    transfer_contract.outcome IS NOT NULL AND
                    $3 <= transfer_contract.close_time
            `,
			contractId,
			ContractPartyDestination,
			ClockSinceTime,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&byteCount))
			}
		})
	})
	return byteCount
}
