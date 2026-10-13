package model

// The public transfer clock is a Redis projection of finalized contracts. The
// database remains the source used for the block-9 backfill; requests read one
// Redis key and never touch PostgreSQL.

import (
	"context"
	"fmt"
	"math"
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

type clockDailyRollup struct {
	day       time.Time
	byteCount ByteCount
	rowCount  int
}

// clockContiguousRollupPrefix accepts only an unbroken prefix of completed
// UTC-day rollups. If even one day is absent, the raw tail starts at that day;
// later rollups are deliberately ignored so a missing day can never become a
// silent hole in the lifetime clock.
func clockContiguousRollupPrefix(
	since time.Time,
	today time.Time,
	rollups []clockDailyRollup,
) (byteCount ByteCount, tailStart time.Time) {
	since = since.UTC()
	today = startOfUtcDay(today)
	if !since.Equal(startOfUtcDay(since)) || !since.Before(today) {
		return 0, since
	}

	expectedDay := since
	for _, rollup := range rollups {
		day := startOfUtcDay(rollup.day)
		if day.Before(expectedDay) {
			continue
		}
		if !day.Equal(expectedDay) || !day.Before(today) || rollup.rowCount != 1 || rollup.byteCount < 0 {
			break
		}
		if ByteCount(math.MaxInt64)-byteCount < rollup.byteCount {
			server.Raise(fmt.Errorf("invalid clock rollup total"))
		}
		byteCount += rollup.byteCount
		expectedDay = expectedDay.Add(24 * time.Hour)
	}
	return byteCount, expectedDay
}

func clockBackfillRollupPrefix(ctx context.Context, since time.Time, now time.Time) (ByteCount, time.Time) {
	today := startOfUtcDay(now)
	if !since.UTC().Equal(startOfUtcDay(since)) || !since.Before(today) {
		return 0, since
	}

	rollups := []clockDailyRollup{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					date_trunc('day', event_time) AS day,
					SUM(transfer_byte_count)::bigint AS byte_count,
					COUNT(*)::int AS row_count
				FROM audit_contract_event
				WHERE
					event_details = $1 AND
					$2 <= event_time AND
					event_time < $3
				GROUP BY 1
				ORDER BY 1
			`,
			AuditEventDetailsTransferRollup,
			since,
			today,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				rollup := clockDailyRollup{}
				server.Raise(result.Scan(&rollup.day, &rollup.byteCount, &rollup.rowCount))
				rollups = append(rollups, rollup)
			}
		})
	})
	return clockContiguousRollupPrefix(since, today, rollups)
}

// Marks the raw tail's chunk statements in pg_stat_activity: the marker the
// operator runbook attributes to the bounded clock tail.
const clockUnrolledTailMarker = "clock_unrolled_tail"

// The resumable state of one block-9 aggregate reconciliation: the contiguous
// daily-rollup prefix plus a bounded walk of the still-unrolled raw tail (see
// TransferCloseByteSum). It does not try to decide whether individual
// contracts were already counted; a scalar Redis value contains no such
// membership information. The json form is a task checkpoint. Not safe for
// concurrent use.
type ClockBackfill struct {
	// the rollup days before the tail start, fixed when the backfill begins
	PrefixByteCount ByteCount `json:"prefix_byte_count"`
	// finalized contracts from the first unrolled UTC day onward
	Tail TransferCloseByteSum `json:"tail"`
}

// Fixes the rollup prefix and starts the raw tail at the first unrolled UTC
// day. The tail ends when the backfill begins; contracts that settle later
// already advance the live counter.
func NewClockBackfill(ctx context.Context) *ClockBackfill {
	now := server.NowUtc().Truncate(time.Microsecond)
	prefixByteCount, tailStart := clockBackfillRollupPrefix(ctx, ClockSinceTime, now)
	return &ClockBackfill{
		PrefixByteCount: prefixByteCount,
		Tail: TransferCloseByteSum{
			Start:        tailStart,
			End:          now,
			TerminalOnly: true,
		},
	}
}

// Moves the tail end forward, so a resumed walk also covers the contracts
// finalized since the backfill began. The walk is in key order: the contracts
// it already read stay behind the cursor and none is read twice.
func (self *ClockBackfill) ExtendTo(end time.Time) {
	end = end.UTC().Truncate(time.Microsecond)
	if self.Tail.End.Before(end) {
		self.Tail.End = end
		self.Tail.Complete = false
	}
}

// Continues the raw tail walk by up to chunkCount chunks and reports whether
// it is complete.
func (self *ClockBackfill) Advance(ctx context.Context, chunkCount int) bool {
	return self.Tail.advance(ctx, clockUnrolledTailMarker, chunkCount)
}

// Raises the Redis clock to the completed candidate when the clock is behind
// it, and returns the reconciled clock.
func (self *ClockBackfill) Reconcile(ctx context.Context) ByteCount {
	return reconcileClockCandidate(ctx, self.candidate())
}

// The block-9 aggregate of a completed walk.
func (self *ClockBackfill) candidate() ByteCount {
	if !self.Tail.Complete {
		server.Raise(fmt.Errorf("incomplete clock backfill tail"))
	}
	if self.PrefixByteCount < 0 || self.Tail.ByteCount < 0 ||
		ByteCount(math.MaxInt64)-self.PrefixByteCount < self.Tail.ByteCount {
		server.Raise(fmt.Errorf("invalid clock backfill total"))
	}
	return self.PrefixByteCount + self.Tail.ByteCount
}

// Recomputes the block-9 aggregate from the durable daily-rollup prefix plus
// the still-unrolled raw tail, walked to completion in bounded chunks.
func clockBackfillCandidate(ctx context.Context) ByteCount {
	backfill := NewClockBackfill(ctx)
	backfill.Advance(ctx, math.MaxInt)
	return backfill.candidate()
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

// Performs a best-effort block-9 aggregate reconciliation in one call, walking
// the raw tail to completion. The destination close is the canonical transfer
// count, matching RollupTransferAuditEvents. The Lua max can fill an aggregate
// gap without overwriting a newer live value. The task checkpoints the same
// walk across bounded runs with ClockBackfill instead.
//
// Live settlement increments that commit during the walk already advance
// Redis; the monotonic reconcile preserves that newer value. Repeating the
// same aggregate cannot make PostgreSQL and Redis atomic and formerly doubled
// a multi-billion-row production scan, so one bounded candidate is the
// intentional work unit.
//
// This compares aggregates; it does not establish per-contract membership or
// repair an ambiguous increment exactly. Completed contract retention
// eventually removes source rows, so Redis persistence remains the lifetime
// source after retained history is gone.
func BackfillClock(ctx context.Context) ByteCount {
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
