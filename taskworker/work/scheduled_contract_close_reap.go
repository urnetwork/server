// Queued scheduled closes that outlived their contracts, from before closers
// removed them in the closing transaction, are only claimed, read as terminal
// and moved into finished_task; on Main they were a third of the queue and
// nearly every claim at its head. The reaper deletes them directly instead:
// it walks the queue in claim order through the function poll index, reads one
// bounded chunk per statement, checks each chunk's contracts by primary key and
// deletes the rows whose contract is closed or missing. It runs only while
// contract_close.yml enables it:
//
//	scheduled_close_reap:
//	  enabled: true
//	  chunk_size: 256     # queued closes read per chunk, at most 1024
//	  chunks: 8           # chunks per run, at most 64
//	  delay_millis: 1000  # pause between runs, at least 100
//
// A disabled, absent or unreadable config makes a run read only the config and
// wake again after scheduledCloseReapIdleDelay, so enabling or disabling it
// needs no deploy. The reaper never removes a row of an open contract, a row a
// live claim leases, or a row another transaction holds, and never waits: the
// claim or the next sweep finishes those. It keeps its position in its own
// arguments and starts again at the head once a sweep reaches the end.
package work

import (
	"context"
	"encoding/json"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

const scheduledCloseReapResourceName = contractCloseConcurrencyResourceName

const scheduledCloseReapDefaultChunkSize = 256
const scheduledCloseReapMaxChunkSize = 1024
const scheduledCloseReapDefaultChunks = 8
const scheduledCloseReapMaxChunks = 64
const scheduledCloseReapDefaultDelay = time.Second
const scheduledCloseReapMinDelay = 100 * time.Millisecond

// A disabled reaper rereads its config this often.
const scheduledCloseReapIdleDelay = time.Minute

// A run stops starting chunks after this, well inside its task deadline.
const scheduledCloseReapBudget = 30 * time.Second
const scheduledCloseReapMaxTime = 2 * time.Minute

type scheduledCloseReapConfig struct {
	Enabled     bool `yaml:"enabled"`
	ChunkSize   *int `yaml:"chunk_size"`
	Chunks      *int `yaml:"chunks"`
	DelayMillis *int `yaml:"delay_millis"`
}

type scheduledCloseReapSettings struct {
	enabled   bool
	chunkSize int
	chunks    int
	delay     time.Duration
}

// The configured settings; any value outside its bounds keeps its default and
// any unreadable config disables the reaper.
func readScheduledCloseReapSettings() scheduledCloseReapSettings {
	settings := scheduledCloseReapSettings{
		chunkSize: scheduledCloseReapDefaultChunkSize,
		chunks:    scheduledCloseReapDefaultChunks,
		delay:     scheduledCloseReapDefaultDelay,
	}
	resource, err := server.Config.SimpleResource(scheduledCloseReapResourceName)
	if err != nil {
		return settings
	}
	var config struct {
		Reap *scheduledCloseReapConfig `yaml:"scheduled_close_reap"`
	}
	if err := resource.UnmarshalYamlE(&config); err != nil || config.Reap == nil {
		return settings
	}
	settings.enabled = config.Reap.Enabled
	if value := config.Reap.ChunkSize; value != nil && 1 <= *value && *value <= scheduledCloseReapMaxChunkSize {
		settings.chunkSize = *value
	}
	if value := config.Reap.Chunks; value != nil && 1 <= *value && *value <= scheduledCloseReapMaxChunks {
		settings.chunks = *value
	}
	if value := config.Reap.DelayMillis; value != nil && scheduledCloseReapMinDelay <= time.Duration(*value)*time.Millisecond {
		settings.delay = time.Duration(*value) * time.Millisecond
	}
	return settings
}

// A queued close's place in claim order: the function poll index order.
type ScheduledCloseReapPosition struct {
	AvailableBlock int64     `json:"available_block"`
	Priority       int       `json:"priority"`
	MaxTimeSeconds int       `json:"max_time_seconds"`
	TaskId         server.Id `json:"task_id"`
}

// After is the last queued close visited; nil starts at the head.
type ReapScheduledContractClosesArgs struct {
	After *ScheduledCloseReapPosition `json:"after,omitempty"`
}

type ReapScheduledContractClosesResult struct {
	Enabled bool                        `json:"enabled"`
	Next    *ScheduledCloseReapPosition `json:"next,omitempty"`
	Visited int                         `json:"visited"`
	Removed int                         `json:"removed"`
	// The sweep reached the end of the queue; the next run starts at its head.
	Wrapped bool `json:"wrapped,omitempty"`
}

// Startup arms the reaper at the head; a pending run keeps its position.
func ScheduleReapScheduledContractCloses(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleReapScheduledContractCloses(clientSession, tx, &ReapScheduledContractClosesArgs{}, server.NowUtc())
}

// A merge with a pending run keeps that run's position.
func scheduleReapScheduledContractCloses(clientSession *session.ClientSession, tx server.PgTx, args *ReapScheduledContractClosesArgs, runAt time.Time) {
	task.ScheduleTaskInTx(tx, ReapScheduledContractCloses, args, clientSession,
		task.RunOnce("reap_scheduled_contract_closes"), task.RunAt(runAt), task.MaxTime(scheduledCloseReapMaxTime))
}

// One run of up to the configured chunks from the retained position. A failed
// chunk ends the run with that position kept; the run itself never fails.
func ReapScheduledContractCloses(args *ReapScheduledContractClosesArgs, clientSession *session.ClientSession) (*ReapScheduledContractClosesResult, error) {
	settings := readScheduledCloseReapSettings()
	result := &ReapScheduledContractClosesResult{Enabled: settings.enabled, Next: args.After}
	if !settings.enabled {
		return result, nil
	}
	started := time.Now()
	for range settings.chunks {
		if scheduledCloseReapBudget <= time.Since(started) {
			break
		}
		next, visited, removed, err := reapScheduledContractClosesChunk(clientSession.Ctx, result.Next, settings.chunkSize, server.NowUtc())
		if err != nil {
			// The successor retries this chunk from the retained position.
			glog.Infof("[close-reap]chunk failed: %v\n", err)
			break
		}
		result.Visited += visited
		result.Removed += removed
		result.Next = next
		if visited < settings.chunkSize {
			result.Wrapped = true
			result.Next = nil
			break
		}
	}
	glog.Infof("[close-reap]visited=%d removed=%d wrapped=%t\n", result.Visited, result.Removed, result.Wrapped)
	return result, nil
}

// The successor waits the configured delay while enabled and the idle delay
// otherwise, from the position this run reached.
func ReapScheduledContractClosesPost(_ *ReapScheduledContractClosesArgs, result *ReapScheduledContractClosesResult,
	clientSession *session.ClientSession, tx server.PgTx) error {
	delay := scheduledCloseReapIdleDelay
	if result.Enabled {
		delay = readScheduledCloseReapSettings().delay
	}
	scheduleReapScheduledContractCloses(clientSession, tx, &ReapScheduledContractClosesArgs{After: result.Next}, server.NowUtc().Add(delay))
	return nil
}

// Registered in both workload profiles beside the queued closes it reaps.
func NewScheduledContractCloseReapTaskTarget() task.Target {
	return task.NewTaskTargetWithPost(ReapScheduledContractCloses, ReapScheduledContractClosesPost)
}

// The function poll index orders queued closes by (available_block,
// run_priority DESC, run_max_time_seconds DESC, task_id). A position after a
// row is four disjoint index ranges in that order; each branch is an index
// range bounded by the chunk size, and the merge reads at most one chunk.
const scheduledCloseReapColumns = `available_block,run_priority,run_max_time_seconds,task_id,release_time,args_json`

const scheduledCloseReapOrder = `regexp_replace(function_name, '/v[0-9]+', '', 'g'),available_block,run_priority DESC,run_max_time_seconds DESC,task_id`

const scheduledCloseReapHeadSql = `SELECT ` + scheduledCloseReapColumns + ` FROM pending_task
	WHERE regexp_replace(function_name, '/v[0-9]+', '', 'g')=$1
	ORDER BY ` + scheduledCloseReapOrder + ` LIMIT $2`

const scheduledCloseReapAfterSql = `SELECT ` + scheduledCloseReapColumns + ` FROM (
	(SELECT ` + scheduledCloseReapColumns + ` FROM pending_task
		WHERE regexp_replace(function_name, '/v[0-9]+', '', 'g')=$1
			AND available_block=$3 AND run_priority=$4 AND run_max_time_seconds=$5 AND task_id>$6
		ORDER BY ` + scheduledCloseReapOrder + ` LIMIT $2)
	UNION ALL
	(SELECT ` + scheduledCloseReapColumns + ` FROM pending_task
		WHERE regexp_replace(function_name, '/v[0-9]+', '', 'g')=$1
			AND available_block=$3 AND run_priority=$4 AND run_max_time_seconds<$5
		ORDER BY ` + scheduledCloseReapOrder + ` LIMIT $2)
	UNION ALL
	(SELECT ` + scheduledCloseReapColumns + ` FROM pending_task
		WHERE regexp_replace(function_name, '/v[0-9]+', '', 'g')=$1
			AND available_block=$3 AND run_priority<$4
		ORDER BY ` + scheduledCloseReapOrder + ` LIMIT $2)
	UNION ALL
	(SELECT ` + scheduledCloseReapColumns + ` FROM pending_task
		WHERE regexp_replace(function_name, '/v[0-9]+', '', 'g')=$1
			AND available_block>$3
		ORDER BY ` + scheduledCloseReapOrder + ` LIMIT $2)
) AS chunk ORDER BY available_block,run_priority DESC,run_max_time_seconds DESC,task_id LIMIT $2`

// Locked rows are skipped, and the release time excludes a live claim's lease.
const scheduledCloseReapDeleteSql = `DELETE FROM pending_task
	WHERE ctid=ANY(ARRAY(SELECT ctid FROM pending_task
		WHERE task_id=ANY($1) AND release_time<=$2
			AND regexp_replace(function_name, '/v[0-9]+', '', 'g')=$3
		FOR UPDATE SKIP LOCKED))
	AND task_id=ANY($1)`

// Visits the next chunk after the position at now and removes its queued closes
// whose contract is closed or missing. Returns the last row visited, the rows
// visited and the rows removed. Each statement runs on its own; a chunk holds
// no lock across them.
func reapScheduledContractClosesChunk(ctx context.Context, after *ScheduledCloseReapPosition, chunkSize int, now time.Time) (next *ScheduledCloseReapPosition, visited int, removed int, returnErr error) {
	next = after
	server.HandleError(func() {
		type queuedClose struct {
			position    ScheduledCloseReapPosition
			releaseTime time.Time
			contractId  server.Id
			parsed      bool
		}
		chunk := []queuedClose{}
		server.Db(ctx, func(conn server.PgConn) {
			sql, sqlArgs := scheduledCloseReapHeadSql, []any{scheduledCloseFunctionName, chunkSize}
			if after != nil {
				sql = scheduledCloseReapAfterSql
				sqlArgs = append(sqlArgs, after.AvailableBlock, after.Priority, after.MaxTimeSeconds, after.TaskId)
			}
			rows, err := conn.Query(ctx, sql, sqlArgs...)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var row queuedClose
					var argsJson string
					server.Raise(rows.Scan(&row.position.AvailableBlock, &row.position.Priority, &row.position.MaxTimeSeconds,
						&row.position.TaskId, &row.releaseTime, &argsJson))
					var args CloseScheduledContractArgs
					if json.Unmarshal([]byte(argsJson), &args) == nil && args.ContractId != (server.Id{}) {
						row.contractId, row.parsed = args.ContractId, true
					}
					chunk = append(chunk, row)
				}
			})
		}, server.OptNoRetry())
		if len(chunk) == 0 {
			return
		}
		contractIds := []server.Id{}
		for _, row := range chunk {
			if row.parsed {
				contractIds = append(contractIds, row.contractId)
			}
		}
		openContractIdSet := map[server.Id]bool{}
		if 0 < len(contractIds) {
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=ANY($1) AND outcome IS NULL`, contractIds)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var contractId server.Id
						server.Raise(rows.Scan(&contractId))
						openContractIdSet[contractId] = true
					}
				})
			}, server.OptNoRetry())
		}
		// A terminal outcome is final and a missing contract is never created
		// again, so the delete only rechecks the lease and the row lock.
		taskIds := []server.Id{}
		for _, row := range chunk {
			if row.parsed && !openContractIdSet[row.contractId] && !now.Before(row.releaseTime) {
				taskIds = append(taskIds, row.position.TaskId)
			}
		}
		if 0 < len(taskIds) {
			server.Tx(ctx, func(tx server.PgTx) {
				tag := server.RaisePgResult(tx.Exec(ctx, scheduledCloseReapDeleteSql, taskIds, now, scheduledCloseFunctionName))
				removed = int(tag.RowsAffected())
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		last := chunk[len(chunk)-1].position
		next, visited = &last, len(chunk)
	}, func(err error) {
		next, visited, removed, returnErr = after, 0, 0, err
	})
	return
}
