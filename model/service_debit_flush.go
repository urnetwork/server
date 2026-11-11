package model

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// The service debit worker (DESIGN.md 20.6's "a FlushTransferDebits-style worker debits, no payout";
// D-QUOTA-4, D-QUOTA-8; QUOTA-M2-06), FlushTransferDebits step for step (subscription_transfer_debit.go):
// 16 shards by a balance's last byte, a bounded page by balance key from a persisted cursor, each
// balance flushed in its own transaction that probes the balance's ownership first (TryOwnedTx on
// transferBalanceOwnershipKeys) and locks it FOR NO KEY UPDATE SKIP LOCKED, so a busy balance is
// skipped and revisited, never waited on.
//
//	commit   one transaction subtracts the page's unapplied debits from balance_byte_count, reading
//	         the balance the debit left (RETURNING), adds each meter's bytes to network_service_usage
//	         for (network, the UTC day of this apply, meter), since a debit counts on the day it was
//	         applied (§20.9) and the journal has no time column (plan-QUOTA-overage-accounting), and
//	         sets applied
//	release  after the commit, each row's Redis token is released by release-debited, which lowers
//	         the balance's floor to what the debit left in the same script (DESIGN-DETAILS 13.2.2):
//	         svc/<usage_id> for a lease's carved row, the contract id, which is the usage id, for
//	         meter relay; evs's rows hold no token
//	delete   a second owned transaction deletes the applied rows
//
// Every applied row stays until all its releases are acknowledged: the applied flag stops a second
// debit, and a partly run pipeline is replayable, so a kill at any step reruns to the same end with
// nothing debited twice. A rerun that finds its rows applied releases them with the balance its own
// transaction read, which is at most what the earlier debit left, since a balance only falls: the
// floor is lowered too far at worst, the safe side. urnetwork_service_debit_oldest_seconds is the age
// of each shard's oldest row, pending or awaiting its release: past 24 h a carved token lapses before
// its debit and the bytes could be granted twice, so it alerts long before (13.5's flush behind). A balance that is gone while it has rows is an invariant failure, never leave
// to discard the debt; the delete guard on transfer_balance keeps it from happening. There is no
// transfer_escrow repair and no provider write: service debits pay no one (D-QUOTA-4).
//
// The journal belongs to MG2: a binary that does not link it flushes nothing.

const ServiceDebitShardCount = 16

type ServiceDebitFlushSettings struct {
	// The most balances one page visits: 64, as FlushTransferDebits.
	MaxBalanceCount int
	// The rows one balance transaction applies.
	BatchSize int
	// A balance with a full batch flushes again in the same page, up to this many batches, while the
	// page has RepeatMargin left.
	BatchesPerBalance int
	RepeatMargin      time.Duration
	// One page's deadline.
	PageTimeout time.Duration
	// Each balance transaction's statement and lock bounds.
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	// The bound on one balance's token releases.
	ReleaseTimeout time.Duration
	// The ttl a release lowers a balance's floor with: DefaultLeaseReservationSettings' FloorTtl.
	FloorTtl time.Duration
	// The rows of a shard's state the oldest-age gauge reads at most, so a long backlog costs one bounded
	// read a page; past it the gauge is the oldest of the rows it read.
	OldestExaminedRowCount int
	// The age a batch row reads whose service_usage_batch row retention already removed: at least
	// service_usage_batch's 24 h.
	OldestBatchlessAge time.Duration
}

func DefaultServiceDebitFlushSettings() *ServiceDebitFlushSettings {
	return &ServiceDebitFlushSettings{
		MaxBalanceCount:   64,
		BatchSize:         512,
		BatchesPerBalance: 16,
		RepeatMargin:      5 * time.Second,
		PageTimeout:       15 * time.Second,
		StatementTimeout:  2 * time.Second,
		LockTimeout:       250 * time.Millisecond,
		ReleaseTimeout:    time.Second,
		FloorTtl:          DefaultLeaseReservationSettings().FloorTtl,
		// a shard's 16th of a minute of 10,000 busy leases' carves
		OldestExaminedRowCount: 4096,
		OldestBatchlessAge:     24 * time.Hour,
	}
}

var serviceDebitFlushSettings = DefaultServiceDebitFlushSettings()

// Replaces the process's flush settings, returning a restore function. For tests.
func Testing_SetServiceDebitFlushSettings(settings *ServiceDebitFlushSettings) func() {
	prev := serviceDebitFlushSettings
	serviceDebitFlushSettings = settings
	return func() {
		serviceDebitFlushSettings = prev
	}
}

var errServiceDebitPageDeadline = errors.New("service debit page deadline")

var serviceDebitFlushResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_service_debit_flush_total",
	Help: "Service debit flush batches by finite result: committed, released, busy or error. No payout.",
}, []string{"result"})

var serviceDebitOldestAge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "urnetwork_service_debit_oldest_seconds",
	Help: "Oldest service debit journal row's age by shard and state, pending or awaiting its Redis release; a carved token lapses at 24 h, so it alerts at 10 min.",
}, []string{"shard", "state"})

func init() {
	prometheus.MustRegister(serviceDebitFlushResults, serviceDebitOldestAge)
}

// The age of a shard's oldest journal row in one applied state, 0 for none, over at most $3 of its rows.
// The journal has no time column: a batch row dates from its batch's applied_time, or reads $4 when
// retention removed the batch row, and a relayed row from its contract id, which is its usage id and a
// ULID holding its contract's creation in its first 48 bits.
const serviceDebitOldestAgeSql = `
	SELECT COALESCE(GREATEST(0, EXTRACT(epoch FROM ((clock_timestamp() AT TIME ZONE 'UTC') - MIN(row_time))))::double precision, 0)
	FROM (
		SELECT CASE
			WHEN journal.batch_id IS NULL THEN
				to_timestamp((('x' || substr(replace(journal.usage_id::text, '-', ''), 1, 12))::bit(48)::bigint)::double precision / 1000) AT TIME ZONE 'UTC'
			ELSE COALESCE(batch.applied_time, $4)
		END AS row_time
		FROM (
			SELECT usage_id, batch_id
			FROM service_debit_journal
			WHERE (get_byte(uuid_send(balance_id), 15) % 16) = $1 AND applied = $2
			LIMIT $3
		) journal
		LEFT JOIN service_usage_batch batch ON batch.batch_id = journal.batch_id
	) row_times
`

// Sets the shard's oldest-age gauge for both states. A failed read leaves the last value.
func sampleServiceDebitOldestAge(ctx context.Context, shard int, now time.Time) {
	settings := serviceDebitFlushSettings
	label := strconv.Itoa(shard)
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			for _, state := range []struct {
				name    string
				applied bool
			}{{"pending", false}, {"redis_release", true}} {
				var age float64
				server.Raise(conn.QueryRow(
					ctx,
					serviceDebitOldestAgeSql,
					shard,
					state.applied,
					max(settings.OldestExaminedRowCount, 1),
					now.UTC().Add(-settings.OldestBatchlessAge),
				).Scan(&age))
				serviceDebitOldestAge.WithLabelValues(label, state.name).Set(age)
			}
		}, server.OptNoRetry())
	}, func(err error) {
		if glog.V(1) {
			glog.Infof("[quota]service debit shard %d's oldest age not read: %s\n", shard, err)
		}
	})
}

// The shard a balance's service debits flush in: transfer_debit_journal's, computed, since this
// journal has no shard column (its index computes the same expression).
func ServiceDebitShard(balanceId server.Id) int {
	return int(balanceId[15]) % ServiceDebitShardCount
}

// The raw run-once key of a shard's flush chain, the form KickTasks and the idle marks take.
func ServiceDebitFlushRunOnceKey(shard int) string {
	return fmt.Sprintf("flush_service_debits_%d", shard)
}

// Whether the shard holds any journal row, pending or applied and awaiting its release: exactly the
// rows the flush visits. One seek of service_debit_journal_shard.
func ServiceDebitShardPending(ctx context.Context, shard int) (pending bool) {
	if !server.SidecarMg2Linked() {
		return false
	}
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT EXISTS(SELECT 1 FROM service_debit_journal WHERE (get_byte(uuid_send(balance_id), 15) % 16) = $1)`,
			shard,
		).Scan(&pending))
	}, server.OptNoRetry())
	return
}

// Kicks the backed-off flush chain of each shard the balances' rows belong to, after those rows
// committed. A chain that is not backed off is left alone.
func wakeServiceDebitFlushes(ctx context.Context, balanceIds []server.Id) (kickedKeys []string, err error) {
	keys := []string{}
	for _, balanceId := range balanceIds {
		key := ServiceDebitFlushRunOnceKey(ServiceDebitShard(balanceId))
		if !slices.Contains(keys, key) {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	return task.WakeIdleRunOnce(ctx, task.DefaultIdleWakeSettings(), keys...)
}

type ServiceDebitFlushResult struct {
	LastBalanceId *server.Id `json:"last_balance_id,omitempty"`
	Balances      int        `json:"balances"`
	Applied       int        `json:"applied"`
	Released      int        `json:"released"`
	Busy          int        `json:"busy"`
	Failed        int        `json:"failed"`
	More          bool       `json:"more"`
}

type serviceDebit struct {
	usageId   server.Id
	meter     string
	byteCount int64
	applied   bool
}

// The Redis token a row's debit releases, if it holds one: a lease's carve its svc/ token, a relayed
// contract's row its contract id. evs's bytes reserve nothing.
func serviceDebitToken(meter string, usageId server.Id) (string, bool) {
	switch meter {
	case sidecar.QuotaMeterEvs:
		return "", false
	case sidecar.QuotaMeterRelay:
		return usageId.String(), true
	default:
		return "svc/" + usageId.String(), true
	}
}

// Seams a test holds the flush at, each a kill point: an error ends the balance's flush there, as a
// killed worker would. Nil in production.
type serviceDebitFlushHooks struct {
	// inside the debit's transaction, before its commit
	beforeCommit func(balanceId server.Id) error
	// after the commit, before any release
	afterCommit func(balanceId server.Id) error
	// after the releases, before the delete
	afterRelease func(balanceId server.Id) error
	// after each balance of a page, with the page's own context
	afterBalance func(pageCtx context.Context, balanceId server.Id)
}

// One page of shard from after (see the file header), on clock, which dates each day row. Each
// balance's failure is counted and ends the page with its cursor, so the next starts after it.
func FlushServiceDebits(ctx context.Context, shard int, after *server.Id, clock sidecar.Clock) (ServiceDebitFlushResult, error) {
	return flushServiceDebits(ctx, shard, after, clock, nil)
}

func flushServiceDebits(ctx context.Context, shard int, after *server.Id, clock sidecar.Clock, hooks *serviceDebitFlushHooks) (result ServiceDebitFlushResult, returnErr error) {
	settings := serviceDebitFlushSettings
	if shard < 0 || ServiceDebitShardCount <= shard {
		return result, fmt.Errorf("no service debit shard %d", shard)
	}
	if !server.SidecarMg2Linked() {
		return result, nil
	}
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	bounded, cancel := context.WithTimeoutCause(ctx, settings.PageTimeout, errServiceDebitPageDeadline)
	defer cancel()
	sampleServiceDebitOldestAge(bounded, shard, clock.Now())
	hasTime := func() bool {
		if bounded.Err() != nil {
			return false
		}
		deadline, ok := bounded.Deadline()
		return !ok || settings.RepeatMargin <= time.Until(deadline)
	}
	server.HandleError(func() {
		for range max(settings.MaxBalanceCount, 1) {
			next := nextServiceDebitBalance(bounded, shard, after)
			if next == nil {
				result.LastBalanceId = nil
				return
			}
			applied, released, busy, err := flushServiceDebitBalance(bounded, *next, clock, hooks)
			last := released
			for batches := 1; err == nil && !busy && last == settings.BatchSize && batches < settings.BatchesPerBalance && hasTime(); batches += 1 {
				var batchApplied int
				batchApplied, last, busy, err = flushServiceDebitBalance(bounded, *next, clock, hooks)
				applied += batchApplied
				released += last
			}
			if err != nil {
				result.Failed += 1
				serviceDebitFlushResults.WithLabelValues("error").Inc()
			}
			if last == settings.BatchSize {
				result.More = true
			}
			result.Balances += 1
			result.Applied += applied
			result.Released += released
			if busy {
				result.Busy += 1
			}
			result.LastBalanceId = next
			after = next
			if err != nil {
				if ctx.Err() != nil {
					server.Raise(ctx.Err())
				}
				return
			}
			if hooks != nil && hooks.afterBalance != nil {
				hooks.afterBalance(bounded, *next)
			}
		}
		result.More = true
	}, func(err error) {
		returnErr = err
	})
	// a page that ran out of its own time after some balances returns them, for the post to resume
	if returnErr != nil && 0 < result.Balances && result.LastBalanceId != nil &&
		ctx.Err() == nil && bounded.Err() != nil && context.Cause(bounded) == errServiceDebitPageDeadline {
		result.More = true
		returnErr = nil
	}
	return
}

// The next balance key of the shard after after. One seek of the shard index.
func nextServiceDebitBalance(ctx context.Context, shard int, after *server.Id) (next *server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		query := `
			SELECT balance_id FROM service_debit_journal
			WHERE (get_byte(uuid_send(balance_id), 15) % 16) = $1
			ORDER BY (get_byte(uuid_send(balance_id), 15) % 16), balance_id
			LIMIT 1
		`
		args := []any{shard}
		if after != nil {
			query = `
				SELECT balance_id FROM service_debit_journal
				WHERE (get_byte(uuid_send(balance_id), 15) % 16) = $1 AND balance_id > $2
				ORDER BY (get_byte(uuid_send(balance_id), 15) % 16), balance_id
				LIMIT 1
			`
			args = append(args, *after)
		}
		result, err := conn.Query(ctx, query, args...)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var balanceId server.Id
				server.Raise(result.Scan(&balanceId))
				next = &balanceId
			}
		})
	})
	return
}

// The UTC day a debit applied at now counts on.
func serviceUsageDay(now time.Time) time.Time {
	year, month, day := now.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// One batch of one balance's rows: debit, then release, then delete (see the file header). An
// ambiguous commit is safe: a rerun reads the rows applied and debits nothing again.
func flushServiceDebitBalance(ctx context.Context, balanceId server.Id, clock sidecar.Clock, hooks *serviceDebitFlushHooks) (appliedCount int, releasedCount int, busy bool, returnErr error) {
	settings := serviceDebitFlushSettings
	var records []*serviceDebit
	// the balance the transaction's debit left, or read when its rows were applied before: the floor
	// each release lowers to, a negative balance shown as 0
	var balanceByteCount int64
	committed := false
	server.HandleError(func() {
		admitted := server.TryOwnedTx(ctx, transferBalanceOwnershipKeys([]server.Id{balanceId}), func(tx server.PgTx) {
			records = nil
			appliedCount = 0
			busy = false
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(
				`SET LOCAL statement_timeout = %d; SET LOCAL lock_timeout = %d`,
				max(settings.StatementTimeout.Milliseconds(), 1),
				max(settings.LockTimeout.Milliseconds(), 1),
			)))
			var networkId server.Id
			found := false
			result, err := tx.Query(ctx, `SELECT network_id, balance_byte_count FROM transfer_balance WHERE balance_id = $1 FOR NO KEY UPDATE SKIP LOCKED`, balanceId)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&networkId, &balanceByteCount))
					found = true
				}
			})
			if !found {
				// a missing balance is an invariant failure, not leave to discard debt
				var exists bool
				server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_balance WHERE balance_id = $1)`, balanceId).Scan(&exists))
				if !exists {
					server.Raise(fmt.Errorf("the balance of pending service debits is missing"))
				}
				busy = true
				return
			}
			result, err = tx.Query(
				ctx,
				`
					SELECT usage_id, meter, debit_byte_count, applied
					FROM service_debit_journal
					WHERE balance_id = $1
					ORDER BY usage_id
					LIMIT $2
					FOR UPDATE SKIP LOCKED
				`,
				balanceId,
				settings.BatchSize,
			)
			byteCount := int64(0)
			meterByteCounts := map[string]int64{}
			server.WithPgResult(result, err, func() {
				for result.Next() {
					record := &serviceDebit{}
					server.Raise(result.Scan(&record.usageId, &record.meter, &record.byteCount, &record.applied))
					if !record.applied {
						if math.MaxInt64-byteCount < record.byteCount {
							break
						}
						byteCount += record.byteCount
						meterByteCounts[record.meter] += record.byteCount
						appliedCount += 1
					}
					records = append(records, record)
				}
			})
			if len(records) == 0 {
				return
			}
			if 0 < byteCount {
				debited := false
				result, err := tx.Query(
					ctx,
					`UPDATE transfer_balance SET balance_byte_count = balance_byte_count - $2 WHERE balance_id = $1 RETURNING balance_byte_count`,
					balanceId,
					byteCount,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&balanceByteCount))
						debited = true
					}
				})
				if !debited {
					server.Raise(fmt.Errorf("the debited balance disappeared"))
				}
				meters := make([]string, 0, len(meterByteCounts))
				quantities := make([]int64, 0, len(meterByteCounts))
				for meter, meterByteCount := range meterByteCounts {
					if 0 < meterByteCount {
						meters = append(meters, meter)
						quantities = append(quantities, meterByteCount)
					}
				}
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_service_usage (network_id, usage_day, meter, quantity)
						SELECT $1, $2::date, meter, quantity
						FROM unnest($3::varchar[], $4::bigint[]) AS usage(meter, quantity)
						ORDER BY meter
						ON CONFLICT (network_id, usage_day, meter) DO UPDATE
						SET quantity = network_service_usage.quantity + EXCLUDED.quantity
					`,
					networkId,
					serviceUsageDay(clock.Now()),
					meters,
					quantities,
				))
			}
			usageIds := make([]server.Id, 0, len(records))
			for _, record := range records {
				usageIds = append(usageIds, record.usageId)
			}
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE service_debit_journal SET applied = true WHERE balance_id = $1 AND usage_id = ANY($2::uuid[]) AND NOT applied`,
				balanceId,
				usageIds,
			))
			if hooks != nil && hooks.beforeCommit != nil {
				server.Raise(hooks.beforeCommit(balanceId))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		committed = admitted
		if !admitted {
			busy = true
		}
		if busy {
			serviceDebitFlushResults.WithLabelValues("busy").Inc()
			return
		}
		if len(records) == 0 {
			return
		}
		serviceDebitFlushResults.WithLabelValues("committed").Inc()
		if hooks != nil && hooks.afterCommit != nil {
			server.Raise(hooks.afterCommit(balanceId))
		}

		// keep every applied row until every release is acknowledged; the applied flag stops a
		// second debit
		tokens := []string{}
		for _, record := range records {
			if token, ok := serviceDebitToken(record.meter, record.usageId); ok {
				tokens = append(tokens, token)
			}
		}
		if 0 < len(tokens) {
			bounded, cancel := context.WithTimeout(ctx, settings.ReleaseTimeout)
			defer cancel()
			floorByteCount := strconv.FormatInt(max(balanceByteCount, 0), 10)
			floorMs := strconv.FormatInt(max(settings.FloorTtl.Milliseconds(), 1), 10)
			// no preflight PING, as for the transfer debits, and outside a transaction
			server.Raise(server.RedisWithDeadline(bounded, func(client server.RedisClient) error {
				commands, err := client.Pipelined(bounded, func(pipe redis.Pipeliner) error {
					for _, token := range tokens {
						pipe.Eval(bounded, redisContractReservationScript, redisLeaseReservationKeys(balanceId),
							"release-debited", token, floorByteCount, "0", strconv.FormatInt(redisContractReservationLease.Milliseconds(), 10), floorMs)
					}
					return nil
				})
				if err != nil {
					return err
				}
				for _, command := range commands {
					if err := command.Err(); err != nil {
						return err
					}
				}
				return nil
			}, server.OptRedisNoPreflight(), server.OptRedisKeepLateReplies()))
		}
		if hooks != nil && hooks.afterRelease != nil {
			server.Raise(hooks.afterRelease(balanceId))
		}

		usageIds := make([]server.Id, 0, len(records))
		for _, record := range records {
			usageIds = append(usageIds, record.usageId)
		}
		// another admitted pass may hold these applied rows after their release: refuse known
		// contention before the delete; the kept rows make a later release and delete replayable
		if !server.TryOwnedTx(ctx, transferBalanceOwnershipKeys([]server.Id{balanceId}), func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM service_debit_journal WHERE balance_id = $1 AND usage_id = ANY($2::uuid[]) AND applied`,
				balanceId,
				usageIds,
			))
			releasedCount = int(tag.RowsAffected())
		}, server.TxReadCommitted, server.OptNoRetry()) {
			busy = true
			serviceDebitFlushResults.WithLabelValues("busy").Inc()
			return
		}
		serviceDebitFlushResults.WithLabelValues("released").Inc()
	}, func(err error) {
		returnErr = err
		if !committed {
			// an unacknowledged commit is reconciled by the next journal read
			appliedCount = 0
		}
	})
	return
}
