// PostgreSQL owns reservation amounts and revisions. Every Redis writer uses
// a fenced absolute snapshot, including recovery, delayed posts and retries.
package model

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server/v2026"
)

// Read these fields together: a revision obtained in a later statement cannot
// authorize a reservation amount from an earlier PostgreSQL snapshot.
type netEscrowSnapshot struct {
	revision int64
	reserved ByteCount
	endTime  *time.Time
}

// The fence shares its balance's Redis slot and survives a zero reservation or
// counter expiry. Deleting a counter must not make delayed old work admissible.
func netEscrowRevisionKey(balanceId server.Id) string {
	return fmt.Sprintf("{escrow_%s}net_revision", balanceId)
}

// Decimal comparison preserves all 63 revision bits; Lua numbers would round
// neighboring revisions above 2^53. Quantities also stay decimal strings until
// Go parses the response. An in-band observation does not refresh the ttl.
const netEscrowSnapshotScript = `
local fence = redis.call('GET', KEYS[2])
local revision = nil
local reserved = nil
if fence then
    revision, reserved = string.match(fence, '^(%d+):(%d+)$')
    if not revision then
        return redis.error_reply('invalid net escrow revision fence')
    end
end
local newer = revision and (#revision > #ARGV[1] or (#revision == #ARGV[1] and revision > ARGV[1]))
if newer then
    return {0, '0'}
end
if revision == ARGV[1] and reserved ~= ARGV[2] then
    return redis.error_reply('conflicting net escrow snapshot at the same revision')
end
local counter = redis.call('GET', KEYS[1])
local previous = counter or '0'
if ARGV[4] == '1' then
    if revision ~= ARGV[1] and (revision or ARGV[1] ~= '0' or previous ~= '0' or ARGV[2] ~= '0') then
        redis.call('SET', KEYS[2], ARGV[1] .. ':' .. ARGV[2])
    end
    if ARGV[2] == '0' then
        if counter then
            redis.call('DEL', KEYS[1])
        end
    elseif previous ~= ARGV[2] then
        redis.call('SET', KEYS[1], ARGV[2], 'PXAT', ARGV[3])
    else
        local expiry = redis.call('PEXPIRETIME', KEYS[1])
        if expiry < 0 or expiry > tonumber(ARGV[3]) then
            redis.call('PEXPIREAT', KEYS[1], ARGV[3])
        end
    end
end
return {1, previous}
`

// Apply and observation use the same atomic comparison. A newer cached source
// is skipped rather than misreported as drift against an older database page.
func applyNetEscrowSnapshot(
	ctx context.Context,
	scripter redis.Scripter,
	balanceId server.Id,
	snapshot netEscrowSnapshot,
	apply bool,
) *redis.Cmd {
	now := server.NowUtc()
	expiration := now.Add(netEscrowFallbackTtl)
	if snapshot.endTime != nil {
		if precise := netEscrowExpiration(now, *snapshot.endTime); now.Before(precise) {
			expiration = precise
		}
	}
	applyFlag := "0"
	if apply {
		applyFlag = "1"
	}
	return scripter.Eval(ctx, netEscrowSnapshotScript,
		[]string{netEscrowKey(balanceId), netEscrowRevisionKey(balanceId)},
		strconv.FormatInt(snapshot.revision, 10),
		strconv.FormatInt(int64(snapshot.reserved), 10),
		expiration.UnixMilli(), applyFlag)
}

// The complete pipeline is idempotent, including a partially applied timeout.
// Missing or malformed snapshots are never interpreted as a zero reservation.
func reconcileNetEscrowBatch(
	ctx context.Context,
	pending map[server.Id]netEscrowSnapshot,
	balanceIds []server.Id,
	apply bool,
) (drift map[server.Id]ByteCount) {
	drift = map[server.Id]ByteCount{}
	server.Redis(ctx, func(r server.RedisClient) {
		cmds := map[server.Id]*redis.Cmd{}
		_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, balanceId := range balanceIds {
				snapshot, ok := pending[balanceId]
				if !ok || snapshot.revision < 0 || snapshot.reserved < 0 {
					return fmt.Errorf("missing or invalid net escrow snapshot for balance %s", balanceId)
				}
				cmds[balanceId] = applyNetEscrowSnapshot(ctx, pipe, balanceId, snapshot, apply)
			}
			return nil
		})
		server.Raise(err)
		for balanceId, cmd := range cmds {
			values, err := cmd.Slice()
			server.Raise(err)
			if len(values) != 2 {
				server.Raise(fmt.Errorf("invalid net escrow snapshot response for balance %s", balanceId))
			}
			if values[0] == int64(0) {
				drift[balanceId] = 0
				continue
			}
			previous, err := strconv.ParseInt(fmt.Sprint(values[1]), 10, 64)
			server.Raise(err)
			if previous < 0 {
				// Legacy negative mirrors have never been available reservation.
				previous = 0
			}
			drift[balanceId] = ByteCount(previous) - pending[balanceId].reserved
		}
	})
	return
}

// Posts reload committed state instead of replaying their original delta. The
// source revision fences another publisher that overtakes this read/write pair.
func refreshNetEscrow(ctx context.Context, balanceIds []server.Id) {
	if len(balanceIds) == 0 {
		return
	}
	mirrorCtx, cancel := netEscrowMirrorCtx(ctx)
	defer cancel()
	// Billing retention can return many deleted balances. Keep every source
	// read and cluster pipeline within the same bound as reconciliation.
	const batchSize = 10000
	for start := 0; start < len(balanceIds); start += batchSize {
		batch := balanceIds[start:min(start+batchSize, len(balanceIds))]
		pending := openEscrowReservedForBalances(mirrorCtx, batch)
		reconcileNetEscrowBatch(mirrorCtx, pending, batch, true)
	}
}

// A quarantine commits its outcome before refreshing the affected balances.
// Retrying this post cannot release another contract's reservation.
func releaseNetEscrowForContract(ctx context.Context, contractId server.Id) {
	balanceIds := []server.Id{}
	mirrorCtx, cancel := netEscrowMirrorCtx(ctx)
	defer cancel()
	server.Db(mirrorCtx, func(conn server.PgConn) {
		result, err := conn.Query(mirrorCtx,
			`SELECT balance_id FROM transfer_escrow WHERE contract_id = $1 AND balance_byte_count <> 0`, contractId)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				server.Raise(result.Scan(&balanceId))
				balanceIds = append(balanceIds, balanceId)
			}
		})
	})
	if len(balanceIds) > 0 {
		pending := openEscrowReservedForBalances(mirrorCtx, balanceIds)
		reconcileNetEscrowBatch(mirrorCtx, pending, balanceIds, true)
	}
}
