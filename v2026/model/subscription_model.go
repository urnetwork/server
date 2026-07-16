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

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
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

// closed contracts that have not been bundled into a completed payout after
// this long will not be paid; `RemoveCompletedContracts` hard deletes them
const StragglerContractExpiration = 90 * 24 * time.Hour

// completed contracts are reaped this long after their payment completes
// (reap_time = complete_time + CompletedContractExpiration, set in CompletePayment)
const CompletedContractExpiration = 7 * 24 * time.Hour

// per-call wall-clock budget for the contract reaper's assign + delete passes.
// Bounds each RemoveCompletedContracts run so a large one-time backlog drains
// over many 30-min runs instead of one unbounded run; steady state finishes well
// under it. A var (not const) so tests can inject a tiny budget to exercise the
// mid-backlog stop.
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
func netEscrowKey(balanceId server.Id) string {
	return fmt.Sprintf("{escrow_%s}net", balanceId)
}

// netEscrowEndTimeSlack extends the counter ttl past the balance `end_time`
// at the escrow-creation write, which knows the end time. The counter is only
// meaningful while the balance is active; the slack covers contracts that
// straddle the end of the balance window.
const netEscrowEndTimeSlack = 30 * 24 * time.Hour

// netEscrowFallbackTtl bounds counters touched at write sites that do not
// know the balance `end_time` (the reconcile `Set`, and a `DecrBy` that
// recreates a missing key). The reconcile task revisits every active balance
// and refreshes this, and the escrow-creation write restores the precise
// end-time deadline, so the fallback only has to outlast the gap between
// those writes.
const netEscrowFallbackTtl = 90 * 24 * time.Hour

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
		r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, transferBalance := range transferBalances {
				netEscrowCmds[transferBalance.BalanceId] = pipe.Get(ctx, netEscrowKey(transferBalance.BalanceId))
			}
			return nil
		})
		for _, transferBalance := range transferBalances {
			netEscrowCmd := netEscrowCmds[transferBalance.BalanceId]
			netEscrowBalanceByteCount, _ := netEscrowCmd.Int()
			netEscrowBalanceByteCount = max(0, netEscrowBalanceByteCount)
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
// true, resets them to it -- correcting accumulated drift. It returns the drift
// it found per network either way.
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
// When apply is true the counter is written with SET, so a concurrent
// `IncrBy`/`DecrBy` in the small window between the postgres read and the redis
// write can be lost; the next run re-derives the value and converges. The
// lost-write bias is toward a lower counter (more available), the same fail-open
// direction as a missing counter (see `TestProxyContractRedisMirrorLoss`). When
// apply is false the counters are only read (a dry run): the returned drift
// reports the accumulated error without changing anything.
//
// Drift is the signed difference (previous counter minus reconciled value)
// summed per network. Positive drift means the counter was over-reserved -- the
// direction that starves the available balance and produces spurious
// "Insufficient balance". Only networks with nonzero net drift are returned.
func ReconcileNetEscrow(ctx context.Context, apply bool) (driftByNetworkId map[server.Id]ByteCount, balanceCount int) {
	now := server.NowUtc()
	driftByNetworkId = map[server.Id]ByteCount{}

	pending := openEscrowReservedByBalance(ctx, nil)

	// visit every active balance -- the same set `createTransferEscrowInTx`
	// reads -- paginated by balance_id (the primary key) so the scan is bounded
	// and resumable
	const batchSize = 1000
	var cursor server.Id
	for {
		type balanceRow struct {
			balanceId server.Id
			networkId server.Id
		}
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

// ReconcileNetEscrowForNetwork reconciles the redis net escrow counters for one
// network's active balances. See [ReconcileNetEscrow]; this is the targeted form
// used to immediately clear (or, with apply false, just measure) drift on a
// single affected network. The returned drift is the signed total over the
// network's balances (previous counters minus reconciled values).
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
	if len(balanceIds) == 0 {
		return
	}

	pending := openEscrowReservedByBalance(ctx, &networkId)
	drift := reconcileNetEscrowBatch(ctx, pending, balanceIds, apply)
	for _, d := range drift {
		driftByteCount += d
	}
	return driftByteCount, len(balanceIds)
}

// openEscrowReservedByBalance returns the reserved (open-contract) escrow bytes
// per balance, the source of truth for the net escrow counter. When networkId
// is non-nil the scan is restricted to that network's balances.
func openEscrowReservedByBalance(ctx context.Context, networkId *server.Id) map[server.Id]ByteCount {
	pending := map[server.Id]ByteCount{}
	server.Db(ctx, func(conn server.PgConn) {
		var result server.PgResult
		var err error
		if networkId == nil {
			result, err = conn.Query(
				ctx,
				`
                    SELECT
                        transfer_escrow.balance_id,
                        SUM(transfer_escrow.balance_byte_count)
                    FROM transfer_escrow
                    INNER JOIN transfer_contract ON
                        transfer_contract.contract_id = transfer_escrow.contract_id
                    WHERE
                        transfer_contract.outcome IS NULL
                    GROUP BY transfer_escrow.balance_id
                `,
			)
		} else {
			result, err = conn.Query(
				ctx,
				`
                    SELECT
                        transfer_escrow.balance_id,
                        SUM(transfer_escrow.balance_byte_count)
                    FROM transfer_escrow
                    INNER JOIN transfer_contract ON
                        transfer_contract.contract_id = transfer_escrow.contract_id
                    INNER JOIN transfer_balance ON
                        transfer_balance.balance_id = transfer_escrow.balance_id
                    WHERE
                        transfer_contract.outcome IS NULL AND
                        transfer_balance.network_id = $1
                    GROUP BY transfer_escrow.balance_id
                `,
				*networkId,
			)
		}
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
// then resets each counter to its reserved value -- zero (no live reservation)
// deletes the key so it reads as zero, matching the "missing counter is zero"
// invariant.
func reconcileNetEscrowBatch(
	ctx context.Context,
	pending map[server.Id]ByteCount,
	balanceIds []server.Id,
	apply bool,
) (drift map[server.Id]ByteCount) {
	drift = map[server.Id]ByteCount{}
	server.Redis(ctx, func(r server.RedisClient) {
		// the net escrow keys use per-balance hash tags (different slots), so
		// use plain pipelines, which auto-route per slot on cluster
		getCmds := map[server.Id]*redis.StringCmd{}
		r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, balanceId := range balanceIds {
				getCmds[balanceId] = pipe.Get(ctx, netEscrowKey(balanceId))
			}
			return nil
		})
		for _, balanceId := range balanceIds {
			previous, _ := getCmds[balanceId].Int64() // missing key -> 0
			drift[balanceId] = ByteCount(previous) - pending[balanceId]
		}

		if !apply {
			return
		}

		r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, balanceId := range balanceIds {
				if reserved := pending[balanceId]; 0 < reserved {
					// the reconcile does not know the balance end time; the
					// fallback ttl is refreshed on every apply and the
					// escrow-creation write restores the end-time deadline
					pipe.Set(ctx, netEscrowKey(balanceId), reserved, netEscrowFallbackTtl)
				} else {
					pipe.Del(ctx, netEscrowKey(balanceId))
				}
			}
			return nil
		})
	})
	return
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
	server.Redis(ctx, func(r server.RedisClient) {
		// per-balance hash tags (different slots): plain pipeline auto-routes
		r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for balanceId, byteCount := range escrowed {
				key := netEscrowKey(balanceId)
				pipe.DecrBy(ctx, key, byteCount)
				// a decr that recreates a missing key must not leave it
				// without a ttl; nx never shortens the end-time deadline
				pipe.ExpireNX(ctx, key, netEscrowFallbackTtl)
			}
			return nil
		})
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
		r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, transferBalance := range orderedTransferBalances {
				netEscrowCmds[transferBalance.balanceId] = pipe.Get(ctx, netEscrowKey(transferBalance.balanceId))
			}
			return nil
		})
		for _, transferBalance := range orderedTransferBalances {
			netEscrowCmd := netEscrowCmds[transferBalance.balanceId]
			netEscrowBalanceByteCount, _ := netEscrowCmd.Int()
			netEscrowBalanceByteCount = max(0, netEscrowBalanceByteCount)
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
		server.Redis(ctx, func(r server.RedisClient) {
			// per-balance hash tags (different slots): plain pipeline auto-routes
			r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for balanceId, escrow := range balanceEscrows {
					key := netEscrowKey(balanceId)
					pipe.IncrBy(ctx, key, escrow.balanceByteCount)
					// the counter is only meaningful while the balance is
					// active; pin the ttl to the balance end time plus slack
					// (non-nx, so it also corrects a shorter fallback ttl)
					pipe.ExpireAt(ctx, key, escrow.endTime.Add(netEscrowEndTimeSlack))
				}
				return nil
			})
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
			returnErr = fmt.Errorf("Missing origin contract for companion.")
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
			}
		}
	}, server.TxReadCommitted)

	if returnErr != nil {
		return
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
			}
		})
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
			server.Redis(ctx, func(r server.RedisClient) {
				// per-balance hash tags (different slots): plain pipeline auto-routes
				r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
					for balanceId, sweepPayout := range sweepPayouts {
						key := netEscrowKey(balanceId)
						pipe.DecrBy(ctx, key, sweepPayout.escrowBalanceByteCount)
						// a decr that recreates a missing key must not leave
						// it without a ttl; nx never shortens the end-time
						// deadline
						pipe.ExpireNX(ctx, key, netEscrowFallbackTtl)
					}
					return nil
				})
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
	// cooperatively partition contracts across the block tasks
	appendBlockOpenContract := func(openContract *OpenContract) {
		if 0 < blockSize && int(openContract.contractId.Hash()%uint64(blockSize)) != blockIndex%blockSize {
			return
		}
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
			releaseNetEscrowForContract(ctx, openContract.contractId)
		}
	}

	closeContract := func(tag string, openContract *OpenContract) error {
		if openContract.dispute {
			// todo: improve this with better detection of th eroot causes
			glog.Infof("%ssettle contract dispute: both sides\n", tag)
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
			glog.Infof("%sforce close contract: both sides\n", tag)

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
			// source accepts destination
			glog.Infof("%sforce close contract: source accepts destination\n", tag)

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

		} else if openContract.destinationCloseTime == nil {
			// destination accepts source
			glog.Infof("%sforce close contract: destination accepts source\n", tag)

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

		} else if *openContract.sourceCheckpoint || *openContract.destinationCheckpoint {
			// finalize one or more checkpoints

			if *openContract.sourceCheckpoint {
				glog.Infof("%sforce close contract: finalize source checkpoint\n", tag)

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
				glog.Infof("%sforce close contract: finalize destination checkpoint\n", tag)
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

	nextIndex := 0
	var nextIndexLock sync.Mutex
	getAndIncrNextIndex := func() int {
		nextIndexLock.Lock()
		defer nextIndexLock.Unlock()

		i := nextIndex
		nextIndex += 1
		return i
	}

	var wg sync.WaitGroup

	for range parallel {
		wg.Add(1)
		go server.HandleError(func() {
			defer wg.Done()
			for j := getAndIncrNextIndex(); j < len(openContracts); j = getAndIncrNextIndex() {
				select {
				case <-ctx.Done():
					return
				default:
				}

				server.HandleError(func() {
					openContract := openContracts[j]
					tag := fmt.Sprintf("[sm][%s][%d/%d]", openContract.contractId, j+1, len(openContracts))
					err := closeContract(tag, openContracts[j])
					if err != nil {
						// glog.Infof("%sforce close contract err = %s\n", tag, err)
						closeMalformedContract(tag, openContract, err)
					}
				})
			}
		})
	}

	wg.Wait()

	closeCount += int64(len(openContracts))

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
			ON CONFLICT (network_id, subscription_type, end_time, start_time) DO UPDATE
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
				supporters[networkId] = subsidyNetRevenue
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

	// The reaper is driven entirely by the indexed reap_time column, never an
	// anti-join or full scan. reap_time is the instant a contract becomes due for
	// hard deletion:
	//   - CompletePayment sets it to complete_time + CompletedContractExpiration
	//     for the payment's contracts (the completed-payout retention window),
	//   - the assign pass below stamps now() on aged closed-but-never-completed
	//     contracts (stragglers and sweep-less quarantine rows) so they too become
	//     due.
	// The delete pass then removes every contract whose reap_time has passed,
	// cascading contract_close/transfer_escrow/transfer_escrow_sweep for those same
	// contract ids in one statement so dependents never linger as orphans. Both
	// passes are bounded by a partial index and batched (one maintenance tx each,
	// no long lock), so a single run drains the eligible set regardless of cadence.
	//
	// This replaces three prior reaper blocks: the sweep-driven completed reaper,
	// the sweep-less reaper, and the straggler reaper. The last two ran a
	// non-selective, un-indexable anti-join over ~the whole old-closed contract
	// table (open = false is nearly every old contract; the sweep / completed-
	// payment anti-join can't be indexed on transfer_contract) with a LIMIT that
	// never early-terminates -- so every run walked the world and tanked the DB
	// (prod incident 2026-07-14). SweepOrphanContractData is the low-cadence safety
	// net for orphans left by any other path (e.g. crashes mid-statement in older
	// releases).

	// assign pass: give aged closed-but-never-completed contracts a reap_time so
	// the delete pass removes them. Bounded by the
	// transfer_contract_reap_pending_create_time partial index (reap_time IS NULL
	// AND close_time IS NOT NULL, ordered by create_time), so this is an index
	// range-scan, not the anti-join it replaces.
	assignStragglerReapTimeBatches(ctx, server.NowUtc().Add(-StragglerContractExpiration), maxRowCount)

	// delete pass: hard delete every contract whose reap_time is due, cascading
	// its dependent rows. Bounded by the transfer_contract_reap_time partial index.
	// This reaps both completed contracts (reap_time = complete_time +
	// CompletedContractExpiration, set at CompletePayment) and the stragglers just
	// assigned above. Candidate contract_ids are distinct (from the
	// transfer_contract primary key), so a batch deletes exactly its candidates;
	// removeContractBatches drains until an empty batch.
	removeContractBatches(
		ctx,
		`
			WITH candidate AS (
				SELECT transfer_contract.contract_id
				FROM transfer_contract
				WHERE transfer_contract.reap_time IS NOT NULL AND transfer_contract.reap_time < $1
				LIMIT $2
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
			)
			DELETE FROM transfer_contract
			USING candidate
			WHERE transfer_contract.contract_id = candidate.contract_id
			`,
		server.NowUtc(),
		maxRowCount,
	)
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
// that were never reaped (reap_time IS NULL) and are older than minCreateTime, so
// the reaper's delete pass removes them. This is the straggler + sweep-less
// cleanup: a closed contract whose payment never completed (or that was never
// swept at all) is never assigned a reap_time by CompletePayment, so it would
// otherwise live forever. Bounded by the transfer_contract_reap_pending_create_time
// partial index (reap_time IS NULL AND close_time IS NOT NULL), this is an index
// range-scan, not the anti-join full scan it replaces.
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
						transfer_contract.create_time < $1
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
// paged fully by its primary key in bounded sliceSize slices (see
// sweepOrphanCursor), so a call never full-scans a child table even when there
// are no orphans.
func SweepOrphanContractData(ctx context.Context, sliceSize int) (removedCount int64) {
	// contract_close, keyed by (contract_id, party)
	removedCount += sweepOrphanCursor(
		ctx,
		sliceSize,
		`
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
		func() []any { return []any{new(server.Id), new(string)} },
	)

	// transfer_escrow, keyed by (contract_id, balance_id)
	removedCount += sweepOrphanCursor(
		ctx,
		sliceSize,
		`
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
		func() []any { return []any{new(server.Id), new(server.Id)} },
	)

	// transfer_escrow_sweep, keyed by (contract_id, balance_id, network_id)
	removedCount += sweepOrphanCursor(
		ctx,
		sliceSize,
		`
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
		func() []any { return []any{new(server.Id), new(server.Id), new(server.Id)} },
	)

	return
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
func sweepOrphanCursor(
	ctx context.Context,
	sliceSize int,
	sql string,
	newCursorTargets func() []any,
) (removedCount int64) {
	// seed the cursor with the zero value of each key column; the first slice
	// passes firstSlice=true so the bound is ignored, then it advances to each
	// slice's max key.
	cursor := derefCursor(newCursorTargets())
	firstSlice := true
	for {
		args := make([]any, 0, len(cursor)+2)
		args = append(args, firstSlice)
		args = append(args, cursor...)
		args = append(args, sliceSize)

		var sliceCount, deletedCount int64
		targets := newCursorTargets()
		gotRow := false
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// reset in case the tx is retried on a transient error
			sliceCount = 0
			deletedCount = 0
			gotRow = false
			scanTargets := make([]any, 0, len(targets)+2)
			scanTargets = append(scanTargets, &sliceCount, &deletedCount)
			scanTargets = append(scanTargets, targets...)
			result, err := tx.Query(ctx, sql, args...)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(scanTargets...))
					gotRow = true
				}
			})
		}, server.TxReadCommitted)

		removedCount += deletedCount
		if !gotRow || sliceCount < int64(sliceSize) {
			return
		}
		cursor = derefCursor(targets)
		firstSlice = false
	}
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
// completed-payout window. New completions are stamped inline by CompletePayment;
// this is the one-time companion to the reap_time deploy. Idempotent: stamped
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
// original backfill); live CompletePayment converges new completions with LEAST.
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
