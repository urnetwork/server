// The lingering origins' side table (server contract_origin_linger_schema.go;
// OPEN-SOURCE-ID-RETIRE.md §2): its readers' switch and its sweep.
//
// The switch. Each companion origin read takes the pair's closed origins from
// contract_origin_linger and then each contract by its primary key, once the
// side table is complete: its state row's complete time has passed, so every
// origin still lingering closed after the trigger existed. Before that, and
// while the state row is absent, the reads keep their previous statements
// (contract_origin_linger_legacy.go), which read the closed pair range of
// transfer_contract_open_source_id; those go with the index's drop record.
// Each process reads the state on its own pooled connection, before the read
// that uses it acquires one, at most once per CompletionReadInterval, and the
// first decision waits for the first read: a process never guesses complete.
//
// The sweep. Rows past their prune time can satisfy no read, since every read
// requires an unexpired deadline. The taskworker deletes them in short
// batches, oldest prune time first (work.RemoveExpiredContractOriginLinger),
// about one delete per early close.
package model

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

type ContractOriginLingerSettings struct {
	// How long a process trusts its last read of the side table's state. The
	// state changes once, at its complete time, and back only with an
	// operator's rollback, which waits this long before the trigger goes.
	CompletionReadInterval time.Duration
	// Bounds one read of the state.
	CompletionReadTimeout time.Duration
}

func DefaultContractOriginLingerSettings() *ContractOriginLingerSettings {
	return &ContractOriginLingerSettings{
		CompletionReadInterval: 30 * time.Second,
		CompletionReadTimeout:  5 * time.Second,
	}
}

// Which statements an origin read used.
const (
	// the side table and the primary key
	contractOriginLingerReadSideTable = "side_table"
	// the previous statements, before the side table is complete
	contractOriginLingerReadLegacy = "legacy"
)

var contractOriginLingerReadsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_contract_origin_linger_reads_total",
	Help: "Companion origin reads by the statements they used: side_table (contract_origin_linger, then the contract's primary key) or legacy (the closed pair range of transfer_contract_open_source_id, before the side table's state row is complete). Legacy stays at zero in every process before transfer_contract_open_source_id is dropped",
}, []string{"path"})

var (
	contractOriginLingerSideTableReads = contractOriginLingerReadsCounter.WithLabelValues(contractOriginLingerReadSideTable)
	contractOriginLingerLegacyReads    = contractOriginLingerReadsCounter.WithLabelValues(contractOriginLingerReadLegacy)
)

func init() {
	prometheus.MustRegister(contractOriginLingerReadsCounter)
}

// The side table's state as this process last read it. Safe for concurrent
// use. A read that fails keeps the previous decision until the next read.
type contractOriginLingerCompletion struct {
	settings *ContractOriginLingerSettings

	stateLock sync.Mutex
	// the state row's complete time, nil while there is none
	completeTime *time.Time
	// a read has ended, and when
	known    bool
	readTime time.Time
	// closed when the read in flight ends; nil while none is
	reading chan struct{}
}

func newContractOriginLingerCompletion(settings *ContractOriginLingerSettings) *contractOriginLingerCompletion {
	return &contractOriginLingerCompletion{
		settings: settings,
	}
}

// The pair's lingering origins: each early close of ($1, $2) still before its
// deadline that the side table holds, read from transfer_contract by its
// primary key, with every condition of a closed origin arm: closed, closed no
// earlier than $3, unexpired, and the pair's own. Those conditions are one
// boolean test kept out of every index condition, and each lookup is a
// lateral subquery the planner keeps whole (OFFSET 0), so the side table's ids
// are the only way in: transfer_contract_open_source_id or another pair index
// can never serve it. The side table is only an access path: a row that
// reopened, or whose contract changed or went, fails the conditions. Each arm
// selects its columns and its companion condition from this.
var contractOriginLingerClosedSql = `(
		SELECT
			closed_contract.contract_id,
			closed_contract.create_time,
			closed_contract.transfer_byte_count,
			closed_contract.payer_network_id,
			closed_contract.source_network_id,
			closed_contract.destination_network_id,
			closed_contract.companion_contract_id
		FROM (
			SELECT contract_id FROM contract_origin_linger
			WHERE
				source_id = $1 AND
				destination_id = $2 AND
				prune_time > statement_timestamp() AT TIME ZONE 'UTC'
			OFFSET 0
		) AS linger
		CROSS JOIN LATERAL (
			SELECT
				transfer_contract.contract_id,
				transfer_contract.create_time,
				transfer_contract.transfer_byte_count,
				transfer_contract.payer_network_id,
				transfer_contract.source_network_id,
				transfer_contract.destination_network_id,
				transfer_contract.companion_contract_id
			FROM transfer_contract
			WHERE
				transfer_contract.contract_id = linger.contract_id AND
				-- ids lead with their creation millisecond: an unexpired
				-- contract's id is at least this bound
				` + unexpiredContractIdBoundSql("transfer_contract.contract_id") + ` AND
				(
					transfer_contract.open = false AND
					$3 <= transfer_contract.close_time AND
					COALESCE(transfer_contract.expiration_time, transfer_contract.create_time + interval '60 minutes') > statement_timestamp() AT TIME ZONE 'UTC' AND
					transfer_contract.source_id = $1 AND
					transfer_contract.destination_id = $2
				) IS TRUE
			OFFSET 0
		) AS closed_contract
	)`

// do's raise as an error, nil when do returned.
func contractOriginLingerRecover(do func()) (returnErr error) {
	server.HandleError(do, func(err error) {
		returnErr = err
	})
	return
}

// The process's switch for every companion origin read.
var contractOriginLingerReads = newContractOriginLingerCompletion(DefaultContractOriginLingerSettings())

// Whether the origin reads use the side table now. Call it before acquiring
// the connection the read runs on: a stale state is read on a connection of
// its own. Counts the decision.
func (self *contractOriginLingerCompletion) ready(ctx context.Context) bool {
	for {
		completeTime, wait, refresh := func() (*time.Time, chan struct{}, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.known && time.Since(self.readTime) < self.settings.CompletionReadInterval {
				return self.completeTime, nil, false
			}
			if self.reading != nil {
				if self.known {
					// another caller reads it; keep the previous decision
					return self.completeTime, nil, false
				}
				return nil, self.reading, false
			}
			self.reading = make(chan struct{})
			return nil, nil, true
		}()
		if refresh {
			self.refresh()
			continue
		}
		if wait != nil {
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				// the caller's own read fails on its context; never guess
				// complete
				contractOriginLingerLegacyReads.Inc()
				return false
			}
		}
		if completeTime != nil && !server.NowUtc().Before(*completeTime) {
			contractOriginLingerSideTableReads.Inc()
			return true
		}
		contractOriginLingerLegacyReads.Inc()
		return false
	}
}

// Reads the state row on a connection of its own, outside the caller's
// context: one short catalog check and one row, bounded by
// CompletionReadTimeout.
func (self *contractOriginLingerCompletion) refresh() {
	var completeTime *time.Time
	readErr := contractOriginLingerRecover(func() {
		readCtx, cancel := context.WithTimeout(context.Background(), self.settings.CompletionReadTimeout)
		defer cancel()
		server.Db(readCtx, func(conn server.PgConn) {
			var present bool
			server.Raise(conn.QueryRow(readCtx, `SELECT to_regclass('public.contract_origin_linger_state') IS NOT NULL`).Scan(&present))
			if !present {
				return
			}
			result, err := conn.Query(readCtx, `SELECT complete_time FROM contract_origin_linger_state WHERE singleton`)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&completeTime))
				}
			})
		})
	})
	if readErr != nil {
		glog.Infof("[contract_origin_linger]state read: %s\n", readErr)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if readErr == nil {
		self.completeTime = completeTime
	}
	self.known = true
	self.readTime = time.Now()
	close(self.reading)
	self.reading = nil
}

// Forgets the last read, so the next decision reads the state again. For
// tests that change the state row.
func (self *contractOriginLingerCompletion) forget() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.known = false
	self.completeTime = nil
}

type ContractOriginLingerSweepSettings struct {
	// Rows one transaction deletes, oldest prune time first.
	BatchSize int
	// One run's work, at most: batches, and wall clock. About a minute of
	// early closes on Main is 80,000 rows, 16 batches.
	MaxBatches int
	RunBudget  time.Duration
	// Bound each batch's statement and its lock waits; a batch that times out
	// ends the run, and the next run retries.
	StatementTimeout time.Duration
	LockTimeout      time.Duration
	// The chain's schedule: a run that caught up starts again after
	// RunInterval, one that left due rows after MoreInterval; the chain starts
	// StartDelay after a taskworker starts, and one claim is held at most
	// TaskMaxTime.
	RunInterval  time.Duration
	MoreInterval time.Duration
	StartDelay   time.Duration
	TaskMaxTime  time.Duration
}

func DefaultContractOriginLingerSweepSettings() *ContractOriginLingerSweepSettings {
	return &ContractOriginLingerSweepSettings{
		BatchSize:        5000,
		MaxBatches:       120,
		RunBudget:        50 * time.Second,
		StatementTimeout: 10 * time.Second,
		LockTimeout:      2 * time.Second,
		RunInterval:      time.Minute,
		MoreInterval:     5 * time.Second,
		StartDelay:       time.Minute,
		TaskMaxTime:      5 * time.Minute,
	}
}

// Why a sweep run ended.
const (
	// no row past its prune time is left
	ContractOriginLingerSweepStopCaughtUp = "caught_up"
	// the run deleted MaxBatches batches
	ContractOriginLingerSweepStopBatches = "batches"
	// the run spent its RunBudget
	ContractOriginLingerSweepStopBudget   = "budget"
	ContractOriginLingerSweepStopCanceled = "canceled"
	// a batch failed; the next run retries it
	ContractOriginLingerSweepStopError = "error"
)

var contractOriginLingerSweepStops = []string{
	ContractOriginLingerSweepStopCaughtUp,
	ContractOriginLingerSweepStopBatches,
	ContractOriginLingerSweepStopBudget,
	ContractOriginLingerSweepStopCanceled,
	ContractOriginLingerSweepStopError,
}

var (
	contractOriginLingerSweepRunsCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_contract_origin_linger_sweep_runs_total",
		Help: "Runs of the lingering origins' side table sweep, by why they ended: caught_up, batches (the run's batch limit), budget, canceled or error",
	}, []string{"stop"})
	contractOriginLingerSweepLagGauge = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "urnetwork_contract_origin_linger_sweep_lag_seconds",
		Help: "At the end of the last sweep run in this process, how long the oldest row of contract_origin_linger has been past its prune time: zero when the sweep caught up",
	})
)

func init() {
	for _, stop := range contractOriginLingerSweepStops {
		contractOriginLingerSweepRunsCounter.WithLabelValues(stop)
	}
	prometheus.MustRegister(contractOriginLingerSweepRunsCounter, contractOriginLingerSweepLagGauge)
}

// What one sweep run did.
type ContractOriginLingerSweepReport struct {
	Deleted int64
	Batches int
	// How long the oldest row left was past its prune time at the run's end.
	Lag  time.Duration
	Stop string
}

// The run left rows past their prune time.
func (self *ContractOriginLingerSweepReport) More() bool {
	return self.Stop == ContractOriginLingerSweepStopBatches || self.Stop == ContractOriginLingerSweepStopBudget
}

// One batch: the rows with the oldest prune times up to the database clock,
// read from the prune time index, deleted by their tuple ids. A row the
// trigger rewrote meanwhile moved, and waits for the next batch.
const contractOriginLingerSweepBatchSql = `
	DELETE FROM contract_origin_linger
	WHERE
		ctid = ANY(ARRAY(
			SELECT ctid FROM contract_origin_linger
			WHERE prune_time <= statement_timestamp() AT TIME ZONE 'UTC'
			ORDER BY prune_time
			LIMIT $1
		)) AND
		prune_time <= statement_timestamp() AT TIME ZONE 'UTC'
`

// How long the oldest row has been past its prune time, from the prune time
// index; zero when no row is.
const contractOriginLingerSweepLagSql = `
	SELECT COALESCE(EXTRACT(EPOCH FROM statement_timestamp() AT TIME ZONE 'UTC' - min(prune_time)), 0)::float8
	FROM contract_origin_linger
	WHERE prune_time <= statement_timestamp() AT TIME ZONE 'UTC'
`

// Deletes the side table's rows past their prune time, in batches, until none
// is left or the run's limits end it. A failed batch ends the run with its
// error; every batch before it committed.
func RemoveExpiredContractOriginLinger(
	ctx context.Context,
	settings *ContractOriginLingerSweepSettings,
) (report *ContractOriginLingerSweepReport, returnErr error) {
	report = &ContractOriginLingerSweepReport{
		Stop: ContractOriginLingerSweepStopError,
	}
	defer func() {
		contractOriginLingerSweepRunsCounter.WithLabelValues(report.Stop).Inc()
	}()
	runEnd := time.Now().Add(settings.RunBudget)
	for {
		if ctx.Err() != nil {
			report.Stop = ContractOriginLingerSweepStopCanceled
			return
		}
		if settings.MaxBatches <= report.Batches {
			report.Stop = ContractOriginLingerSweepStopBatches
			break
		}
		if !time.Now().Before(runEnd) {
			report.Stop = ContractOriginLingerSweepStopBudget
			break
		}
		var deleted int64
		err := contractOriginLingerRecover(func() {
			server.MaintenanceTx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d`, settings.StatementTimeout.Milliseconds())))
				server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`SET LOCAL lock_timeout = %d`, settings.LockTimeout.Milliseconds())))
				tag := server.RaisePgResult(tx.Exec(ctx, contractOriginLingerSweepBatchSql, settings.BatchSize))
				deleted = tag.RowsAffected()
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if err != nil {
			if ctx.Err() != nil {
				report.Stop = ContractOriginLingerSweepStopCanceled
				return
			}
			report.Stop = ContractOriginLingerSweepStopError
			returnErr = err
			return
		}
		report.Batches += 1
		report.Deleted += deleted
		countRetentionDeletedRows(RetentionTableContractOriginLinger, deleted)
		if deleted < int64(settings.BatchSize) {
			report.Stop = ContractOriginLingerSweepStopCaughtUp
			break
		}
	}
	lagErr := contractOriginLingerRecover(func() {
		server.Db(ctx, func(conn server.PgConn) {
			var lagSeconds float64
			server.Raise(conn.QueryRow(ctx, contractOriginLingerSweepLagSql).Scan(&lagSeconds))
			report.Lag = time.Duration(lagSeconds * float64(time.Second))
		})
	})
	if lagErr == nil {
		contractOriginLingerSweepLagGauge.Set(report.Lag.Seconds())
	}
	return
}
