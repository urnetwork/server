// Current settlements append independent consumption records. PostgreSQL credit
// catches up in bounded batches; Redis continues to reserve the consumed bytes
// until a batch commits. Lost callbacks cannot erase or repeat a durable debit.
package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

const transferDebitBatchSize = 512
const TransferDebitShardCount = 16

// A balance with a full batch flushes again in the same page, up to this many
// batches, while the page has at least transferDebitRepeatMargin left. One
// very large payer's grant then releases its reservations at the rate it
// admits them instead of one batch per task claim; the cap and the margin keep
// the page's later balances and its deadline intact.
const transferDebitBatchesPerBalance = 16
const transferDebitRepeatMargin = 5 * time.Second

// The page may start another batch: it is live and, when it has a deadline,
// at least the margin remains.
func transferDebitPageHasTime(bounded context.Context) bool {
	if bounded.Err() != nil {
		return false
	}
	deadline, ok := bounded.Deadline()
	return !ok || transferDebitRepeatMargin <= time.Until(deadline)
}

var errTransferDebitPageDeadline = errors.New("transfer debit page deadline")

// A flush that finds its grant's owner held waits for it in the owner's lock
// queue (server.QueuedOwnedTx) instead of skipping the grant at once. A
// deadline close holds the owner for a whole turn, and its turns can follow
// each other with no gap, so a flush that only probed found the owner held on
// every pass and its grant's debits and reservations stayed pending for hours
// (2026-11-25). Queued, the flush is admitted when the current turn releases
// the owner, ahead of the next turn. It waits at most this long, for the
// apply and again for the cleanup, and only while the page keeps its reserve
// for the work after the wait; then it skips the grant as before.
const transferDebitOwnerWait = 5 * time.Second

// After a queued apply the page still needs time for the apply's statements,
// the Redis release and the cleanup; after a queued cleanup, for its delete.
const transferDebitApplyWaitReserve = transferDebitRepeatMargin
const transferDebitCleanupWaitReserve = time.Second

// A private test seam replaces transferDebitOwnerWait. Production contexts
// carry none.
type transferDebitOwnerWaitKey struct{}

// The bounded wait for a held owner at now: at most transferDebitOwnerWait,
// and none once the page has no more than reserve left.
func transferDebitOwnerWaitAt(ctx context.Context, reserve time.Duration, now time.Time) time.Duration {
	wait := transferDebitOwnerWait
	if override, ok := ctx.Value(transferDebitOwnerWaitKey{}).(time.Duration); ok {
		wait = override
	}
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, deadline.Sub(now)-reserve)
	}
	return max(0, wait)
}

// A queued admission when the bound allows a wait; otherwise one probe.
func transferDebitOwnedTx(ctx context.Context, balanceId server.Id, reserve time.Duration, callback func(server.PgTx), options ...any) bool {
	keys := transferBalanceOwnershipKeys([]server.Id{balanceId})
	if wait := transferDebitOwnerWaitAt(ctx, reserve, time.Now()); 0 < wait {
		return server.QueuedOwnedTx(ctx, keys[0], wait, callback, options...)
	}
	return server.TryOwnedTx(ctx, keys, callback, options...)
}

// The age of a shard's oldest journal row in one applied state, 0 for none. It
// reads transfer_debit_journal_shard_create_time oldest first and filters the
// state, since no index keys applied: marking a row applied stays HOT.
const transferDebitOldestAgeSql = `SELECT COALESCE((SELECT GREATEST(0,EXTRACT(epoch FROM (clock_timestamp() AT TIME ZONE 'UTC'-create_time)))::double precision FROM transfer_debit_journal WHERE shard=$1 AND applied=$2 ORDER BY create_time LIMIT 1),0)`

func transferDebitShard(balanceId server.Id) int { return int(balanceId[15]) % TransferDebitShardCount }

var transferDebitResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_transfer_debit_flush_total",
	Help: "Asynchronous payer debit batches by finite result; not contract throughput or provider payout.",
}, []string{"result"})

var transferDebitFlushPhaseErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_transfer_debit_flush_phase_errors_total",
	Help: "Recovered per-balance flush errors by last entered apply, Redis release or cleanup phase. A commit error leaves the durable outcome unknown.",
}, []string{"phase"})

var transferDebitOldestAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_transfer_debit_oldest_seconds", Help: "Oldest retained debit age by bounded worker partition; includes committed records awaiting Redis release.",
}, []string{"shard", "state"})
var transferDebitSampleTime = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_transfer_debit_sample_timestamp_seconds", Help: "Last successful oldest-debit observation, including empty partitions.",
}, []string{"shard"})

var transferDebitActive = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "urnetwork_transfer_debit_flush_active", Help: "Current bounded debit worker owners by partition and reporting process."}, []string{"shard"})
var transferDebitCompleteTime = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "urnetwork_transfer_debit_complete_timestamp_seconds", Help: "Last debit-worker completion without a failed balance; busy balances can remain pending."}, []string{"shard"})

func init() {
	prometheus.MustRegister(transferDebitResults, transferDebitFlushPhaseErrors, transferDebitOldestAge, transferDebitSampleTime, transferDebitActive, transferDebitCompleteTime)
}

type TransferDebitFlushResult struct {
	LastBalanceId *server.Id `json:"last_balance_id,omitempty"`
	Balances      int        `json:"balances"`
	Applied       int        `json:"applied"`
	Released      int        `json:"released"`
	Busy          int        `json:"busy"`
	Failed        int        `json:"failed"`
	More          bool       `json:"more"`
}

type transferDebit struct {
	contractId server.Id
	amount     ByteCount
	applied    bool
}

// Operations belong to one bounded traversal. The public worker supplies the
// real indexed lookup and durable financial owner; no shared mutable hooks exist.
type transferDebitPageOperations struct {
	nextBalance  func(context.Context, int, *server.Id) *server.Id
	flushBalance func(context.Context, server.Id) (int, int, bool, error)
}

// Advance by balance key rather than repeatedly retrying one busy oldest grant.
// Each next-key lookup is one index seek, each grant has one finite journal page.
// The task persists LastBalanceId; reaching the end restarts on the next cadence.
func FlushTransferDebits(ctx context.Context, shard int, after *server.Id, maxBalances int) (result TransferDebitFlushResult, returnErr error) {
	if shard < 0 || shard >= TransferDebitShardCount || maxBalances < 1 || maxBalances > 64 {
		return result, fmt.Errorf("invalid debit flush limit")
	}
	label := strconv.Itoa(shard)
	transferDebitActive.WithLabelValues(label).Inc()
	defer func() {
		transferDebitActive.WithLabelValues(label).Dec()
		if returnErr == nil && result.Failed == 0 {
			transferDebitCompleteTime.WithLabelValues(label).SetToCurrentTime()
		}
	}()
	bounded, cancel := context.WithTimeoutCause(ctx, 15*time.Second, errTransferDebitPageDeadline)
	defer cancel()
	server.HandleError(func() {
		server.Db(bounded, func(conn server.PgConn) {
			for _, state := range []struct {
				name    string
				applied bool
			}{{"pending", false}, {"redis_release", true}} {
				var age float64
				server.Raise(conn.QueryRow(bounded, transferDebitOldestAgeSql, shard, state.applied).Scan(&age))
				transferDebitOldestAge.WithLabelValues(label, state.name).Set(age)
			}
			transferDebitSampleTime.WithLabelValues(label).SetToCurrentTime()
		})
		var err error
		result, err = flushTransferDebitPage(ctx, bounded, shard, after, maxBalances, transferDebitPageOperations{
			nextBalance: nextTransferDebitBalance, flushBalance: flushTransferDebitBalance,
		})
		server.Raise(err)
	}, func(err error) { returnErr = err })
	if returnErr != nil {
		transferDebitResults.WithLabelValues("error").Inc()
	}
	return
}

// This indexed read has its own checkout; the previous balance's financial
// commit and Redis release have finished before the next key is requested.
func nextTransferDebitBalance(ctx context.Context, shard int, after *server.Id) (next *server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		query := `SELECT balance_id FROM transfer_debit_journal WHERE shard=$1 ORDER BY balance_id LIMIT 1`
		args := []any{shard}
		if after != nil {
			query = `SELECT balance_id FROM transfer_debit_journal WHERE shard=$1 AND balance_id>$2 ORDER BY balance_id LIMIT 1`
			args = append(args, *after)
		}
		rows, err := conn.Query(ctx, query, args...)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				next = &id
			}
		})
	})
	return
}

// Persist only visited balance keys. Per-balance failures retain their existing
// fair-cursor policy; the durable journal remains the accounting replay fence.
func flushTransferDebitPage(ctx, bounded context.Context, shard int, after *server.Id, maxBalances int,
	operations transferDebitPageOperations) (result TransferDebitFlushResult, returnErr error) {
	pageExpiredAfterProgress := func() bool {
		return result.LastBalanceId != nil && bounded.Err() != nil &&
			context.Cause(bounded) == errTransferDebitPageDeadline && ctx.Err() == nil
	}
	server.HandleError(func() {
		for range maxBalances {
			// The previous balance finished. An already-expired checkout
			// cannot visit another key; hand the cursor back to the task.
			if pageExpiredAfterProgress() {
				result.More = true
				return
			}
			next := operations.nextBalance(bounded, shard, after)
			if next == nil {
				result.LastBalanceId = nil
				return
			}
			applied, released, busy, err := operations.flushBalance(bounded, *next)
			// Repeat only after a full, released, uncontended batch.
			last := released
			for batches := 1; err == nil && !busy && last == transferDebitBatchSize &&
				batches < transferDebitBatchesPerBalance && transferDebitPageHasTime(bounded); batches++ {
				var batchApplied int
				batchApplied, last, busy, err = operations.flushBalance(bounded, *next)
				applied += batchApplied
				released += last
			}
			if err != nil {
				result.Failed++
				transferDebitResults.WithLabelValues("error").Inc()
			}
			if last == transferDebitBatchSize {
				result.More = true
			}
			result.Balances++
			result.Applied += applied
			result.Released += released
			if busy {
				result.Busy++
			}
			result.LastBalanceId = next
			after = next
			if err != nil {
				// Persist this cursor before another slow failure can consume the
				// owner deadline. The next cadence starts after the failed grant.
				if ctx.Err() != nil {
					server.Raise(ctx.Err())
				}
				return
			}
		}
		result.More = true
	}, func(err error) { returnErr = err })
	// A next-key checkout/read can exhaust the page after earlier balances
	// committed. Return that prefix for the task post instead of replaying its
	// old cursor through task failure backoff. No unvisited key is acknowledged.
	if returnErr != nil && pageExpiredAfterProgress() && isSettlementPageCancellation(returnErr) {
		result.More = true
		returnErr = nil
	}
	return
}

// Operation names are assigned by the flush itself. The transaction owner sets
// its phase at the actual boundary, including immediately before Commit.
func transferDebitFlushFailurePhase(operation string, phase server.DbOperationPhase) string {
	switch operation {
	case "redis_release":
		return "redis_release"
	case "apply", "cleanup":
	default:
		return "unknown"
	}
	switch phase {
	case server.DbOperationOwnershipConfiguration:
		return operation + "_configuration"
	case server.DbOperationOwnershipAcquire, server.DbOperationAcquire:
		return operation + "_acquire"
	case server.DbOperationAdmission:
		return operation + "_admission"
	case server.DbOperationSessionSetup:
		return operation + "_session_setup"
	case server.DbOperationBegin:
		return operation + "_begin"
	case server.DbOperationCallback:
		return operation + "_callback"
	case server.DbOperationCommit:
		return operation + "_commit"
	case server.DbOperationAcknowledged, server.DbOperationPostCommit:
		return operation + "_after_commit"
	default:
		return operation + "_unknown"
	}
}

// Admit common same-backend ownership before shared grant SQL, waiting in its
// queue for a bounded time; an owner still held leaves the journal for the
// next fair traversal. The wait holds no row or key, and both admitted
// transactions take the owner before the grant row, as every owner does, so a
// waiting flush cannot join a deadlock. SKIP LOCKED keeps the compatibility
// guard against older writers that lack that ownership.
// No Redis I/O occurs while PostgreSQL rows are locked. An ambiguous SQL commit
// is safe: a retry observes applied=true before attempting another debit.
func flushTransferDebitBalance(ctx context.Context, balanceId server.Id) (appliedCount, releasedCount int, busy bool, returnErr error) {
	var records []transferDebit
	committed := false
	operation := "apply"
	var phase server.DbPhaseObservation
	server.HandleError(func() {
		admitted := transferDebitOwnedTx(ctx, balanceId, transferDebitApplyWaitReserve, func(tx server.PgTx) {
			records = nil
			appliedCount = 0
			busy = false
			server.RaisePgResult(tx.Exec(ctx, `SET LOCAL statement_timeout='2s'; SET LOCAL lock_timeout='250ms'`))
			var found bool
			rows, err := tx.Query(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR NO KEY UPDATE SKIP LOCKED`, balanceId)
			server.WithPgResult(rows, err, func() { found = rows.Next() })
			if !found {
				// A deleted balance is an invariant failure, not permission to discard debt.
				var exists bool
				server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id=$1)`, balanceId).Scan(&exists))
				if !exists {
					server.Raise(fmt.Errorf("pending debit balance is missing"))
				}
				busy = true
				return
			}
			rows, err = tx.Query(ctx, `SELECT contract_id,debit_byte_count,applied FROM transfer_debit_journal
       WHERE balance_id=$1 ORDER BY contract_id LIMIT $2 FOR UPDATE SKIP LOCKED`, balanceId, transferDebitBatchSize)
			var amount ByteCount
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var record transferDebit
					server.Raise(rows.Scan(&record.contractId, &record.amount, &record.applied))
					if !record.applied {
						if record.amount > math.MaxInt64-amount {
							break
						}
						amount += record.amount
						appliedCount++
					}
					records = append(records, record)
				}
			})
			if len(records) == 0 {
				return
			}
			ids := make([]server.Id, 0, len(records))
			amounts := make([]ByteCount, 0, len(records))
			for _, record := range records {
				ids = append(ids, record.contractId)
				amounts = append(amounts, record.amount)
			}
			if amount > 0 {
				tag := server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET balance_byte_count=balance_byte_count-$2 WHERE balance_id=$1`, balanceId, amount))
				if tag.RowsAffected() != 1 {
					server.Raise(fmt.Errorf("debit balance disappeared"))
				}
			}
			// Repair a lost metadata post from durable consumption, independently of
			// payout eligibility. Retention may already have removed the contract row.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow AS escrow
       SET settled=true,settle_time=COALESCE(escrow.settle_time,clock_timestamp() AT TIME ZONE 'UTC'),payout_byte_count=entry.amount
       FROM unnest($2::uuid[],$3::bigint[]) AS entry(contract_id,amount)
       WHERE escrow.balance_id=$1 AND escrow.contract_id=entry.contract_id AND escrow.redis_reserved AND NOT escrow.settled`, balanceId, ids, amounts))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_debit_journal SET applied=true WHERE balance_id=$1 AND contract_id=ANY($2) AND NOT applied`, balanceId, ids))
		}, server.TxReadCommitted, server.OptNoRetry(), &phase)
		operation = ""
		committed = admitted
		if !admitted {
			busy = true
		}
		if busy {
			transferDebitResults.WithLabelValues("busy").Inc()
			return
		}
		if len(records) == 0 {
			return
		}
		transferDebitResults.WithLabelValues("committed").Inc()
		// A partially executed Redis pipeline is replayable. Keep ALL applied rows
		// until every release was acknowledged; the applied flag prevents re-debit.
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		// no preflight PING, as for redisContractReservation, and outside a transaction
		operation = "redis_release"
		server.Raise(server.RedisWithDeadline(bounded, func(r server.RedisClient) error {
			_, err := r.Pipelined(bounded, func(pipe redis.Pipeliner) error {
				for _, record := range records {
					pipe.Eval(bounded, redisContractReservationScript, redisContractReservationKeys(balanceId),
						"release", record.contractId.String(), "0", "0", redisContractReservationLease.Milliseconds())
				}
				return nil
			})
			return err
		}, server.OptRedisNoPreflight(), server.OptRedisKeepLateReplies()))
		operation = ""
		ids := make([]server.Id, 0, len(records))
		for _, record := range records {
			ids = append(ids, record.contractId)
		}
		// Another admitted pass may hold these applied rows after Redis release.
		// An owner still held after the bounded wait refuses cleanup SQL; the
		// retained journal makes the later release/deletion replay safe without
		// another financial debit.
		operation = "cleanup"
		phase = server.DbPhaseObservation{}
		var deletedCount int
		if !transferDebitOwnedTx(ctx, balanceId, transferDebitCleanupWaitReserve, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(ctx, `DELETE FROM transfer_debit_journal WHERE balance_id=$1 AND contract_id=ANY($2) AND applied`, balanceId, ids))
			deletedCount = int(tag.RowsAffected())
		}, server.TxReadCommitted, server.OptNoRetry(), &phase) {
			busy = true
			transferDebitResults.WithLabelValues("busy").Inc()
			return
		}
		releasedCount = deletedCount
		operation = ""
		transferDebitResults.WithLabelValues("released").Inc()
	}, func(err error) {
		returnErr = err
		transferDebitFlushPhaseErrors.WithLabelValues(transferDebitFlushFailurePhase(operation, phase.Phase())).Inc()
		if !committed {
			// An unacknowledged commit is reconciled by the next journal read.
			// It must not be reported as an observed committed debit.
			appliedCount = 0
		}
	})
	return
}
