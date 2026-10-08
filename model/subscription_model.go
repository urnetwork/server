package model

import (
	"context"
	"fmt"
	"math"
	"time"

	// "crypto/rand"
	// "encoding/hex"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"

	// "maps"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type ByteCount = int64

const Kib = ByteCount(1024)
const Mib = ByteCount(1024 * 1024)
const Gib = ByteCount(1024 * 1024 * 1024)
const Tib = ByteCount(1024 * 1024 * 1024 * 1024)

type Priority = uint32

const UnpaidPriority = 0
const PaidPriority = 100
const TrustedPriority = 200

// Closed contracts that remain safely unplanned after this long expire. Active
// and ambiguous processor payments are protected independently of age.
const StragglerContractExpiration = 300 * 24 * time.Hour

// completed contracts are reaped this long after their payment completes
// (reap_time = complete_time + CompletedContractExpiration, assigned by the
// bounded retention worker after CompletePayment durably queues the payment)
const CompletedContractExpiration = 7 * 24 * time.Hour

// Per-phase wall-clock budget for the contract reaper's completed assignment,
// straggler assignment, and delete passes. A large one-time backlog drains over
// many 30-min runs instead of one unbounded transaction; steady state finishes
// well under it. A var (not const) so tests can inject a tiny budget to exercise
// the mid-backlog stop.
var reaperRunBudget = 5 * time.Minute

func ByteCountHumanReadable(count ByteCount) string {
	trimFloatString := func(value float64, precision int, suffix string) string {
		s := fmt.Sprintf("%."+strconv.Itoa(precision)+"f", value)
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
		return s + suffix
	}

	if 1024*1024*1024*1024 <= count {
		return trimFloatString(
			float64(1000*count/(1024*1024*1024*1024))/1000.0,
			2,
			"tib",
		)
	} else if 1024*1024*1024 <= count {
		return trimFloatString(
			float64(1000*count/(1024*1024*1024))/1000.0,
			2,
			"gib",
		)
	} else if 1024*1024 <= count {
		return trimFloatString(
			float64(1000*count/(1024*1024))/1000.0,
			2,
			"mib",
		)
	} else if 1024 <= count {
		return trimFloatString(
			float64(1000*count/(1024))/1000.0,
			2,
			"kib",
		)
	} else {
		return fmt.Sprintf("%db", count)
	}
}

func ParseByteCount(humanReadable string) (ByteCount, error) {
	humanReadableLower := strings.ToLower(humanReadable)
	tibLower := "tib"
	gibLower := "gib"
	mibLower := "mib"
	kibLower := "kib"
	bLower := "b"
	if strings.HasSuffix(humanReadableLower, tibLower) {
		countFloat, err := strconv.ParseFloat(
			humanReadableLower[0:len(humanReadableLower)-len(tibLower)],
			64,
		)
		if err != nil {
			return ByteCount(0), err
		}
		return ByteCount(countFloat * 1024 * 1024 * 1024 * 1024), nil
	} else if strings.HasSuffix(humanReadableLower, gibLower) {
		countFloat, err := strconv.ParseFloat(
			humanReadableLower[0:len(humanReadableLower)-len(gibLower)],
			64,
		)
		if err != nil {
			return ByteCount(0), err
		}
		return ByteCount(countFloat * 1024 * 1024 * 1024), nil
	} else if strings.HasSuffix(humanReadableLower, mibLower) {
		countFloat, err := strconv.ParseFloat(
			humanReadableLower[0:len(humanReadableLower)-len(mibLower)],
			64,
		)
		if err != nil {
			return ByteCount(0), err
		}
		return ByteCount(countFloat * 1024 * 1024), nil
	} else if strings.HasSuffix(humanReadableLower, kibLower) {
		countFloat, err := strconv.ParseFloat(
			humanReadableLower[0:len(humanReadableLower)-len(kibLower)],
			64,
		)
		if err != nil {
			return ByteCount(0), err
		}
		return ByteCount(countFloat * 1024), nil
	} else if strings.HasSuffix(humanReadableLower, bLower) {
		countFloat, err := strconv.ParseFloat(
			humanReadableLower[0:len(humanReadableLower)-len(bLower)],
			64,
		)
		if err != nil {
			return ByteCount(0), err
		}
		return ByteCount(countFloat), nil
	} else {
		countInt, err := strconv.ParseInt(humanReadableLower, 10, 63)
		if err != nil {
			return ByteCount(0), err
		}
		return ByteCount(countInt), nil
	}
}

type NanoCents = int64

func UsdToNanoCents(usd float64) NanoCents {
	return NanoCents(math.Round(usd * float64(1000000000)))
}

func NanoCentsToUsd(nanoCents NanoCents) float64 {
	return float64(nanoCents) / float64(1000000000)
}

type NanoPoints = int64

// 1 point = 1_000_000 nano points

func PointsToNanoPoints(points float64) NanoPoints {
	return NanoPoints(math.Round(float64(points) * 1_000_000))
}

func NanoPointsToPoints(nanoPoints NanoPoints) int {
	return int(math.Round(float64(nanoPoints) / 1_000_000))
}

// 12 months
// const BalanceCodeDuration = 365 * 24 * time.Hour

// up to 16MiB
const AcceptableTransfersByteDifference = 16 * 1024 * 1024

const ProviderRevenueShare float64 = 0.5

const MaxSubscriptionPaymentIdsPerHour = 5

type TransferPair struct {
	A server.Id
	B server.Id
}

func NewTransferPair(sourceId server.Id, destinationId server.Id) TransferPair {
	return TransferPair{
		A: sourceId,
		B: destinationId,
	}
}

func NewUnorderedTransferPair(a server.Id, b server.Id) TransferPair {
	// store in ascending order
	if a.Less(b) {
		return TransferPair{
			A: a,
			B: b,
		}
	} else {
		return TransferPair{
			A: b,
			B: a,
		}
	}
}

// the escrow model has been updated so that:
//   - `transfer_balance` tracks unspent credit; settlement debits it atomically
//     with the contract outcome
//   - the net balance in a `transfer_escrow` is tracked approximately in redis `netEscrowKey`
//     Because redis is not atomic with Postgres, the value in the `netEscrowKey` will
//     eventually be consistent with the real value, but may be off by some amount at any given time.
//
// The hash tag is per balance so the counters spread across cluster slots.
// A previous format, `{escrow}net_<balanceId>`, put every counter under one
// shared tag (a single slot/node hot spot); keys in that old format are
// abandoned-but-finite and can be removed with a one-time scan-delete.
//
// Every write site also gives the counter a ttl (see `netEscrowEndTimeSlack`
// and `netEscrowFallbackTtl`), so a counter that outlives its balance -- for
// example when the `RemoveCompletedContracts` delete is missed -- expires on
// its own instead of accumulating without bound. A missing counter reads as
// zero for approximate balance display. Admission locks database balances and
// reads durable reservations, so cache loss cannot authorize more credit. The
// reconcile task re-derives the displayed value from postgres
// (see `ReconcileNetEscrow`).
// netEscrowMirrorTimeout bounds a detached mirror update so a wedged redis
// cannot retain the goroutine.
const netEscrowMirrorTimeout = 60 * time.Second

// netEscrowMirrorCtx detaches a net escrow mirror update from the caller's
// request context.
//
// By the time a mirror update runs, its reservation (or settlement) is already
// committed in postgres, so the update is not optional work the caller may
// cancel — it is the second half of a write that has already happened. Binding
// it to the caller meant a client that disconnected in that window silently
// lost it: the redis call fails with a non-retryable context error, the post
// goroutine's HandleError swallows the panic, and the counter is permanently
// wrong. A lost increment drives the counter negative and over-reports
// available balance; a lost decrement inflates it and hides balance until a
// reconcile (the "insufficient balance" lockup). Values are preserved; only
// cancellation is dropped.
func netEscrowMirrorCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), netEscrowMirrorTimeout)
}

func netEscrowKey(balanceId server.Id) string {
	return fmt.Sprintf("{escrow_%s}net", balanceId)
}

// netEscrowEndTimeSlack extends the precise counter deadline past the balance
// `end_time`. The counter is only meaningful while the balance is active; the
// slack covers contracts that straddle the end of the balance window.
const netEscrowEndTimeSlack = 30 * 24 * time.Hour

// netEscrowFallbackTtl bounds every counter, including balances whose durable
// end_time is intentionally many years away. A missing counter reads as zero;
// the recurring reconcile compares it with PostgreSQL reservations and
// recreates it, so Redis never needs to retain the mirror for the balance's
// complete lifetime.
const netEscrowFallbackTtl = 90 * 24 * time.Hour

func netEscrowExpiration(now time.Time, balanceEndTime time.Time) time.Time {
	preciseExpiration := balanceEndTime.Add(netEscrowEndTimeSlack)
	rollingExpiration := now.Add(netEscrowFallbackTtl)
	if rollingExpiration.Before(preciseExpiration) {
		return rollingExpiration
	}
	return preciseExpiration
}

type TransferBalance struct {
	BalanceId             server.Id `json:"balance_id"`
	NetworkId             server.Id `json:"network_id"`
	StartTime             time.Time `json:"start_time"`
	EndTime               time.Time `json:"end_time"`
	StartBalanceByteCount ByteCount `json:"start_balance_byte_count"`
	// how much money the platform made after subtracting fees
	NetRevenue        NanoCents `json:"net_revenue_nano_cents"`
	SubsidyNetRevenue NanoCents `json:"subsidy_net_revenue_nano_cents,omitempty"`
	BalanceByteCount  ByteCount `json:"balance_byte_count"`
	PurchaseToken     string    `json:"purchase_token,omitempty"`
	// Paid means the balance carries revenue. It is NOT the same as Pro: a data
	// code is paid but data-only.
	Paid bool `json:"paid,omitempty"`
	// Pro means the balance carries the Pro entitlement. A network is Pro iff it
	// has an in-window balance with this set -- see pro_model.go.
	Pro bool `json:"pro,omitempty"`
	// the recurring grant that wrote the balance, GrantKindNone for any other
	// balance. Read by the balance summary (see SupersededGrants).
	GrantKind GrantKind `json:"-"`
	// what open contracts reserve from the balance, already subtracted from
	// BalanceByteCount (see applyActiveTransferEscrow)
	reservedByteCount ByteCount
}

func GetActiveTransferBalances(ctx context.Context, networkId server.Id) []*TransferBalance {
	transferBalances := getActiveTransferBalancesWithoutDrain(ctx, networkId)
	// an acceptance-test drain reads as zero available (see test_balance_drain_model.go)
	applyTestBalanceDrain(transferBalances, IsTestBalanceDrainActive(ctx, networkId))
	return transferBalances
}

func getActiveTransferBalancesWithoutDrain(ctx context.Context, networkId server.Id) []*TransferBalance {
	var transferBalances []*TransferBalance
	server.Db(ctx, func(conn server.PgConn) {
		transferBalances = getActiveTransferBalanceRows(ctx, conn, networkId)
	})
	applyActiveTransferEscrow(ctx, transferBalances)
	return transferBalances
}

const activeTransferBalanceSql = `
                SELECT
                    balance_id,
                    start_time,
                    end_time,
                    start_balance_byte_count,
                    net_revenue_nano_cents,
                    balance_byte_count,
                    paid,
                    pro,
                    COALESCE(grant_kind, '')
                FROM transfer_balance
                WHERE
                    network_id = $1 AND
                    active = true AND
                    start_time <= $2 AND $2 < end_time
            `

// Share the balance query with transaction owners without acquiring another
// pool connection while they hold a row lock.
func getActiveTransferBalanceRows(ctx context.Context, query server.PgCanQuery, networkId server.Id) []*TransferBalance {
	transferBalances := []*TransferBalance{}
	result, err := query.Query(
		ctx,
		activeTransferBalanceSql,
		networkId,
		server.NowUtc(),
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			transferBalance := &TransferBalance{
				NetworkId: networkId,
			}
			server.Raise(result.Scan(
				&transferBalance.BalanceId,
				&transferBalance.StartTime,
				&transferBalance.EndTime,
				&transferBalance.StartBalanceByteCount,
				&transferBalance.NetRevenue,
				&transferBalance.BalanceByteCount,
				&transferBalance.Paid,
				&transferBalance.Pro,
				&transferBalance.GrantKind,
			))
			transferBalances = append(transferBalances, transferBalance)
		}
	})
	return transferBalances
}

// Preserve the existing per-balance escrow clamp and fail closed on Redis
// errors. Ordinary reads release PostgreSQL before fetching this mirror.
func applyActiveTransferEscrow(ctx context.Context, transferBalances []*TransferBalance) {
	server.Redis(ctx, func(r server.RedisClient) {
		netEscrowCmds := map[server.Id]*redis.StringCmd{}
		approxCmds := map[server.Id]*redis.StringCmd{}
		// the net escrow keys use per-balance hash tags (different slots), so
		// use a plain pipeline, which auto-routes per slot on cluster; a tx
		// pipeline would be cross-slot
		_, pipelineErr := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, transferBalance := range transferBalances {
				netEscrowCmds[transferBalance.BalanceId] = pipe.Get(ctx, netEscrowKey(transferBalance.BalanceId))
				approxCmds[transferBalance.BalanceId] = pipe.Get(ctx, redisContractReservationKeys(transferBalance.BalanceId)[0])
			}
			return nil
		})
		if pipelineErr != nil && !errors.Is(pipelineErr, redis.Nil) {
			server.Raise(pipelineErr)
		}
		for _, transferBalance := range transferBalances {
			netEscrowCmd := netEscrowCmds[transferBalance.BalanceId]
			netEscrowBalanceByteCount, commandErr := netEscrowCmd.Int64()
			if errors.Is(commandErr, redis.Nil) {
				netEscrowBalanceByteCount = 0
			} else {
				server.Raise(commandErr)
			}
			netEscrowBalanceByteCount = max(int64(0), netEscrowBalanceByteCount)
			transferBalance.BalanceByteCount = max(0, transferBalance.BalanceByteCount-ByteCount(netEscrowBalanceByteCount))
			approx, err := approxCmds[transferBalance.BalanceId].Int64()
			if err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
			transferBalance.BalanceByteCount = max(0, transferBalance.BalanceByteCount-max(0, approx))
			transferBalance.reservedByteCount = ByteCount(netEscrowBalanceByteCount) + ByteCount(max(0, approx))
		}
	})
}

func GetActiveTransferBalanceByteCount(ctx context.Context, networkId server.Id) ByteCount {
	net := ByteCount(0)
	for _, transferBalance := range GetActiveTransferBalances(ctx, networkId) {
		net += transferBalance.BalanceByteCount
	}
	return net
}

// Testing_NetEscrowByteCount reads the raw redis net escrow counter for a
// balance without clamping, so tests can assert exact reconciliation (drift in
// either direction) after all contracts for the balance settle.
func Testing_NetEscrowByteCount(ctx context.Context, balanceId server.Id) ByteCount {
	var byteCount ByteCount
	server.Redis(ctx, func(r server.RedisClient) {
		if v, err := r.Get(ctx, netEscrowKey(balanceId)).Int64(); err == nil {
			byteCount = ByteCount(v)
		}
		if v, err := r.Get(ctx, redisContractReservationKeys(balanceId)[0]).Int64(); err == nil {
			byteCount += v
		}
	})
	return byteCount
}

// Testing_DeleteNetEscrow removes the redis net escrow counter for a balance,
// simulating lost mirrored state.
func Testing_DeleteNetEscrow(ctx context.Context, balanceId server.Id) {
	server.Redis(ctx, func(r server.RedisClient) {
		r.Del(ctx, netEscrowKey(balanceId))
	})
}

// ReconcileNetEscrow compares the redis net escrow counters for all active
// transfer balances against the postgres source of truth and, when apply is
// true, corrects their drift. It returns the drift it found per network either
// way.
//
// The counter remains an approximate mirror between a PostgreSQL commit and
// its cache post. A crashed post can leave it behind until this reconciliation
// runs; the bounded counter ttl alone does not repair that drift. Drift affects
// approximate balance readers only. Admission uses locked database credit and
// durable reservations instead of granting authority to the cache.
//
// The reserved bytes for a balance is the sum of its escrow rows whose contract
// is still open. `transfer_contract.outcome` is claimed atomically in the
// settle transaction (`claimContractOutcomeInTx`), so `outcome IS NULL` is the
// reliable signal that a reservation is live -- unlike `transfer_escrow.settled`,
// which is itself set in a best-effort post and can leak. A disputed contract
// (`outcome` still null, generated `open` false) still holds its reservation, so
// it is matched by `outcome IS NULL` and would be missed by `open`.
//
// Each bounded PostgreSQL page reads reservations and their durable revisions
// in one statement snapshot. Redis accepts only the same or a newer revision.
// Create, settle and quarantine posts use this same idempotent publisher: a
// delayed post or stale page cannot undo a newer mirror, and a missed post is
// repaired without adding or subtracting a reservation twice.
//
// Drift is the signed difference (previous counter minus reconciled value)
// summed per network. Positive drift means the counter was over-reserved -- the
// direction that starves the available balance and produces spurious
// "Insufficient balance". Only networks with nonzero net drift are returned.
func ReconcileNetEscrow(ctx context.Context, apply bool) (driftByNetworkId map[server.Id]ByteCount, balanceCount int) {
	return reconcileNetEscrow(ctx, apply, false)
}

// ReconcileCachedNetEscrow repairs scheduled mirror drift using a durable
// snapshot only when its revision matches the same-statement source revision.
// Revision triggers invalidate legacy changes; misses retain exact census.
// Fleet repair never writes the cache. Operators use ReconcileNetEscrow for an independent
// exact audit, including detection of a corrupted same-revision cache entry.
func ReconcileCachedNetEscrow(ctx context.Context) (driftByNetworkId map[server.Id]ByteCount, balanceCount int) {
	return reconcileNetEscrow(ctx, true, true)
}

// Both callers retain page-local authority and the same Redis revision fence.
// Only scheduled repair may reuse the durable snapshot cache.
func reconcileNetEscrow(ctx context.Context, apply, useCache bool) (driftByNetworkId map[server.Id]ByteCount, balanceCount int) {
	now := server.NowUtc()
	driftByNetworkId = map[server.Id]ByteCount{}

	// visit every active balance -- the same set `createTransferEscrowInTx`
	// reads -- paginated by balance_id (the primary key) so the scan is bounded.
	// Ten thousand keeps 1.8M mostly-empty balances to ~180 source reads rather
	// than the former ~1,800 round trips while remaining a bounded Redis/SQL
	// payload.
	const batchSize = 10000
	type balanceRow struct {
		balanceId server.Id
		networkId server.Id
	}
	var cursor server.Id
	for {
		rows := []balanceRow{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
                    SELECT balance_id, network_id
                    FROM transfer_balance
                    WHERE
                        active = true AND
                        start_time <= $1 AND $1 < end_time AND
                        balance_id > $2
                    ORDER BY balance_id
                    LIMIT $3
                `,
				now,
				cursor,
				batchSize,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var row balanceRow
					server.Raise(result.Scan(&row.balanceId, &row.networkId))
					rows = append(rows, row)
				}
			})
		})
		if len(rows) == 0 {
			break
		}
		balanceIds := make([]server.Id, len(rows))
		for i, row := range rows {
			balanceIds[i] = row.balanceId
		}
		// Read reservations immediately before correcting this page. A durable
		// cached amount is exact only at the revision read in the same statement;
		// stale or missing amounts retain the per-balance history fallback. Never
		// move this above pagination and recreate a stale global snapshot.
		pending := readReconcileNetEscrowSnapshots(ctx, balanceIds, useCache)
		drift := reconcileNetEscrowBatch(ctx, pending, balanceIds, apply)
		for _, row := range rows {
			driftByNetworkId[row.networkId] += drift[row.balanceId]
		}
		balanceCount += len(rows)
		cursor = rows[len(rows)-1].balanceId
		if len(rows) < batchSize {
			break
		}
	}

	// A balance stops being available at end_time, but its contracts are closed
	// only after a grace period and can remain open longer when the close worker
	// is backlogged. The current-window scan above used to abandon those live
	// reservations at the exact expiry boundary. A lost create mirror during the
	// final interval could therefore never be repaired before the delayed
	// settlement released it and drove the counter negative.
	//
	// Check non-current balances in bounded primary-key pages. Every candidate
	// advances the cursor, including pages with no live escrow. An existence
	// check stops at the first live witness instead of grouping every unsettled
	// row; zero-byte and Redis-owned witnesses still allow stale legacy mirrors
	// to be cleared without changing their separate reservation owner.
	cursor = server.Id{}
	for {
		rows := []balanceRow{}
		candidateCount := 0
		nextCursor := cursor
		server.Tx(ctx, func(tx server.PgTx) {
			// A transaction retry must restart from the same input cursor and
			// discard any rows returned by its previous attempt.
			pageRows := []balanceRow{}
			pageCount := 0
			pageCursor := cursor
			configureNetEscrowReservationPageTimeout(ctx, tx, netEscrowReservationPageStatementTimeout)
			result, err := tx.Query(
				ctx,
				netEscrowNoncurrentOpenBalancePageSQL,
				now,
				cursor,
				batchSize,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var row balanceRow
					var hasOpenEscrow bool
					server.Raise(result.Scan(&row.balanceId, &row.networkId, &hasOpenEscrow))
					pageCount++
					pageCursor = row.balanceId
					if hasOpenEscrow {
						pageRows = append(pageRows, row)
					}
				}
			})
			rows, candidateCount, nextCursor = pageRows, pageCount, pageCursor
		}, server.TxReadCommitted, pgx.ReadOnly)
		cursor = nextCursor
		if len(rows) == 0 {
			if candidateCount < batchSize {
				break
			}
			continue
		}
		balanceIds := make([]server.Id, len(rows))
		for i, row := range rows {
			balanceIds[i] = row.balanceId
		}
		pending := readReconcileNetEscrowSnapshots(ctx, balanceIds, useCache)
		drift := reconcileNetEscrowBatch(ctx, pending, balanceIds, apply)
		for _, row := range rows {
			driftByNetworkId[row.networkId] += drift[row.balanceId]
		}
		balanceCount += len(rows)
		if candidateCount < batchSize {
			break
		}
	}

	for networkId, drift := range driftByNetworkId {
		if drift == 0 {
			delete(driftByNetworkId, networkId)
		}
	}

	return
}

// A live zero-byte or Redis-owned row remains a cleanup witness for a stale
// legacy mirror. Preserve that lifecycle set, but stop at the first live row
// rather than aggregating all unsettled history. The escrow and contract
// boundaries prevent stale statistics from replacing either key lookup with
// a scan of unrelated history. Disputes hold their reservation until outcome
// is non-NULL, even though their generated open flag is false.
const netEscrowOpenBalanceWitnessSQL = `
    EXISTS (
        SELECT 1
        FROM (
            SELECT contract_id
            FROM transfer_escrow
            WHERE transfer_escrow.balance_id = transfer_balance.balance_id AND
                transfer_escrow.settled = false
            OFFSET 0
        ) AS selected_escrow
        INNER JOIN LATERAL (
            SELECT outcome FROM transfer_contract
            WHERE contract_id = selected_escrow.contract_id
            OFFSET 0
        ) AS transfer_contract ON transfer_contract.outcome IS NULL
    )
`

// Page candidate balances before testing their witnesses. LIMIT after a
// GROUP BY bounded output only: discovery could still join millions of native
// or zero-byte rows for a single balance before returning the first page.
// Returning the witness flag also lets an empty candidate page advance the
// caller's cursor without reconciling balances that have no live reservation.
const netEscrowNoncurrentOpenBalancePageSQL = `
    SELECT transfer_balance.balance_id, transfer_balance.network_id,
        ` + netEscrowOpenBalanceWitnessSQL + ` AS has_open_escrow
    FROM (
        SELECT balance_id, network_id
        FROM transfer_balance
        WHERE
            NOT (
                transfer_balance.active = true AND
                transfer_balance.start_time <= $1 AND $1 < transfer_balance.end_time
            ) AND
            transfer_balance.balance_id > $2
        ORDER BY transfer_balance.balance_id
        LIMIT $3
    ) AS transfer_balance
    ORDER BY transfer_balance.balance_id
`

// ReconcileNetEscrowForNetwork reconciles the redis net escrow counters for one
// network's current balances plus any non-current balance that still owns open
// escrow. See [ReconcileNetEscrow]; this is the targeted form used to
// immediately clear (or, with apply false, just measure) drift on a single
// affected network. The returned drift is the signed total over the network's
// balances (previous counters minus reconciled values).
func ReconcileNetEscrowForNetwork(ctx context.Context, networkId server.Id, apply bool) (driftByteCount ByteCount, balanceCount int) {
	now := server.NowUtc()

	balanceIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT balance_id
                FROM transfer_balance
                WHERE
                    network_id = $1 AND
                    active = true AND
                    start_time <= $2 AND $2 < end_time
            `,
			networkId,
			now,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				server.Raise(result.Scan(&balanceId))
				balanceIds = append(balanceIds, balanceId)
			}
		})
	})
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT transfer_balance.balance_id
                FROM transfer_balance
                WHERE
                    transfer_balance.network_id = $1 AND
                    NOT (
                        transfer_balance.active = true AND
                        transfer_balance.start_time <= $2 AND $2 < transfer_balance.end_time
                    ) AND `+netEscrowOpenBalanceWitnessSQL+`
                ORDER BY transfer_balance.balance_id
            `,
			networkId,
			now,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				server.Raise(result.Scan(&balanceId))
				balanceIds = append(balanceIds, balanceId)
			}
		})
	})
	if len(balanceIds) == 0 {
		return
	}

	const batchSize = 10000
	for start := 0; start < len(balanceIds); start += batchSize {
		end := min(start+batchSize, len(balanceIds))
		batch := balanceIds[start:end]
		pending := openEscrowReservedForBalances(ctx, batch)
		drift := reconcileNetEscrowBatch(ctx, pending, batch, apply)
		for _, d := range drift {
			driftByteCount += d
		}
	}
	return driftByteCount, len(balanceIds)
}

// openEscrowReservedForBalances returns the current reserved (open-contract)
// escrow bytes for exactly one bounded balance page. The partial unsettled
// balance index is a required part of this algorithm.
//
// Keep the requested balances as the outer relation and OFFSET 0 as an
// optimization boundary. On the billion-row production table PostgreSQL 18.4
// estimates a 10,000-value ANY predicate broadly enough to choose a parallel
// sequential scan of all transfer_escrow history for every page, even though
// most active balances have no historical escrow rows. The lateral lookup
// makes the intended bound structural: each requested balance gets one range
// scan of transfer_escrow_unsettled_balance_contract, and no page can become a
// whole transfer_escrow scan merely because statistics or table size change.
//
// outcome IS NULL remains the authoritative live-reservation predicate.
// settled is changed only after claimContractOutcomeInTx commits a non-NULL
// outcome, so an open contract's escrow is necessarily unsettled. The reverse
// is intentionally not assumed: the best-effort settled post can be missed and
// leave closed escrow rows unsettled. `settled = false` is therefore a safe
// partial-index prefilter only while the outcome join remains in this query.
// Zero-byte anchors cannot affect SUM. Reject them inside the same boundary
// before spending a contract lookup on every retained control connection.
// Keep the contract lookup inside its own key boundary too. False-zero
// outcome-index statistics can otherwise turn that join into repeated scans
// of unrelated unresolved contracts while an admission holds its grant locks.
// Apply outcome outside this boundary so it cannot replace the exact lookup
// with a global partial-index scan; disputed unresolved contracts still count.
// Fence the two metadata lookups as well: stale estimates can otherwise turn
// even a late ten-balance page into full balance and revision table scans.
const netEscrowReservationPageSQL = `
    SELECT requested_balance.balance_id,
        COALESCE(revision.revision, 0),
        CASE WHEN balance.balance_id IS NULL THEN 0 ELSE reserved.byte_count END,
        balance.end_time
    FROM unnest($1::uuid[]) AS requested_balance(balance_id)
    LEFT JOIN LATERAL (
        SELECT revision
        FROM transfer_balance_net_escrow_revision
        WHERE balance_id = requested_balance.balance_id
        OFFSET 0
    ) AS revision ON true
    LEFT JOIN LATERAL (
        SELECT balance_id, end_time
        FROM transfer_balance
        WHERE balance_id = requested_balance.balance_id
        OFFSET 0
    ) AS balance ON true
    CROSS JOIN LATERAL (
        SELECT COALESCE(SUM(selected_escrow.balance_byte_count), 0) AS byte_count
        FROM (
            SELECT transfer_escrow.contract_id, transfer_escrow.balance_byte_count
            FROM transfer_escrow
            WHERE transfer_escrow.balance_id = requested_balance.balance_id AND
                transfer_escrow.settled = false AND
                transfer_escrow.balance_byte_count <> 0 AND NOT transfer_escrow.redis_reserved
            OFFSET 0
        ) AS selected_escrow
        INNER JOIN LATERAL (
            SELECT outcome FROM transfer_contract
            WHERE contract_id = selected_escrow.contract_id
            OFFSET 0
        ) AS transfer_contract ON transfer_contract.outcome IS NULL
    ) AS reserved
`

// The healthy bounded-lateral page completes below one second, and the prior
// degraded implementation averaged about seven seconds. Two minutes leaves a
// wide load margin while remaining far inside the task's 30-minute client-side
// deadline and the monitor's overrun boundary. PostgreSQL must own this fence:
// a dead taskworker cannot send a context cancellation for detached work.
const netEscrowReservationPageStatementTimeout = 2 * time.Minute

// Captures the one transaction-local configuration operation used by the
// production PgTx and deterministic recorder.
type netEscrowReservationPageConfigurer interface {
	Exec(context.Context, string, ...any) (server.PgTag, error)
}

// Applies the server-side fence only to the transaction containing one page;
// generic-plan JIT compilation can dominate this bounded census. Keep both
// settings local so pooled sessions retain their configured timeout and JIT.
func configureNetEscrowReservationPageTimeout(
	ctx context.Context,
	tx netEscrowReservationPageConfigurer,
	timeout time.Duration,
) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT set_config('statement_timeout', $1, true), set_config('jit', 'off', true)`,
		strconv.FormatInt(timeout.Milliseconds(), 10)+"ms",
	))
}

func openEscrowReservedForBalances(ctx context.Context, balanceIds []server.Id) map[server.Id]netEscrowSnapshot {
	pending := map[server.Id]netEscrowSnapshot{}
	if len(balanceIds) == 0 {
		return pending
	}
	defer enterLegacySettlementTiming(ctx, legacySettlementColdCensus)()
	server.Tx(ctx, func(tx server.PgTx) {
		configureNetEscrowReservationPageTimeout(ctx, tx, netEscrowReservationPageStatementTimeout)
		pending = readNetEscrowSnapshots(ctx, tx, balanceIds)
	}, server.TxReadCommitted, pgx.ReadOnly)
	return pending
}

// Reuses the bounded reservation census inside the caller's transaction. For
// admission this statement must follow the balance locks, so read committed
// observes reservations committed by a creator that held those locks first.
func readNetEscrowSnapshots(ctx context.Context, query server.PgCanQuery, balanceIds []server.Id) map[server.Id]netEscrowSnapshot {
	pending := map[server.Id]netEscrowSnapshot{}
	if len(balanceIds) == 0 {
		return pending
	}
	result, err := query.Query(ctx, netEscrowReservationPageSQL, balanceIds)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId server.Id
			var snapshot netEscrowSnapshot
			server.Raise(result.Scan(&balanceId, &snapshot.revision, &snapshot.reserved, &snapshot.endTime))
			if snapshot.revision < 0 || snapshot.reserved < 0 {
				server.Raise(fmt.Errorf("invalid net escrow snapshot for balance %s", balanceId))
			}
			pending[balanceId] = snapshot
		}
	})
	return pending
}

// AddTransferBalanceInTx adds a balance, taking the Pro entitlement from
// transferBalance.Pro.
//
// Pro is set EXPLICITLY here rather than left to the column default (which is true,
// so that the migration keeps existing subscribers Pro). Relying on the default would
// mean any new caller silently grants Pro -- which is exactly how a data-only
// purchase could end up upgrading a network for free. Callers must say what they mean:
// subscription activation sets Pro: true, and data purchases leave it false.
func AddTransferBalanceInTx(ctx context.Context, tx server.PgTx, transferBalance *TransferBalance) {
	balanceId := server.NewId()

	server.RaisePgResult(tx.Exec(
		ctx,
		`
                INSERT INTO transfer_balance (
                    balance_id,
                    network_id,
                    start_time,
                    end_time,
                    start_balance_byte_count,
                    balance_byte_count,
                    net_revenue_nano_cents,
                    purchase_token,
                    subsidy_net_revenue_nano_cents,
                    pro
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
            `,
		balanceId,
		transferBalance.NetworkId,
		transferBalance.StartTime,
		transferBalance.EndTime,
		transferBalance.StartBalanceByteCount,
		transferBalance.BalanceByteCount,
		transferBalance.NetRevenue,
		transferBalance.PurchaseToken,
		transferBalance.SubsidyNetRevenue,
		transferBalance.Pro,
	))

	transferBalance.BalanceId = balanceId
}

func AddTransferBalance(ctx context.Context, transferBalance *TransferBalance) {
	server.Tx(ctx, func(tx server.PgTx) {
		AddTransferBalanceInTx(ctx, tx, transferBalance)
	})

	if transferBalance.Pro {
		// The balance is committed, so refresh the entitlement cache HERE rather than
		// making every caller remember to. A Pro balance that does not read as Pro
		// until the cache expires is exactly the flaky upgrade we are avoiding: the
		// "false" cached before the purchase would keep being served, leaving a user
		// who just paid on the free plan for up to ProCacheTtl.
		//
		// Callers that add a Pro balance inside their OWN tx (AddTransferBalanceInTx,
		// AddProTransferBalanceInTx) must do this themselves once it commits.
		UpdateProNetwork(ctx, transferBalance.NetworkId)
	}
}

// TODO GetLastTransferData returns the transfer data with
// 1. the given purhase record
// 2. that starte before and ends after sub.ExpiryTime
// TODO with the max end time
// TODO if none, return err
func GetOverlappingTransferBalance(ctx context.Context, purchaseToken string, expiryTime time.Time) (balanceId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		balanceId, returnErr = getOverlappingTransferBalance(conn, ctx, purchaseToken, expiryTime)
	})

	return
}

// GetOverlappingTransferBalanceInTx is the in-tx variant, for callers that gate a
// credit on the check and need the check and the credit in ONE transaction (the
// Play renewal path re-checks under an advisory lock before crediting).
func GetOverlappingTransferBalanceInTx(tx server.PgTx, ctx context.Context, purchaseToken string, expiryTime time.Time) (balanceId server.Id, returnErr error) {
	return getOverlappingTransferBalance(tx, ctx, purchaseToken, expiryTime)
}

// overlappingBalanceQuerier is the intersection of PgConn and PgTx this query
// needs, so the Db and InTx variants can share one implementation.
type overlappingBalanceQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func getOverlappingTransferBalance(conn overlappingBalanceQuerier, ctx context.Context, purchaseToken string, expiryTime time.Time) (balanceId server.Id, returnErr error) {
	result, err := conn.Query(
		ctx,
		`
                SELECT
                    balance_id
                FROM transfer_balance
                WHERE
                    purchase_token = $1 AND
                    $2 < end_time AND
                    start_time <= $2
                ORDER BY end_time DESC
                LIMIT 1
            `,
		purchaseToken,
		expiryTime,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&balanceId))
		} else {
			returnErr = errors.New("Overlapping transfer balance not found.")
		}
	})

	return
}

func AddBasicTransferBalanceInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	transferBalance ByteCount,
	startTime time.Time,
	endTime time.Time,
) (returnErr error) {
	balanceId := server.NewId()

	// pro = false: an unpaid, data-only balance. It must never confer Pro -- see
	// pro_model.go. It records no grant kind, so it is for balances that are not a
	// recurring grant (prober credit, fixtures); the daily free grant and referral
	// bonuses use AddGrantTransferBalanceInTx.
	_, err := tx.Exec(
		ctx,
		`
                INSERT INTO transfer_balance (
                    balance_id,
                    network_id,
                    start_time,
                    end_time,
                    start_balance_byte_count,
                    net_revenue_nano_cents,
                    balance_byte_count,
                    pro
                )
                VALUES ($1, $2, $3, $4, $5, $6, $5, false)
            `,
		balanceId,
		networkId,
		startTime,
		endTime,
		transferBalance,
		NanoCents(0),
	)
	// a failed insert aborts the caller's transaction, so it raises rather
	// than leave the caller to commit a rollback
	server.Raise(err)
	return
}

// add balance to a network at no cost
// AddProTransferBalanceInTx grants one network a Pro balance for the window. The
// balance carries pro = true, which is what confers the Pro entitlement -- see
// pro_model.go. The caller must refresh the Pro cache (UpdateProNetwork) once the
// tx commits, so the upgrade is visible immediately.
//
// It records no grant kind: it is for a Pro balance bought for its own window
// (x402), which the next monthly grant must not supersede in the summary. The
// monthly Pro grant uses AddGrantTransferBalanceInTx.
func AddProTransferBalanceInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	transferBalance ByteCount,
	startTime time.Time,
	endTime time.Time,
) (returnErr error) {
	balanceId := server.NewId()

	_, err := tx.Exec(
		ctx,
		`
                INSERT INTO transfer_balance (
                    balance_id,
                    network_id,
                    start_time,
                    end_time,
                    start_balance_byte_count,
                    net_revenue_nano_cents,
                    balance_byte_count,
                    pro
                )
                VALUES ($1, $2, $3, $4, $5, $6, $5, true)
            `,
		balanceId,
		networkId,
		startTime,
		endTime,
		transferBalance,
		NanoCents(0),
	)
	// a failed insert aborts the caller's transaction, so it raises rather
	// than leave the caller to commit a rollback
	server.Raise(err)
	return
}

func AddBasicTransferBalance(
	ctx context.Context,
	networkId server.Id,
	transferBalance ByteCount,
	startTime time.Time,
	endTime time.Time,
) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		returnErr = AddBasicTransferBalanceInTx(
			tx,
			ctx,
			networkId,
			transferBalance,
			startTime,
			endTime,
		)
	})

	return
}

// this finds networks with no entries in transfer_balance
// this is potentially different than networks with zero transfer balance
func FindNetworksWithoutTransferBalance(ctx context.Context) (networkIds []server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    network.network_id
                FROM network
                WHERE NOT EXISTS (
                    SELECT 1 FROM transfer_balance
                    WHERE transfer_balance.network_id = network.network_id
                )
            `,
		)

		networkIds = []server.Id{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})
	})
	return
}

type ContractOutcome = string

const (
	ContractOutcomeSettled                      ContractOutcome = "settled"
	ContractOutcomeDisputeResolvedToSource      ContractOutcome = "dispute_resolved_to_source"
	ContractOutcomeDisputeResolvedToDestination ContractOutcome = "dispute_resolved_to_destination"
)

// errContractAlreadySettled distinguishes a benign duplicate close from
// malformed settlement and conflicting terminal outcomes. The expiry sweep
// can select an open contract immediately before a live close settles it; that
// race is successful convergence once the sweep verifies the terminal row and
// removes the stale stream entry.
var errContractAlreadySettled = errors.New("Contract already closed with outcome settled")

// Identity, not diagnostic text, authorizes the bounded disputed-row retry.
var errContractInsufficientEscrow = errors.New("Escrow does not have enough value to pay out the full amount.")

// A fresh existing row was read after the attempt; no stream cleanup ran.
type forceCloseNonfinalError struct {
	disputed bool
}

// Preserve the existing diagnostic while keeping verifier authority private.
func (self *forceCloseNonfinalError) Error() string {
	return "contract remained non-final after force-close attempt"
}

// Terminal settlement of a newly created dispute rejected only its escrow
// guard, then one fresh read verified that the row remained disputed/nonfinal.
type forceCloseDisputeRejectionError struct {
	cause error
}

// Retain the settlement failure and its independent verification together.
func (self *forceCloseDisputeRejectionError) Error() string { return self.cause.Error() }

// Do not hide either cause from the batch or task's ordinary error inspection.
func (self *forceCloseDisputeRejectionError) Unwrap() error { return self.cause }

// The bounded batch completed with only verified accounting rejections: either
// a still-reserved dispute or an existing no-payout quarantine verified terminal.
// This is still a failure; full causes and unresolved reservations remain.
type ForceCloseAccountingError struct {
	cause                               error
	verifiedCloseCount                  int64
	accountingRejectionCount            int64
	quarantinedAccountingRejectionCount int64
}

// Keep the durable error text unchanged.
func (self *ForceCloseAccountingError) Error() string { return self.cause.Error() }

// Preserve every original failure for ordinary error inspection.
func (self *ForceCloseAccountingError) Unwrap() error { return self.cause }

// Counts fresh terminal verification followed by successful stream cleanup,
// including the separately reported no-payout quarantine subset.
func (self *ForceCloseAccountingError) VerifiedCloseCount() int64 { return self.verifiedCloseCount }

// Counts still-reserved disputed rows, never successful closes.
func (self *ForceCloseAccountingError) AccountingRejectionCount() int64 {
	return self.accountingRejectionCount
}

// Subset of verified closes that retained a rejected report and paid nothing;
// terminal progress is not authority to report successful financial settlement.
func (self *ForceCloseAccountingError) QuarantinedAccountingRejectionCount() int64 {
	return self.quarantinedAccountingRejectionCount
}

// Only a complete, bounded single-cause chain preserves sentinel authority.
// Truncated, cyclic, nil, and multi-error graphs cannot authorize an outcome.
func isOnlyContractError(err error, expected error) bool {
	causes := server.InspectErrorCauses(err)
	if !causes.Complete || causes.NilBranches != 0 {
		return false
	}
	for _, cause := range causes.Nodes {
		if _, multiple := cause.Err.(interface{ Unwrap() []error }); multiple {
			return false
		}
		if cause.Leaf {
			return cause.Err == expected
		}
	}
	return false
}

// Only the exact settled duplicate can skip malformed-contract quarantine.
func isOnlyContractAlreadySettled(err error) bool {
	return isOnlyContractError(err, errContractAlreadySettled)
}

// Every phase must be accounted for separately, not found somewhere in a join.
func isForceCloseAccountingRejection(closeErr error, quarantineErr error, cleanupErr error) bool {
	if quarantineErr != nil {
		return false
	}
	if closeErr == nil {
		rejection, ok := cleanupErr.(*forceCloseDisputeRejectionError)
		return ok && rejection != nil && rejection.cause != nil
	}
	if !isOnlyContractError(closeErr, errContractInsufficientEscrow) {
		return false
	}
	verification, ok := cleanupErr.(*forceCloseNonfinalError)
	return ok && verification != nil && verification.disputed
}

// Only this attempt's successful no-payout claim, complete posts and fresh
// terminal/stream verification authorize progress despite the exact escrow
// guard. A concurrent terminal row alone cannot donate quarantine authority.
func isForceCloseQuarantinedAccountingRejection(closeErr error, quarantineClaimed bool, quarantineErr error, cleanupErr error) bool {
	return quarantineClaimed && quarantineErr == nil && cleanupErr == nil &&
		isOnlyContractError(closeErr, errContractInsufficientEscrow)
}

// A fresh intent read delegates this row only after every preceding phase
// succeeded. Finding the pending sentinel in a joined error cannot erase an
// operational failure or authorize an accounting-only page checkpoint.
func isForceCloseDeferredSettlement(closeErr error, quarantineErr error, cleanupErr error) bool {
	return closeErr == nil && quarantineErr == nil && cleanupErr == errLegacySettlementPending
}

// Only the exact guard permits one post-failure read. Missing, changed, or
// unavailable state preserves ordinary failure; cancellation invalidates even
// a positive verifier result. No financial operation is retried here.
func finishForceCloseDisputeSettlement(ctx context.Context, settleErr error, verify func() error) error {
	if !isOnlyContractError(settleErr, errContractInsufficientEscrow) {
		return settleErr
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(settleErr, ctxErr)
	}
	verificationErr := verify()
	joined := errors.Join(settleErr, verificationErr)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Join(joined, ctxErr)
	}
	verification, ok := verificationErr.(*forceCloseNonfinalError)
	if ok && verification != nil && verification.disputed {
		return &forceCloseDisputeRejectionError{cause: joined}
	}
	return joined
}

func finishForceCloseContract(closeErr error, quarantine func() error, cleanup func() error) error {
	alreadySettled := isOnlyContractAlreadySettled(closeErr)
	// A busy financial/publication owner retains the original settlement. Its
	// scheduling refusal cannot authorize the malformed no-payout transition,
	// even if that owner has already released before the next branch runs.
	ownershipBusy := isOnlyContractError(closeErr, errTransferBalanceOwnershipBusy)
	if closeErr != nil && !alreadySettled && !ownershipBusy && forceCloseErrorAllowsQuarantine(closeErr) {
		closeErr = errors.Join(closeErr, quarantine())
	}

	// A live close may settle a contract after the sweep selected its open
	// snapshot. Accept only that exact terminal outcome, and only when the
	// independent final-state check and stream cleanup both succeeded.
	cleanupErr := cleanup()
	if cleanupErr == nil && alreadySettled {
		return nil
	}
	return errors.Join(closeErr, cleanupErr)
}

type ContractParty = string

const (
	ContractPartySource      ContractParty = "source"
	ContractPartyDestination ContractParty = "destination"
	ContractPartyCheckpoint  ContractParty = "checkpoint"
)

type TransferEscrow struct {
	ContractId          server.Id
	CompanionContractId *server.Id
	ExpirationTime      time.Time
	Priority            Priority
	TransferByteCount   ByteCount
	Balances            []*TransferEscrowBalance
}

type TransferEscrowBalance struct {
	BalanceId        server.Id
	BalanceByteCount ByteCount
}

// ContractParticipant is one service client whose hop carries a contract's
// traffic. The payer/origin endpoint is not a participant; the opposite
// endpoint (egress) is, along with every intermediary attached to the
// contract's stream and the provider client of every extender party. Every hop
// has equal weight, so one client appears at most once whatever its roles.
type ContractParticipant struct {
	ClientId  server.Id
	NetworkId server.Id
}

// SetContractStream durably associates a contract with its Redis stream and
// records the stream's intermediary contract participants. Participants are
// keyed by stream id rather than contract id so companion contracts, which do
// not repeat the intermediary list, settle against the same participant set.
// The first network snapshot survives retries and later membership changes.
func SetContractStream(
	ctx context.Context,
	contractId server.Id,
	streamId server.Id,
	intermediaryIds []server.Id,
) (returnErr error) {
	ctx = providerWorkSessionContext(ctx)
	defer server.EnterContractCreationStage(ctx, server.ContractStageStream)()
	// Join callers do not carry the original path. Recover it from the Redis
	// contract marking while it is present, both to populate a newly joined
	// contract and to backfill streams created before participant persistence
	// was deployed. Once recorded below, settlement no longer depends on Redis.
	if intermediaryIds == nil {
		markedStreamId, streamKey, ok := GetStream(ctx, contractId)
		if ok {
			if markedStreamId != streamId {
				return fmt.Errorf(
					"Contract Redis stream %s does not match requested stream %s: %s",
					markedStreamId.String(),
					streamId.String(),
					contractId.String(),
				)
			}
			intermediaryIds = streamKey.IntermediaryIds()
		}
	}
	intermediaryIds = slices.Clone(intermediaryIds)
	slices.SortFunc(intermediaryIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	intermediaryIds = slices.Compact(intermediaryIds)

	server.Tx(ctx, func(tx server.PgTx) {
		participantNetworks := map[server.Id]server.Id{}
		if 0 < len(intermediaryIds) {
			// Retries own the retained identity even after directory removal.
			// Only a first-seen participant needs a current directory entry.
			result, err := tx.Query(
				ctx,
				`
					SELECT client_id, network_id
					FROM contract_participant
					WHERE stream_id = $1 AND client_id = ANY($2)
					UNION ALL
					SELECT client_id, network_id
					FROM network_client
					WHERE client_id = ANY($2) AND NOT EXISTS (
						SELECT 1 FROM contract_participant
						WHERE stream_id = $1 AND client_id = network_client.client_id
					)
				`,
				streamId,
				intermediaryIds,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					var networkId server.Id
					server.Raise(result.Scan(&clientId, &networkId))
					participantNetworks[clientId] = networkId
				}
			})
			if len(participantNetworks) != len(intermediaryIds) {
				missingIds := []string{}
				for _, clientId := range intermediaryIds {
					if _, ok := participantNetworks[clientId]; !ok {
						missingIds = append(missingIds, clientId.String())
					}
				}
				returnErr = fmt.Errorf("Contract intermediary clients do not exist: %s", strings.Join(missingIds, ", "))
				return
			}
		}

		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE transfer_contract
				SET stream_id = $2
				WHERE
					contract_id = $1 AND
					(stream_id IS NULL OR stream_id = $2)
			`,
			contractId,
			streamId,
		))
		if tag.RowsAffected() != 1 {
			returnErr = fmt.Errorf("Contract not found or already belongs to another stream: %s", contractId.String())
			return
		}

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, clientId := range intermediaryIds {
				batch.Queue(
					`
						INSERT INTO contract_participant (
							stream_id,
							client_id,
							network_id
						)
						VALUES ($1, $2, $3)
						ON CONFLICT (stream_id, client_id) DO NOTHING
					`,
					streamId,
					clientId,
					participantNetworks[clientId],
				)
			}
		})
		providerWorkAttachStreamInTx(ctx, tx, contractId, streamId)
	})

	return
}

// Writes the extender parties of a new contract (connect/EXTENDER.md J2): the
// distinct active extenders tagged on the currently connected connections of
// the source client, as party source, and those of the destination client, as
// party destination. The extender's own
// provider client and network are copied in, so settlement never joins the
// directory. Zero rows is the normal case, and a contract need not have
// carried its data over the extender -- every active extender of an endpoint
// counts, which is fuzzy per contract but averages to the right allocation.
//
// The two branches carry different party values so they cannot collide, and
// each is distinct in itself, so several connections of one client through one
// extender are one row.
//
// create_time is copied from the contract, not defaulted: the hourly counts of
// connect/EXTENDER.md M3 bucket these rows by create_time and the contracts by
// theirs, so the two must be the same instant to the microsecond. The contract
// writes its own create_time from clock_timestamp(), which the column's
// DEFAULT now() -- the transaction start -- would only approximate, and a
// transaction that straddles an hour boundary would put the contract and its
// parties in different buckets. The join is on the transfer_contract row this
// same transaction inserted just above, so it is a primary key lookup that
// always hits; a contract that somehow is not there gets no party rows rather
// than a null create_time.
//
// $1 contract, $2 source client, $3 destination client, $4 source party,
// $5 destination party.
const contractExtenderInsertSql = `
	INSERT INTO contract_extender (
		contract_id,
		extender_id,
		party,
		client_id,
		network_id,
		create_time
	)
	SELECT
		transfer_contract.contract_id,
		endpoint.extender_id,
		endpoint.party,
		network_extender.client_id,
		network_extender.network_id,
		transfer_contract.create_time
	FROM (
		SELECT DISTINCT extender_id, $4::varchar AS party
		FROM network_client_connection
		WHERE
			client_id = $2 AND
			connected AND
			extender_id IS NOT NULL

		UNION ALL

		SELECT DISTINCT extender_id, $5::varchar AS party
		FROM network_client_connection
		WHERE
			client_id = $3 AND
			connected AND
			extender_id IS NOT NULL
	) AS endpoint
	INNER JOIN network_extender ON
		network_extender.extender_id = endpoint.extender_id AND
		network_extender.active
	INNER JOIN transfer_contract ON
		transfer_contract.contract_id = $1
`

// MinShrinkContractTransferByteCount is the floor for shrink-to-fit escrow.
// When the payer's available balance is below the requested contract size but
// at least min(requested, MinShrinkContractTransferByteCount), the contract is
// granted for the available balance instead of failing with "Insufficient
// balance". Clients size their send capacity from the signed
// `StoredContract.TransferByteCount`, so a smaller grant is transparent to them.
// The floor matches the client's initial contract size, so a granted contract
// always fits at least one message.
const MinShrinkContractTransferByteCount = Mib

// grantTransferEscrowByteCount decides the contract size granted for a request
// of `requestedByteCount` when `availableByteCount` can be escrowed for it.
// A request the balance covers is granted as is. Otherwise the contract
// shrinks to fit the available balance, provided that is at least
// min(requested, MinShrinkContractTransferByteCount); below the floor the
// request is refused (`ok` false). The grant never exceeds the available
// balance, and a zero-byte request is always granted as zero bytes.
func grantTransferEscrowByteCount(
	requestedByteCount ByteCount,
	availableByteCount ByteCount,
) (grantedByteCount ByteCount, ok bool) {
	if requestedByteCount <= availableByteCount {
		return requestedByteCount, true
	}
	if availableByteCount < min(requestedByteCount, MinShrinkContractTransferByteCount) {
		return 0, false
	}
	// shrink to fit
	return availableByteCount, true
}

// Ordinary payers reserve from the earliest available grants. The persisted
// internal prober first tries a bounded whole-request free grant. Both paths
// hold balance locks through commit for positive-byte admission and read
// reservations afterward in read committed, so a lock wait cannot authorize
// already-reserved credit.
// Zero-byte contracts read their earliest-grant anchor and priority without
// taking financial locks; their client lifecycle fences precede this read.
// A positive request larger than the available balance is granted for the
// available balance when it is at least the shrink floor (see
// grantTransferEscrowByteCount); the returned escrow's TransferByteCount is the
// granted size on both the Redis and the PostgreSQL admission path.
func createTransferEscrowInTx(
	ctx context.Context,
	tx server.PgTx,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	payerNetworkId server.Id,
	contractTransferByteCount ByteCount,
	companionContractId *server.Id,
) (transferEscrow *TransferEscrow, posts []func() any, returnErr error) {
	ctx = providerWorkSessionContext(ctx)
	// an acceptance-test drain refuses like an empty balance, on both the Redis
	// and the PostgreSQL admission path. The allowlist is checked in memory, so
	// other payers add no query here.
	if 0 < contractTransferByteCount {
		if err := testBalanceDrainEscrowError(
			testBalanceDrainActive(ctx, tx, payerNetworkId, server.NowUtc()),
			contractTransferByteCount,
		); err != nil {
			return nil, nil, err
		}
		// a paying client at a data cap refuses the same way, on both paths
		// (network_client_data_cap_model.go). The capped set is checked in
		// memory, so payers in networks without a capped client add no query.
		payerClientId := sourceId
		if sourceNetworkId != payerNetworkId {
			payerClientId = destinationId
		}
		if err := clientDataCapEscrowError(ctx, tx, payerNetworkId, payerClientId, contractTransferByteCount, server.NowUtc()); err != nil {
			return nil, nil, err
		}
	}
	if admission := redisAdmissionFromContext(ctx); admission != nil && contractTransferByteCount > 0 {
		return createRedisTransferEscrowInTx(ctx, tx, admission, sourceNetworkId, sourceId,
			destinationNetworkId, destinationId, payerNetworkId, contractTransferByteCount, companionContractId)
	}
	// *important note* this function is one of the hotspots in the system,
	// since it is called before every transfer pair.
	// a small regression here can cause a backlog in the overall throughput of the network.
	// You must make sure the queries here are optimized correctly.
	// TODO we need better performance regression tools to measure small regressions in hotspots like this

	// note it is possible to create a contract with `contractTransferByteCount = 0`
	if contractTransferByteCount < 0 {
		return nil, nil, fmt.Errorf("negative contract transfer byte count")
	}
	shardDeadline, err := validateProberShardPayerInTx(ctx, tx, sourceNetworkId, destinationNetworkId, payerNetworkId)
	if err != nil {
		return nil, nil, err
	}
	if contractTransferByteCount == 0 {
		if err := lockActiveContractClientsInTx(ctx, tx, sourceNetworkId, sourceId, destinationNetworkId, destinationId); err != nil {
			return nil, nil, err
		}
	}

	contractId := server.NewId()

	type escrow = escrowTransferBalance

	now := server.NowUtc()

	// add up the balance_byte_count until >= contractTransferByteCount
	// if not enough, shrink to fit or error (see grantTransferEscrowByteCount)
	balanceEscrows := map[server.Id]*escrow{}

	// attempt to split up across remaining transfer balances

	// Reverse companions still use the payer-side client to spread independent
	// prober allocations. Ordinary payers and zero-byte anchors keep their order.
	payerClientId := sourceId
	if sourceNetworkId != payerNetworkId {
		payerClientId = destinationId
	}
	orderedTransferBalances := loadTransferEscrowBalances(ctx, tx, payerNetworkId, payerClientId, now, contractTransferByteCount)
	// Waiting for a payer's grant must not retain provider/client row locks
	// and block unrelated connection refreshes or lifecycle changes. Revalidate
	// both endpoints after the financial wait, retaining the locks through commit.
	if 0 < contractTransferByteCount {
		if err := lockActiveContractClientsInTx(ctx, tx, sourceNetworkId, sourceId, destinationNetworkId, destinationId); err != nil {
			return nil, nil, err
		}
		// Client-lock waits can outlive a grant even though its row stayed
		// locked and unchanged. Keep the final positive-byte eligibility check
		// after that wait; zero anchors preserve their original snapshot order.
		now = server.NowUtc()
		orderedTransferBalances = slices.DeleteFunc(orderedTransferBalances, func(balance *escrow) bool {
			return balance.startTime.After(now) || !now.Before(balance.endTime)
		})
	}

	slices.SortFunc(orderedTransferBalances, func(a *escrow, b *escrow) int {
		if a.endTime.Before(b.endTime) {
			return -1
		} else if b.endTime.Before(a.endTime) {
			return 1
		}

		if a.startTime.Before(b.startTime) {
			return -1
		} else if b.startTime.Before(a.startTime) {
			return 1
		}

		return a.balanceId.Cmp(b.balanceId)
	})

	netEscrowBalanceByteCount := ByteCount(0)

	for _, transferBalance := range orderedTransferBalances {
		// Active balances can be fully reserved by open contracts. They
		// must not create empty rows, dilute priority, or refresh mirrors.
		if 0 < contractTransferByteCount && transferBalance.balanceByteCount == 0 {
			continue
		}
		escrowBalanceByteCount := min(
			contractTransferByteCount-netEscrowBalanceByteCount,
			transferBalance.balanceByteCount,
		)

		balanceEscrows[transferBalance.balanceId] = &escrow{
			balanceId:        transferBalance.balanceId,
			paid:             transferBalance.paid,
			balanceByteCount: escrowBalanceByteCount,
			// carried to stamp the net escrow counter ttl in the redis post
			endTime:     transferBalance.endTime,
			reservation: transferBalance.reservation,
		}
		netEscrowBalanceByteCount += escrowBalanceByteCount
		if contractTransferByteCount <= netEscrowBalanceByteCount {
			// we have enough balances for this escrow
			break
		}
	}

	// The loop drew every available byte when the balances fall short, so a
	// shrunk grant equals the escrow rows' sum.
	grantedTransferByteCount, ok := grantTransferEscrowByteCount(contractTransferByteCount, netEscrowBalanceByteCount)
	if !ok {
		returnErr = fmt.Errorf("Insufficient balance (%d).", netEscrowBalanceByteCount)
		return
	}
	// the contract row, escrow rows, mirror and returned escrow all carry the
	// granted size, which may be smaller than the request (shrink to fit)
	contractTransferByteCount = grantedTransferByteCount

	// the priority is blended between 0 and 100 depending on escrows
	var priority Priority
	if 0 < len(balanceEscrows) {
		for _, escrow := range balanceEscrows {
			if escrow.paid {
				priority += PaidPriority
			} else {
				priority += UnpaidPriority
			}
		}
		priority /= Priority(len(balanceEscrows))
	} else {
		priority = UnpaidPriority
	}

	// Revision triggers serialize per balance. Keep multi-balance reservations
	// in the same order as settlement and reconciliation revision updates.
	balanceIds := make([]server.Id, 0, len(balanceEscrows))
	for balanceId := range balanceEscrows {
		balanceIds = append(balanceIds, balanceId)
	}
	slices.SortFunc(balanceIds, func(a, b server.Id) int { return a.Cmp(b) })
	if err := validateProberShardAdmissionDeadlineInTx(ctx, tx, shardDeadline); err != nil {
		return nil, nil, err
	}
	if err := validateCompanionContractExpirationInTx(ctx, tx, companionContractId); err != nil {
		return nil, nil, err
	}
	pending := make(map[server.Id]netEscrowSnapshot, len(balanceIds))
	if 0 < contractTransferByteCount {
		for _, balanceId := range balanceIds {
			escrow := balanceEscrows[balanceId]
			snapshot := escrow.reservation
			snapshot.revision += 2
			snapshot.reserved += escrow.balanceByteCount
			pending[balanceId] = snapshot
		}
	}
	var expirationTime time.Time
	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for _, balanceId := range balanceIds {
			escrow := balanceEscrows[balanceId]
			batch.Queue(
				`
	                INSERT INTO transfer_escrow (
	                    contract_id,
	                    balance_id,
	                    balance_byte_count
	                )
	                VALUES ($1, $2, $3)
	            `,
				contractId,
				balanceId,
				escrow.balanceByteCount,
			)
		}

		batch.Queue(
			`
	            WITH creation_clock AS MATERIALIZED (
	                SELECT clock_timestamp() AT TIME ZONE 'UTC' AS create_time
	            )
	            INSERT INTO transfer_contract (
	                contract_id,
	                source_network_id,
	                source_id,
	                destination_network_id,
	                destination_id,
	                transfer_byte_count,
	                companion_contract_id,
	                payer_network_id,
	                usage_origin_is_source,
	                create_time,
	                priority,
	                expiration_time
	            )
	            SELECT
	                $1, $2, $3, $4, $5, $6, $7, $8, ($7::uuid IS NULL),
	                create_time, $9,
	                date_trunc('milliseconds', create_time) + $10 * INTERVAL '1 millisecond'
	            FROM creation_clock
	            RETURNING expiration_time
	        `,
			contractId,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferByteCount,
			companionContractId,
			payerNetworkId,
			priority,
			DefaultContractExpiration.Milliseconds(),
		).QueryRow(func(row pgx.Row) error { return row.Scan(&expirationTime) })

		batch.Queue(
			contractExtenderInsertSql,
			contractId,
			sourceId,
			destinationId,
			ContractPartySource,
			ContractPartyDestination,
		)
		if 0 < contractTransferByteCount {
			// The same commit publishes only the exact expected revision. A
			// legacy or concurrent writer leaves a miss rather than stale credit.
			batch.Queue(netEscrowPublishAdmissionCacheSQL, netEscrowAdmissionCacheArgs(pending, balanceIds)...)
		}
	})
	server.AddTxCommitCount(tx, &contractOpenedCounter, 1)
	providerWorkRetainReservationInTx(ctx, tx, contractId)
	contractHoleEventInTx(ctx, tx, contractId, sourceId, destinationId, "create", expirationTime)

	if 0 < contractTransferByteCount {
		// The escrow insert and subsequent open-contract insert each advance
		// this balance's revision once. Validate that exact committed state in
		// the post before reusing the census already performed under its lock.
		// RunPosts may run callbacks concurrently; only its synchronous caller
		// owns the joined post_commit timing span.
		postCtx := server.WithoutContractCreationTiming(ctx)
		posts = append(posts, func() any {
			publishCreatedNetEscrow(postCtx, contractId, pending, balanceIds)
			return nil
		})
	}

	balances := []*TransferEscrowBalance{}
	for balanceId, escrow := range balanceEscrows {
		balance := &TransferEscrowBalance{
			BalanceId:        balanceId,
			BalanceByteCount: escrow.balanceByteCount,
		}
		balances = append(balances, balance)
	}

	transferEscrow = &TransferEscrow{
		ContractId:          contractId,
		CompanionContractId: companionContractId,
		ExpirationTime:      expirationTime,
		TransferByteCount:   contractTransferByteCount,
		Priority:            priority,
		Balances:            balances,
	}

	return
}

type contractClientLifecycle struct {
	networkId server.Id
	active    bool
}

func lockActiveContractClientsInTx(
	ctx context.Context,
	tx server.PgTx,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
) error {
	defer server.EnterContractCreationStage(ctx, server.ContractStageClientFence)()
	providerWorkLockEndpointsInTx(ctx, tx, sourceId, destinationId)
	clientIds := []server.Id{sourceId}
	if destinationId != sourceId {
		clientIds = append(clientIds, destinationId)
	}
	slices.SortFunc(clientIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})

	// Contract creations may share these rows, while lifecycle UPDATE and
	// DELETE must wait. Stable order prevents opposite-direction contracts
	// from acquiring the same pair in opposite orders.
	clientLifecycles := map[server.Id]contractClientLifecycle{}
	result, err := tx.Query(
		ctx,
		`
			/* contract_lifecycle_write_boundary */
			SELECT client_id, network_id, active
			FROM network_client
			WHERE client_id = ANY($1)
			ORDER BY client_id
			FOR SHARE
		`,
		clientIds,
	)
	server.Raise(err)
	defer result.Close()
	for result.Next() {
		var clientId server.Id
		var lifecycle contractClientLifecycle
		server.Raise(result.Scan(&clientId, &lifecycle.networkId, &lifecycle.active))
		clientLifecycles[clientId] = lifecycle
	}
	server.Raise(result.Err())

	sourceLifecycle, sourceFound := clientLifecycles[sourceId]
	if !sourceFound || !sourceLifecycle.active || sourceLifecycle.networkId != sourceNetworkId {
		return ErrActiveClientNotFound
	}
	destinationLifecycle, destinationFound := clientLifecycles[destinationId]
	if !destinationFound || !destinationLifecycle.active || destinationLifecycle.networkId != destinationNetworkId {
		return ErrContractDestinationInactive
	}
	return nil
}

// renaming of `CreateTransferEscrow` since contract is the top level concept
func CreateContract(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
) (contractId server.Id, transferEscrow *TransferEscrow, returnErr error) {
	transferEscrow, returnErr = CreateTransferEscrow(
		ctx,
		sourceNetworkId,
		sourceId,
		destinationNetworkId,
		destinationId,
		contractTransferByteCount,
	)
	if transferEscrow != nil {
		contractId = transferEscrow.ContractId
	}
	return
}

func CreateTransferEscrow(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
) (transferEscrow *TransferEscrow, returnErr error) {
	return runRedisContractAdmission(ctx, func(ctx context.Context) (*TransferEscrow, error) {
		return createTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, contractTransferByteCount)
	})
}

// Shared transaction owner; public entry points always supply Redis admission.
// Raw contexts retain the legacy ledger path for compatibility and recovery controls.
func createTransferEscrow(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
) (transferEscrow *TransferEscrow, returnErr error) {
	var posts []func() any

	if err := transferEscrowTx(ctx, sourceNetworkId, contractTransferByteCount, func(tx server.PgTx) {
		transferEscrow, posts, returnErr = createTransferEscrowInTx(
			ctx,
			tx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			// source is payer
			sourceNetworkId,
			contractTransferByteCount,
			nil,
		)
	}); err != nil {
		return nil, err
	}

	if returnErr != nil {
		return
	}
	leavePosts := server.EnterContractCreationStage(ctx, server.ContractStagePostCommit)
	notifyCommittedContractOrigin(ctx, sourceId, destinationId)
	server.RunPosts(ctx, posts...)
	leavePosts()
	defer server.EnterContractCreationStage(ctx, server.ContractStageClientStamp)()
	// the source is the paying side: count its top-level identity in the
	// block users stat
	StampTopLevelClientContractTime(ctx, sourceId)
	return
}

// renaming of `CreateCompanionTransferEscrow` since contract is the top level concept
func CreateCompanionContract(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
	originContractTimeout time.Duration,
) (contractId server.Id, transferEscrow *TransferEscrow, returnErr error) {
	transferEscrow, returnErr = CreateCompanionTransferEscrow(
		ctx,
		sourceNetworkId,
		sourceId,
		destinationNetworkId,
		destinationId,
		contractTransferByteCount,
		originContractTimeout,
	)
	if transferEscrow != nil {
		contractId = transferEscrow.ContractId
	}
	return
}

// ErrMissingCompanionOrigin: a companion contract request arrived before any
// open origin contract in the opposite direction exists. At cold start this
// is an ORDERING RACE, not a terminal condition: both sides bring their
// sessions up simultaneously and the encryption control carrier requests its
// companion contract at session setup, frequently beating the peer's origin
// creation by milliseconds. The controller retries this case briefly (see
// nextContract) because the client cannot: every contract failure reaches the
// client collapsed into InsufficientBalance, and the client's blind
// CreateContractTimeout retry loop turned this race into a 30s sequence
// starve (observed 12 times per full test-suite run; also the mechanism that
// manufactured dead-on-arrival multiclient window clients).
var ErrMissingCompanionOrigin = fmt.Errorf("Missing origin contract for companion.")

// The internal prober owns its return-path reservation ramp as well as its
// outgoing ramp. A provider's defaults must not reserve a larger companion
// against that account than the probe's own origin ramp. The earliest origin
// remains the stream anchor, while a newer eligible origin may raise the
// probe's reservation size. Ordinary accounts retain asymmetric request sizes.
func CreateCompanionTransferEscrow(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
	originContractTimeout time.Duration,
) (transferEscrow *TransferEscrow, returnErr error) {
	return runRedisContractAdmission(ctx, func(ctx context.Context) (*TransferEscrow, error) {
		return createCompanionTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, contractTransferByteCount, originContractTimeout)
	})
}

// Shared transaction owner; public entry points always supply Redis admission.
// Raw contexts retain the legacy ledger path for compatibility and recovery controls.
func createCompanionTransferEscrow(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
	originContractTimeout time.Duration,
) (transferEscrow *TransferEscrow, returnErr error) {
	var posts []func() any
	payerNetworkId := destinationNetworkId
	requestedBytes := contractTransferByteCount
	var inheritedPayer *server.Id
	create := func(tx server.PgTx) {
		// A transaction retry or payer handoff must re-read the current origin,
		// without retaining a previous attempt's clamp, posts or outcome.
		transferEscrow, posts, returnErr, inheritedPayer = nil, nil, nil, nil
		contractTransferByteCount = requestedBytes
		// find the earliest open transfer contract in the opposite direction
		// with null companion_contract_id
		// there can be many companion contracts for an original contract

		result, err := tx.Query(
			ctx,
			`
                SELECT contract_id,
                    CASE WHEN EXISTS (SELECT 1 FROM prober_identity WHERE singleton AND network_id = $4
                        UNION ALL SELECT 1 FROM prober_shard_run WHERE network_id = $4)
                    THEN GREATEST(transfer_byte_count, (
                        SELECT max(transfer_byte_count)
                        FROM (
                            SELECT transfer_byte_count FROM transfer_contract
                            WHERE
                                (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
                                COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                source_id = $1 AND destination_id = $2 AND
                                companion_contract_id IS NULL
                            UNION ALL
                            SELECT transfer_byte_count FROM transfer_contract
                            WHERE open = false AND $3 <= close_time AND
                                COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                source_id = $1 AND destination_id = $2 AND
                                companion_contract_id IS NULL
                        ) AS eligible_probe_origins
                    )) END AS prober_reservation_byte_count
                FROM (
                    (
                        SELECT contract_id, create_time, transfer_byte_count
                        FROM transfer_contract
                        WHERE
							-- The CASE is equivalent to the generated open flag but
							-- opaque to legacy false-zero open/outcome indexes.
							(CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
                            COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                            source_id = $1 AND
                            destination_id = $2 AND
                            companion_contract_id IS NULL
                        ORDER BY create_time ASC
                        LIMIT 1
                    )

                    UNION ALL

                    (
                        SELECT contract_id, create_time, transfer_byte_count
                        FROM transfer_contract
                        WHERE
                            open = false AND
                            $3 <= close_time AND
                            COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                            source_id = $1 AND
                            destination_id = $2 AND
                            companion_contract_id IS NULL
                        ORDER BY create_time ASC
                        LIMIT 1
                    )

                    -- the two branches are disjoint on open, so the global
                    -- earliest is the earlier of each branch's earliest; each
                    -- inner query is a bounded index range (see
                    -- transfer_contract_open_source_id_companion_contract_id),
                    -- which keeps the planner off the create_time full scan
                    ORDER BY create_time ASC
                    LIMIT 1
                ) AS earliest_origin
            `,
			// note the origin direction is reversed
			destinationId,
			sourceId,
			server.NowUtc().Add(-originContractTimeout),
			destinationNetworkId,
		)
		var companionContractId *server.Id
		var proberReservationByteCount *ByteCount
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&companionContractId, &proberReservationByteCount))
			}
		})

		if companionContractId == nil {
			// Fall back to a companion contract as the origin anchor. In an
			// asymmetric relationship every return-direction contract is
			// itself a companion (the return side has no plain contract path
			// by definition), and the ONLY companion-on-companion requester
			// is the forward side's TLS-server EncryptedControl reply
			// carrier (EncryptionControlUseCompanion): its reply direction
			// mirrors the peer's companion-carried return direction. With
			// plain-origin-only matching that carrier can never open — a
			// deadlock that EncryptionModeRequired surfaces as a hard
			// establishment failure (Opportunistic silently downgraded the
			// peer's direction to plaintext instead, which is how it went
			// unnoticed). Ordinary destination-payer semantics stay unchanged.
			// A private shard reply inherits only its exact reverse anchor's
			// private payer; otherwise the shard fence still rejects it.
			// Plain origins stay preferred; the chain is bounded
			// in practice at depth two (a reply carrier answering a return
			// direction).
			result, err := tx.Query(
				ctx,
				`
                    SELECT contract_id,
                        CASE WHEN EXISTS (SELECT 1 FROM prober_identity WHERE singleton AND network_id = $4
                            UNION ALL SELECT 1 FROM prober_shard_run WHERE network_id = $4)
                        THEN GREATEST(transfer_byte_count, (
                            SELECT max(transfer_byte_count)
                            FROM (
                                SELECT transfer_byte_count, payer_network_id,
                                    source_network_id, destination_network_id
                                FROM transfer_contract
                                WHERE
                                    (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
                                    COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                    source_id = $1 AND destination_id = $2 AND
                                    companion_contract_id IS NOT NULL
                                UNION ALL
                                SELECT transfer_byte_count, payer_network_id,
                                    source_network_id, destination_network_id
                                FROM transfer_contract
                                WHERE open = false AND $3 <= close_time AND
                                    COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                    source_id = $1 AND destination_id = $2 AND
                                    companion_contract_id IS NOT NULL
                                -- Filter private ownership outside the pair boundary;
                                -- false-zero stats must not substitute a payer scan.
                                OFFSET 0
                            ) AS eligible_probe_companion_origins
                            WHERE ($4::uuid <> $5 OR (payer_network_id = $4 AND
                                source_network_id = $6 AND destination_network_id = $5))
                        )) END AS prober_reservation_byte_count,
                        CASE WHEN payer_network_id = $5 AND source_network_id = $6
                            AND destination_network_id = $5
                            AND EXISTS (SELECT 1 FROM prober_shard_run WHERE network_id = $5)
                        THEN $5::uuid END AS inherited_private_payer
                    FROM (
                        (
                            SELECT contract_id, create_time, transfer_byte_count,
                                payer_network_id, source_network_id, destination_network_id
                            FROM transfer_contract
                            WHERE
								-- Keep both generic open and outcome-null partial
								-- indexes ineligible for this pair lookup.
								(CASE WHEN outcome IS NULL THEN dispute = false ELSE false END) AND
                                COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                source_id = $1 AND
                                destination_id = $2 AND
                                companion_contract_id IS NOT NULL
                            ORDER BY create_time ASC
                            LIMIT 1
                        )

                        UNION ALL

                        (
                            SELECT contract_id, create_time, transfer_byte_count,
                                payer_network_id, source_network_id, destination_network_id
                            FROM transfer_contract
                            WHERE
                                open = false AND
                                $3 <= close_time AND
                                COALESCE(expiration_time, create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                                source_id = $1 AND
                                destination_id = $2 AND
                                companion_contract_id IS NOT NULL
                            ORDER BY create_time ASC
                            LIMIT 1
                        )

                        ORDER BY create_time ASC
                        LIMIT 1
                    ) AS earliest_companion_origin
                `,
				destinationId,
				sourceId,
				server.NowUtc().Add(-originContractTimeout),
				payerNetworkId,
				sourceNetworkId,
				destinationNetworkId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&companionContractId, &proberReservationByteCount, &inheritedPayer))
				}
			})
		}

		if companionContractId == nil {
			returnErr = ErrMissingCompanionOrigin
			return
		}

		if inheritedPayer != nil && payerNetworkId != *inheritedPayer {
			// End this read-only transaction before joining the true payer's
			// process-local queue. Never wait for another gate with a connection.
			return
		}
		if payerNetworkId != destinationNetworkId && inheritedPayer == nil {
			returnErr = errors.New("probe companion origin payer changed")
			return
		}

		if proberReservationByteCount != nil {
			contractTransferByteCount = min(contractTransferByteCount, *proberReservationByteCount)
		}

		transferEscrow, posts, returnErr = createTransferEscrowInTx(
			ctx,
			tx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			payerNetworkId,
			contractTransferByteCount,
			companionContractId,
		)
	}
	if err := transferEscrowTx(ctx, payerNetworkId, requestedBytes, create); err != nil {
		return nil, err
	}
	if inheritedPayer != nil && payerNetworkId != *inheritedPayer {
		payerNetworkId = *inheritedPayer
		if err := transferEscrowTx(ctx, payerNetworkId, requestedBytes, create); err != nil {
			return nil, err
		}
	}

	if returnErr != nil {
		return
	}
	leavePosts := server.EnterContractCreationStage(ctx, server.ContractStagePostCommit)
	notifyCommittedContractOrigin(ctx, sourceId, destinationId)
	server.RunPosts(ctx, posts...)
	leavePosts()
	defer server.EnterContractCreationStage(ctx, server.ContractStageClientStamp)()
	// Stamp the identity that actually funded this contract.
	if inheritedPayer != nil && payerNetworkId == sourceNetworkId {
		StampTopLevelClientContractTime(ctx, sourceId)
	} else {
		StampTopLevelClientContractTime(ctx, destinationId)
	}
	return
}

// contract_ids ordered by create time with:
// - at least `contractTransferByteCount` available
// - not closed by any party
// - with transfer escrow
func GetOpenTransferEscrowsOrderedByPriorityCreateTime(
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
) []*TransferEscrow {
	transferEscrows := []*TransferEscrow{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT

                    transfer_contract.contract_id,
                    transfer_contract.transfer_byte_count,
                    transfer_contract.priority

                FROM transfer_contract

                LEFT OUTER JOIN contract_close ON
                    contract_close.contract_id = transfer_contract.contract_id

                INNER JOIN transfer_escrow ON
                    transfer_escrow.contract_id = transfer_contract.contract_id

                WHERE
					-- This is equivalent to the generated-open expression but
					-- remains opaque to false-zero legacy partial indexes.
					(CASE WHEN transfer_contract.outcome IS NULL THEN transfer_contract.dispute = false ELSE false END) AND
                    COALESCE(transfer_contract.expiration_time, transfer_contract.create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
                    transfer_contract.source_id = $1 AND
                    transfer_contract.destination_id = $2 AND
                    transfer_contract.transfer_byte_count <= $3 AND
                    contract_close.contract_id IS NULL

                ORDER BY transfer_contract.priority DESC, transfer_contract.create_time ASC
            `,
			sourceId,
			destinationId,
			contractTransferByteCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId server.Id
				var transferByteCount ByteCount
				var priority Priority
				server.Raise(result.Scan(&contractId, &transferByteCount, &priority))
				transferEscrow := &TransferEscrow{
					ContractId:        contractId,
					Priority:          priority,
					TransferByteCount: transferByteCount,
				}
				transferEscrows = append(transferEscrows, transferEscrow)
			}
		})
	})

	return transferEscrows
}

func GetTransferEscrow(ctx context.Context, contractId server.Id) (transferEscrow *TransferEscrow) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    transfer_byte_count,
                    priority

                FROM transfer_byte_count
                WHERE
                    contract_id = $1
            `,
			contractId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				transferEscrow = &TransferEscrow{}
				server.Raise(result.Scan(
					&transferEscrow.TransferByteCount,
					&transferEscrow.Priority,
				))
			}
		})
		if transferEscrow == nil {
			// not found
			return
		}

		result, err = conn.Query(
			ctx,
			`
                SELECT
                    balance_id,
                    balance_byte_count

                FROM transfer_escrow
                WHERE
                    contract_id = $1
            `,
			contractId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				balance := &TransferEscrowBalance{}
				server.Raise(result.Scan(
					&balance.BalanceId,
					&balance.BalanceByteCount,
				))
				transferEscrow.Balances = append(transferEscrow.Balances, balance)
			}
		})
	})

	return
}

// some clients - platform, friends and family, etc - do not need an escrow
// typically `provide_mode < Public` does not use an escrow1
func CreateContractNoEscrow(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
) (contractId server.Id, returnErr error) {
	return CreateContractNoEscrowWithUsageOrigin(ctx, sourceNetworkId, sourceId,
		destinationNetworkId, destinationId, contractTransferByteCount, true)
}

// Preserves the service origin before a same-network companion is normalized
// onto the no-escrow transport path. The funding mode never determines usage.
func CreateContractNoEscrowWithUsageOrigin(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
	usageOriginIsSource bool,
) (contractId server.Id, returnErr error) {
	contractId, _, returnErr = CreateContractNoEscrowWithExpiration(ctx, sourceNetworkId, sourceId,
		destinationNetworkId, destinationId, contractTransferByteCount, usageOriginIsSource)
	return
}

// Returns the INSERT's immutable database deadline only after its transaction
// commits. The usage origin is independent of the no-escrow funding mode.
func CreateContractNoEscrowWithExpiration(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
	usageOriginIsSource bool,
) (contractId server.Id, expirationTime time.Time, returnErr error) {
	leaveTransaction := server.EnterContractCreationStage(ctx, server.ContractStageTransaction)
	server.Tx(ctx, func(tx server.PgTx) {
		contractId, expirationTime, returnErr = createContractNoEscrowInTx(
			ctx,
			tx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferByteCount,
			usageOriginIsSource,
		)
	}, server.TxReadCommitted)
	leaveTransaction()
	if returnErr != nil {
		return
	}
	// network / friends-and-family egress has no payer but is still
	// contract-creating usage: count the source's top-level identity in the
	// block users stat
	leavePosts := server.EnterContractCreationStage(ctx, server.ContractStagePostCommit)
	notifyCommittedContractOrigin(ctx, sourceId, destinationId)
	leavePosts()
	defer server.EnterContractCreationStage(ctx, server.ContractStageClientStamp)()
	StampTopLevelClientContractTime(ctx, sourceId)
	return
}

// Returns the attempt's INSERT result; the caller owns the commit boundary.
func createContractNoEscrowInTx(
	ctx context.Context,
	tx server.PgTx,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
	usageOriginIsSource bool,
) (contractId server.Id, expirationTime time.Time, returnErr error) {
	ctx = providerWorkSessionContext(ctx)
	// A shard-owned probe always pays from its private grant. Ordinary network
	// and friends-and-family contracts keep their existing no-payer behavior.
	if _, err := validateProberShardPayerInTx(ctx, tx, sourceNetworkId, destinationNetworkId, server.Id{}); err != nil {
		return server.Id{}, time.Time{}, err
	}
	if err := lockActiveContractClientsInTx(
		ctx,
		tx,
		sourceNetworkId,
		sourceId,
		destinationNetworkId,
		destinationId,
	); err != nil {
		return server.Id{}, time.Time{}, err
	}

	contractId = server.NewId()
	server.Raise(tx.QueryRow(
		ctx,
		`
	            WITH creation_clock AS MATERIALIZED (
	                SELECT clock_timestamp() AT TIME ZONE 'UTC' AS create_time
	            )
                INSERT INTO transfer_contract (
                    contract_id,
                    source_network_id,
                    source_id,
                    destination_network_id,
                    destination_id,
                    transfer_byte_count,
                    usage_origin_is_source,
                    create_time,
                    expiration_time
                )
	            SELECT
	                $1, $2, $3, $4, $5, $6, $7,
	                create_time, date_trunc('milliseconds', create_time) + $8 * INTERVAL '1 millisecond'
	            FROM creation_clock
	            RETURNING expiration_time
	        `,
		contractId,
		sourceNetworkId,
		sourceId,
		destinationNetworkId,
		destinationId,
		contractTransferByteCount,
		usageOriginIsSource,
		DefaultContractExpiration.Milliseconds(),
	).Scan(&expirationTime))
	server.RaisePgResult(tx.Exec(
		ctx,
		contractExtenderInsertSql,
		contractId,
		sourceId,
		destinationId,
		ContractPartySource,
		ContractPartyDestination,
	))
	server.AddTxCommitCount(tx, &contractOpenedCounter, 1)
	providerWorkRetainReservationInTx(ctx, tx, contractId)
	contractHoleEventInTx(ctx, tx, contractId, sourceId, destinationId, "create", expirationTime)
	return
}

// this will create a close entry,
// then settle if all parties agree, or set dispute if there is a dispute
func CloseContract(
	ctx context.Context,
	contractId server.Id,
	clientId server.Id,
	usedTransferByteCount ByteCount,
	checkpoint bool,
) error {
	_, err := closeContractReport(ctx, contractId, clientId, usedTransferByteCount, checkpoint, nil)
	return err
}

// CloseContractReport acknowledges an exact logical report once. A retry may
// resume settlement after the report commit without repeating its increment.
// The caller must preserve reportId across transport retries; equal-sized
// independent checkpoints have different identities.
func CloseContractReport(
	ctx context.Context,
	contractId, clientId server.Id,
	usedTransferByteCount ByteCount,
	checkpoint bool,
	reportId server.Id,
) (bool, error) {
	if reportId == (server.Id{}) {
		return false, fmt.Errorf("invalid close report identity")
	}
	return closeContractReport(ctx, contractId, clientId, usedTransferByteCount, checkpoint, &reportId)
}

func closeContractReport(
	ctx context.Context,
	contractId, clientId server.Id,
	usedTransferByteCount ByteCount,
	checkpoint bool,
	reportId *server.Id,
) (applied bool, returnErr error) {
	// settle := false
	// dispute := false
	if usedTransferByteCount < 0 {
		return false, fmt.Errorf("Invalid used transfer byte count: %d", usedTransferByteCount)
	}

	terminalReplay := false
	server.Tx(ctx, func(tx server.PgTx) {
		applied, terminalReplay, returnErr = applyContractCloseReportInTx(ctx, tx, contractId, clientId, usedTransferByteCount, checkpoint, reportId)
	}, server.TxReadCommitted, server.OptNoRetry())

	if terminalReplay {
		return
	}
	if returnErr != nil {
		return
	}

	closed, err := settleContract(ctx, contractId)
	if err != nil {
		returnErr = err
		return
	}
	if closed {
		RemoveFromStream(ctx, contractId)
	}
	return
}

// Receipt publication and the existing close increment share this transaction.
// Splitting the owner from settlement also permits exact crash/rollback controls.
func applyContractCloseReportInTx(ctx context.Context, tx server.PgTx,
	contractId, clientId server.Id, usedTransferByteCount ByteCount,
	checkpoint bool, reportId *server.Id,
) (applied, terminalReplay bool, returnErr error) {
	found := false
	var sourceId server.Id
	var destinationId server.Id
	var outcome *ContractOutcome
	var dispute bool
	var party ContractParty

	result, err := tx.Query(
		ctx,
		`
                SELECT
                    source_id,
                    destination_id,
                    outcome,
                    dispute
                FROM transfer_contract
                WHERE
                    contract_id = $1
                FOR UPDATE
            `,
		contractId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			found = true
			server.Raise(result.Scan(&sourceId, &destinationId, &outcome, &dispute))
			if clientId == sourceId {
				party = ContractPartySource
			} else if clientId == destinationId {
				party = ContractPartyDestination
			}
		}
	})

	if !found {
		returnErr = fmt.Errorf("Contract not found: %s", contractId.String())
		return
	}
	if party == "" {
		returnErr = fmt.Errorf("Client is not a party to the contract: %s %s %s->%s", contractId.String(), clientId.String(), sourceId.String(), destinationId.String())
		return
	}
	if reportId != nil {
		var priorBytes ByteCount
		var priorCheckpoint bool
		foundReport := false
		rows, err := tx.Query(ctx, `SELECT used_transfer_byte_count,checkpoint
 FROM contract_close_report WHERE contract_id=$1 AND party=$2 AND report_id=$3`, contractId, party, *reportId)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				server.Raise(rows.Scan(&priorBytes, &priorCheckpoint))
				foundReport = true
			}
		})
		if foundReport {
			if priorBytes != usedTransferByteCount || priorCheckpoint != checkpoint {
				returnErr = fmt.Errorf("close report identity payload conflicts")
			} else {
				terminalReplay = outcome != nil || dispute
				if !priorCheckpoint || terminalReplay {
					contractHoleEventInTx(ctx, tx, contractId, sourceId, destinationId, "remove")
				}
			}
			return
		}
	}
	if outcome != nil {
		if *outcome == ContractOutcomeSettled {
			returnErr = fmt.Errorf("%w: %s %s %s->%s", errContractAlreadySettled, contractId.String(), clientId.String(), sourceId.String(), destinationId.String())
		} else {
			returnErr = fmt.Errorf("Contract already closed with outcome %s: %s %s %s->%s", *outcome, contractId.String(), clientId.String(), sourceId.String(), destinationId.String())
		}
		return
	}
	if dispute {
		returnErr = fmt.Errorf("Contract in dispute: %s %s %s->%s", contractId.String(), clientId.String(), sourceId.String(), destinationId.String())
		return
	}

	if checkpoint {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
                    INSERT INTO contract_close (
                        contract_id,
                        party,
                        used_transfer_byte_count,
                        close_time,
                        checkpoint
                    )
                    VALUES ($1, $2, $3, $4, true)
                    ON CONFLICT (contract_id, party) DO UPDATE
                    SET
                        used_transfer_byte_count = contract_close.used_transfer_byte_count + $3,
                        close_time = $4
                    WHERE
                        contract_close.checkpoint = true
                `,
			contractId,
			party,
			usedTransferByteCount,
			server.NowUtc(),
		))
		applied = tag.RowsAffected() == 1

	} else {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
                    INSERT INTO contract_close (
                        contract_id,
                        party,
                        used_transfer_byte_count,
                        close_time,
                        checkpoint
                    )
                    VALUES ($1, $2, $3, $4, false)
                    ON CONFLICT (contract_id, party) DO UPDATE
                    SET
                        used_transfer_byte_count = contract_close.used_transfer_byte_count + $3,
                        close_time = $4,
                        checkpoint = false
                    WHERE
                        contract_close.checkpoint = true
                `,
			contractId,
			party,
			usedTransferByteCount,
			server.NowUtc(),
		))
		applied = tag.RowsAffected() == 1
	}
	if reportId != nil {
		// The existing contract row lock serializes this receipt with its
		// party increment. No shared payer/network row is acquired here.
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report
 (contract_id,party,report_id,used_transfer_byte_count,checkpoint) VALUES($1,$2,$3,$4,$5)`,
			contractId, party, *reportId, usedTransferByteCount, checkpoint))
	}
	// A checkpoint permits resuming the same contract. A final party close
	// revokes transport permission even when financial settlement is deferred.
	if !checkpoint {
		contractHoleEventInTx(ctx, tx, contractId, sourceId, destinationId, "remove")
	}
	return
}

func settleContract(ctx context.Context, contractId server.Id) (closed bool, returnErr error) {
	return settleContractWithExpiryScope(ctx, contractId, nil)
}

func settleContractWithExpiryScope(ctx context.Context, contractId server.Id, scope *contractExpiryRepairScope) (closed bool, returnErr error) {
	var posts []func() any
	var clockTransferByteCount ByteCount

	contractExpiryContinuationTx(ctx, contractId, scope, func(tx server.PgTx) {
		// party -> close record. Pull all close rows (checkpoint or not).
		// Inline settlement fires only when BOTH parties have done a
		// non-checkpoint close ("done"). A checkpoint means "pausing — the
		// sender may send again on this contract" (ReceiveSequence.Run defer),
		// so any one-sided checkpoint leaves the contract open and resumable
		// instead of settling on the hot request path. Genuinely-abandoned
		// checkpoint contracts (one side done + the other still checkpoint, or
		// both checkpoint) are finalized off the request path by the
		// CloseExpiredContracts task -> ForceCloseOpenContractIds, which converts
		// the checkpoint rows to non-checkpoint closes before settling once they
		// age past the expiry window.
		type closeRecord struct {
			usedTransferByteCount ByteCount
			checkpoint            bool
		}
		closes := map[ContractParty]closeRecord{}
		result, err := tx.Query(
			ctx,
			`
            SELECT
                party,
                used_transfer_byte_count,
                checkpoint
            FROM contract_close
            WHERE
                contract_id = $1
            `,
			contractId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var closeParty ContractParty
				var closeUsedTransferByteCount ByteCount
				var closeCheckpoint bool
				server.Raise(result.Scan(
					&closeParty,
					&closeUsedTransferByteCount,
					&closeCheckpoint,
				))
				closes[closeParty] = closeRecord{
					usedTransferByteCount: closeUsedTransferByteCount,
					checkpoint:            closeCheckpoint,
				}
			}
		})

		sourceClose, sourceOk := closes[ContractPartySource]
		destinationClose, destinationOk := closes[ContractPartyDestination]
		sourceUsedTransferByteCount := sourceClose.usedTransferByteCount
		destinationUsedTransferByteCount := destinationClose.usedTransferByteCount

		// Settle only when both parties have closed non-checkpoint. If either
		// side is still a checkpoint, leave the contract open to be resumed; the
		// background expiry task finalizes it if no final close ever arrives.
		if sourceOk && destinationOk && !sourceClose.checkpoint && !destinationClose.checkpoint {
			hasEscrow := false

			result, err := tx.Query(
				ctx,
				`
                    SELECT balance_id FROM transfer_escrow
                    WHERE contract_id = $1
                    LIMIT 1
                `,
				contractId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					hasEscrow = true
				}
			})

			if hasEscrow {
				diff := sourceUsedTransferByteCount - destinationUsedTransferByteCount
				if math.Abs(float64(diff)) <= AcceptableTransfersByteDifference {
					// fmt.Printf("CLOSE CONTRACT SETTLE (%s) %s\n", clientId.String(), contractId.String())
					posts, closed, returnErr = settleEscrowForegroundWithExpiryScopeInTx(ctx, tx, contractId, ContractOutcomeSettled, scope)
				} else {
					if scope == nil {
						glog.Infof("[sub]contract[%s]diff %d (%d <> %d)\n", contractId.String(), diff, sourceUsedTransferByteCount, destinationUsedTransferByteCount)
					}
					// fmt.Printf("CLOSE CONTRACT DISPUTE (%s) %s\n", clientId.String(), contractId.String())
					closed = setContractDisputeInTx(ctx, tx, contractId, true)
				}
			} else {
				// nothing to settle, just close the transaction
				closed, returnErr = claimContractOutcomeInTx(ctx, tx, contractId, ContractOutcomeSettled)
				if closed {
					clockTransferByteCount = destinationUsedTransferByteCount
				}
			}
		}
		if scope != nil {
			server.Raise(returnErr)
		}
	})

	if returnErr != nil {
		return
	}
	if closed && 0 < clockTransferByteCount {
		posts = append(posts, clockTransferPost(ctx, clockTransferByteCount))
	}
	server.RunPosts(ctx, posts...)
	return
}

func SettleEscrow(ctx context.Context, contractId server.Id, outcome ContractOutcome) (returnErr error) {
	var posts []func() any

	server.Tx(ctx, func(tx server.PgTx) {
		posts, _, returnErr = settleEscrowForegroundInTx(ctx, tx, contractId, outcome)
	}, server.TxReadCommitted, server.OptNoRetry())

	if returnErr != nil {
		return
	}
	server.RunPosts(ctx, posts...)
	return
}

func claimContractOutcomeInTx(
	ctx context.Context,
	tx server.PgTx,
	contractId server.Id,
	outcome ContractOutcome,
) (bool, error) {
	usage, err := contractUsageSnapshotInTx(ctx, tx, contractId, outcome)
	if err != nil {
		return false, err
	}
	return claimContractOutcomeWithUsageInTx(ctx, tx, contractId, outcome, usage)
}

// The usage came from this transaction's locked contract and original reports.
// Keep the guarded outcome write, signed provenance and commit event together.
func claimContractOutcomeWithUsageInTx(ctx context.Context, tx server.PgTx, contractId server.Id, outcome ContractOutcome, usage *contractUsageSnapshot) (bool, error) {
	ctx = providerWorkSessionContext(ctx)
	// Return the database clock from the outcome write, avoiding another round
	// trip while grants are held. The signed original uses this exact stored time.
	var closedAt time.Time
	var sourceId, destinationId server.Id
	err := tx.QueryRow(
		ctx,
		`
            UPDATE transfer_contract
            SET
                outcome = $2,
                close_time = clock_timestamp() AT TIME ZONE 'UTC',
                provider_usage = $3
            WHERE
                contract_id = $1 AND
                outcome IS NULL
            RETURNING close_time, source_id, destination_id
        `,
		contractId,
		outcome,
		usage,
	).Scan(&closedAt, &sourceId, &destinationId)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	server.AddTxCommitCount(tx, &contractClosedCounter, 1)
	providerWorkRetainOutcomeInTx(ctx, tx, contractId, outcome, closedAt)
	contractHoleEventInTx(ctx, tx, contractId, sourceId, destinationId, "remove")
	return true, nil
}

// contractParticipantsInTx returns the service side of a contract: the
// non-payer endpoint (the egress hop), the intermediary clients persisted for
// its stream, and the extenders the endpoints were connected through. The
// participant set deliberately still contains clients on the origin network;
// settlement uses the full set as the even-split denominator, then suppresses
// those ineligible shares.
func contractParticipantsInTx(
	ctx context.Context,
	tx server.PgTx,
	contractId server.Id,
) (
	participants []ContractParticipant,
	originNetworkId server.Id,
	returnErr error,
) {
	return contractParticipantsWithUsageOriginInTx(ctx, tx, contractId, nil)
}

// Uses an explicit retained service direction for subnet accounting while the
// billing caller keeps its existing payer/companion direction.
func contractParticipantsWithUsageOriginInTx(
	ctx context.Context,
	tx server.PgTx,
	contractId server.Id,
	usageOriginIsSource *bool,
) (participants []ContractParticipant, originNetworkId server.Id, returnErr error) {
	var sourceNetworkId server.Id
	var sourceId server.Id
	var destinationNetworkId server.Id
	var destinationId server.Id
	var payerNetworkId *server.Id
	var companionContractId *server.Id
	var streamId *server.Id
	found := false

	result, err := tx.Query(
		ctx,
		`
			SELECT
				source_network_id,
				source_id,
				destination_network_id,
				destination_id,
				payer_network_id,
				companion_contract_id,
				stream_id
			FROM transfer_contract
			WHERE contract_id = $1
		`,
		contractId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			found = true
			server.Raise(result.Scan(
				&sourceNetworkId,
				&sourceId,
				&destinationNetworkId,
				&destinationId,
				&payerNetworkId,
				&companionContractId,
				&streamId,
			))
		}
	})
	if !found {
		returnErr = fmt.Errorf("Contract not found while loading participants: %s", contractId.String())
		return
	}
	return contractParticipantsFromOwnerInTx(ctx, tx, contractId, contractParticipantOwner{
		sourceNetworkId: sourceNetworkId, sourceId: sourceId,
		destinationNetworkId: destinationNetworkId, destinationId: destinationId,
		payerNetworkId: payerNetworkId, companionContractId: companionContractId, streamId: streamId,
	}, usageOriginIsSource)
}

// Reuse only the locked contract's header. Shared stream membership is not
// protected by this contract's lock, so each caller still reads fresh rows.
func contractParticipantsFromOwnerInTx(ctx context.Context, tx server.PgTx, contractId server.Id, owner contractParticipantOwner, usageOriginIsSource *bool) ([]ContractParticipant, server.Id, error) {
	retained := []ContractParticipant{}
	read := func(query string) {
		rows, err := tx.Query(ctx, query, contractId)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var participant ContractParticipant
				server.Raise(rows.Scan(&participant.ClientId, &participant.NetworkId))
				retained = append(retained, participant)
			}
		})
	}
	if owner.streamId != nil {
		read(`SELECT contract_participant.client_id,contract_participant.network_id
            FROM transfer_contract INNER JOIN contract_participant ON
                contract_participant.stream_id=transfer_contract.stream_id
            WHERE transfer_contract.contract_id=$1`)
	}
	read(`SELECT client_id,network_id FROM contract_extender WHERE contract_id=$1`)
	return contractParticipantsFromRows(contractId, owner, usageOriginIsSource, retained)
}

// evenContractPayoutShare partitions a non-negative integer exactly. Stable
// participant ordering makes the at-most-one-unit remainder deterministic.
func evenContractPayoutShare(total int64, participantIndex int, participantCount int) int64 {
	if total <= 0 || participantCount <= 0 {
		return 0
	}
	share := total / int64(participantCount)
	if int64(participantIndex) < total%int64(participantCount) {
		share += 1
	}
	return share
}

func allocateContractParticipantPayouts(
	contractParticipants []ContractParticipant,
	originNetworkId server.Id,
	sweepPayouts map[server.Id]sweepPayout,
) (
	participantSweepPayouts map[participantSweepKey]*participantSweepPayout,
	accountPayouts map[server.Id]*contractPayout,
) {
	participantSweepPayouts = map[participantSweepKey]*participantSweepPayout{}
	accountPayouts = map[server.Id]*contractPayout{}
	balanceIds := make([]server.Id, 0, len(sweepPayouts))
	for balanceId := range sweepPayouts {
		balanceIds = append(balanceIds, balanceId)
	}
	slices.SortFunc(balanceIds, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})

	// Allocate the delta between cumulative even shares, rather than splitting
	// each funding balance from zero. Otherwise every small per-balance
	// remainder would go to the same first participant and the contract total
	// would not be even. UUID ordering makes the balance traversal stable while
	// the full (pre-eligibility) allocation for every balance still reconciles to
	// that balance's exact byte and revenue totals.
	var cumulativeByteCount ByteCount
	var cumulativePayout NanoCents
	for _, balanceId := range balanceIds {
		sweepPayout := sweepPayouts[balanceId]
		previousCumulativeByteCount := cumulativeByteCount
		previousCumulativePayout := cumulativePayout
		cumulativeByteCount += sweepPayout.payoutByteCount
		cumulativePayout += sweepPayout.payout
		for participantIndex, participant := range contractParticipants {
			participantByteCount := ByteCount(
				evenContractPayoutShare(
					int64(cumulativeByteCount),
					participantIndex,
					len(contractParticipants),
				) - evenContractPayoutShare(
					int64(previousCumulativeByteCount),
					participantIndex,
					len(contractParticipants),
				),
			)
			participantPayout := NanoCents(
				evenContractPayoutShare(
					int64(cumulativePayout),
					participantIndex,
					len(contractParticipants),
				) - evenContractPayoutShare(
					int64(previousCumulativePayout),
					participantIndex,
					len(contractParticipants),
				),
			)

			// A service hop on the payer/origin network represents
			// same-network traffic. Its precomputed even share is omitted, not
			// transferred to the remaining participants.
			if participant.NetworkId == originNetworkId ||
				(participantByteCount <= 0 && participantPayout <= 0) {
				continue
			}

			key := participantSweepKey{
				balanceId: balanceId,
				networkId: participant.NetworkId,
			}
			participantSweep := participantSweepPayouts[key]
			if participantSweep == nil {
				participantSweep = &participantSweepPayout{
					destinationId: participant.ClientId,
				}
				participantSweepPayouts[key] = participantSweep
			} else if participant.ClientId.Cmp(participantSweep.destinationId) < 0 {
				// Payout wallets are network-scoped and the sweep primary key is
				// per network. If two hops belong to one eligible network, combine
				// their payment shares and keep only a legacy representative here.
				// providerPayouts retains every client's actual subnet attribution.
				participantSweep.destinationId = participant.ClientId
			}
			participantSweep.payoutByteCount += participantByteCount
			participantSweep.payout += participantPayout
			participantSweep.providerPayouts = append(participantSweep.providerPayouts, contractProviderPayout{
				ClientId: participant.ClientId, PayoutByteCount: participantByteCount, PayoutNanoCents: participantPayout,
			})

			accountPayout := accountPayouts[participant.NetworkId]
			if accountPayout == nil {
				accountPayout = &contractPayout{}
				accountPayouts[participant.NetworkId] = accountPayout
			}
			accountPayout.payoutByteCount += participantByteCount
			accountPayout.payout += participantPayout
		}
	}
	return
}

// Keep both lookups parameterized: historical contract cardinality estimates
// can otherwise launch parallel workers for a single contract's small escrow.
// The offset fences prevent flattening without limiting rows or ordering ties.
const settlementEscrowReadSql = `
    SELECT
        selected_escrow.balance_id,
        selected_escrow.balance_byte_count,
        selected_balance.start_balance_byte_count,
        selected_balance.net_revenue_nano_cents
    FROM unnest(ARRAY[$1::uuid]) AS requested_contract(contract_id)
    CROSS JOIN LATERAL (
        SELECT balance_id, balance_byte_count
        FROM transfer_escrow
        WHERE contract_id = requested_contract.contract_id
        OFFSET 0
    ) AS selected_escrow
    CROSS JOIN LATERAL (
        SELECT start_balance_byte_count, net_revenue_nano_cents, end_time
        FROM transfer_balance
        WHERE balance_id = selected_escrow.balance_id
        OFFSET 0
    ) AS selected_balance
    ORDER BY selected_balance.end_time ASC
`

// Computes the floor mean of two non-negative reports without summing them.
// Valid reports can each reach the full signed storage limit.
func meanContractByteCount(first, second ByteCount) (ByteCount, error) {
	if first < 0 || second < 0 {
		return 0, fmt.Errorf("negative contract close byte count")
	}
	lower, upper := min(first, second), max(first, second)
	return lower + (upper-lower)/2, nil
}

// The exact legacy implementation remains the worker's financial authority.
// Foreground calls select the durable deferred path below instead.
func settleEscrowInTx(
	ctx context.Context,
	tx server.PgTx,
	contractId server.Id,
	outcome ContractOutcome,
) (posts []func() any, closed bool, returnErr error) {
	return settleEscrowWithOptionsInTx(ctx, tx, contractId, outcome, false, false)
}

func settleEscrowForegroundInTx(ctx context.Context, tx server.PgTx, contractId server.Id, outcome ContractOutcome) ([]func() any, bool, error) {
	return settleEscrowForegroundWithExpiryScopeInTx(ctx, tx, contractId, outcome, nil)
}

// Ordinary and scoped closes use the same durable debit owner. The caller's
// expiry transaction already enforces its scope before this continuation.
func settleEscrowForegroundWithExpiryScopeInTx(ctx context.Context, tx server.PgTx, contractId server.Id, outcome ContractOutcome, _ *contractExpiryRepairScope) ([]func() any, bool, error) {
	return settleEscrowWithOptionsInTx(ctx, tx, contractId, outcome, true, false)
}

// Current Redis contracts append independent consumption records. Legacy
// callers queue an intent without releasing their reservation; the worker uses
// the original atomic debit/outcome path and commits exact earnings plus durable total-projection ownership.
func settleEscrowWithOptionsInTx(ctx context.Context, tx server.PgTx, contractId server.Id, outcome ContractOutcome, deferLegacy, inlineFinancial bool) (posts []func() any, closed bool, returnErr error) {
	// CloseContract already owns this lock; direct and recovery settlement
	// must acquire it before balance locks to keep the same lock order.
	server.RaisePgResult(tx.Exec(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id = $1 FOR UPDATE`, contractId))
	// The immutable reservation mode is read under the owning contract lock.
	// Current contracts must never queue behind another contract's grant debit.
	var asyncDebit, hasEscrow bool
	server.Raise(tx.QueryRow(ctx, `SELECT COALESCE(bool_and(redis_reserved),false),count(*)>0
        FROM transfer_escrow WHERE contract_id=$1`, contractId).Scan(&asyncDebit, &hasEscrow))
	if deferLegacy && hasEscrow && !asyncDebit {
		return nil, false, queueLegacySettlementInTx(ctx, tx, contractId, outcome, false)
	}
	var result pgx.Rows
	var err error
	if asyncDebit {
		// Native close writes no shared grant. Its immutable provider owner
		// still needs queue admission before publishing the outcome and debt.
		admitted, ownershipErr := server.TryTxOwnership(ctx, tx,
			[]server.PgOwnershipKey{task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contractId))})
		if ownershipErr != nil {
			return nil, false, ownershipErr
		}
		if !admitted {
			return nil, false, errTransferBalanceOwnershipBusy
		}
		result, err = tx.Query(ctx, `SELECT balance_id FROM transfer_escrow WHERE contract_id=$1 ORDER BY balance_id`, contractId)
	} else {
		admitted, ownershipErr := tryLegacyFinancialOwnershipInTx(ctx, tx, []server.Id{contractId})
		if ownershipErr != nil {
			return nil, false, ownershipErr
		}
		if !admitted {
			return nil, false, errTransferBalanceOwnershipBusy
		}
		result, err = tx.Query(ctx, `
			SELECT transfer_balance.balance_id
			FROM transfer_balance INNER JOIN transfer_escrow USING (balance_id)
			WHERE transfer_escrow.contract_id=$1
			ORDER BY transfer_balance.balance_id FOR UPDATE OF transfer_balance
		`, contractId)
	}
	lockedBalanceIds := []server.Id{}
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId server.Id
			server.Raise(result.Scan(&balanceId))
			lockedBalanceIds = append(lockedBalanceIds, balanceId)
		}
	})
	var usedTransferByteCount ByteCount
	var clockTransferByteCount ByteCount
	settlementOwner, err := readContractSettlementOwnerInTx(ctx, tx, contractId)
	if err != nil {
		return nil, false, err
	}

	usedTransferByteCount, clockTransferByteCount, returnErr = contractSettlementReportAmounts(settlementOwner.reports, outcome)
	if returnErr != nil {
		return
	}

	contractParticipants, originNetworkId, err := contractParticipantsFromOwnerInTx(ctx, tx, contractId, settlementOwner.participants, nil)
	if err != nil {
		returnErr = err
		return
	}

	// Keep each positive reservation's exact tuple stable through the outcome
	// transition, including when a legacy writer does not take balance locks.
	positiveReservations, redisReservations := lockSettlementReservations(ctx, tx, contractId, lockedBalanceIds)

	// order balances by end date, ascending
	// take from the earlier before the later
	result, err = tx.Query(
		ctx,
		settlementEscrowReadSql,
		contractId,
	)

	escrows := []contractSettlementEscrow{}
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var escrow contractSettlementEscrow
			server.Raise(result.Scan(&escrow.balanceId, &escrow.amount, &escrow.start, &escrow.revenue))
			escrows = append(escrows, escrow)
		}
	})
	sweepPayouts, returnErr := contractSettlementSweepPayouts(usedTransferByteCount, escrows)
	if returnErr != nil {
		return
	}

	participantSweepPayouts, accountPayouts := allocateContractParticipantPayouts(
		contractParticipants,
		originNetworkId,
		sweepPayouts,
	)

	reservationSnapshots := readSettlementNetEscrowSnapshots(ctx, tx, settlementReservationIds(positiveReservations))
	usage, err := settlementOwner.usageSnapshotInTx(ctx, tx, contractId, outcome)
	if err != nil {
		return nil, false, err
	}
	closed, returnErr = claimContractOutcomeWithUsageInTx(ctx, tx, contractId, outcome, usage)
	if returnErr != nil || !closed {
		return
	}
	// The outcome claim runs once per contract, so the paying client's billable
	// bytes are metered once (network_client_data_cap_model.go): a redis
	// increment after the commit, for the client billing pays from. A crash
	// before the post under-counts; nothing re-meters a contract.
	if hasEscrow && 0 < usedTransferByteCount {
		if payerClientId, _, _, err := contractOrigin(contractId, settlementOwner.participants, nil); err == nil {
			meteredByteCount := usedTransferByteCount
			posts = append(posts, func() any {
				RecordClientDataUsage(ctx, payerClientId, meteredByteCount, server.NowUtc())
				return nil
			})
		}
	}
	if asyncDebit {
		// Journal insertion and outcome claim share a commit. No provider payout
		// row is used as a proxy for payer consumption or as the replay fence.
		for _, balanceId := range lockedBalanceIds {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal
                (contract_id,balance_id,debit_byte_count,shard) VALUES($1,$2,$3,$4)`,
				contractId, balanceId, sweepPayouts[balanceId].payoutByteCount, transferDebitShard(balanceId)))
		}
	} else {
		for _, balanceId := range lockedBalanceIds {
			if payout := sweepPayouts[balanceId].payoutByteCount; payout > 0 {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance
                    SET balance_byte_count=balance_byte_count-$2 WHERE balance_id=$1`, balanceId, payout))
			}
		}
	}
	publishSettlementNetEscrowSnapshots(ctx, tx, reservationSnapshots, positiveReservations, true)
	if 0 < clockTransferByteCount {
		posts = append(posts, legacySettlementClockPost(ctx, clockTransferByteCount))
	}

	// run all the posts in parallel in as small blocks as reasonable to minimize the work for serialization errors

	if 0 < len(sweepPayouts) {
		metadataPost := func() any {
			server.Tx(ctx, func(tx server.PgTx) {
				settleEscrowMetadataInTx(ctx, tx, contractId, server.NowUtc(), sweepPayouts)
			}, server.TxReadCommitted, server.OptNoRetry())
			return nil
		}
		if inlineFinancial && !asyncDebit {
			// Only the synchronous branch actually owns the grant rows. Reuse
			// its exact reservation locks and already-advanced snapshots when
			// they cover every metadata target; unusual sets retain fresh locks.
			if !settleEscrowOwnedMetadataInTx(ctx, tx, contractId, server.NowUtc(), sweepPayouts, positiveReservations, reservationSnapshots) {
				settleEscrowMetadataInTx(ctx, tx, contractId, server.NowUtc(), sweepPayouts)
			}
			metadataPost = func() any { return nil }
		}

		if 0 < len(participantSweepPayouts) {
			writePayouts := func(tx server.PgTx) {
				server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
					for key, payout := range participantSweepPayouts {
						batch.Queue(participantSweepInsertSQL, contractId, key.balanceId, key.networkId,
							payout.payoutByteCount, payout.payout, payout.destinationId, payout.providerPayouts)
					}
				})
			}
			if inlineFinancial || asyncDebit {
				// Required earnings commit with the outcome and debit authority.
				// The Redis debit worker cannot reconstruct a lost payout callback;
				// these per-contract rows do not lock a shared payer or provider.
				writePayouts(tx)
			} else {
				posts = append(posts, func() any {
					server.Tx(ctx, writePayouts, server.TxReadCommitted)
					return nil
				})
			}
		}

		mirrorBalanceIds := make([]server.Id, 0, len(sweepPayouts))
		for balanceId, sweepPayout := range sweepPayouts {
			if 0 < sweepPayout.escrowBalanceByteCount && positiveReservations[balanceId] > 0 {
				mirrorBalanceIds = append(mirrorBalanceIds, balanceId)
			}
		}
		if len(mirrorBalanceIds) > 0 {
			if inlineFinancial {
				// One durable owner per balance coalesces every close, including a
				// cache hit that can be invalidated before this post gets to run.
				queueLegacyNetEscrowMirrorsInTx(ctx, tx, mirrorBalanceIds)
			}
			mirrorPost := observeLegacySettlementPost(ctx, legacySettlementMirror, func() any {
				refreshNetEscrow(ctx, mirrorBalanceIds)
				return nil
			})
			if inlineFinancial {
				mirrorPost = legacySettlementMirrorPost(ctx, mirrorBalanceIds)
			}
			if len(reservationSnapshots) < len(positiveReservations) {
				// A cold mirror must follow this metadata attempt: otherwise it
				// can warm the preceding revision after metadata already read a
				// cache miss, only to be invalidated by its harmless settled flag.
				// Keep both in one independently replayable post, with no channel
				// dependency on another callback that could be lost. As with the
				// separate posts, report metadata errors and still try the mirror.
				posts = append(posts, func() any {
					server.HandleError(func() { metadataPost() })
					return mirrorPost()
				})
			} else {
				posts = append(posts, metadataPost, mirrorPost)
			}
		} else if !asyncDebit {
			posts = append(posts, metadataPost)
		}
	}

	// The durable debit worker alone updates marked escrow metadata and releases
	// its Redis reservation after the debit commit. A foreground metadata post
	// can race that same row; releasing earlier also duplicates its cleanup work.
	// Retain the original reservation until the worker confirms consumption.
	// Unusual mixed-mode reservations keep their existing reconciliation path.
	if len(redisReservations) > 0 && !asyncDebit {
		posts = append(posts, func() any {
			ReconcileRedisContractReservation(ctx, contractId)
			return nil
		})
	}

	if (inlineFinancial || asyncDebit) && len(accountPayouts) > 0 {
		// Exact earnings remain in the sweep ledger. Their lifetime display
		// totals have an independent per-contract durable owner, so a held
		// provider row cannot retain this transaction. No Redis increment is
		// added; account totals and the task's replay marker commit together.
		queueLegacyProviderTotalsInTx(ctx, tx, contractId, accountPayouts)
	} else if 0 < len(accountPayouts) {
		posts = append(posts, func() any {
			server.Redis(ctx, func(r server.RedisClient) {
				// Participant networks occupy independent Redis hash slots. Keep
				// each network's byte/revenue pair atomic in its own transaction.
				for networkId, payout := range accountPayouts {
					_, pipelineErr := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
						pipe.IncrBy(ctx, accountBalanceNetPayoutByteCountKey(networkId), payout.payoutByteCount)
						pipe.IncrBy(ctx, accountBalanceNetPayout(networkId), payout.payout)
						return nil
					})
					server.Raise(pipelineErr)
				}
			})
			return nil
		})
	}

	return
}

type sweepPayout struct {
	escrowBalanceByteCount ByteCount
	payoutByteCount        ByteCount
	returnByteCount        ByteCount
	payout                 NanoCents
}

type participantSweepKey struct {
	balanceId server.Id
	networkId server.Id
}

type participantSweepPayout struct {
	destinationId   server.Id
	payoutByteCount ByteCount
	payout          NanoCents
	providerPayouts []contractProviderPayout
}

// The account sweep remains network-scoped; this immutable allocation keeps
// each provider's byte/revenue share for subnet eligibility and attribution.
type contractProviderPayout struct {
	ClientId        server.Id `json:"client_id"`
	PayoutByteCount ByteCount `json:"payout_byte_count"`
	PayoutNanoCents NanoCents `json:"payout_nano_cents"`
}

type contractPayout struct {
	payoutByteCount ByteCount
	payout          NanoCents
}

// `server.ComplexValue`
func (self *sweepPayout) Values() []any {
	return []any{
		self.payoutByteCount,
		self.returnByteCount,
		self.payout,
	}
}

func SetContractDispute(ctx context.Context, contractId server.Id, dispute bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		setContractDisputeInTx(ctx, tx, contractId, dispute)
	})
}

func setContractDisputeInTx(
	ctx context.Context,
	tx server.PgTx,
	contractId server.Id,
	dispute bool,
) bool {
	var sourceId, destinationId server.Id
	err := tx.QueryRow(
		ctx,
		`
            UPDATE transfer_contract
            SET
                dispute = $2,
                close_time = $3
            WHERE
                contract_id = $1 AND
                outcome IS NULL
            RETURNING source_id, destination_id
        `,
		contractId,
		dispute,
		server.NowUtc(),
	).Scan(&sourceId, &destinationId)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	server.Raise(err)
	operation := "remove"
	if !dispute {
		// Reopening requires the complete close predicate; let background source
		// reconciliation restore it rather than guessing from one updated flag.
		operation = "invalidate"
	}
	contractHoleEventInTx(ctx, tx, contractId, sourceId, destinationId, operation)
	return true
}

func GetOpenContractIdsWithNoPartialClose(
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
) map[server.Id]bool {
	contractIds := map[server.Id]bool{}
	for contractId, parties := range GetOpenContractIds(ctx, sourceId, destinationId) {
		if len(parties) == 0 {
			contractIds[contractId] = true
		}
	}
	return contractIds
}

func GetOpenContractIdsWithPartialClose(
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
) map[server.Id]ContractParty {
	contractIdPartialCloseParties := map[server.Id]ContractParty{}
	for contractId, parties := range GetOpenContractIds(ctx, sourceId, destinationId) {
		switch len(parties) {
		case 1:
			contractIdPartialCloseParties[contractId] = parties[0]
		case 2:
			// Both sides have a close row. If exactly one is
			// `ContractPartyCheckpoint` (one side done, the other only paused via
			// `CheckpointContract`), surface it under the non-checkpoint party so
			// callers (e.g. test cleanup) finalize it like a 1-party partial close.
			// Otherwise the cleanup loop misses it and its escrow stays deducted.
			var nonCheckpoint ContractParty
			checkpointCount := 0
			for _, p := range parties {
				if p == ContractPartyCheckpoint {
					checkpointCount += 1
				} else {
					nonCheckpoint = p
				}
			}
			if checkpointCount == 1 {
				contractIdPartialCloseParties[contractId] = nonCheckpoint
			}
		}
	}
	return contractIdPartialCloseParties
}

// contract id -> partially closed contract party, or "" if none
func GetOpenContractIds(
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
) map[server.Id][]ContractParty {
	contractIdPartialCloseParties := map[server.Id][]ContractParty{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    transfer_contract.contract_id,
                    contract_close.party,
                    contract_close.checkpoint
                FROM transfer_contract

                LEFT JOIN contract_close ON contract_close.contract_id = transfer_contract.contract_id

                WHERE
					-- Use the opaque equivalent predicate so unrelated
					-- false-zero open/outcome indexes are ineligible.
					(CASE WHEN transfer_contract.outcome IS NULL THEN transfer_contract.dispute = false ELSE false END) AND
                    transfer_contract.source_id = $1 AND
                    transfer_contract.destination_id = $2
            `,
			sourceId,
			destinationId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId server.Id
				var party_ *ContractParty
				var checkpoint_ *bool
				server.Raise(result.Scan(&contractId, &party_, &checkpoint_))
				var party ContractParty
				if party_ != nil {
					party = *party_
				}
				var checkpoint bool
				if checkpoint_ != nil {
					checkpoint = *checkpoint_
				}
				// there can be up to two rows per contractId (one checkpoint)
				// non-checkpoint takes precedence
				// if checkpoint {
				// 	if contractIdPartialCloseParties[contractId] == "" {
				// 		contractIdPartialCloseParties[contractId] = ContractPartyCheckpoint
				// 	}
				// } else {
				// 	contractIdPartialCloseParties[contractId] = party
				// }
				if checkpoint {
					contractIdPartialCloseParties[contractId] = append(contractIdPartialCloseParties[contractId], ContractPartyCheckpoint)
				} else if party != "" {
					contractIdPartialCloseParties[contractId] = append(contractIdPartialCloseParties[contractId], party)
				} else if _, ok := contractIdPartialCloseParties[contractId]; !ok {
					contractIdPartialCloseParties[contractId] = []ContractParty{}
				}
			}
		})
	})

	return contractIdPartialCloseParties
}

// expired contracts are open:
// - 2 closes - one non-checkpoint party and one checkpoint
// TODO - 0 closes can be used if the contract has a max lived time
// TODO   add this to the protocol
// TODO there may be some overlap with https://github.com/bringyour/bringyour/commit/4a8150083083161be04737f0cc4b087906d9b449
func GetExpiredOpenContractIds(
	ctx context.Context,
	contractCloseTimeout time.Duration,
) map[server.Id]bool {
	contractIdPartialCloseParties := map[server.Id]map[ContractParty]bool{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    transfer_contract.contract_id,
                    contract_close.party,
                    contract_close.checkpoint
                FROM transfer_contract

                INNER JOIN contract_close ON
                    contract_close.contract_id = transfer_contract.contract_id AND
                    contract_close.close_time < $1

                WHERE
                    transfer_contract.open = true
            `,
			time.Now().Add(-contractCloseTimeout),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var contractId server.Id
				var party_ *ContractParty
				var checkpoint_ *bool
				server.Raise(result.Scan(&contractId, &party_, &checkpoint_))
				var party ContractParty
				if party_ != nil {
					party = *party_
				}
				var checkpoint bool
				if checkpoint_ != nil {
					checkpoint = *checkpoint_
				}
				if checkpoint {
					party = ContractPartyCheckpoint
				}
				partialCloseParties, ok := contractIdPartialCloseParties[contractId]
				if !ok {
					partialCloseParties = map[ContractParty]bool{}
					contractIdPartialCloseParties[contractId] = partialCloseParties
				}
				partialCloseParties[party] = true
			}
		})
	})

	contractIdCloses := map[server.Id]bool{}
	for contractId, partialCloseParties := range contractIdPartialCloseParties {
		hasSource := partialCloseParties[ContractPartySource]
		hasDestination := partialCloseParties[ContractPartyDestination]
		hasCheckpoint := partialCloseParties[ContractPartyCheckpoint]
		if (hasSource || hasDestination) && hasCheckpoint {
			contractIdCloses[contractId] = true
		}
	}

	return contractIdCloses
}

/*
func GetOpenContractIdsForSourceOrDestinationWithNoPartialClose(
    ctx context.Context,
    clientId server.Id,
) map[TransferPair]map[server.Id]bool {
    pairContractIdPartialCloseParties := GetOpenContractIdsForSourceOrDestination(ctx, clientId)
    pairContractIds := map[TransferPair]map[server.Id]bool{}
    for transferPair, contractIdPartialCloseParties := range pairContractIdPartialCloseParties {
        for contractId, party := range contractIdPartialCloseParties {
            if party == "" {
                contractIds, ok := pairContractIds[transferPair]
                if !ok {
                    contractIds = map[server.Id]bool{}
                    pairContractIds[transferPair] = contractIds
                }
                contractIds[contractId] = true
            }
        }
    }
    return pairContractIds
}
*/

// return key is unordered transfer pair
func GetOpenContractIdsForSourceOrDestination(
	ctx context.Context,
	clientId server.Id,
) map[TransferPair]map[server.Id]ContractParty {
	pairContractIdPartialCloseParties := map[TransferPair]map[server.Id]ContractParty{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    transfer_contract.source_id,
                    transfer_contract.destination_id,
                    transfer_contract.contract_id,
                    contract_close.party,
                    contract_close.checkpoint
                FROM transfer_contract

                LEFT JOIN contract_close ON
                    contract_close.contract_id = transfer_contract.contract_id

                WHERE
					-- Both endpoint arms have isolated, symmetric partial
					-- indexes over this opaque equivalent predicate.
					(CASE WHEN transfer_contract.outcome IS NULL THEN transfer_contract.dispute = false ELSE false END) AND (
                        transfer_contract.source_id = $1 OR
                        transfer_contract.destination_id = $1
                    )
            `,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var sourceId server.Id
				var destinationId server.Id
				var contractId server.Id
				var party_ *ContractParty
				var checkpoint_ *bool
				server.Raise(result.Scan(
					&sourceId,
					&destinationId,
					&contractId,
					&party_,
					&checkpoint_,
				))
				transferPair := NewUnorderedTransferPair(sourceId, destinationId)
				contractIdPartialCloseParties, ok := pairContractIdPartialCloseParties[transferPair]
				var party ContractParty
				if party_ != nil {
					party = *party_
				}
				if !ok {
					contractIdPartialCloseParties = map[server.Id]ContractParty{}
					pairContractIdPartialCloseParties[transferPair] = contractIdPartialCloseParties
				}
				var checkpoint bool
				if checkpoint_ != nil {
					checkpoint = *checkpoint_
				}
				// non-checkpoint takes precedence over checkpoint
				if checkpoint {
					if contractIdPartialCloseParties[contractId] == "" {
						contractIdPartialCloseParties[contractId] = ContractPartyCheckpoint
					}
				} else if party != "" {
					contractIdPartialCloseParties[contractId] = party
				} else if _, ok2 := contractIdPartialCloseParties[contractId]; !ok2 {
					contractIdPartialCloseParties[contractId] = ""
				}
			}
		})
	})

	return pairContractIdPartialCloseParties
}

func ForceCloseAllOpenContractIds(ctx context.Context, minTime time.Time) error {
	var cursor *ContractExpiryCursor
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("Done.")
		default:
		}
		_, next, err := ForceCloseOpenContractIdsPage(ctx, minTime, 1000, 1, 0, 0, cursor)
		if err != nil {
			return err
		}
		if next == nil {
			return nil
		}
		cursor = next
	}
}

// forceCloseContractCounter partitions force-closed expired contracts by the
// resolution taken. The volume is driven by clients that leave contracts open
// — a per-contract log line ran ~87k/day, dwarfing every other log source —
// so the disposition is counted and the per-contract detail (which carries
// the contract id) stays at V(1).
var forceCloseContractCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "contract",
		Name:      "force_closed_total",
		Help:      "Expired contracts force closed by the sweep, partitioned by resolution",
	},
	[]string{"resolution"},
)

func init() {
	prometheus.MustRegister(forceCloseContractCounter)
}

// recordForceCloseContract counts one force-close resolution and emits the
// per-contract detail at V(1). `tag` carries the contract id and batch index.
func recordForceCloseContract(resolution string, tag string) {
	forceCloseContractCounter.WithLabelValues(resolution).Inc()
	if glog.V(1) {
		glog.Infof("%sforce close contract: %s\n", tag, resolution)
	}
}

// Closes contracts whose creation and latest report are at or before minTime.
// A retained expiry proof resumes immediately even after synthetic reports.
// cases handled:
// - no closes
// - single close
// - one or more checkpoints
// - dispute (settled with both sides accepted)
// ContractExpiryPosition is an ordered continuation through a bounded raw
// selection page, including contracts already owned by legacy settlement.
type ContractExpiryPosition struct {
	CreateTime time.Time `json:"create_time"`
	ContractId server.Id `json:"contract_id"`
}

type ContractExpiryCursor struct {
	ScanBefore  time.Time               `json:"scan_before"`
	Open        *ContractExpiryPosition `json:"open,omitempty"`
	Dispute     *ContractExpiryPosition `json:"dispute,omitempty"`
	OpenDone    bool                    `json:"open_done,omitempty"`
	DisputeDone bool                    `json:"dispute_done,omitempty"`
}

// Single-page compatibility boundary. Scheduled expiry persists the returned
// cursor from ForceCloseOpenContractIdsPage instead of restarting at the head.
func ForceCloseOpenContractIds(ctx context.Context, minTime time.Time, maxCount, parallel, blockSize, blockIndex int) (int64, error) {
	count, _, err := ForceCloseOpenContractIdsPage(ctx, minTime, maxCount, parallel, blockSize, blockIndex, nil)
	return count, err
}

func ForceCloseOpenContractIdsPage(ctx context.Context, minTime time.Time, maxCount, parallel, blockSize, blockIndex int,
	after *ContractExpiryCursor,
) (closeCount int64, next *ContractExpiryCursor, err error) {
	if parallel <= 0 {
		return 0, nil, fmt.Errorf("force close parallelism must be positive: %d", parallel)
	}

	if maxCount <= 0 {
		return 0, nil, fmt.Errorf("force close page size must be positive: %d", maxCount)
	}
	next = &ContractExpiryCursor{}
	if after != nil {
		*next = *after
	}
	if next.ScanBefore.IsZero() {
		next.ScanBefore = server.NowUtc()
	}

	type OpenContract = contractExpiryState

	openContracts := []*OpenContract{}
	openContractIndexes := map[server.Id]int{}
	// cooperatively partition contracts across the block tasks
	appendBlockOpenContract := func(openContract *OpenContract) {
		if 0 < blockSize && int(openContract.contractId.Hash()%uint64(blockSize)) != blockIndex%blockSize {
			return
		}
		if index, ok := openContractIndexes[openContract.contractId]; ok {
			// The open and dispute scans are separate. A contract can enter
			// dispute between them; retain only the newer disputed snapshot so
			// two workers never race to finalize the same contract.
			openContracts[index] = openContract
			return
		}
		openContractIndexes[openContract.contractId] = len(openContracts)
		openContracts = append(openContracts, openContract)
	}

	// LIMIT bounds raw candidates before pending-intent and quiet-period
	// checks. The cursor advances through skipped rows too; a retained old
	// financial cohort cannot occupy every future selection. The existing
	// row-level proof and intent race guard still own every mutation.
	rawOpen, rawDisputed := 0, 0
	if !next.OpenDone {
		position := ContractExpiryPosition{}
		if next.Open != nil {
			position = *next.Open
		}
		seen := 0
		server.Db(ctx, func(conn server.PgConn) {
			rows, queryErr := conn.Query(ctx, forceCloseOpenContractPageSql, ContractPartySource, ContractPartyDestination, minTime.UTC(), maxCount, position.CreateTime, position.ContractId, next.ScanBefore)
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					c := &OpenContract{}
					var created time.Time
					var eligible bool
					server.Raise(rows.Scan(&c.contractId, &c.sourceId, &c.destinationId, &c.dispute,
						&c.sourceCloseTime, &c.sourceUsedTransferByteCount, &c.sourceCheckpoint,
						&c.destinationCloseTime, &c.destinationUsedTransferByteCount, &c.destinationCheckpoint, &created, &eligible))
					seen++
					next.Open = &ContractExpiryPosition{CreateTime: created, ContractId: c.contractId}
					if eligible {
						appendBlockOpenContract(c)
					}
				}
			})
		})
		rawOpen = seen
		if seen < maxCount {
			next.OpenDone = true
		}
	}
	openContractCount := len(openContracts)
	if !next.DisputeDone {
		position := ContractExpiryPosition{}
		if next.Dispute != nil {
			position = *next.Dispute
		}
		seen := 0
		server.Db(ctx, func(conn server.PgConn) {
			rows, queryErr := conn.Query(ctx, forceCloseDisputedContractPageSql, minTime.UTC(), maxCount, position.CreateTime, position.ContractId, next.ScanBefore)
			server.WithPgResult(rows, queryErr, func() {
				for rows.Next() {
					c := &OpenContract{dispute: true}
					var created time.Time
					var eligible bool
					server.Raise(rows.Scan(&c.contractId, &c.sourceId, &c.destinationId, &created, &eligible))
					seen++
					next.Dispute = &ContractExpiryPosition{CreateTime: created, ContractId: c.contractId}
					if eligible {
						appendBlockOpenContract(c)
					}
				}
			})
		})
		rawDisputed = seen
		if seen < maxCount {
			next.DisputeDone = true
		}
	}
	if next.OpenDone && next.DisputeDone {
		next = nil
	}

	glog.Infof("[sm]found %d contracts to close (%d disputes)\n", len(openContracts), len(openContracts)-openContractCount)

	// quarantine a contract that cannot be settled by marking it settled
	// without settling the escrow.
	// `outcome IS NULL` so that a concurrent close/settle is not overwritten.
	// `dispute = false` so that a contract that entered dispute mid-close is
	// left for the dispute scan to settle correctly on a later pass.
	closeMalformedContract := func(tag string, openContract *OpenContract, err error) bool {
		glog.Infof("%sforce close malformed contract: %s\n", tag, err)

		claimed := false
		server.Tx(ctx, func(tx server.PgTx) {
			// Billing failure cannot revoke independently retained delivered
			// usage. Validate it under the outcome lock before quarantining;
			// a missing or corrupt original proof grants no new credit.
			usage, usageErr := contractUsageSnapshotInTx(ctx, tx, openContract.contractId, ContractOutcomeSettled)
			server.Raise(usageErr)
			// Quarantine changes no debit, but the terminal transition advances
			// every retained legacy reservation revision. Admit that complete
			// scope after private contract custody and before the outcome write.
			admitted, ownershipErr := tryContractTransferBalanceOwnershipInTx(ctx, tx, []server.Id{openContract.contractId})
			server.Raise(ownershipErr)
			if !admitted {
				server.Raise(errTransferBalanceOwnershipBusy)
			}
			commandTag := server.RaisePgResult(tx.Exec(
				ctx,
				`
                    UPDATE transfer_contract
                    SET
                        outcome = $2,
                        close_time = $3,
                        usage_unverified = true,
                        provider_usage = $4
                    WHERE
                        contract_id = $1 AND
                        outcome IS NULL AND
                        dispute = false
                `,
				openContract.contractId,
				ContractOutcomeSettled,
				server.NowUtc(),
				usage,
			))
			claimed = commandTag.RowsAffected() == 1
			if claimed {
				server.AddTxCommitCount(tx, &contractClosedCounter, 1)
				contractHoleEventInTx(ctx, tx, openContract.contractId, openContract.sourceId, openContract.destinationId, "remove")
			}
		}, server.TxReadCommitted, server.OptNoRetry())

		// the quarantine settles the contract with no payout, so release its
		// reservation back to the payer's available balance instead of leaking
		// it into the net escrow counter. only when this call claimed the
		// contract -- otherwise a concurrent settle/dispute owns the release.
		if claimed {
			server.RunPosts(
				ctx,
				clockTransferPost(ctx, clockContractTransferByteCount(ctx, openContract.contractId)),
			)
			releaseNetEscrowForContract(ctx, openContract.contractId)
		}
		return claimed
	}

	runForceClose := func(do func() error) (runErr error) {
		var callErr error
		recovered := server.HandleError(func() {
			callErr = do()
		})
		if recovered == nil {
			return callErr
		}
		switch value := recovered.(type) {
		case error:
			return value
		default:
			return fmt.Errorf("%v", value)
		}
	}

	removeFinalizedContractFromStream := func(tag string, openContract *OpenContract, allowDisputeSettlement bool) error {
		found := false
		finalized := false
		disputed := false
		pending := false
		readState := func() {
			found = false
			finalized = false
			disputed = false
			pending = false
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
                        SELECT outcome IS NOT NULL, dispute,
                            EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
                        FROM transfer_contract
                        WHERE contract_id = $1
                    `,
					openContract.contractId,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						found = true
						server.Raise(result.Scan(&finalized, &disputed, &pending))
					}
				})
			})
		}
		readState()
		if found && !finalized && pending {
			return errLegacySettlementPending
		}
		if allowDisputeSettlement && found && !finalized && disputed {
			// A successful close can create a dispute after both selection scans.
			// Resolve once, then re-read; failed closes never enter this path.
			settleErr := runForceClose(func() error {
				settleExpiredContractDispute(ctx, tag, openContract.contractId, nil)
				return nil
			})
			if settleErr != nil {
				return finishForceCloseDisputeSettlement(ctx, settleErr, func() error {
					return runForceClose(func() error {
						readState()
						if !found {
							return errors.New("contract disappeared before force-close verification")
						}
						if !finalized {
							return &forceCloseNonfinalError{disputed: disputed}
						}
						return nil
					})
				})
			}
			readState()
			if found && !finalized && pending {
				return errLegacySettlementPending
			}
		}
		if !found {
			return fmt.Errorf("contract disappeared before force-close verification")
		}
		if !finalized {
			return &forceCloseNonfinalError{disputed: disputed}
		}
		RemoveFromStream(ctx, openContract.contractId)
		return nil
	}

	nextIndex := 0
	var nextIndexLock sync.Mutex
	getAndIncrNextIndex := func() int {
		nextIndexLock.Lock()
		defer nextIndexLock.Unlock()

		i := nextIndex
		nextIndex += 1
		return i
	}

	attempted := make([]bool, len(openContracts))
	contractErrors := make([]error, len(openContracts))
	contractCompleted := make([]bool, len(openContracts))
	eligibilitySkipped := make([]bool, len(openContracts))
	deferredSettlements := make([]bool, len(openContracts))
	accountingRejections := make([]bool, len(openContracts))
	quarantinedAccountingRejections := make([]bool, len(openContracts))
	workerErrors := make(chan error, parallel)
	var wg sync.WaitGroup

	for range parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recovered := server.HandleError(func() {
				for j := getAndIncrNextIndex(); j < len(openContracts); j = getAndIncrNextIndex() {
					select {
					case <-ctx.Done():
						return
					default:
					}

					openContract := openContracts[j]
					tag := fmt.Sprintf("[sm][%s][%d/%d]", openContract.contractId, j+1, len(openContracts))
					var fresh *OpenContract
					prepareErr := runForceClose(func() error {
						var err error
						server.Tx(ctx, func(tx server.PgTx) {
							fresh = nil
							var pending bool
							server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)`, openContract.contractId).Scan(&pending))
							if pending {
								return
							}
							fresh, err = prepareContractExpiryInTx(ctx, tx, openContract.contractId, minTime)
							server.Raise(err)
						}, server.TxReadCommitted)
						return err
					})
					if prepareErr != nil && !errors.Is(prepareErr, errContractAlreadySettled) {
						// A failed proof read/write is not authority to quarantine.
						contractErrors[j] = prepareErr
						continue
					}
					if fresh == nil && prepareErr == nil {
						// A fresh report or pending intent withdrew this candidate.
						// Its eligibility check completed without a financial close;
						// the proper owner or a later quiet pass retains retirement.
						eligibilitySkipped[j] = true
						continue
					}
					attempted[j] = true
					closeErr := prepareErr
					if fresh != nil {
						openContract = fresh
						closeErr = runForceClose(func() error {
							return continueContractExpiry(forceCloseContinuationContext(ctx, openContract.contractId), tag, openContract, nil)
						})
					}
					var quarantineErr, cleanupErr error
					var quarantineClaimed bool
					contractErrors[j] = finishForceCloseContract(
						closeErr,
						func() error {
							quarantineErr = runForceClose(func() error {
								quarantineClaimed = closeMalformedContract(tag, openContract, closeErr)
								return nil
							})
							return quarantineErr
						},
						func() error {
							cleanupErr = runForceClose(func() error {
								return removeFinalizedContractFromStream(tag, openContract, closeErr == nil)
							})
							return cleanupErr
						},
					)
					accountingRejections[j] = isForceCloseAccountingRejection(closeErr, quarantineErr, cleanupErr)
					quarantinedAccountingRejections[j] = isForceCloseQuarantinedAccountingRejection(closeErr, quarantineClaimed, quarantineErr, cleanupErr)
					deferredSettlements[j] = isForceCloseDeferredSettlement(closeErr, quarantineErr, cleanupErr)
					contractCompleted[j] = true
				}
			})
			if recovered != nil {
				switch value := recovered.(type) {
				case error:
					workerErrors <- value
				default:
					workerErrors <- fmt.Errorf("%v", value)
				}
			}
		}()
	}

	wg.Wait()
	close(workerErrors)

	for index, closed := range attempted {
		if closed && !deferredSettlements[index] {
			closeCount++
		}
	}
	accountingOnly := true
	var verifiedCloseCount, accountingRejectionCount, quarantinedAccountingRejectionCount int64
	for index, contractErr := range contractErrors {
		if eligibilitySkipped[index] {
			// A current eligibility rejection is a completed scan visit, not
			// a close or a reason to discard other rows' classified errors.
			continue
		}
		if deferredSettlements[index] {
			// A durable intent is acknowledged work, not a verified close or an
			// accounting success. Its separate worker owns progress and errors.
			forceCloseContractCounter.WithLabelValues("deferred").Inc()
			continue
		}
		if !contractCompleted[index] {
			accountingOnly = false
		} else if contractErr == nil {
			verifiedCloseCount++
		} else if quarantinedAccountingRejections[index] {
			verifiedCloseCount++
			quarantinedAccountingRejectionCount++
		} else if accountingRejections[index] {
			accountingRejectionCount++
		} else {
			accountingOnly = false
		}
		if contractErr != nil {
			err = errors.Join(err, fmt.Errorf("force close contract %s at index %d: %w", openContracts[index].contractId, index, contractErr))
		}
	}
	for workerErr := range workerErrors {
		accountingOnly = false
		err = errors.Join(err, fmt.Errorf("force close worker: %w", workerErr))
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		accountingOnly = false
		err = errors.Join(err, ctxErr)
	}
	if accountingOnly && 0 < accountingRejectionCount+quarantinedAccountingRejectionCount {
		err = &ForceCloseAccountingError{
			cause:                               err,
			verifiedCloseCount:                  verifiedCloseCount,
			accountingRejectionCount:            accountingRejectionCount,
			quarantinedAccountingRejectionCount: quarantinedAccountingRejectionCount,
		}
	}
	glog.Infof("[close-expired]page returned success=%t raw_open=%d raw_disputed=%d selected=%d terminal_verified=%d unresolved_accounting=%d quarantined_accounting=%d has_more=%t\n",
		err == nil, rawOpen, rawDisputed, len(openContracts), verifiedCloseCount, accountingRejectionCount, quarantinedAccountingRejectionCount, next != nil)

	return
}

type ContractClose struct {
	CloseTime time.Time
	Dispute   bool
	Outcome   string
}

func GetContractClose(ctx context.Context, contractId server.Id) (contractClose *ContractClose, closed bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    close_time,
                    dispute,
                    outcome
                FROM transfer_contract
                WHERE
                    contract_id = $1
            `,
			contractId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var closeTime *time.Time
				var dispute bool
				var outcome *string
				server.Raise(result.Scan(&closeTime, &dispute, &outcome))
				if outcome != nil {
					closed = true
					contractClose = &ContractClose{
						CloseTime: *closeTime,
						Dispute:   dispute,
						Outcome:   *outcome,
					}
				}
			}
		})
	})

	return
}

// update 2026-01-30: the net payout byte count and net payout are now tracked in redis
// FIXME this should be merged back into the database at regular checkpoint intervals

func accountBalanceNetPayoutByteCountKey(networkId server.Id) string {
	return fmt.Sprintf("{account_balance_%s}npbc", networkId)
}

func accountBalanceNetPayout(networkId server.Id) string {
	return fmt.Sprintf("{account_balance_%s}np", networkId)
}

type AccountBalance struct {
	NetworkId          server.Id
	ProvidedByteCount  ByteCount
	ProvidedNetRevenue NanoCents
	PaidByteCount      ByteCount
	PaidNetRevenue     NanoCents
}

type GetAccountBalanceResult struct {
	Balance *AccountBalance
	Error   *GetAccountBalanceError
}

type GetAccountBalanceError struct {
	Message string
}

func GetAccountBalance(session *session.ClientSession) *GetAccountBalanceResult {
	balance := &AccountBalance{
		NetworkId: session.ByJwt.NetworkId,
	}
	server.Db(session.Ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			session.Ctx,
			`
            SELECT
	            provided_byte_count,
                provided_net_revenue_nano_cents,
                paid_byte_count,
                paid_net_revenue_nano_cents
            FROM account_balance
            WHERE
                network_id = $1
            `,
			session.ByJwt.NetworkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&balance.ProvidedByteCount,
					&balance.ProvidedNetRevenue,
					&balance.PaidByteCount,
					&balance.PaidNetRevenue,
				))
			}
			// else empty balance
		})
	})
	server.Redis(session.Ctx, func(r server.RedisClient) {
		var providedNetByteCountCmd *redis.StringCmd
		var providedNetPayoutCmd *redis.StringCmd
		r.Pipelined(session.Ctx, func(pipe redis.Pipeliner) error {
			providedNetByteCountCmd = pipe.Get(session.Ctx, accountBalanceNetPayoutByteCountKey(session.ByJwt.NetworkId))
			providedNetPayoutCmd = pipe.Get(session.Ctx, accountBalanceNetPayout(session.ByJwt.NetworkId))
			return nil
		})
		providedNetByteCount, _ := providedNetByteCountCmd.Int64()
		balance.ProvidedByteCount += providedNetByteCount
		providedNetRevenue, _ := providedNetPayoutCmd.Int64()
		balance.ProvidedNetRevenue += providedNetRevenue
	})
	return &GetAccountBalanceResult{
		Balance: balance,
	}
}

type SubscriptionCreatePaymentIdArgs struct {
}

type SubscriptionCreatePaymentIdResult struct {
	SubscriptionPaymentId server.Id                         `json:"subscription_payment_id,omitempty"`
	Error                 *SubscriptionCreatePaymentIdError `json:"error,omitempty"`
}

type SubscriptionCreatePaymentIdError struct {
	Message string `json:"message"`
}

func SubscriptionCreatePaymentId(createPaymentId *SubscriptionCreatePaymentIdArgs, clientSession *session.ClientSession) (createPaymentIdResult *SubscriptionCreatePaymentIdResult, returnErr error) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			clientSession.Ctx,
			`
            SELECT
                COUNT(subscription_payment_id) AS subscription_payment_id_count
            FROM subscription_payment
            WHERE
                network_id = $1 AND
                $2 <= create_time
            `,
			clientSession.ByJwt.NetworkId,
			server.NowUtc().Add(-1*time.Hour),
		)

		limitExceeded := false

		server.WithPgResult(result, err, func() {
			if result.Next() {
				var count int
				server.Raise(result.Scan(&count))
				if MaxSubscriptionPaymentIdsPerHour <= count {
					limitExceeded = true
				}
			}
		})

		if limitExceeded {
			createPaymentIdResult = &SubscriptionCreatePaymentIdResult{
				Error: &SubscriptionCreatePaymentIdError{
					Message: "Too many subscription payments in the last hour. Try again later.",
				},
			}
			return
		}

		subscriptionPaymentId := server.NewId()

		// a failed insert raises: the payment id must not be handed out
		// unless its row commits
		server.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
            INSERT INTO subscription_payment (
                subscription_payment_id,
                network_id,
                user_id
            ) VALUES ($1, $2, $3)
            `,
			subscriptionPaymentId,
			clientSession.ByJwt.NetworkId,
			clientSession.ByJwt.UserId,
		))

		createPaymentIdResult = &SubscriptionCreatePaymentIdResult{
			SubscriptionPaymentId: subscriptionPaymentId,
		}
	})

	return
}

func SubscriptionGetNetworkIdForPaymentId(ctx context.Context, subscriptionPaymentId server.Id) (networkId server.Id, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
            SELECT network_id FROM subscription_payment
            WHERE subscription_payment_id = $1
            `,
			subscriptionPaymentId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&networkId))
			} else {
				returnErr = errors.New("Invalid subscription payment.")
			}
		})
	})
	return
}

type SubscriptionType = string

const SubscriptionTypeSupporter = "supporter"

type SubscriptionMarket = string

const SubscriptionMarketApple = "apple"
const SubscriptionMarketGoogle = "google"
const SubscriptionMarketStripe = "stripe"
const SubscriptionMarketSolana = "solana"
const SubscriptionMarketManual = "manual"

// an agent paid inline over x402 (HTTP 402), settled through the Stripe facilitator.
// See controller/x402_controller.go.
const SubscriptionMarketX402 = "x402"

type SubscriptionRenewal struct {
	NetworkId          server.Id
	SubscriptionType   SubscriptionType
	StartTime          time.Time
	EndTime            time.Time
	NetRevenue         NanoCents
	PurchaseToken      string
	SubscriptionMarket SubscriptionMarket // google or apple
	TransactionId      string             // for tracking on Google Play or Apple App Store
}

var ErrPaymentNetworkNotFound = errors.New("payment network does not exist")

// LockPaymentNetworkInTx establishes the deletion/credit ordering boundary for
// every paid entitlement and data writer. A network delete takes FOR UPDATE on
// the same row. At ReadCommitted, whichever operation gets the row first wins:
// a credit that wins is visible to the delete's later active-renewal check,
// while a credit waiting behind a committed delete observes no row and stops
// before consuming an idempotency ledger or payment intent.
func LockPaymentNetworkInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
) error {
	var lockedNetworkId server.Id
	err := tx.QueryRow(
		ctx,
		`
			/* payment-network-credit-lock */
			SELECT network_id
			FROM network
			WHERE network_id = $1
			FOR KEY SHARE
		`,
		networkId,
	).Scan(&lockedNetworkId)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPaymentNetworkNotFound
	}
	// any other failure aborts the caller's transaction: it raises, so no
	// credit path goes on to commit a rollback
	server.Raise(err)
	return nil
}

// LockPlaySubscriptionPurchaseInTx serializes a Play purchase's credit and end
// paths. Both take the network lifecycle lock first and this token advisory
// lock second, so a terminal poll that follows an in-flight ACTIVE response
// sees and ends the committed credit rather than racing past it.
func LockPlaySubscriptionPurchaseInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	purchaseToken string,
) error {
	if purchaseToken == "" {
		return errors.New("Play purchase token is empty")
	}
	if err := LockPaymentNetworkInTx(tx, ctx, networkId); err != nil {
		return err
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		purchaseToken,
	))
	return nil
}

func AddSubscriptionRenewalInTx(tx server.PgTx, ctx context.Context, renewal *SubscriptionRenewal) (returnErr error) {
	if err := LockPaymentNetworkInTx(tx, ctx, renewal.NetworkId); err != nil {
		return err
	}

	_, err := tx.Exec(
		ctx,
		`
			INSERT INTO subscription_renewal (
				network_id,
		        subscription_type,
		        start_time,
		        end_time,
		        net_revenue_nano_cents,
		        purchase_token,
						market,
						transaction_id
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (network_id, subscription_type, end_time, start_time, market) DO UPDATE
			SET
				net_revenue_nano_cents = $5,
				purchase_token = $6
		`,
		renewal.NetworkId,
		renewal.SubscriptionType,
		renewal.StartTime,
		renewal.EndTime,
		renewal.NetRevenue,
		renewal.PurchaseToken,
		renewal.SubscriptionMarket,
		renewal.TransactionId,
	)
	// a failed insert aborts the caller's transaction, so it raises rather
	// than leave the credit path to commit a rollback
	server.Raise(err)
	return
}

func AddSubscriptionRenewal(ctx context.Context, renewal *SubscriptionRenewal) (returnErr error) {

	server.Tx(ctx, func(tx server.PgTx) {

		returnErr = AddSubscriptionRenewalInTx(tx, ctx, renewal)

	}, server.TxReadCommitted)

	return
}

func HasSubscriptionRenewal(
	ctx context.Context,
	networkId server.Id,
	subscriptionType SubscriptionType,
) (bool, *string) {
	active := false
	var market *string
	server.Db(ctx, func(conn server.PgConn) {
		active, market = hasSubscriptionRenewal(ctx, conn, networkId, subscriptionType)
	})
	return active, market
}

// HasSubscriptionRenewal read in the caller's tx. It sees the renewals the tx itself
// wrote and nothing committed outside its snapshot, and it does not acquire a second
// pool connection while the tx holds one.
func HasSubscriptionRenewalInTx(
	tx server.PgTx,
	ctx context.Context,
	networkId server.Id,
	subscriptionType SubscriptionType,
) (bool, *string) {
	return hasSubscriptionRenewal(ctx, tx, networkId, subscriptionType)
}

// The renewal read on query, a pooled connection or the caller's tx: whether a
// renewal of subscriptionType is active now, and the market of one of them.
func hasSubscriptionRenewal(
	ctx context.Context,
	query server.PgCanQuery,
	networkId server.Id,
	subscriptionType SubscriptionType,
) (active bool, market *string) {
	result, err := query.Query(
		ctx,
		`
		SELECT
			MIN(market) AS market,
			COUNT(*) AS subscription_renewal_count
		FROM subscription_renewal
		WHERE
			network_id = $1
			AND subscription_type = $2
			AND start_time <= $3
			AND $3 < end_time;
		`,
		networkId,
		subscriptionType,
		server.NowUtc(),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var count int
			server.Raise(result.Scan(
				&market,
				&count,
			))
			active = (0 < count)
		}
	})
	return
}

// GetActiveSubscriptionRenewalMarkets returns every market that is currently
// billing the network for subscriptionType, one entry per market.
//
// A network can hold concurrent renewals in more than one market -- the same
// person subscribing on an iPhone and again on the web is billed twice, by two
// unrelated payment systems, each of which must be cancelled where it lives.
// HasSubscriptionRenewal collapses that set with MIN(market) and can only ever
// name one of them, which leaves the other silently charging; use this when the
// caller has to show or act on all of them.
//
// Several sequential renewal rows in one market are one subscription to cancel,
// so the set is deduped by market. Market is nullable (it predates the column)
// and older rows also wrote the empty string, so both are normalized to "" and
// share a single "unknown store" entry. Ordered for a stable result, with the
// unknown entry first.
func GetActiveSubscriptionRenewalMarkets(
	ctx context.Context,
	networkId server.Id,
	subscriptionType SubscriptionType,
) []SubscriptionMarket {
	markets := []SubscriptionMarket{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT DISTINCT
				COALESCE(market, '') AS market
			FROM subscription_renewal
			WHERE
				network_id = $1
				AND subscription_type = $2
				AND start_time <= $3
				AND $3 < end_time
			ORDER BY market
			`,
			networkId,
			subscriptionType,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var market SubscriptionMarket
				server.Raise(result.Scan(&market))
				markets = append(markets, market)
			}
		})
	})
	return markets
}

// IsPro reports whether a network currently holds the Pro entitlement.
//
// This is a thin wrapper for the many existing callers that hold a *server.Id;
// pro_model.go is where Pro is actually tracked (an in-window transfer_balance
// with pro = true, cached per network in redis). Do not reimplement this check --
// in particular, "has any paid balance" is NOT Pro, because data codes create paid
// balances and are data-only.
func IsPro(
	ctx context.Context,
	networkId *server.Id,
) bool {
	if networkId == nil {
		return false
	}
	return IsProNetwork(ctx, *networkId)
}

// IsProFresh is IsPro read from the source of truth (see IsProNetworkFresh) — use it when
// the value is stamped into a durable ByJwt, so a stale cache entry can't freeze a wrong
// Pro into a 30-day token.
func IsProFresh(
	ctx context.Context,
	networkId *server.Id,
) bool {
	if networkId == nil {
		return false
	}
	return IsProNetworkFresh(ctx, *networkId)
}

// AddProTransferBalanceToAllNetworks grants the Pro data allowance to every network
// with an active supporter subscription, for the window [startTime, endTime).
//
// The balance carries pro = true, so THIS GRANT is what makes a network Pro for the
// period (see pro_model.go). It also carries the subscription's revenue pro-rated to
// the grant window, which drives provider subsidy accounting: a yearly subscription
// contributes roughly 1/12 of its revenue to each monthly grant.
//
// Eligibility comes from subscription_renewal, NOT from the pro column -- otherwise
// the grant would renew its own entitlement forever and a lapsed subscription would
// never drop to free.
//
// The Pro cache is refreshed for every granted network so the upgrade is visible
// immediately instead of after ProCacheTtl.
func AddProTransferBalanceToAllNetworks(
	ctx context.Context,
	startTime time.Time,
	endTime time.Time,
	balanceByteCount ByteCount,
) (addedTransferBalances map[server.Id]ByteCount) {
	addedTransferBalances = map[server.Id]ByteCount{}

	server.Tx(ctx, func(tx server.PgTx) {
		// network_id -> subscription revenue pro-rated to this grant window
		supporters := map[server.Id]NanoCents{}

		result, err := tx.Query(
			ctx,
			`
				SELECT
					network.network_id,
					subscription_renewal.net_revenue_nano_cents,
					subscription_renewal.start_time,
					subscription_renewal.end_time
				FROM network

				INNER JOIN subscription_renewal ON
					subscription_renewal.network_id = network.network_id AND
					subscription_renewal.subscription_type = $1 AND
					subscription_renewal.start_time <= $2 AND
					$2 < subscription_renewal.end_time
			`,
			SubscriptionTypeSupporter,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				var netRevenueNanoCents NanoCents
				var supporterStartTime time.Time
				var supporterEndTime time.Time
				server.Raise(result.Scan(
					&networkId,
					&netRevenueNanoCents,
					&supporterStartTime,
					&supporterEndTime,
				))

				subsidyNetRevenue := NanoCents(0)
				if supporterDuration := supporterEndTime.Sub(supporterStartTime); 0 < supporterDuration {
					fraction := float64(endTime.Sub(startTime)) / float64(supporterDuration)
					subsidyNetRevenue = NanoCents(fraction * float64(netRevenueNanoCents))
				}
				// SUM, do not overwrite: a network can hold several active renewals
				// at once (one row per market), and each contributes its own
				// pro-rated revenue to the subsidy accounting
				supporters[networkId] += subsidyNetRevenue
			}
		})

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for networkId, subsidyNetRevenue := range supporters {
				batch.Queue(
					`
		                INSERT INTO transfer_balance (
		                    balance_id,
		                    network_id,
		                    start_time,
		                    end_time,
		                    start_balance_byte_count,
		                    net_revenue_nano_cents,
		                    subsidy_net_revenue_nano_cents,
		                    balance_byte_count,
		                    pro,
		                    grant_kind
		                )
		                VALUES ($1, $2, $3, $4, $5, $6, $7, $5, true, $8)
		            `,
					server.NewId(),
					networkId,
					startTime,
					endTime,
					balanceByteCount,
					NanoCents(0),
					subsidyNetRevenue,
					GrantKindPro,
				)
				addedTransferBalances[networkId] = balanceByteCount
			}
		})
	})

	networkIds := make([]server.Id, 0, len(addedTransferBalances))
	for networkId := range addedTransferBalances {
		networkIds = append(networkIds, networkId)
	}
	UpdateProNetworks(ctx, networkIds...)

	return
}

// AddFreeTransferBalanceToAllNetworks grants the free-tier data allowance to every
// network WITHOUT an active supporter subscription, for the window
// [startTime, endTime). The balance is unpaid and carries pro = false, so the free
// grant can never confer Pro.
func AddFreeTransferBalanceToAllNetworks(
	ctx context.Context,
	startTime time.Time,
	endTime time.Time,
	balanceByteCount ByteCount,
) (addedTransferBalances map[server.Id]ByteCount) {
	addedTransferBalances = map[server.Id]ByteCount{}

	// Seeker/Saga holders get their free daily data scaled (pro.yml seeker.data_multiplier).
	seekers := GetAllSeekerHolders(ctx)
	seekerMultiplier := Pro().SeekerDataMultiplier()

	server.Tx(ctx, func(tx server.PgTx) {
		networkIds := []server.Id{}

		result, err := tx.Query(
			ctx,
			`
				SELECT
					network.network_id
				FROM network

				LEFT JOIN subscription_renewal ON
					subscription_renewal.network_id = network.network_id AND
					subscription_renewal.subscription_type = $1 AND
					subscription_renewal.start_time <= $2 AND
					$2 < subscription_renewal.end_time

				WHERE subscription_renewal.network_id IS NULL
			`,
			SubscriptionTypeSupporter,
			server.NowUtc(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIds = append(networkIds, networkId)
			}
		})

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, networkId := range networkIds {
				byteCount := balanceByteCount
				if seekerMultiplier != 1.0 && seekers[networkId] {
					byteCount = ByteCount(float64(balanceByteCount) * seekerMultiplier)
				}
				batch.Queue(
					`
		                INSERT INTO transfer_balance (
		                    balance_id,
		                    network_id,
		                    start_time,
		                    end_time,
		                    start_balance_byte_count,
		                    net_revenue_nano_cents,
		                    subsidy_net_revenue_nano_cents,
		                    balance_byte_count,
		                    pro,
		                    grant_kind
		                )
		                VALUES ($1, $2, $3, $4, $5, $6, $7, $5, false, $8)
		            `,
					server.NewId(),
					networkId,
					startTime,
					endTime,
					byteCount,
					NanoCents(0),
					NanoCents(0),
					GrantKindFree,
				)
				addedTransferBalances[networkId] = byteCount
			}
		})
	})

	return
}

func RemoveCompletedContracts(ctx context.Context, minTime time.Time) {
	maxRowCount := 50000

	removeCompletedTransferBalanceBatches(ctx, minTime)

	// The reaper is driven by the indexed reap_time column. reap_time is the
	// instant a contract becomes due for hard deletion:
	//   - CompletePayment queues bounded retention work; the completed-payment
	//     pass stamps complete_time + CompletedContractExpiration,
	//   - the straggler pass stamps now() on aged closed contracts that are not
	//     owned by an active or otherwise ambiguous payment.
	// The delete pass then removes every contract whose reap_time has passed,
	// cascading contract_close/transfer_escrow/transfer_escrow_sweep for those same
	// contract ids in one statement so dependents never linger as orphans. Every
	// pass is index-driven, row-bounded, and committed one batch at a time, so a
	// backlog is worked down without one long lock or transaction.
	//
	// This replaces three prior reaper blocks: the sweep-driven completed reaper,
	// the sweep-less reaper, and the straggler reaper. The last two ran a
	// non-selective, un-indexable anti-join over ~the whole old-closed contract
	// table (open = false is nearly every old contract; the sweep / completed-
	// payment anti-join could not be indexed on transfer_contract) with a LIMIT that
	// never early-terminates -- so every run walked the world and tanked the DB
	// (prod incident 2026-07-14). SweepOrphanContractData is the low-cadence safety
	// net for orphans left by any other path (e.g. crashes mid-statement in older
	// releases).

	// CompletePayment only records the payment and queues this work. Advance each
	// queued payment through its sweeps in keyset batches, committing the cursor
	// after every batch so a timeout or worker restart resumes instead of replaying
	// one enormous update.
	assignCompletedContractReapTimeBatches(ctx, maxRowCount)

	// assign pass: give aged closed-but-never-completed contracts a reap_time so
	// the delete pass removes them. Bounded by the
	// transfer_contract_reap_pending_create_time partial index (reap_time IS NULL
	// AND close_time IS NOT NULL, ordered by create_time), so this is an index
	// range-scan, not the anti-join it replaces.
	assignStragglerReapTimeBatches(ctx, server.NowUtc().Add(-StragglerContractExpiration), maxRowCount)

	// delete pass: hard delete every contract whose reap_time is due, cascading
	// its dependent rows. Bounded by the transfer_contract_reap_time partial index.
	// This reaps both completed contracts (reap_time = complete_time +
	// CompletedContractExpiration) and the stragglers just assigned above.
	// Candidate contract_ids are distinct (from the transfer_contract primary
	// key). removeDueContractBatches drains until an empty batch; protected
	// candidates are repaired back to reap_time = NULL instead of deleted.
	reapTime := server.NowUtc()
	removeDueContractBatches(
		ctx,
		reapTime,
		reapTime.Add(-StragglerContractExpiration),
		maxRowCount,
	)
}

// assignCompletedContractReapTimeBatches drains the durable retention queue set
// by CompletePayment. One payment is advanced by at most maxRowCount distinct
// contract ids per transaction. The UUID cursor is committed with the contract
// updates, making the work resumable without ever delaying payment completion.
func assignCompletedContractReapTimeBatches(ctx context.Context, maxRowCount int) (assignedCount int64) {
	budgetEnd := server.NowUtc().Add(reaperRunBudget)
	for {
		var batchCount int64
		var stampedCount int64
		processedPayment := false
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			result, err := tx.Query(
				ctx,
				`
				WITH payment AS MATERIALIZED (
					SELECT
						account_payment.payment_id,
						account_payment.complete_time,
						account_payment.contract_retention_cursor
					FROM account_payment
					WHERE
						account_payment.contract_retention_pending AND
						account_payment.completed AND
						account_payment.complete_time IS NOT NULL
					ORDER BY account_payment.complete_time, account_payment.payment_id
					LIMIT 1
					FOR UPDATE SKIP LOCKED
				), batch AS MATERIALIZED (
					SELECT DISTINCT transfer_escrow_sweep.contract_id
					FROM payment
					INNER JOIN transfer_escrow_sweep ON
						transfer_escrow_sweep.payment_id = payment.payment_id
					WHERE
						payment.contract_retention_cursor IS NULL OR
						payment.contract_retention_cursor < transfer_escrow_sweep.contract_id
					ORDER BY transfer_escrow_sweep.contract_id
					LIMIT $1
				), stamped AS (
					UPDATE transfer_contract
					SET reap_time = GREATEST(
						COALESCE(transfer_contract.reap_time, '-infinity'::timestamp),
						payment.complete_time + interval '7 days'
					)
					FROM payment, batch
					WHERE
						transfer_contract.contract_id = batch.contract_id AND
						(
							transfer_contract.reap_time IS NULL OR
							transfer_contract.reap_time < payment.complete_time + interval '7 days'
						)
					RETURNING transfer_contract.contract_id
				), advanced AS (
					UPDATE account_payment
					SET
						contract_retention_cursor = COALESCE(
							(
								SELECT batch.contract_id
								FROM batch
								ORDER BY batch.contract_id DESC
								LIMIT 1
							),
							account_payment.contract_retention_cursor
						),
						contract_retention_pending = ((SELECT COUNT(*) FROM batch) = $1)
					FROM payment
					WHERE account_payment.payment_id = payment.payment_id
					RETURNING (SELECT COUNT(*) FROM batch) AS batch_count
				)
				SELECT
					advanced.batch_count,
					(SELECT COUNT(*) FROM stamped) AS stamped_count
				FROM advanced
				`,
				maxRowCount,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					processedPayment = true
					server.Raise(result.Scan(&batchCount, &stampedCount))
				}
			})
		}, server.TxReadCommitted)
		assignedCount += stampedCount
		if !processedPayment || budgetEnd.Before(server.NowUtc()) {
			return
		}
	}
}

// removeDueContractBatches consumes the first bounded slice of the reap_time
// index before doing any payment lookup. Due contracts held by an active or
// ambiguous payment are repaired to reap_time = NULL. So are unpaid contracts
// stamped by the former 90-day rule that have not yet reached the new 300-day
// horizon. All other due contracts and their dependent rows are deleted.
// Classifying only the already-bounded slice avoids turning the safety checks
// into an anti-join over contract history. The payment guard also covers
// completed payments whose queued retention cursor has not finished yet.
func removeDueContractBatches(ctx context.Context, minTime time.Time, minStragglerCreateTime time.Time, maxRowCount int) {
	budgetEnd := server.NowUtc().Add(reaperRunBudget)
	for {
		var processedCount int64
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// Fix the bounded contract slice before shared balance ownership.
			// The mutation below can only revisit these admitted identities;
			// newly due contracts remain for the next pass.
			var contractIds []server.Id
			rows, err := tx.Query(ctx, `SELECT contract_id FROM transfer_contract
 WHERE reap_time IS NOT NULL AND reap_time<$1 ORDER BY reap_time LIMIT $2`, minTime.UTC(), maxRowCount)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					server.Raise(rows.Scan(&id))
					contractIds = append(contractIds, id)
				}
			})
			admitted, err := tryContractTransferBalanceOwnershipInTx(ctx, tx, contractIds)
			server.Raise(err)
			if !admitted || len(contractIds) == 0 {
				return
			}
			result, err := tx.Query(
				ctx,
				`
				WITH due AS MATERIALIZED (
					SELECT
						transfer_contract.contract_id,
						transfer_contract.create_time
					FROM transfer_contract
					WHERE
						transfer_contract.reap_time IS NOT NULL AND
						transfer_contract.reap_time < $1 AND
						transfer_contract.contract_id=ANY($4::uuid[])
					ORDER BY transfer_contract.reap_time
					LIMIT $3
				), protected AS MATERIALIZED (
					SELECT due.contract_id
					FROM due
					WHERE
						EXISTS (SELECT 1 FROM legacy_settlement_intent WHERE contract_id=due.contract_id) OR
						EXISTS (
							SELECT 1
							FROM transfer_escrow_sweep
							INNER JOIN account_payment ON
								account_payment.payment_id = transfer_escrow_sweep.payment_id
							WHERE
								transfer_escrow_sweep.contract_id = due.contract_id AND
								(
									account_payment.contract_retention_pending OR
									(
										NOT account_payment.completed AND
										(
											NOT account_payment.canceled OR
											account_payment.circle_idempotency_key IS NOT NULL OR
											account_payment.payment_record IS NOT NULL OR
											account_payment.tx_hash IS NOT NULL
										)
									)
								)
						) OR
						(
							due.create_time >= $2 AND
							NOT EXISTS (
								SELECT 1
								FROM transfer_escrow_sweep
								INNER JOIN account_payment ON
									account_payment.payment_id = transfer_escrow_sweep.payment_id
								WHERE
									transfer_escrow_sweep.contract_id = due.contract_id AND
									account_payment.completed
							)
						)
				), cleared AS (
					UPDATE transfer_contract
					SET reap_time = NULL
					FROM protected
					WHERE transfer_contract.contract_id = protected.contract_id
					RETURNING transfer_contract.contract_id
				), candidate AS MATERIALIZED (
					SELECT due.contract_id
					FROM due
					WHERE NOT EXISTS (
						SELECT 1
						FROM protected
						WHERE protected.contract_id = due.contract_id
					)
				), deleted_close AS (
					DELETE FROM contract_close
					USING candidate
					WHERE contract_close.contract_id = candidate.contract_id
				), deleted_escrow AS (
					DELETE FROM transfer_escrow
					USING candidate
					WHERE transfer_escrow.contract_id = candidate.contract_id
				), deleted_sweep AS (
					DELETE FROM transfer_escrow_sweep
					USING candidate
					WHERE transfer_escrow_sweep.contract_id = candidate.contract_id
				), deleted_extender AS (
					DELETE FROM contract_extender
					USING candidate
					WHERE contract_extender.contract_id = candidate.contract_id
				), deleted_contract AS (
					DELETE FROM transfer_contract
					USING candidate
					WHERE transfer_contract.contract_id = candidate.contract_id
					RETURNING transfer_contract.contract_id, source_id, destination_id,
						outcome IS NULL AS unresolved
				)
				SELECT (SELECT COUNT(*) FROM due), contract_id, source_id, destination_id, unresolved
				FROM deleted_contract
				UNION ALL
				SELECT (SELECT COUNT(*) FROM due), NULL, NULL, NULL, false
				WHERE NOT EXISTS (SELECT 1 FROM deleted_contract)
				`,
				minTime.UTC(),
				minStragglerCreateTime.UTC(),
				maxRowCount,
				contractIds,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var contractId, sourceId, destinationId *server.Id
					var unresolved bool
					server.Raise(result.Scan(&processedCount, &contractId, &sourceId, &destinationId, &unresolved))
					if contractId != nil {
						// Deleting unresolved custody is its terminal lifecycle event.
						// Read the deleted version so a concurrent outcome wins once.
						if unresolved {
							server.AddTxCommitCount(tx, &contractClosedCounter, 1)
						}
						contractHoleEventInTx(ctx, tx, *contractId, *sourceId, *destinationId, "remove")
					}
				}
			})
		}, server.TxReadCommitted, server.OptNoRetry())
		if processedCount == 0 || budgetEnd.Before(server.NowUtc()) {
			return
		}
	}
}

// removeContractBatches repeatedly runs a bounded contract-delete cascade (one
// batch per maintenance tx, so no long lock is held) until a batch deletes no
// contracts, meaning the eligible set is drained. This decouples retention
// throughput from the task cadence: a single run fully catches up regardless of
// how many contracts became eligible since the last, so the task can run on a
// low cadence instead of every minute.
//
// Termination is on an empty batch, not a short one: a candidate row is a
// contract_id that may repeat (a contract can have several sweeps), so the
// final DELETE FROM transfer_contract can affect fewer rows than the LIMIT even
// when more work remains. Each non-empty batch deletes its candidates (and
// their sweeps), so the eligible set strictly shrinks and the loop terminates.
func removeContractBatches(ctx context.Context, sql string, minTime time.Time, maxRowCount int) {
	// Cap per call to a time budget so a large backlog of reap-due contracts
	// (e.g. right after a mass straggler assign) drains over many bounded runs
	// instead of one unbounded run that pegs the DB (see reaperRunBudget).
	budgetEnd := server.NowUtc().Add(reaperRunBudget)
	for {
		var batchCount int64
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(ctx, sql, minTime.UTC(), maxRowCount))
			batchCount = tag.RowsAffected()
		}, server.TxReadCommitted)
		if batchCount == 0 || budgetEnd.Before(server.NowUtc()) {
			return
		}
	}
}

// reap_time is a timestamp without zone on the UTC storage clock. Preserve
// the transaction-start clock while making its storage zone explicit.
const assignStragglerReapTimeSQL = `
WITH batch AS (
	SELECT transfer_contract.contract_id
	FROM transfer_contract
	WHERE
		transfer_contract.reap_time IS NULL AND
		transfer_contract.close_time IS NOT NULL AND
		transfer_contract.create_time < $1 AND
		NOT EXISTS (
			SELECT 1
			FROM transfer_escrow_sweep
			INNER JOIN account_payment ON
				account_payment.payment_id = transfer_escrow_sweep.payment_id
			WHERE
				transfer_escrow_sweep.contract_id = transfer_contract.contract_id AND
				(
					account_payment.contract_retention_pending OR
					(
						NOT account_payment.completed AND
						(
							NOT account_payment.canceled OR
							account_payment.circle_idempotency_key IS NOT NULL OR
							account_payment.payment_record IS NOT NULL OR
							account_payment.tx_hash IS NOT NULL
						)
					)
				)
		)
	ORDER BY transfer_contract.create_time
	LIMIT $2
)
UPDATE transfer_contract
SET reap_time = now() AT TIME ZONE 'UTC'
FROM batch
WHERE transfer_contract.contract_id = batch.contract_id
`

// assignStragglerReapTimeBatches stamps the UTC transaction time on closed contracts
// that were never reaped (reap_time IS NULL), are older than minCreateTime, and
// are not held by an active/ambiguous payment. This is the straggler + sweep-less
// cleanup: safely unplanned value otherwise lives forever. Bounded by the
// transfer_contract_reap_pending_create_time partial index (reap_time IS NULL
// AND close_time IS NOT NULL), this is an index range-scan, not the anti-join
// full scan it replaces.
//
// Runs one bounded batch per maintenance tx (no long lock) until a batch marks
// fewer than maxRowCount rows. Each candidate is a distinct transfer_contract row
// (from the primary key) that gets reap_time set and so leaves the partial index,
// so the eligible set strictly shrinks and a short batch means drained.
func assignStragglerReapTimeBatches(ctx context.Context, minCreateTime time.Time, maxRowCount int) (assignedCount int64) {
	// Cap the work per call to a time budget so a large one-time backlog -- e.g.
	// a fresh deploy before `bringyourctl db backfill-contract-reap-time` has run,
	// where reap_time IS NULL matches almost the whole table -- drains over many
	// bounded runs instead of one unbounded run that pegs the DB. The task
	// reschedules every 30 min, so the backlog is still worked down steadily.
	budgetEnd := server.NowUtc().Add(reaperRunBudget)
	for {
		var batchCount int64
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// UPDATE ... LIMIT is not valid Postgres; bound the batch with a CTE
			// that picks the contract ids first, then update exactly those rows.
			// ORDER BY create_time makes the planner take the ordered
			// transfer_contract_reap_pending_create_time partial-index path (oldest
			// stragglers first) rather than risking a seq scan.
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				assignStragglerReapTimeSQL,
				minCreateTime.UTC(),
				maxRowCount,
			))
			batchCount = tag.RowsAffected()
		}, server.TxReadCommitted)
		assignedCount += batchCount
		if batchCount < int64(maxRowCount) || budgetEnd.Before(server.NowUtc()) {
			return
		}
	}
}

// SweepOrphanContractData removes contract_close/transfer_escrow/
// transfer_escrow_sweep/contract_extender rows whose transfer_contract no
// longer exists, plus contract_participant rows whose stream is no longer
// referenced by any retained contract.
// RemoveCompletedContracts cascades the per-contract rows atomically with the
// contract delete. Stream participants are shared by all contracts on a stream,
// so this sweep removes them after the last reference disappears. Each table is
// paged by its primary key in bounded sliceSize slices (see sweepOrphanCursor),
// so a call never full-scans a child table even when there are no orphans.
//
// A call pages at most maxRowCount rows starting from start, and returns the
// position it stopped at. Pass the returned cursor as the next call's start to
// resume; done reports that every table has been fully paged, so the caller can
// begin a fresh pass. maxRowCount <= 0 pages every table to completion in one
// call (the on-demand `bringyourctl db sweep-orphans` path).
func SweepOrphanContractData(
	ctx context.Context,
	start SweepOrphanCursor,
	maxRowCount int,
	sliceSize int,
) (removedCount int64, end SweepOrphanCursor, done bool) {
	return sweepOrphanSteps(
		ctx,
		sweepOrphanContractSteps(),
		start,
		maxRowCount,
		sliceSize,
	)
}

// SweepOrphanCursor is a resumable position in a multi-table orphan sweep: which
// table step, and how far that step's key cursor has advanced. It is returned in
// the task result and handed back as the next run's start, which is what keeps a
// budgeted sweep making forward progress instead of restarting its pass.
//
// Key holds the step's key columns as strings so the cursor round trips through
// the task args as plain json; sweepOrphanSteps decodes it back into the step's
// own typed columns.
type SweepOrphanCursor struct {
	Step int      `json:"step"`
	Key  []string `json:"key,omitempty"`
}

// A step supplies the page SQL and fresh typed cursor targets. Contract-scale
// steps separate first/resumed SQL. Smaller legacy steps omit firstSql and use
// the boolean-plus-cursor parameter convention documented below.
type sweepOrphanStep struct {
	table            string
	firstSql         string
	sql              string
	deleteSql        string
	newCursorTargets func() []any
}

// sweepOrphanSteps pages steps in order starting from start, stopping once every
// step is fully paged (done) or maxRowCount rows have been examined. When it
// stops early the returned cursor is the exact resume point; pass it as the next
// call's start. maxRowCount <= 0 pages every step to completion.
//
// The row budget is what makes this safe to run as a recurring task: the caller
// always returns normally, so the task's Post hook runs and re-arms the chain. A
// sweep that instead relied on the task deadline would be CANCELED rather than
// completed, Post would never run, and the chain would fall back to error-retry
// and restart its pass from zero every time (the 2026-08-11 finding: the sweep
// had never completed a pass in its life and was re-walking the same prefix of
// contract_close continuously, at ~7.6% of all db time).
func sweepOrphanSteps(
	ctx context.Context,
	steps []sweepOrphanStep,
	start SweepOrphanCursor,
	maxRowCount int,
	sliceSize int,
) (removedCount int64, end SweepOrphanCursor, done bool) {
	step := start.Step
	key := start.Key
	if step < 0 || len(steps) <= step {
		// the step list changed under an in-flight cursor; restart the pass
		// rather than skip tables
		step = 0
		key = nil
	}

	remaining := maxRowCount
	for ; step < len(steps); step++ {
		var startKey []any
		if 0 < len(key) {
			// a cursor that no longer decodes (a step's key columns changed)
			// restarts that step, for the same reason as above
			startKey, _ = decodeSweepCursorKey(key, steps[step].newCursorTargets())
		}
		key = nil

		stepRemoved, rowCount, endKey, stepDone := sweepOrphanCursor(
			ctx,
			steps[step],
			startKey,
			remaining,
			sliceSize,
		)
		removedCount += stepRemoved
		if !stepDone {
			return removedCount, SweepOrphanCursor{
				Step: step,
				Key:  encodeSweepCursorKey(endKey),
			}, false
		}
		if 0 < maxRowCount {
			remaining -= rowCount
			if remaining <= 0 && step+1 < len(steps) {
				// budget spent exactly at a table boundary: resume at the head
				// of the next table
				return removedCount, SweepOrphanCursor{Step: step + 1}, false
			}
		}
	}
	return removedCount, SweepOrphanCursor{}, true
}

// encodeSweepCursorKey renders scanned key columns as strings for the cursor
// that round trips through the task args.
func encodeSweepCursorKey(values []any) []string {
	key := make([]string, len(values))
	for i, value := range values {
		switch v := value.(type) {
		case server.Id:
			key[i] = v.String()
		case string:
			key[i] = v
		default:
			// a new key column type must extend both halves of the codec
			panic(fmt.Errorf("unsupported sweep cursor column type %T", value))
		}
	}
	return key
}

// decodeSweepCursorKey parses an encoded cursor back into the column types the
// step's statement expects. ok is false when the encoded cursor does not match
// the step's current key shape, which the caller treats as "restart this step".
func decodeSweepCursorKey(key []string, targets []any) (values []any, ok bool) {
	if len(key) != len(targets) {
		return nil, false
	}
	values = make([]any, len(targets))
	for i, target := range targets {
		switch target.(type) {
		case *server.Id:
			id, err := server.ParseId(key[i])
			if err != nil {
				return nil, false
			}
			values[i] = id
		case *string:
			values[i] = key[i]
		default:
			return nil, false
		}
	}
	return values, true
}

// Page a child table and advance past every examined key, including retained
// rows. Each page has its own read-committed maintenance transaction. Contract
// steps use firstSql(limit) or sql(cursor..., limit) to select and lock a page,
// then deleteSql(locked tuple addresses) in the same transaction. The second
// statement sees row versions returned after a concurrent update's lock wait.
//
// Legacy small-table steps omit firstSql and use sql(first, cursor..., limit).
// Contract pages return (examined int8, locked tuple addresses text[], max key
// columns...). Legacy pages return (examined int8, deleted int8, max keys...).
// A nonempty page supplies its original cursor; no result row means EOF.
// Nil startKey starts at the head; maxRowCount <= 0 pages to EOF, otherwise the
// returned cursor resumes the next invocation without restarting the history.
func sweepOrphanCursor(
	ctx context.Context,
	step sweepOrphanStep,
	startKey []any,
	maxRowCount int,
	sliceSize int,
) (removedCount int64, rowCount int, endKey []any, done bool) {
	// on the first slice of a table the lower bound is disabled, so the cursor
	// columns are only placeholders; resuming passes the real key and keeps the
	// bound live from the start.
	cursor := startKey
	firstSlice := cursor == nil
	if firstSlice {
		cursor = derefCursor(step.newCursorTargets())
	}
	for {
		query := step.sql
		args := make([]any, 0, len(cursor)+2)
		if step.firstSql != "" {
			if firstSlice {
				query = step.firstSql
			} else {
				args = append(args, cursor...)
			}
		} else {
			// Small legacy sweeps still use the boolean-plus-cursor form.
			args = append(args, firstSlice)
			args = append(args, cursor...)
		}
		args = append(args, sliceSize)

		var sliceCount, deletedCount int64
		var targets []any
		gotRow := false
		ownershipRefused := false
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			sliceCount = 0
			deletedCount = 0
			gotRow = false
			targets = step.newCursorTargets()
			var lockedTuples []string
			scanTargets := make([]any, 0, len(targets)+2)
			scanTargets = append(scanTargets, &sliceCount)
			if step.deleteSql != "" {
				scanTargets = append(scanTargets, &lockedTuples)
			} else {
				scanTargets = append(scanTargets, &deletedCount)
			}
			scanTargets = append(scanTargets, targets...)
			if step.table == "transfer_escrow" {
				var admitted bool
				sliceCount, lockedTuples, targets, gotRow, admitted = ownedTransferEscrowOrphanPageInTx(ctx, tx, firstSlice, cursor, sliceSize)
				if !admitted {
					ownershipRefused = true
					return
				}
			} else {
				result, err := tx.Query(ctx, query, args...)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(scanTargets...))
						gotRow = true
					}
				})
			}
			// WithPgResult closes the page before the fresh snapshot is taken.
			// The acquired child locks stay held until this transaction commits.
			if 0 < len(lockedTuples) {
				if int64(len(lockedTuples)) > sliceCount {
					panic("orphan sweep locked beyond its selected page")
				}
				tag, err := tx.Exec(ctx, step.deleteSql, lockedTuples)
				server.Raise(err)
				deletedCount = tag.RowsAffected()
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if ownershipRefused {
			// Keep the refused page's input cursor. Durable orphan rows remain
			// discoverable after the admitted financial owner has completed.
			if firstSlice {
				return removedCount, rowCount, nil, false
			}
			return removedCount, rowCount, cursor, false
		}

		removedCount += deletedCount
		rowCount += int(sliceCount)
		if !gotRow || sliceCount < int64(sliceSize) {
			return removedCount, rowCount, nil, true
		}
		cursor = derefCursor(targets)
		firstSlice = false
		if 0 < maxRowCount && maxRowCount <= rowCount {
			return removedCount, rowCount, cursor, false
		}
	}
}

// sweepOrphanTable pages one child table to completion in a single call: the
// unbudgeted, non-resumable form of sweepOrphanCursor, for sweeps whose driver
// tables are small enough that a whole pass fits comfortably in one run (see
// SweepOrphanNetworkClientData). Sweeps over the contract-scale tables must use
// the budgeted form instead — see the note on sweepOrphanSteps.
func sweepOrphanTable(
	ctx context.Context,
	sliceSize int,
	sql string,
	newCursorTargets func() []any,
) (removedCount int64) {
	removedCount, _, _, _ = sweepOrphanCursor(
		ctx,
		sweepOrphanStep{sql: sql, newCursorTargets: newCursorTargets},
		nil,
		0,
		sliceSize,
	)
	return
}

// derefCursor dereferences a slice of typed pointers into a slice of their
// values, so scanned key columns can be reused as the next slice's cursor args.
func derefCursor(ptrs []any) []any {
	values := make([]any, len(ptrs))
	for i, ptr := range ptrs {
		values[i] = reflect.ValueOf(ptr).Elem().Interface()
	}
	return values
}

// AddSweepDestinationIdColumn adds transfer_escrow_sweep.destination_id if it is
// not already present (idempotent). The column must exist before the sweep
// writer (settleEscrowInTx) is deployed, since the writer inserts it.
func AddSweepDestinationIdColumn(ctx context.Context) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`ALTER TABLE transfer_escrow_sweep ADD COLUMN IF NOT EXISTS destination_id uuid NULL`,
		))
	}, server.TxReadCommitted)
}

// BackfillSweepDestinationIds denormalizes transfer_contract.destination_id onto
// transfer_escrow_sweep for rows created before the column existed, in bounded
// batches (one maintenance tx each) until a batch comes up short. New sweeps are
// stamped by settleEscrowInTx, so this only touches the pre-existing set. Orphan
// sweeps whose contract no longer exists are left NULL (no destination to copy)
// and are reaped by SweepOrphanContractData; the stats filters exclude NULL.
//
// The `destination_id IS NULL` scan rides
// transfer_escrow_sweep_destination_id_sweep_time (btree indexes NULLs), so each
// batch is index-driven rather than a full table scan. It is safe to re-run.
func BackfillSweepDestinationIds(ctx context.Context, limit int) (backfilledCount int64) {
	for {
		var batchCount int64
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
				WITH batch AS (
					SELECT s.contract_id, s.balance_id, s.network_id, tc.destination_id
					FROM transfer_escrow_sweep s
					INNER JOIN transfer_contract tc ON tc.contract_id = s.contract_id
					WHERE s.destination_id IS NULL
					LIMIT $1
				)
				UPDATE transfer_escrow_sweep s
				SET destination_id = batch.destination_id
				FROM batch
				WHERE
					s.contract_id = batch.contract_id AND
					s.balance_id = batch.balance_id AND
					s.network_id = batch.network_id
				`,
				limit,
			))
			batchCount = tag.RowsAffected()
		}, server.TxReadCommitted)
		backfilledCount += batchCount
		if batchCount < int64(limit) {
			return
		}
	}
}

// completedReapBackfillPaymentLookback bounds the completed-contract backfill to
// recently completed payments. A contract is strictly older than its payment's
// completion (create -> settle/sweep -> plan -> complete), so every contract of
// a payment completed more than StragglerContractExpiration ago is itself older
// than StragglerContractExpiration and is stamped by the straggler assign pass
// instead (the reaper runs it every cycle; the ctl backfill runs it first). The
// extra 7 days is slack so boundary timing between the two passes cannot leave a
// payment uncovered.
const completedReapBackfillPaymentLookback = StragglerContractExpiration + 7*24*time.Hour

// BackfillCompletedContractReapTime seeds reap_time on existing contracts whose
// payment already completed, so the indexed reaper can retire them on the normal
// completed-payout window. New completions are handled by the recurring bounded
// retention queue; this is the one-time companion to the reap_time deploy.
// Idempotent: stamped
// contracts (reap_time set) are skipped, so a converged re-run writes nothing.
//
// It drives from the payment side: only payments completed within
// completedReapBackfillPaymentLookback can cover contracts that the straggler
// assign pass does not already stamp (see the constant's invariant), and each
// payment's contracts are reached through the transfer_escrow_sweep payment_id
// index. This replaces two slower shapes: the original
// `WHERE reap_time IS NULL ... LIMIT` batching (O(N^2): every batch re-read the
// already-stamped prefix of the sweep/payment join) and a keyset-cursor page
// over the whole sweep table (O(N), but N = every sweep ever written -- hours of
// heap fetches at prod scale). The payment window makes the work proportional to
// ~one straggler-expiration of payouts regardless of table history.
//
// A contract with several completed payments takes the first-encountered
// payment's complete_time (the reap_time IS NULL guard, same semantics as the
// original backfill); live queued retention keeps at least seven days after
// every completion.
// Each statement stamps at most rowLimit contracts of ONE payment in its own tx
// (a single payment can cover a huge number of contracts, so bounding by
// payments alone produced multi-minute WAL-heavy transactions — observed live as
// a WalWrite stall). progress, when non-nil, is called after each payment.
func BackfillCompletedContractReapTime(ctx context.Context, rowLimit int, progress func(stampedCount int64, processedPaymentCount int, totalPaymentCount int)) (backfilledCount int64) {
	minCompleteTime := server.NowUtc().Add(-completedReapBackfillPaymentLookback)

	// the payment ids in the window; small (a payment is one network's payout
	// for a cycle), so load once and iterate in memory
	paymentIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT payment_id
			FROM account_payment
			WHERE completed AND $1 <= complete_time
			ORDER BY complete_time
			`,
			minCompleteTime.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var paymentId server.Id
				server.Raise(result.Scan(&paymentId))
				paymentIds = append(paymentIds, paymentId)
			}
		})
	})

	for i, paymentId := range paymentIds {
		// drain this payment's unstamped contracts in row-bounded batches. A
		// batch of sweep rows can map to fewer contract updates (multi-sweep
		// contracts), so drain on an EMPTY batch, not a short one; stamped rows
		// leave the reap_time IS NULL set, so the loop terminates.
		for {
			var batchCount int64
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				// interval '7 days' mirrors CompletedContractExpiration
				tag := server.RaisePgResult(tx.Exec(
					ctx,
					`
					WITH batch AS (
						SELECT
							transfer_escrow_sweep.contract_id,
							account_payment.complete_time
						FROM account_payment
						INNER JOIN transfer_escrow_sweep ON
							transfer_escrow_sweep.payment_id = account_payment.payment_id
						INNER JOIN transfer_contract ON
							transfer_contract.contract_id = transfer_escrow_sweep.contract_id
						WHERE
							account_payment.payment_id = $1 AND
							account_payment.completed AND
							transfer_contract.reap_time IS NULL
						LIMIT $2
					)
					UPDATE transfer_contract
					SET reap_time = batch.complete_time + interval '7 days'
					FROM batch
					WHERE
						transfer_contract.contract_id = batch.contract_id AND
						transfer_contract.reap_time IS NULL
					`,
					paymentId,
					rowLimit,
				))
				batchCount = tag.RowsAffected()
			}, server.TxReadCommitted)
			backfilledCount += batchCount
			if batchCount == 0 {
				break
			}
		}
		if progress != nil {
			progress(backfilledCount, i+1, len(paymentIds))
		}
	}
	return
}

// BackfillStragglerContractReapTime seeds the UTC transaction time on existing aged
// closed contracts (reap_time IS NULL, closed, older than
// StragglerContractExpiration) so the indexed reaper can remove them. This is the
// same work the reaper's assign pass performs each run; it exists as an explicit
// backfill so an operator can drain the backlog in one sitting instead of over
// budget-sized reaper cycles. Safe to re-run. assignStragglerReapTimeBatches
// stops at reaperRunBudget per call (it is shared with the periodic reaper);
// this drives it to completion in budget rounds, reporting after each round when
// progress is non-nil.
func BackfillStragglerContractReapTime(ctx context.Context, limit int, progress func(assignedCount int64)) (backfilledCount int64) {
	for {
		assigned := assignStragglerReapTimeBatches(ctx, server.NowUtc().Add(-StragglerContractExpiration), limit)
		backfilledCount += assigned
		if assigned == 0 {
			return
		}
		if progress != nil {
			progress(backfilledCount)
		}
	}
}

func GetOpenTransferByteCount(
	ctx context.Context,
	payerNetworkId server.Id,
) ByteCount {

	var openTransferByteCount ByteCount = 0

	server.Tx(ctx, func(tx server.PgTx) {

		result, err := tx.Query(
			ctx,
			`
			SELECT
			   COALESCE(SUM(transfer_byte_count), 0)
			FROM transfer_contract
			WHERE
			    payer_network_id = $1 AND
			    -- Isolate this payer aggregate from every false-zero global,
			    -- pair, or create-time partial index.
			    (CASE WHEN outcome IS NULL THEN dispute = false ELSE false END)
			`,
			payerNetworkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {

				server.Raise(result.Scan(
					&openTransferByteCount,
				))

			}
		})

	})

	return openTransferByteCount
}
