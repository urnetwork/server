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
//   - `transfer_balance` tracks the balance not in a `transfer_escrow`
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
// its own instead of accumulating without bound. An early expiry is safe: a
// missing counter reads as zero (fail-open, more available balance) and the
// reconcile task re-derives the true value from postgres
// (see `ReconcileNetEscrow`).
// netEscrowMirrorTimeout bounds a detached mirror update so a wedged redis
// cannot retain the goroutine.
const netEscrowMirrorTimeout = 30 * time.Second

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
}

func GetActiveTransferBalances(ctx context.Context, networkId server.Id) []*TransferBalance {
	now := server.NowUtc()

	transferBalances := []*TransferBalance{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    balance_id,
                    start_time,
                    end_time,
                    start_balance_byte_count,
                    net_revenue_nano_cents,
                    balance_byte_count,
                    paid,
                    pro
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
				))
				transferBalances = append(transferBalances, transferBalance)
			}
		})
	})

	server.Redis(ctx, func(r server.RedisClient) {
		netEscrowCmds := map[server.Id]*redis.StringCmd{}
		// the net escrow keys use per-balance hash tags (different slots), so
		// use a plain pipeline, which auto-routes per slot on cluster; a tx
		// pipeline would be cross-slot
		_, pipelineErr := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, transferBalance := range transferBalances {
				netEscrowCmds[transferBalance.BalanceId] = pipe.Get(ctx, netEscrowKey(transferBalance.BalanceId))
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
		}
	})

	return transferBalances
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
// The `netEscrowKey` counter is an approximate, non-atomic mirror with no
// other reconciliation: a leaked `IncrBy` (a quarantined malformed close, a
// dispute that never settled, or a settle whose redis `DecrBy` post was
// dropped/crashed before running) stays in the counter for the life of the
// balance (the key ttl only bounds a counter that outlives its balance; it
// does not correct drift within the balance window). Upward drift makes
// `createTransferEscrowInTx` compute the available
// balance too low and reject contracts with "Insufficient balance" even when
// the postgres balance is plentiful; downward drift lets a balance over-commit.
//
// The reserved bytes for a balance is the sum of its escrow rows whose contract
// is still open. `transfer_contract.outcome` is claimed atomically in the
// settle transaction (`claimContractOutcomeInTx`), so `outcome IS NULL` is the
// reliable signal that a reservation is live -- unlike `transfer_escrow.settled`,
// which is itself set in a best-effort post and can leak. A disputed contract
// (`outcome` still null, generated `open` false) still holds its reservation, so
// it is matched by `outcome IS NULL` and would be missed by `open`.
//
// Each PostgreSQL reservation snapshot is taken for only the Redis batch that
// is about to be corrected. The old implementation took one fleet-wide
// snapshot, then spent about 30 minutes walking 1.8M balances; by the last
// batch its SET values were 30 minutes stale and overwrote live mirror traffic
// (5.79TiB of under-reservation followed by >10k negative-counter lines in 29s
// on 2026-08-29). Corrections use INCRBY(delta), not SET: a concurrent mirror
// increment/decrement between our GET and correction remains in the result.
//
// INCRBY cannot make PostgreSQL and Redis atomic. PostgreSQL fixes the page's
// statement snapshot before running the reservation query. A mirror write that
// becomes visible after that snapshot but before the later Redis GET can still
// be backed out by a correction toward the old snapshot; the next page pass
// then reverses it. The unsettled partial covering index is correctness-critical
// as well as a performance optimization because it keeps that exposure short.
// Durable per-balance fencing/versioning is required if matched reversals remain
// after pages are consistently fast. A separate commit-to-mirror-post window is
// contained by the atomic release clamp below.
//
// Drift is the signed difference (previous counter minus reconciled value)
// summed per network. Positive drift means the counter was over-reserved -- the
// direction that starves the available balance and produces spurious
// "Insufficient balance". Only networks with nonzero net drift are returned.
func ReconcileNetEscrow(ctx context.Context, apply bool) (driftByNetworkId map[server.Id]ByteCount, balanceCount int) {
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
		// Read reservations immediately before correcting this page. Do not move
		// this above the pagination loop: that recreates the stale-global-snapshot
		// incident described in the function comment. Keep the query on the fast
		// unsettled partial path so its statement-snapshot-to-Redis-GET window stays
		// bounded as well.
		pending := openEscrowReservedForBalances(ctx, balanceIds)
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
	// Visit only non-current balances that still have authoritative open escrow.
	// The settled=false predicate is the same safe partial-index prefilter used
	// by the reservation query; outcome IS NULL remains authoritative. This keeps
	// the second pass proportional to live stragglers rather than every expired
	// balance, and it remains correct for arbitrarily delayed close work.
	cursor = server.Id{}
	for {
		rows := []balanceRow{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				netEscrowNoncurrentOpenBalancePageSQL,
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
		pending := openEscrowReservedForBalances(ctx, balanceIds)
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

	for networkId, drift := range driftByNetworkId {
		if drift == 0 {
			delete(driftByNetworkId, networkId)
		}
	}

	return
}

// netEscrowNoncurrentOpenBalancePageSQL discovers balances that the ordinary
// availability-window scan deliberately excludes but that still own a live
// PostgreSQL reservation. transfer_escrow_unsettled_balance_contract makes the
// balance-id keyset scan bounded; the outcome join excludes closed rows whose
// best-effort settled post was missed.
const netEscrowNoncurrentOpenBalancePageSQL = `
    SELECT
        transfer_escrow.balance_id,
        transfer_balance.network_id
    FROM transfer_escrow
    INNER JOIN transfer_contract ON
        transfer_contract.contract_id = transfer_escrow.contract_id
    INNER JOIN transfer_balance ON
        transfer_balance.balance_id = transfer_escrow.balance_id
    WHERE
        transfer_escrow.settled = false AND
        transfer_contract.outcome IS NULL AND
        NOT (
            transfer_balance.active = true AND
            transfer_balance.start_time <= $1 AND $1 < transfer_balance.end_time
        ) AND
        transfer_escrow.balance_id > $2
    GROUP BY transfer_escrow.balance_id, transfer_balance.network_id
    ORDER BY transfer_escrow.balance_id
    LIMIT $3
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
                SELECT transfer_escrow.balance_id
                FROM transfer_escrow
                INNER JOIN transfer_contract ON
                    transfer_contract.contract_id = transfer_escrow.contract_id
                INNER JOIN transfer_balance ON
                    transfer_balance.balance_id = transfer_escrow.balance_id
                WHERE
                    transfer_balance.network_id = $1 AND
                    transfer_escrow.settled = false AND
                    transfer_contract.outcome IS NULL AND
                    NOT (
                        transfer_balance.active = true AND
                        transfer_balance.start_time <= $2 AND $2 < transfer_balance.end_time
                    )
                GROUP BY transfer_escrow.balance_id
                ORDER BY transfer_escrow.balance_id
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
const netEscrowReservationPageSQL = `
    SELECT
        selected_escrow.balance_id,
        SUM(selected_escrow.balance_byte_count)
    FROM unnest($1::uuid[]) AS requested_balance(balance_id)
    CROSS JOIN LATERAL (
        SELECT
            transfer_escrow.balance_id,
            transfer_escrow.contract_id,
            transfer_escrow.balance_byte_count
        FROM transfer_escrow
        WHERE
            transfer_escrow.balance_id = requested_balance.balance_id AND
            transfer_escrow.settled = false
        OFFSET 0
    ) AS selected_escrow
    INNER JOIN transfer_contract ON
        transfer_contract.contract_id = selected_escrow.contract_id
    WHERE transfer_contract.outcome IS NULL
    GROUP BY selected_escrow.balance_id
`

func openEscrowReservedForBalances(ctx context.Context, balanceIds []server.Id) map[server.Id]ByteCount {
	pending := map[server.Id]ByteCount{}
	if len(balanceIds) == 0 {
		return pending
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			netEscrowReservationPageSQL,
			balanceIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				var reserved ByteCount
				server.Raise(result.Scan(&balanceId, &reserved))
				pending[balanceId] = reserved
			}
		})
	})
	return pending
}

// reconcileNetEscrowBatch reads the current net escrow counter for each balance
// and returns the signed drift against the reserved (true) value (previous
// counter minus reserved; positive means over-reserved). When apply is true it
// atomically adds only nonzero corrections. An already-correct mirror receives
// no write or TTL refresh; this avoids the old fleet-wide SET/DEL storm on a
// logically no-op pass. A corrected zero result deletes the key, matching the
// "missing counter is zero" invariant. The caller's pending values are an
// earlier PostgreSQL statement snapshot; additive correction protects changes
// after the Redis GET, not mirror changes already visible before that GET.
const netEscrowCorrectionScript = `
local value = redis.call('INCRBY', KEYS[1], ARGV[1])
if value == 0 then
    redis.call('DEL', KEYS[1])
else
    redis.call('EXPIRE', KEYS[1], ARGV[2])
end
return value
`

func applyNetEscrowCorrection(
	ctx context.Context,
	scripter redis.Scripter,
	key string,
	correction ByteCount,
) *redis.Cmd {
	return scripter.Eval(
		ctx,
		netEscrowCorrectionScript,
		[]string{key},
		correction,
		int64(netEscrowFallbackTtl/time.Second),
	)
}

// A PostgreSQL reservation is committed before its Redis mirror post. Even a
// page-local additive reconcile cannot make those two stores atomic: it can
// observe a just-settled PostgreSQL row before that settlement's Redis DECRBY,
// correct the still-reserved mirror to zero, and then receive the delayed
// decrement. Apply releases through one Lua command so that irreducible race
// still returns the negative value for diagnosis but never leaves a negative
// counter behind. Positive counters retain a shorter precise deadline and cap
// a missing/legacy-long ttl at the rolling fallback horizon.
const netEscrowReleaseScript = `
local value = redis.call('DECRBY', KEYS[1], ARGV[1])
if value <= 0 then
    redis.call('DEL', KEYS[1])
else
    local ttl = redis.call('TTL', KEYS[1])
    local max_ttl = tonumber(ARGV[2])
    if ttl < 0 or max_ttl < ttl then
        redis.call('EXPIRE', KEYS[1], max_ttl)
    end
end
return value
`

func applyNetEscrowRelease(
	ctx context.Context,
	scripter redis.Scripter,
	key string,
	release ByteCount,
) *redis.Cmd {
	return scripter.Eval(
		ctx,
		netEscrowReleaseScript,
		[]string{key},
		release,
		int64(netEscrowFallbackTtl/time.Second),
	)
}

func reconcileNetEscrowBatch(
	ctx context.Context,
	pending map[server.Id]ByteCount,
	balanceIds []server.Id,
	apply bool,
) (drift map[server.Id]ByteCount) {
	drift = map[server.Id]ByteCount{}
	server.RedisDoOnce(ctx, func(r server.RedisClient) {
		// the net escrow keys use per-balance hash tags (different slots), so
		// use plain pipelines, which auto-route per slot on cluster
		getCmds := map[server.Id]*redis.StringCmd{}
		r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, balanceId := range balanceIds {
				getCmds[balanceId] = pipe.Get(ctx, netEscrowKey(balanceId))
			}
			return nil
		})
		corrections := map[server.Id]ByteCount{}
		for _, balanceId := range balanceIds {
			previous, getErr := getCmds[balanceId].Int64()
			if errors.Is(getErr, redis.Nil) {
				previous = 0
			} else if getErr != nil {
				server.Raise(getErr)
			}
			drift[balanceId] = ByteCount(previous) - pending[balanceId]
			if correction := pending[balanceId] - ByteCount(previous); correction != 0 {
				corrections[balanceId] = correction
			}
		}

		if !apply || len(corrections) == 0 {
			return
		}

		_, pipelineErr := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for balanceId, correction := range corrections {
				applyNetEscrowCorrection(ctx, pipe, netEscrowKey(balanceId), correction)
			}
			return nil
		})
		reportNetEscrowMirrorWriteFailure("reconciliation", pipelineErr)
	})
	return
}

// reportNegativeNetEscrow reports a net escrow counter that a release drove
// below zero. applyNetEscrowRelease has already atomically deleted the negative
// key, so the log retains the original result and mutation identity without
// leaving availability overstated until the next reconcile.
func reportNegativeNetEscrow(
	decrCmds map[server.Id]*redis.Cmd,
	contractId server.Id,
	site string,
) {
	for balanceId, cmd := range decrCmds {
		if cmd == nil {
			continue
		}
		if netEscrow, err := cmd.Int64(); err == nil && netEscrow < 0 {
			glog.Errorf(
				"[netescrow]negative counter after %s: balance=%s contract=%s result=%d clamped_to=0\n",
				site,
				balanceId,
				contractId,
				netEscrow,
			)
		}
	}
}

// reportNetEscrowMirrorWriteFailure preserves the uncertain-outcome boundary
// without retrying a non-idempotent mutation. The next source-of-truth
// reconciliation repairs either a missing or partially applied write.
func reportNetEscrowMirrorWriteFailure(site string, err error) {
	if err == nil {
		return
	}
	glog.Errorf("[netescrow]mirror write failed after %s: %v\n", site, err)
	server.Raise(err)
}

// releaseNetEscrowForContract returns a quarantined contract's reserved bytes to
// its balances by decrementing the redis net escrow counters. The caller must
// have just claimed the contract (`outcome IS NULL` -> settled) so the
// reservation is released exactly once; the normal settle path owns the `DecrBy`
// otherwise.
func releaseNetEscrowForContract(ctx context.Context, contractId server.Id) {
	escrowed := map[server.Id]ByteCount{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT balance_id, balance_byte_count
                FROM transfer_escrow
                WHERE contract_id = $1
            `,
			contractId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				var byteCount ByteCount
				server.Raise(result.Scan(&balanceId, &byteCount))
				escrowed[balanceId] = byteCount
			}
		})
	})
	if len(escrowed) == 0 {
		return
	}
	// the quarantine claim is committed; the mirror must follow even if the
	// caller has gone away (see netEscrowMirrorCtx)
	mirrorCtx, mirrorCancel := netEscrowMirrorCtx(ctx)
	defer mirrorCancel()
	server.RedisDoOnce(mirrorCtx, func(r server.RedisClient) {
		decrCmds := map[server.Id]*redis.Cmd{}
		// per-balance hash tags (different slots): plain pipeline auto-routes
		_, pipelineErr := r.Pipelined(mirrorCtx, func(pipe redis.Pipeliner) error {
			for balanceId, byteCount := range escrowed {
				key := netEscrowKey(balanceId)
				decrCmds[balanceId] = applyNetEscrowRelease(mirrorCtx, pipe, key, byteCount)
			}
			return nil
		})
		reportNetEscrowMirrorWriteFailure("quarantine release", pipelineErr)
		reportNegativeNetEscrow(decrCmds, contractId, "quarantine release")
	})
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

	// pro = false: this is the unpaid, data-only grant path (the daily free-tier
	// grant and referral bonuses). It must never confer Pro -- see pro_model.go.
	// For a Pro grant use AddProTransferBalanceInTx.
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

	if err != nil {
		returnErr = err
	}
	return
}

// add balance to a network at no cost
// AddProTransferBalanceInTx grants one network a Pro balance for the window. The
// balance carries pro = true, which is what confers the Pro entitlement -- see
// pro_model.go. The caller must refresh the Pro cache (UpdateProNetwork) once the
// tx commits, so the upgrade is visible immediately.
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
	if err != nil {
		returnErr = err
	}

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

type ContractParty = string

const (
	ContractPartySource      ContractParty = "source"
	ContractPartyDestination ContractParty = "destination"
	ContractPartyCheckpoint  ContractParty = "checkpoint"
)

type TransferEscrow struct {
	ContractId          server.Id
	CompanionContractId *server.Id
	Priority            Priority
	TransferByteCount   ByteCount
	Balances            []*TransferEscrowBalance
}

type TransferEscrowBalance struct {
	BalanceId        server.Id
	BalanceByteCount ByteCount
}

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
	// *important note* this function is one of the hotspots in the system,
	// since it is called before every transfer pair.
	// a small regression here can cause a backlog in the overall throughput of the network.
	// You must make sure the queries here are optimized correctly.
	// TODO we need better performance regression tools to measure small regressions in hotspots like this

	// note it is possible to create a contract with `contractTransferByteCount = 0`

	contractId := server.NewId()

	type escrow struct {
		balanceId        server.Id
		paid             bool
		balanceByteCount ByteCount
		startTime        time.Time
		endTime          time.Time
	}

	now := server.NowUtc()

	// add up the balance_byte_count until >= contractTransferByteCount
	// if not enough, error
	balanceEscrows := map[server.Id]*escrow{}

	// attempt to split up across remaining transfer balances

	orderedTransferBalances := []*escrow{}
	result, err := tx.Query(
		ctx,
		`
            SELECT
                balance_id,
                paid,
                balance_byte_count,
                start_time,
                end_time
            FROM transfer_balance
            WHERE
                network_id = $1 AND
                active = true
        `,
		payerNetworkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			transferBalance := &escrow{}
			server.Raise(result.Scan(
				&transferBalance.balanceId,
				&transferBalance.paid,
				&transferBalance.balanceByteCount,
				&transferBalance.startTime,
				&transferBalance.endTime,
			))
			if !transferBalance.startTime.After(now) && now.Before(transferBalance.endTime) {
				orderedTransferBalances = append(orderedTransferBalances, transferBalance)
			}
		}
	})

	server.Redis(ctx, func(r server.RedisClient) {
		netEscrowCmds := map[server.Id]*redis.StringCmd{}
		// the net escrow keys use per-balance hash tags (different slots), so
		// use a plain pipeline, which auto-routes per slot on cluster
		_, pipelineErr := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, transferBalance := range orderedTransferBalances {
				netEscrowCmds[transferBalance.balanceId] = pipe.Get(ctx, netEscrowKey(transferBalance.balanceId))
			}
			return nil
		})
		if pipelineErr != nil && !errors.Is(pipelineErr, redis.Nil) {
			server.Raise(pipelineErr)
		}
		for _, transferBalance := range orderedTransferBalances {
			netEscrowCmd := netEscrowCmds[transferBalance.balanceId]
			netEscrowBalanceByteCount, commandErr := netEscrowCmd.Int64()
			if errors.Is(commandErr, redis.Nil) {
				netEscrowBalanceByteCount = 0
			} else {
				server.Raise(commandErr)
			}
			netEscrowBalanceByteCount = max(int64(0), netEscrowBalanceByteCount)
			transferBalance.balanceByteCount = max(0, transferBalance.balanceByteCount-ByteCount(netEscrowBalanceByteCount))
		}
	})

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
		escrowBalanceByteCount := min(
			contractTransferByteCount-netEscrowBalanceByteCount,
			transferBalance.balanceByteCount,
		)

		balanceEscrows[transferBalance.balanceId] = &escrow{
			balanceId:        transferBalance.balanceId,
			paid:             transferBalance.paid,
			balanceByteCount: escrowBalanceByteCount,
			// carried to stamp the net escrow counter ttl in the redis post
			endTime: transferBalance.endTime,
		}
		netEscrowBalanceByteCount += escrowBalanceByteCount
		if contractTransferByteCount <= netEscrowBalanceByteCount {
			// we have enough balances for this escrow
			break
		}
	}

	if netEscrowBalanceByteCount < contractTransferByteCount {
		returnErr = fmt.Errorf("Insufficient balance (%d).", netEscrowBalanceByteCount)
		return
	}

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

	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		for balanceId, escrow := range balanceEscrows {
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
	            INSERT INTO transfer_contract (
	                contract_id,
	                source_network_id,
	                source_id,
	                destination_network_id,
	                destination_id,
	                transfer_byte_count,
	                companion_contract_id,
	                payer_network_id,
	                create_time,
	                priority
	            )
	            VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	        `,
			contractId,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferByteCount,
			companionContractId,
			payerNetworkId,
			now,
			priority,
		)
	})

	posts = append(posts, func() any {
		// the reservation is committed; the mirror must follow even if the
		// caller has gone away (see netEscrowMirrorCtx)
		mirrorCtx, mirrorCancel := netEscrowMirrorCtx(ctx)
		defer mirrorCancel()
		server.RedisDoOnce(mirrorCtx, func(r server.RedisClient) {
			mirrorTime := server.NowUtc()
			// per-balance hash tags (different slots): plain pipeline auto-routes
			_, pipelineErr := r.Pipelined(mirrorCtx, func(pipe redis.Pipeliner) error {
				for balanceId, escrow := range balanceEscrows {
					key := netEscrowKey(balanceId)
					pipe.IncrBy(mirrorCtx, key, escrow.balanceByteCount)
					// Prefer the balance end time plus slack for short
					// balances, but cap intentionally multi-year balances at
					// the rolling fallback horizon. Non-NX also repairs an old
					// effectively permanent deadline on the next mirror write.
					pipe.ExpireAt(mirrorCtx, key, netEscrowExpiration(mirrorTime, escrow.endTime))
				}
				return nil
			})
			reportNetEscrowMirrorWriteFailure("reservation", pipelineErr)
		})
		return nil
	})

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
		TransferByteCount:   contractTransferByteCount,
		Priority:            priority,
		Balances:            balances,
	}

	return
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
	var posts []func() any

	server.Tx(ctx, func(tx server.PgTx) {
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
	})

	if returnErr != nil {
		return
	}
	server.RunPosts(ctx, posts...)
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

func CreateCompanionTransferEscrow(
	ctx context.Context,
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
	contractTransferByteCount ByteCount,
	originContractTimeout time.Duration,
) (transferEscrow *TransferEscrow, returnErr error) {
	var posts []func() any

	server.Tx(ctx, func(tx server.PgTx) {
		// find the earliest open transfer contract in the opposite direction
		// with null companion_contract_id
		// there can be many companion contracts for an original contract

		result, err := tx.Query(
			ctx,
			`
                SELECT contract_id
                FROM (
                    (
                        SELECT contract_id, create_time
                        FROM transfer_contract
                        WHERE
                            open = true AND
                            source_id = $1 AND
                            destination_id = $2 AND
                            companion_contract_id IS NULL
                        ORDER BY create_time ASC
                        LIMIT 1
                    )

                    UNION ALL

                    (
                        SELECT contract_id, create_time
                        FROM transfer_contract
                        WHERE
                            open = false AND
                            $3 <= close_time AND
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
		)
		var companionContractId *server.Id
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&companionContractId))
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
			// unnoticed). The payer is unchanged: the companion's
			// destination side pays, exactly as for a plain-origin
			// companion. Plain origins stay preferred; the chain is bounded
			// in practice at depth two (a reply carrier answering a return
			// direction).
			result, err := tx.Query(
				ctx,
				`
                    SELECT contract_id
                    FROM (
                        (
                            SELECT contract_id, create_time
                            FROM transfer_contract
                            WHERE
                                open = true AND
                                source_id = $1 AND
                                destination_id = $2 AND
                                companion_contract_id IS NOT NULL
                            ORDER BY create_time ASC
                            LIMIT 1
                        )

                        UNION ALL

                        (
                            SELECT contract_id, create_time
                            FROM transfer_contract
                            WHERE
                                open = false AND
                                $3 <= close_time AND
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
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&companionContractId))
				}
			})
		}

		if companionContractId == nil {
			returnErr = ErrMissingCompanionOrigin
			return
		}

		transferEscrow, posts, returnErr = createTransferEscrowInTx(
			ctx,
			tx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			// destination is payer
			destinationNetworkId,
			contractTransferByteCount,
			companionContractId,
		)
	})

	if returnErr != nil {
		return
	}
	server.RunPosts(ctx, posts...)
	// a companion contract is the return path of an origin contract: the
	// destination is the paying side — count its top-level identity in the
	// block users stat
	StampTopLevelClientContractTime(ctx, destinationId)
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
                    transfer_contract.open = true AND
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
	server.Tx(ctx, func(tx server.PgTx) {
		contractId = server.NewId()

		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO transfer_contract (
                    contract_id,
                    source_network_id,
                    source_id,
                    destination_network_id,
                    destination_id,
                    transfer_byte_count,
                    create_time
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7)
            `,
			contractId,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferByteCount,
			server.NowUtc(),
		))
	})
	// network / friends-and-family egress has no payer but is still
	// contract-creating usage: count the source's top-level identity in the
	// block users stat
	StampTopLevelClientContractTime(ctx, sourceId)
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
) (returnErr error) {
	// settle := false
	// dispute := false
	if usedTransferByteCount < 0 {
		return fmt.Errorf("Invalid used transfer byte count: %d", usedTransferByteCount)
	}

	server.Tx(ctx, func(tx server.PgTx) {
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
		if outcome != nil {
			returnErr = fmt.Errorf("Contract already closed with outcome %s: %s %s %s->%s", *outcome, contractId.String(), clientId.String(), sourceId.String(), destinationId.String())
			return
		}
		if dispute {
			returnErr = fmt.Errorf("Contract in dispute: %s %s %s->%s", contractId.String(), clientId.String(), sourceId.String(), destinationId.String())
			return
		}

		if checkpoint {
			server.RaisePgResult(tx.Exec(
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

		} else {
			server.RaisePgResult(tx.Exec(
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
		}
	}, server.TxReadCommitted)

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

func settleContract(ctx context.Context, contractId server.Id) (closed bool, returnErr error) {
	var posts []func() any
	var clockTransferByteCount ByteCount

	server.Tx(ctx, func(tx server.PgTx) {
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
					posts, closed, returnErr = settleEscrowInTx(ctx, tx, contractId, ContractOutcomeSettled)
				} else {
					glog.Infof("[sub]contract[%s]diff %d (%d <> %d)\n", contractId.String(), diff, sourceUsedTransferByteCount, destinationUsedTransferByteCount)
					// fmt.Printf("CLOSE CONTRACT DISPUTE (%s) %s\n", clientId.String(), contractId.String())
					closed = setContractDisputeInTx(ctx, tx, contractId, true)
				}
			} else {
				// nothing to settle, just close the transaction
				closed = claimContractOutcomeInTx(ctx, tx, contractId, ContractOutcomeSettled)
				if closed {
					clockTransferByteCount = destinationUsedTransferByteCount
				}
			}
		}
	}, server.TxReadCommitted)

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
		posts, _, returnErr = settleEscrowInTx(ctx, tx, contractId, outcome)
	}, server.TxReadCommitted)

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
) bool {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
            UPDATE transfer_contract
            SET
                outcome = $2,
                close_time = $3
            WHERE
                contract_id = $1 AND
                outcome IS NULL
        `,
		contractId,
		outcome,
		server.NowUtc(),
	))
	return tag.RowsAffected() == 1
}

func settleEscrowInTx(
	ctx context.Context,
	tx server.PgTx,
	contractId server.Id,
	outcome ContractOutcome,
) (posts []func() any, closed bool, returnErr error) {
	var usedTransferByteCount ByteCount
	var clockTransferByteCount ByteCount

	switch outcome {
	case ContractOutcomeSettled:
		result, err := tx.Query(
			ctx,
			`
                SELECT
                    used_transfer_byte_count,
                    party,
                    checkpoint
                FROM contract_close
                WHERE
                    contract_id = $1
            `,
			contractId,
		)
		netUsedTransferByteCount := ByteCount(0)
		partyCount := 0
		checkpointCount := 0
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var usedTransferByteCountForParty ByteCount
				var party ContractParty
				var checkpoint bool
				server.Raise(result.Scan(
					&usedTransferByteCountForParty,
					&party,
					&checkpoint,
				))
				// Count the checkpoint row's byte count like any other close:
				// settleContract already established one party is non-checkpoint,
				// so no more activity is coming and the checkpoint's
				// `used_transfer_byte_count` is that party's final contribution.
				if checkpoint {
					checkpointCount += 1
				}
				netUsedTransferByteCount += usedTransferByteCountForParty
				if party == ContractPartyDestination {
					clockTransferByteCount = usedTransferByteCountForParty
				}
				partyCount += 1
			}
		})
		if partyCount != 2 {
			returnErr = fmt.Errorf("Must have 2 parties to settle contract (found %d).", partyCount)
			return
		}
		// Defensive: refuse to settle if both parties are checkpoint.
		// settleContract shouldn't route here; flag the logic bug rather than settle.
		if checkpointCount == 2 {
			returnErr = fmt.Errorf("Cannot settle contract with both parties checkpoint.")
			return
		}
		usedTransferByteCount = netUsedTransferByteCount / ByteCount(partyCount)
	case ContractOutcomeDisputeResolvedToSource, ContractOutcomeDisputeResolvedToDestination:
		var party ContractParty
		switch outcome {
		case ContractOutcomeDisputeResolvedToSource:
			party = ContractPartySource
		default:
			party = ContractPartyDestination
		}
		result, err := tx.Query(
			ctx,
			`
                SELECT
                    used_transfer_byte_count
                FROM contract_close
                WHERE
                    contract_id = $1 AND
                    party = $2
            `,
			contractId,
			party,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&usedTransferByteCount))
				if party == ContractPartyDestination {
					clockTransferByteCount = usedTransferByteCount
				}
			}
		})
		if party != ContractPartyDestination {
			result, err = tx.Query(
				ctx,
				`
                    SELECT used_transfer_byte_count
                    FROM contract_close
                    WHERE contract_id = $1 AND party = $2
                `,
				contractId,
				ContractPartyDestination,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&clockTransferByteCount))
				}
			})
		}
	default:
		returnErr = fmt.Errorf("Unknown contract outcome: %s", outcome)
		return
	}

	// order balances by end date, ascending
	// take from the earlier before the later
	result, err := tx.Query(
		ctx,
		`
            SELECT
                transfer_escrow.balance_id,
                transfer_escrow.balance_byte_count,
                transfer_balance.start_balance_byte_count,
                transfer_balance.net_revenue_nano_cents
            FROM transfer_escrow

            INNER JOIN transfer_balance ON
                transfer_balance.balance_id = transfer_escrow.balance_id

            WHERE
                transfer_escrow.contract_id = $1

            ORDER BY transfer_balance.end_time ASC
        `,
		contractId,
	)

	// balance id -> payout byte count, return byte count, payout
	sweepPayouts := map[server.Id]sweepPayout{}
	netPayoutByteCount := ByteCount(0)
	netPayout := NanoCents(0)

	server.WithPgResult(result, err, func() {
		for result.Next() {
			var balanceId server.Id
			var escrowBalanceByteCount ByteCount
			var startBalanceByteCount ByteCount
			var netRevenue NanoCents
			server.Raise(result.Scan(
				&balanceId,
				&escrowBalanceByteCount,
				&startBalanceByteCount,
				&netRevenue,
			))

			payoutByteCount := min(usedTransferByteCount-netPayoutByteCount, escrowBalanceByteCount)
			returnByteCount := escrowBalanceByteCount - payoutByteCount
			netPayoutByteCount += payoutByteCount
			payout := NanoCents(math.Round(
				ProviderRevenueShare * float64(netRevenue) * float64(payoutByteCount) / float64(startBalanceByteCount),
			))

			netPayout += payout
			sweepPayouts[balanceId] = sweepPayout{
				escrowBalanceByteCount: escrowBalanceByteCount,
				payoutByteCount:        payoutByteCount,
				returnByteCount:        returnByteCount,
				payout:                 payout,
			}
			// fmt.Printf("SETTLE %s %s: payout %d (%d nanocents) return %d\n", contractId.String(), balanceId.String(), payoutByteCount, payout, returnByteCount)
		}
	})

	// if len(sweepPayouts) == 0 {
	// 	returnErr = fmt.Errorf("Invalid contract.")
	// 	return
	// }

	if netPayoutByteCount < usedTransferByteCount {
		returnErr = fmt.Errorf("Escrow does not have enough value to pay out the full amount.")
		return
	}

	var payoutNetworkId *server.Id
	var destinationId server.Id
	result, err = tx.Query(
		ctx,
		`
            SELECT
                source_network_id,
                destination_network_id,
                destination_id,
                payer_network_id,
                companion_contract_id
            FROM transfer_contract
            WHERE
                contract_id = $1
        `,
		contractId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var sourceNetworkId server.Id
			var destinationNetworkId server.Id
			var payerNetworkId *server.Id
			var companionContractId *server.Id
			server.Raise(result.Scan(
				&sourceNetworkId,
				&destinationNetworkId,
				&destinationId,
				&payerNetworkId,
				&companionContractId,
			))
			if payerNetworkId != nil {
				if *payerNetworkId == sourceNetworkId {
					payoutNetworkId = &destinationNetworkId
				} else {
					payoutNetworkId = &sourceNetworkId
				}
			} else {
				// migration, infer for older contracts
				if companionContractId == nil {
					payoutNetworkId = &destinationNetworkId
				} else {
					payoutNetworkId = &sourceNetworkId
				}
			}
		}
	})

	if payoutNetworkId == nil {
		returnErr = fmt.Errorf("Destination client does not exist.")
		return
	}

	if !claimContractOutcomeInTx(ctx, tx, contractId, outcome) {
		return
	}
	closed = true
	if 0 < clockTransferByteCount {
		posts = append(posts, clockTransferPost(ctx, clockTransferByteCount))
	}

	// run all the posts in parallel in as small blocks as reasonable to minimize the work for serialization errors

	if 0 < len(sweepPayouts) {
		posts = append(posts, func() any {
			server.Tx(ctx, func(tx server.PgTx) {
				server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
					for balanceId, sweepPayout := range sweepPayouts {
						batch.Queue(
							`
					            UPDATE transfer_escrow
					            SET
					                settled = true,
					                settle_time = $2,
					                payout_byte_count = $4
					            WHERE
					                transfer_escrow.contract_id = $1 AND
					                transfer_escrow.balance_id = $3
					        `,
							contractId,
							server.NowUtc(),
							balanceId,
							sweepPayout.payoutByteCount,
						)

					}
				})
			}, server.TxReadCommitted)
			return nil
		})

		posts = append(posts, func() any {
			server.Tx(ctx, func(tx server.PgTx) {
				server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
					for balanceId, sweepPayout := range sweepPayouts {

						if 0 < sweepPayout.payoutByteCount {
							batch.Queue(
								`
						            INSERT INTO transfer_escrow_sweep (
						                contract_id,
						                balance_id,
						                network_id,
						                payout_byte_count,
						                payout_net_revenue_nano_cents,
						                destination_id
						            )
						            VALUES ($1, $2, $3, $4, $5, $6)
						            ON CONFLICT (contract_id, balance_id, network_id) DO UPDATE
						            SET
						            	payout_byte_count = $4,
						            	payout_net_revenue_nano_cents = $5,
						            	destination_id = $6
						        `,
								contractId,
								balanceId,
								payoutNetworkId,
								sweepPayout.payoutByteCount,
								sweepPayout.payout,
								destinationId,
							)

						}
					}
				})
			}, server.TxReadCommitted)
			return nil
		})

		posts = append(posts, func() any {
			server.Tx(ctx, func(tx server.PgTx) {
				server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
					for balanceId, sweepPayout := range sweepPayouts {

						if 0 < sweepPayout.payoutByteCount {

							batch.Queue(
								`
						            UPDATE transfer_balance
						            SET
						                balance_byte_count = transfer_balance.balance_byte_count - $2
						            WHERE
						                transfer_balance.balance_id = $1
						        `,
								balanceId,
								sweepPayout.payoutByteCount,
							)
						}
					}
				})
			}, server.TxReadCommitted)
			return nil
		})

		posts = append(posts, func() any {
			// the settlement is committed; the mirror must follow even if the
			// caller has gone away (see netEscrowMirrorCtx)
			mirrorCtx, mirrorCancel := netEscrowMirrorCtx(ctx)
			defer mirrorCancel()
			server.RedisDoOnce(mirrorCtx, func(r server.RedisClient) {
				decrCmds := map[server.Id]*redis.Cmd{}
				// per-balance hash tags (different slots): plain pipeline auto-routes
				_, pipelineErr := r.Pipelined(mirrorCtx, func(pipe redis.Pipeliner) error {
					for balanceId, sweepPayout := range sweepPayouts {
						key := netEscrowKey(balanceId)
						decrCmds[balanceId] = applyNetEscrowRelease(
							mirrorCtx,
							pipe,
							key,
							sweepPayout.escrowBalanceByteCount,
						)
					}
					return nil
				})
				reportNetEscrowMirrorWriteFailure("settle", pipelineErr)
				reportNegativeNetEscrow(decrCmds, contractId, "settle")
			})
			return nil
		})
	}

	posts = append(posts, func() any {
		server.Redis(ctx, func(r server.RedisClient) {
			r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.IncrBy(ctx, accountBalanceNetPayoutByteCountKey(*payoutNetworkId), netPayoutByteCount)
				pipe.IncrBy(ctx, accountBalanceNetPayout(*payoutNetworkId), netPayout)
				return nil
			})
		})
		return nil
	})

	return
}

type sweepPayout struct {
	escrowBalanceByteCount ByteCount
	payoutByteCount        ByteCount
	returnByteCount        ByteCount
	payout                 NanoCents
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
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
            UPDATE transfer_contract
            SET
                dispute = $2,
                close_time = $3
            WHERE
                contract_id = $1 AND
                outcome IS NULL
        `,
		contractId,
		dispute,
		server.NowUtc(),
	))
	return tag.RowsAffected() == 1
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
                    transfer_contract.open = true AND
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
                    transfer_contract.open = true AND (
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
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("Done.")
		default:
		}
		c, err := ForceCloseOpenContractIds(ctx, minTime, 1000, 1, 0, 0)
		if err != nil {
			return err
		}
		if c == 0 {
			return nil
		}
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

// closes all open contracts with no update in the last `timeout`
// cases handled:
// - no closes
// - single close
// - one or more checkpoints
// - dispute (settled with both sides accepted)
func ForceCloseOpenContractIds(
	ctx context.Context,
	minTime time.Time,
	maxCount int,
	parallel int,
	blockSize int,
	blockIndex int,
) (
	closeCount int64,
	err error,
) {
	if parallel <= 0 {
		return 0, fmt.Errorf("force close parallelism must be positive: %d", parallel)
	}

	/*
		// force close contracts where there is nothing to do
		server.Tx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE transfer_contract
				SET
			        outcome = $5,
			        close_time = $6
				FROM (
					SELECT
			            t.contract_id

			        FROM (
			            SELECT
			                transfer_contract.contract_id,
			                transfer_contract.source_id,
			                transfer_contract.destination_id

			            FROM transfer_contract

			            WHERE
			                transfer_contract.open AND
			                transfer_contract.create_time <= $3

			            LIMIT $4

			        ) t

			        LEFT JOIN contract_close source_contract_close ON
			            source_contract_close.contract_id = t.contract_id AND
			            source_contract_close.party = $1

			        LEFT JOIN contract_close destination_contract_close ON
			            destination_contract_close.contract_id = t.contract_id AND
			            destination_contract_close.party = $2

			        WHERE
			        	destination_contract_close.contract_id IS NOT NULL AND
			        	source_contract_close.contract_id IS NOT NULL
			    ) t

			    WHERE
			        transfer_contract.contract_id = t.contract_id

				`,
				ContractPartySource,
				ContractPartyDestination,
				minTime.UTC(),
				maxCount,
				ContractOutcomeSettled,
				server.NowUtc(),
			))

			if c := tag.RowsAffected(); 0 < c {
				glog.Infof("[sm]force closed %d malformed contracts\n", c)
				closeCount += c
			}
		})
	*/

	type OpenContract struct {
		contractId    server.Id
		sourceId      server.Id
		destinationId server.Id
		dispute       bool

		sourceCloseTime             *time.Time
		sourceUsedTransferByteCount *ByteCount
		sourceCheckpoint            *bool

		destinationCloseTime             *time.Time
		destinationUsedTransferByteCount *ByteCount
		destinationCheckpoint            *bool
	}

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

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    t.contract_id,
                    t.source_id,
                    t.destination_id,
                    t.dispute,

                    source_contract_close.close_time AS source_close_time,
                    source_contract_close.used_transfer_byte_count AS source_used_transfer_byte_count,
                    source_contract_close.checkpoint AS source_checkpoint,

                    destination_contract_close.close_time AS destination_close_time,
                    destination_contract_close.used_transfer_byte_count AS destination_used_transfer_byte_count,
                    destination_contract_close.checkpoint AS destination_checkpoint

                FROM (
                    SELECT
                        transfer_contract.contract_id,
                        transfer_contract.source_id,
                        transfer_contract.destination_id,
                        transfer_contract.dispute

                    FROM transfer_contract

                    WHERE
                        transfer_contract.open AND
                        transfer_contract.create_time <= $3

                    ORDER BY transfer_contract.create_time

                    LIMIT $4
                ) t

                LEFT JOIN contract_close source_contract_close ON
                    source_contract_close.contract_id = t.contract_id AND
                    source_contract_close.party = $1

                LEFT JOIN contract_close destination_contract_close ON
                    destination_contract_close.contract_id = t.contract_id AND
                    destination_contract_close.party = $2

            `,
			ContractPartySource,
			ContractPartyDestination,
			minTime.UTC(),
			maxCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				openContract := &OpenContract{}

				server.Raise(result.Scan(
					&openContract.contractId,
					&openContract.sourceId,
					&openContract.destinationId,
					&openContract.dispute,
					&openContract.sourceCloseTime,
					&openContract.sourceUsedTransferByteCount,
					&openContract.sourceCheckpoint,
					&openContract.destinationCloseTime,
					&openContract.destinationUsedTransferByteCount,
					&openContract.destinationCheckpoint,
				))

				appendBlockOpenContract(openContract)
			}
		})
	})

	openContractCount := len(openContracts)

	// settle expired disputes
	// a disputed contract is not `open` (`open` is generated as
	// `dispute = false AND outcome IS NULL`), so scan for disputes separately
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
                SELECT
                    contract_id,
                    source_id,
                    destination_id
                FROM transfer_contract
                WHERE
                    dispute AND
                    outcome IS NULL AND
                    create_time <= $1
                ORDER BY create_time
                LIMIT $2
            `,
			minTime.UTC(),
			maxCount,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				openContract := &OpenContract{
					dispute: true,
				}
				server.Raise(result.Scan(
					&openContract.contractId,
					&openContract.sourceId,
					&openContract.destinationId,
				))
				appendBlockOpenContract(openContract)
			}
		})
	})

	glog.Infof("[sm]found %d contracts to close (%d disputes)\n", len(openContracts), len(openContracts)-openContractCount)

	// quarantine a contract that cannot be settled by marking it settled
	// without settling the escrow.
	// `outcome IS NULL` so that a concurrent close/settle is not overwritten.
	// `dispute = false` so that a contract that entered dispute mid-close is
	// left for the dispute scan to settle correctly on a later pass.
	closeMalformedContract := func(tag string, openContract *OpenContract, err error) {
		glog.Infof("%sforce close malformed contract: %s\n", tag, err)

		claimed := false
		server.Tx(ctx, func(tx server.PgTx) {
			commandTag := server.RaisePgResult(tx.Exec(
				ctx,
				`
                    UPDATE transfer_contract
                    SET
                        outcome = $2,
                        close_time = $3
                    WHERE
                        contract_id = $1 AND
                        outcome IS NULL AND
                        dispute = false
                `,
				openContract.contractId,
				ContractOutcomeSettled,
				server.NowUtc(),
			))
			claimed = commandTag.RowsAffected() == 1
		}, server.TxReadCommitted)

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
	}

	closeContract := func(tag string, openContract *OpenContract) error {
		if openContract.dispute {
			// todo: improve this with better detection of th eroot causes
			forceCloseContractCounter.WithLabelValues("dispute_both_sides").Inc()
			if glog.V(1) {
				glog.Infof("%ssettle contract dispute: both sides\n", tag)
			}
			var posts []func() any
			var err error
			server.Tx(ctx, func(tx server.PgTx) {
				setContractDisputeInTx(ctx, tx, openContract.contractId, false)
				posts, _, err = settleEscrowInTx(ctx, tx, openContract.contractId, ContractOutcomeSettled)
			}, server.TxReadCommitted)
			if err != nil {
				return err
			}
			server.RunPosts(ctx, posts...)

		} else if openContract.sourceCloseTime == nil && openContract.destinationCloseTime == nil {
			// close with both sides 0
			recordForceCloseContract("both sides", tag)

			err := CloseContract(
				ctx,
				openContract.contractId,
				openContract.sourceId,
				ByteCount(0),
				false,
			)
			if err != nil {
				return err
			}

			err = CloseContract(
				ctx,
				openContract.contractId,
				openContract.destinationId,
				ByteCount(0),
				false,
			)
			if err != nil {
				return err
			}

		} else if openContract.sourceCloseTime == nil {
			// Source accepts destination. A lone destination checkpoint must
			// also be made final; adding the missing source close alone leaves
			// one checkpoint row and therefore cannot settle the contract.
			recordForceCloseContract("source accepts destination", tag)

			err := CloseContract(
				ctx,
				openContract.contractId,
				openContract.sourceId,
				*openContract.destinationUsedTransferByteCount,
				false,
			)
			if err != nil {
				return err
			}
			if *openContract.destinationCheckpoint {
				err = CloseContract(
					ctx,
					openContract.contractId,
					openContract.destinationId,
					ByteCount(0),
					false,
				)
				if err != nil {
					return err
				}
			}

		} else if openContract.destinationCloseTime == nil {
			// Destination accepts source. Mirror the checkpoint finalization
			// above so either one-sided orientation converges in one sweep.
			recordForceCloseContract("destination accepts source", tag)

			err := CloseContract(
				ctx,
				openContract.contractId,
				openContract.destinationId,
				*openContract.sourceUsedTransferByteCount,
				false,
			)
			if err != nil {
				return err
			}
			if *openContract.sourceCheckpoint {
				err = CloseContract(
					ctx,
					openContract.contractId,
					openContract.sourceId,
					ByteCount(0),
					false,
				)
				if err != nil {
					return err
				}
			}

		} else if *openContract.sourceCheckpoint || *openContract.destinationCheckpoint {
			// finalize one or more checkpoints

			if *openContract.sourceCheckpoint {
				recordForceCloseContract("finalize source checkpoint", tag)

				err := CloseContract(
					ctx,
					openContract.contractId,
					openContract.sourceId,
					ByteCount(0),
					false,
				)
				if err != nil {
					return err
				}
			}

			if *openContract.destinationCheckpoint {
				recordForceCloseContract("finalize destination checkpoint", tag)
				err := CloseContract(
					ctx,
					openContract.contractId,
					openContract.destinationId,
					ByteCount(0),
					false,
				)
				if err != nil {
					return err
				}
			}

		} else {
			// nothing to settle, just close the transaction
			var posts []func() any
			var err error
			server.Tx(ctx, func(tx server.PgTx) {
				posts, _, err = settleEscrowInTx(ctx, tx, openContract.contractId, ContractOutcomeSettled)
			}, server.TxReadCommitted)
			if err != nil {
				return err
			}
			server.RunPosts(ctx, posts...)
		}

		return nil
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

	removeFinalizedContractFromStream := func(openContract *OpenContract) error {
		found := false
		finalized := false
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
                    SELECT outcome IS NOT NULL
                    FROM transfer_contract
                    WHERE contract_id = $1
                `,
				openContract.contractId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					found = true
					server.Raise(result.Scan(&finalized))
				}
			})
		})
		if !found {
			return fmt.Errorf("contract disappeared before force-close verification")
		}
		if !finalized {
			return fmt.Errorf("contract remained non-final after force-close attempt")
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

	contractErrors := make([]error, len(openContracts))
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
					closeErr := runForceClose(func() error {
						return closeContract(tag, openContract)
					})
					if closeErr != nil {
						quarantineErr := runForceClose(func() error {
							closeMalformedContract(tag, openContract, closeErr)
							return nil
						})
						closeErr = errors.Join(closeErr, quarantineErr)
					}

					streamErr := runForceClose(func() error {
						return removeFinalizedContractFromStream(openContract)
					})
					contractErrors[j] = errors.Join(closeErr, streamErr)
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

	closeCount += int64(len(openContracts))
	for index, contractErr := range contractErrors {
		if contractErr != nil {
			err = errors.Join(err, fmt.Errorf("force close contract %s at index %d: %w", openContracts[index].contractId, index, contractErr))
		}
	}
	for workerErr := range workerErrors {
		err = errors.Join(err, fmt.Errorf("force close worker: %w", workerErr))
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		err = errors.Join(err, ctxErr)
	}

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

		tx.Exec(
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
		)

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

func AddSubscriptionRenewalInTx(tx server.PgTx, ctx context.Context, renewal *SubscriptionRenewal) (returnErr error) {
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

	if err != nil {
		returnErr = err
	}

	return
}

func AddSubscriptionRenewal(ctx context.Context, renewal *SubscriptionRenewal) (returnErr error) {

	server.Tx(ctx, func(tx server.PgTx) {

		returnErr = AddSubscriptionRenewalInTx(tx, ctx, renewal)

	})

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
		result, err := conn.Query(
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
	})
	return active, market
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
		                    pro
		                )
		                VALUES ($1, $2, $3, $4, $5, $6, $7, $5, true)
		            `,
					server.NewId(),
					networkId,
					startTime,
					endTime,
					balanceByteCount,
					NanoCents(0),
					subsidyNetRevenue,
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
		                    pro
		                )
		                VALUES ($1, $2, $3, $4, $5, $6, $7, $5, false)
		            `,
					server.NewId(),
					networkId,
					startTime,
					endTime,
					byteCount,
					NanoCents(0),
					NanoCents(0),
				)
				addedTransferBalances[networkId] = byteCount
			}
		})
	})

	return
}

func RemoveCompletedContracts(ctx context.Context, minTime time.Time) {
	maxRowCount := 50000

	var balanceIds []server.Id

	server.MaintenanceTx(ctx, func(tx server.PgTx) {

		// remove completed transfer balances
		result, err := tx.Query(
			ctx,
			`
			DELETE FROM transfer_balance
			WHERE
				end_time <= $1
			RETURNING balance_id
			`,
			minTime.UTC(),
		)
		balanceIds = []server.Id{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var balanceId server.Id
				server.Raise(result.Scan(&balanceId))
				balanceIds = append(balanceIds, balanceId)
			}
		})

	}, server.TxReadCommitted)

	server.Redis(ctx, func(r server.RedisClient) {
		// per-balance hash tags (different slots): plain pipeline auto-routes
		r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, balanceId := range balanceIds {
				pipe.Del(ctx, netEscrowKey(balanceId))
			}
			return nil
		})
	})

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
						transfer_contract.reap_time < $1
					ORDER BY transfer_contract.reap_time
					LIMIT $3
				), protected AS MATERIALIZED (
					SELECT due.contract_id
					FROM due
					WHERE
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
				), deleted_contract AS (
					DELETE FROM transfer_contract
					USING candidate
					WHERE transfer_contract.contract_id = candidate.contract_id
				)
				SELECT COUNT(*) FROM due
				`,
				minTime.UTC(),
				minStragglerCreateTime.UTC(),
				maxRowCount,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&processedCount))
				}
			})
		}, server.TxReadCommitted)
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

// assignStragglerReapTimeBatches stamps reap_time = now() on closed contracts
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
				`
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
				SET reap_time = now()
				FROM batch
				WHERE transfer_contract.contract_id = batch.contract_id
				`,
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
// transfer_escrow_sweep rows whose transfer_contract no longer exists.
// RemoveCompletedContracts cascades these atomically with the contract delete,
// so this is a low-cadence safety net for orphans left by older releases or
// interrupted statements, not the primary cleanup mechanism. Each table is
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

// sweepOrphanContractSteps is the ordered list of child tables the contract
// sweep pages. The order is part of the persisted cursor (SweepOrphanCursor.Step
// indexes it), so inserting or removing a step invalidates in-flight cursors —
// sweepOrphanSteps restarts the pass rather than skipping a table when the
// persisted step is out of range.
func sweepOrphanContractSteps() []sweepOrphanStep {
	return []sweepOrphanStep{
		// contract_close, keyed by (contract_id, party)
		{
			table: "contract_close",
			newCursorTargets: func() []any {
				return []any{new(server.Id), new(string)}
			},
			sql: `
		WITH slice AS (
			SELECT contract_id, party
			FROM contract_close
			WHERE ($1 OR (contract_id, party) > ($2, $3))
			ORDER BY contract_id, party
			LIMIT $4
		), del AS (
			DELETE FROM contract_close
			USING slice
			WHERE
				contract_close.contract_id = slice.contract_id AND
				contract_close.party = slice.party AND
				NOT EXISTS (
					SELECT 1 FROM transfer_contract
					WHERE transfer_contract.contract_id = contract_close.contract_id
				)
			RETURNING 1
		), bound AS (
			SELECT contract_id, party
			FROM slice
			ORDER BY contract_id DESC, party DESC
			LIMIT 1
		)
		SELECT
			(SELECT count(*) FROM slice),
			(SELECT count(*) FROM del),
			bound.contract_id, bound.party
		FROM bound
		`,
		},

		// transfer_escrow, keyed by (contract_id, balance_id)
		{
			table: "transfer_escrow",
			newCursorTargets: func() []any {
				return []any{new(server.Id), new(server.Id)}
			},
			sql: `
		WITH slice AS (
			SELECT contract_id, balance_id
			FROM transfer_escrow
			WHERE ($1 OR (contract_id, balance_id) > ($2, $3))
			ORDER BY contract_id, balance_id
			LIMIT $4
		), del AS (
			DELETE FROM transfer_escrow
			USING slice
			WHERE
				transfer_escrow.contract_id = slice.contract_id AND
				transfer_escrow.balance_id = slice.balance_id AND
				NOT EXISTS (
					SELECT 1 FROM transfer_contract
					WHERE transfer_contract.contract_id = transfer_escrow.contract_id
				)
			RETURNING 1
		), bound AS (
			SELECT contract_id, balance_id
			FROM slice
			ORDER BY contract_id DESC, balance_id DESC
			LIMIT 1
		)
		SELECT
			(SELECT count(*) FROM slice),
			(SELECT count(*) FROM del),
			bound.contract_id, bound.balance_id
		FROM bound
		`,
		},

		// transfer_escrow_sweep, keyed by (contract_id, balance_id, network_id)
		{
			table: "transfer_escrow_sweep",
			newCursorTargets: func() []any {
				return []any{new(server.Id), new(server.Id), new(server.Id)}
			},
			sql: `
		WITH slice AS (
			SELECT contract_id, balance_id, network_id
			FROM transfer_escrow_sweep
			WHERE ($1 OR (contract_id, balance_id, network_id) > ($2, $3, $4))
			ORDER BY contract_id, balance_id, network_id
			LIMIT $5
		), del AS (
			DELETE FROM transfer_escrow_sweep
			USING slice
			WHERE
				transfer_escrow_sweep.contract_id = slice.contract_id AND
				transfer_escrow_sweep.balance_id = slice.balance_id AND
				transfer_escrow_sweep.network_id = slice.network_id AND
				NOT EXISTS (
					SELECT 1 FROM transfer_contract
					WHERE transfer_contract.contract_id = transfer_escrow_sweep.contract_id
				)
			RETURNING 1
		), bound AS (
			SELECT contract_id, balance_id, network_id
			FROM slice
			ORDER BY contract_id DESC, balance_id DESC, network_id DESC
			LIMIT 1
		)
		SELECT
			(SELECT count(*) FROM slice),
			(SELECT count(*) FROM del),
			bound.contract_id, bound.balance_id, bound.network_id
		FROM bound
		`,
		},
	}
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

// sweepOrphanStep is one child table's paged orphan sweep: the slice statement
// (shape documented on sweepOrphanCursor) and a source of fresh typed pointers
// for its key columns.
type sweepOrphanStep struct {
	table            string
	sql              string
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

// sweepOrphanCursor pages a child table by its primary key in fixed-size slices
// and deletes orphan rows (parent gone) within each slice, returning the total
// removed. Paging by the key (WHERE key > cursor ORDER BY key LIMIT sliceSize)
// means every statement scans a BOUNDED slice of the table, even when orphans
// are rare (steady state ~= 0). This is the fix for the incident pattern of the
// old "DELETE ... USING (SELECT ... WHERE NOT EXISTS(parent) LIMIT n)" sweep: a
// bare LIMIT can only stop once it has FOUND n orphans, so with no orphans it
// scanned the entire child table every call. Each slice runs in its own
// maintenance tx (server.MaintenanceTx), so no single statement holds a long
// lock; a full call still pages the whole table one bounded slice at a time.
//
// sql must be a single statement parameterized as:
//
//	$1           bool, true only for the first slice (disables the lower bound)
//	$2..$(k+1)   the cursor: the previous slice's max key columns, in key order
//	$(k+2)       the slice size
//
// and it must return exactly one row when the slice is non-empty:
//
//	(slice row count int8, deleted row count int8, max key columns...)
//
// or no rows when the slice is empty (the table has been fully paged). The
// canonical shape (single-uuid key, see the composite variants at the call
// sites) is:
//
//	WITH slice AS (
//	    SELECT pk FROM child
//	    WHERE ($1 OR pk > $2)
//	    ORDER BY pk LIMIT $3
//	), del AS (
//	    DELETE FROM child USING slice
//	    WHERE child.pk = slice.pk
//	      AND NOT EXISTS (SELECT 1 FROM parent WHERE parent.fk = child.fk)
//	    RETURNING 1
//	), bound AS (
//	    SELECT pk FROM slice ORDER BY pk DESC LIMIT 1
//	)
//	SELECT (SELECT count(*) FROM slice), (SELECT count(*) FROM del), bound.pk
//	FROM bound
//
// bound reads slice (materialized once, pre-delete), so the cursor advances past
// every row the slice examined — deleted or not — and no row is skipped at a
// slice boundary. newCursorTargets returns k fresh pointers to scan the max key
// columns into; their dereferenced values become the next slice's cursor.
//
// startKey resumes an earlier call at that key (nil starts at the head of the
// table). Paging stops when the table is fully paged (done) or maxRowCount rows
// have been examined, whichever comes first; when it stops early, endKey is the
// resume point. maxRowCount <= 0 pages to the end.
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
		args := make([]any, 0, len(cursor)+2)
		args = append(args, firstSlice)
		args = append(args, cursor...)
		args = append(args, sliceSize)

		var sliceCount, deletedCount int64
		targets := step.newCursorTargets()
		gotRow := false
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// reset in case the tx is retried on a transient error
			sliceCount = 0
			deletedCount = 0
			gotRow = false
			scanTargets := make([]any, 0, len(targets)+2)
			scanTargets = append(scanTargets, &sliceCount, &deletedCount)
			scanTargets = append(scanTargets, targets...)
			result, err := tx.Query(ctx, step.sql, args...)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(scanTargets...))
					gotRow = true
				}
			})
		}, server.TxReadCommitted)

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

// BackfillStragglerContractReapTime seeds reap_time = now() on existing aged
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
			    open = TRUE
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
