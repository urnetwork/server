// PostgreSQL owns reservation amounts and revisions. Every Redis writer uses
// a fenced absolute snapshot, including recovery, delayed posts and retries.
package model

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// Read these fields together: a revision obtained in a later statement cannot
// authorize a reservation amount from an earlier PostgreSQL snapshot.
type netEscrowSnapshot struct {
	revision int64
	reserved ByteCount
	endTime  *time.Time
}

var netEscrowCreationSnapshots = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_net_escrow_creation_snapshot_total",
	Help: "Creation mirror balances published from a verified admission or durable cached snapshot, or reloaded from escrow history.",
}, []string{"result"})

var netEscrowRefreshSnapshots = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_net_escrow_refresh_snapshot_total",
	Help: "Settlement, quarantine, retention and scheduled reconciliation snapshots reused at a durable revision, reloaded from escrow history, or deferred by scheduled reconciliation over its census bound; not completed settlements or census query counts.",
}, []string{"result"})

func init() {
	prometheus.MustRegister(netEscrowCreationSnapshots)
	prometheus.MustRegister(netEscrowRefreshSnapshots)
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

// A creation already censused reservations before its two revision-advancing
// inserts. Reuse that exact snapshot only while a single committed statement
// confirms both the contract and the expected revisions. Contract existence is
// essential: a rolled-back transaction's predicted revision can be reused by
// another writer. An overtaking admission can instead supply a current durable
// cached snapshot: read its amount and matching revision in this same statement.
// Missing, stale or deleted-balance cache entries retain the exact census fallback.
const netEscrowCreatedSnapshotSQL = `
    SELECT requested_balance.balance_id, COALESCE(revision.revision, 0), balance.end_time,
        EXISTS (SELECT 1 FROM transfer_contract WHERE contract_id = $2 AND outcome IS NULL),
        cached.reserved_byte_count
    FROM unnest($1::uuid[]) AS requested_balance(balance_id)
    LEFT JOIN transfer_balance_net_escrow_revision AS revision USING (balance_id)
    LEFT JOIN transfer_balance AS balance USING (balance_id)
    LEFT JOIN LATERAL (
        SELECT revision, reserved_byte_count FROM transfer_balance_net_escrow_snapshot
        WHERE balance_id = requested_balance.balance_id OFFSET 0
    ) AS cached ON cached.revision = COALESCE(revision.revision, 0)
`

func publishCreatedNetEscrow(
	ctx context.Context, contractId server.Id,
	expected map[server.Id]netEscrowSnapshot, balanceIds []server.Id,
) {
	mirrorCtx, cancel := netEscrowMirrorCtx(ctx)
	defer cancel()
	pending := map[server.Id]netEscrowSnapshot{}
	server.Db(mirrorCtx, func(conn server.PgConn) {
		rows, err := conn.Query(mirrorCtx, netEscrowCreatedSnapshotSQL, balanceIds, contractId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var balanceId server.Id
				var revision int64
				var endTime *time.Time
				var created bool
				var cachedReserved *int64
				server.Raise(rows.Scan(&balanceId, &revision, &endTime, &created, &cachedReserved))
				if snapshot, ok := expected[balanceId]; ok && created && endTime != nil && snapshot.revision == revision {
					snapshot.endTime = endTime
					pending[balanceId] = snapshot
				} else if endTime != nil && cachedReserved != nil {
					if revision < 0 || *cachedReserved < 0 {
						server.Raise(fmt.Errorf("invalid committed net escrow cache snapshot"))
					}
					// This amount is committed PostgreSQL state, independent of the
					// original contract's existence or a rolled-back predicted delta.
					pending[balanceId] = netEscrowSnapshot{revision: revision, reserved: ByteCount(*cachedReserved), endTime: endTime}
				}
			}
		})
	})
	changed := make([]server.Id, 0, len(balanceIds)-len(pending))
	for _, balanceId := range balanceIds {
		if _, ok := pending[balanceId]; !ok {
			changed = append(changed, balanceId)
		}
	}
	exact := openEscrowReservedForBalances(mirrorCtx, changed)
	// Reuse only this committed census at its observed revision. Delayed
	// creation posts must not each rescan the same cold balance history.
	// The census connection has returned before the optional ownership probe.
	cacheCommittedNetEscrowSnapshots(mirrorCtx, exact)
	for balanceId, snapshot := range exact {
		pending[balanceId] = snapshot
	}
	reconcileNetEscrowBatch(mirrorCtx, pending, balanceIds, true)
	netEscrowCreationSnapshots.WithLabelValues("reused").Add(float64(len(balanceIds) - len(changed)))
	netEscrowCreationSnapshots.WithLabelValues("reloaded").Add(float64(len(changed)))
}

// The legacy mirror task reads committed state instead of replaying its
// original delta; its revision handshake needs an exact snapshot, so it keeps
// the unbounded census. An overtaking admission may already have cached the
// current exact amount. Keep exact fallback for legacy writes and invalidated
// or deleted balances; the source revision fences publishers that overtake
// either read/write pair. Refresh and quarantine posts read through
// readBoundedNetEscrowSnapshots instead.
func readMirrorNetEscrowSnapshots(ctx context.Context, balanceIds []server.Id) map[server.Id]netEscrowSnapshot {
	return readCachedOrExactNetEscrowSnapshots(ctx, balanceIds)
}

// Exact misses warm the cache. Scheduled fleet repair never warms it: a sweep
// of mostly-empty balances must not create a fleet-wide snapshot write workload.
func readCachedOrExactNetEscrowSnapshots(ctx context.Context, balanceIds []server.Id) map[server.Id]netEscrowSnapshot {
	pending := map[server.Id]netEscrowSnapshot{}
	if len(balanceIds) == 0 {
		return pending
	}
	server.Db(ctx, func(conn server.PgConn) {
		pending = readCachedNetEscrowSnapshots(ctx, conn, balanceIds)
	})
	missing := missingNetEscrowSnapshots(pending, balanceIds)
	exact := openEscrowReservedForBalances(ctx, missing)
	cacheCommittedNetEscrowSnapshots(ctx, exact)
	for balanceId, snapshot := range exact {
		pending[balanceId] = snapshot
	}
	netEscrowRefreshSnapshots.WithLabelValues("reused").Add(float64(len(balanceIds) - len(missing)))
	netEscrowRefreshSnapshots.WithLabelValues("reloaded").Add(float64(len(missing)))
	return pending
}

// Scheduled reconciliation shares committed mirror authority without writing
// the cache, and returns the balances it deferred over its census bound. The
// operator's exact audit and repair do not use the cache and defer nothing.
func readReconcileNetEscrowSnapshots(ctx context.Context, balanceIds []server.Id, useCache bool) (map[server.Id]netEscrowSnapshot, []server.Id) {
	if !useCache {
		return openEscrowReservedForBalances(ctx, balanceIds), nil
	}
	return readBoundedNetEscrowSnapshots(ctx, balanceIds, false, netEscrowReconcileCensusBound)
}

// A census statement probes one transfer_contract row for every live legacy
// escrow row of each balance it names, so its cost follows that history, not
// the number of balances. The unresolved-contract backlog left single current
// balances with about 10^5 such rows, and a 10,000-balance page then could not
// finish inside its two-minute fence; each pass restarted at that page. Scheduled
// repair censuses a cache miss only when a bounded probe finds at most this many
// live legacy rows. Over-bound balances keep their mirror: a later pass, a
// targeted post or the operator's exact audit repairs them.
const netEscrowReconcileCandidateLimit = 64

// Live legacy rows, and so contract probes, that one scheduled census may visit.
const netEscrowReconcileCensusRowBudget = 16384

// Balances named by one probe or census statement, on either path.
const netEscrowReconcileStatementBalances = 1000

// Refresh and quarantine posts maintain the mirror that Redis admission
// subtracts from grant credit, so a deferred mirror stays overstated after
// releases and refuses credit. Only the extreme tail defers here: on Main the
// grants over 64 live legacy rows are nearly all over 4,096 (most over 16,384).
const netEscrowRefreshCensusRowBound = 4096

// Live legacy rows, and so contract probes, that one refresh census may visit.
const netEscrowRefreshCensusStatementRows = 65536

// Bounds one census reader: a balance whose probe finds more live legacy rows
// than candidateLimit is deferred, and one census statement visits at most
// statementRows of them.
type netEscrowCensusBound struct {
	candidateLimit int
	statementRows  int
}

// Scheduled repair only repairs drift, so it defers early.
var netEscrowReconcileCensusBound = netEscrowCensusBound{candidateLimit: netEscrowReconcileCandidateLimit, statementRows: netEscrowReconcileCensusRowBudget}

// Mirror posts defer only the extreme tail; see netEscrowRefreshCensusRowBound.
var netEscrowRefreshCensusBound = netEscrowCensusBound{candidateLimit: netEscrowRefreshCensusRowBound, statementRows: netEscrowRefreshCensusStatementRows}

// Reads at most $2 live legacy rows per balance from the partial legacy index,
// with no contract probe. Every requested balance returns exactly one count.
const netEscrowReconcileCandidateSQL = `
    SELECT requested_balance.balance_id, candidate.row_count
    FROM unnest($1::uuid[]) AS requested_balance(balance_id)
    CROSS JOIN LATERAL (
        SELECT count(*) AS row_count
        FROM (
            SELECT 1
            FROM transfer_escrow
            WHERE transfer_escrow.balance_id = requested_balance.balance_id AND
                transfer_escrow.settled = false AND
                transfer_escrow.balance_byte_count <> 0 AND NOT transfer_escrow.redis_reserved
            LIMIT $2
        ) AS bounded_escrow
    ) AS candidate
`

// Counts are capped one past the limit, in probe statements of at most
// netEscrowReconcileStatementBalances inside the caller's fenced transaction.
func readNetEscrowCandidateCountsInTx(ctx context.Context, tx server.PgTx, balanceIds []server.Id, limit int) map[server.Id]int {
	counts := make(map[server.Id]int, len(balanceIds))
	for start := 0; start < len(balanceIds); start += netEscrowReconcileStatementBalances {
		batch := balanceIds[start:min(start+netEscrowReconcileStatementBalances, len(balanceIds))]
		rows, err := tx.Query(ctx, netEscrowReconcileCandidateSQL, batch, limit+1)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var balanceId server.Id
				var count int
				server.Raise(rows.Scan(&balanceId, &count))
				counts[balanceId] = count
			}
		})
	}
	return counts
}

// One fenced read-only transaction, the shape of openEscrowReservedForBalances,
// probes every balance and then censuses only the admitted ones in bounded
// statements. A retried attempt starts over.
func readBoundedNetEscrowCensus(ctx context.Context, balanceIds []server.Id, bound netEscrowCensusBound) (exact map[server.Id]netEscrowSnapshot, statementBalances int, deferred []server.Id) {
	exact = map[server.Id]netEscrowSnapshot{}
	if len(balanceIds) == 0 {
		return
	}
	defer enterLegacySettlementTiming(ctx, legacySettlementColdCensus)()
	server.Tx(ctx, func(tx server.PgTx) {
		exact, statementBalances = map[server.Id]netEscrowSnapshot{}, 0
		configureNetEscrowReservationPageTimeout(ctx, tx, netEscrowReservationPageStatementTimeout)
		var statements [][]server.Id
		statements, deferred = planNetEscrowCensus(balanceIds, readNetEscrowCandidateCountsInTx(ctx, tx, balanceIds, bound.candidateLimit), bound)
		for _, statement := range statements {
			for balanceId, snapshot := range readNetEscrowSnapshots(ctx, tx, statement) {
				exact[balanceId] = snapshot
			}
			statementBalances += len(statement)
		}
	}, server.TxReadCommitted, pgx.ReadOnly)
	return
}

// Splits probed balances, in order, into census statements within both the
// balance and row budgets. An over-limit or unprobed balance is deferred.
func planNetEscrowCensus(balanceIds []server.Id, counts map[server.Id]int, bound netEscrowCensusBound) (statements [][]server.Id, deferred []server.Id) {
	var statement []server.Id
	rowCount := 0
	for _, balanceId := range balanceIds {
		count, probed := counts[balanceId]
		if !probed || count < 0 || bound.candidateLimit < count {
			deferred = append(deferred, balanceId)
			continue
		}
		if len(statement) == netEscrowReconcileStatementBalances || bound.statementRows < rowCount+count {
			statements = append(statements, statement)
			statement, rowCount = nil, 0
		}
		statement = append(statement, balanceId)
		rowCount += count
	}
	if len(statement) != 0 {
		statements = append(statements, statement)
	}
	return
}

// Current cache entries are reused. Misses are censused only within the probe
// bound, and deferred ones have no snapshot. Targeted mirror posts warm the
// cache from each committed census; scheduled fleet repair never warms it.
func readBoundedNetEscrowSnapshots(ctx context.Context, balanceIds []server.Id, warmCache bool, bound netEscrowCensusBound) (map[server.Id]netEscrowSnapshot, []server.Id) {
	pending := map[server.Id]netEscrowSnapshot{}
	if len(balanceIds) == 0 {
		return pending, nil
	}
	server.Db(ctx, func(conn server.PgConn) {
		pending = readCachedNetEscrowSnapshots(ctx, conn, balanceIds)
	})
	missing := missingNetEscrowSnapshots(pending, balanceIds)
	exact, reloaded, deferred := readBoundedNetEscrowCensus(ctx, missing, bound)
	if warmCache {
		cacheCommittedNetEscrowSnapshots(ctx, exact)
	}
	for balanceId, snapshot := range exact {
		pending[balanceId] = snapshot
	}
	netEscrowRefreshSnapshots.WithLabelValues("reused").Add(float64(len(balanceIds) - len(missing)))
	netEscrowRefreshSnapshots.WithLabelValues("reloaded").Add(float64(reloaded))
	netEscrowRefreshSnapshots.WithLabelValues("deferred").Add(float64(len(deferred)))
	return pending, deferred
}

// Page balances without the deferred ones, in page order.
func withoutNetEscrowBalances(balanceIds []server.Id, deferred []server.Id) []server.Id {
	if len(deferred) == 0 {
		return balanceIds
	}
	skipped := make(map[server.Id]bool, len(deferred))
	for _, balanceId := range deferred {
		skipped[balanceId] = true
	}
	kept := make([]server.Id, 0, len(balanceIds)-len(deferred))
	for _, balanceId := range balanceIds {
		if !skipped[balanceId] {
			kept = append(kept, balanceId)
		}
	}
	return kept
}

// Committed closes, settlements, prober shard and balance retention refresh
// their balances here. Like scheduled repair, the census is bounded: a balance
// with more live legacy rows than netEscrowRefreshCensusRowBound keeps its
// last published mirror (counted as deferred). An unbounded census of grants
// with 10^5 such rows, run per close, saturated the database.
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
		pending, deferred := readBoundedNetEscrowSnapshots(mirrorCtx, batch, true, netEscrowRefreshCensusBound)
		reconcileNetEscrowBatch(mirrorCtx, pending, withoutNetEscrowBalances(batch, deferred), true)
	}
}

// A quarantine commits its outcome before refreshing the affected balances.
// Retrying this post cannot release another contract's reservation.
func releaseNetEscrowForContract(ctx context.Context, contractId server.Id) {
	var legacyIds, redisIds []server.Id
	mirrorCtx, cancel := netEscrowMirrorCtx(ctx)
	defer cancel()
	server.Db(mirrorCtx, func(conn server.PgConn) {
		rows, err := conn.Query(mirrorCtx, `SELECT escrow.balance_id,escrow.redis_reserved,contract.outcome IS NOT NULL
            FROM transfer_contract AS contract
            CROSS JOIN LATERAL (SELECT balance_id,redis_reserved,balance_byte_count FROM transfer_escrow
                WHERE contract_id=contract.contract_id OFFSET 0) AS escrow
            WHERE contract.contract_id=$1 AND escrow.balance_byte_count<>0`, contractId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				var redisReserved, terminal bool
				server.Raise(rows.Scan(&id, &redisReserved, &terminal))
				if redisReserved {
					if terminal {
						redisIds = append(redisIds, id)
					}
				} else {
					legacyIds = append(legacyIds, id)
				}
			}
		})
	})
	if len(redisIds) > 0 {
		ReconcileRedisContractReservation(mirrorCtx, contractId)
	}
	if len(legacyIds) > 0 {
		// The same bounded census as refreshNetEscrow; deferred balances keep
		// their last published mirror.
		pending, deferred := readBoundedNetEscrowSnapshots(mirrorCtx, legacyIds, true, netEscrowRefreshCensusBound)
		reconcileNetEscrowBatch(mirrorCtx, pending, withoutNetEscrowBalances(legacyIds, deferred), true)
	}
}
