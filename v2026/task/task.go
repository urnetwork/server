package task

import (
	"context"
	// "net/http"
	"strings"
	// "strconv"
	// "encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand"
	"reflect"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

// taskPanicError converts a recovered task panic into the task's error.
//
// `server.IsDoneError` already classifies torn-down-connection pg panics
// ("context canceled", "failed to deallocate cached statement(s)") as an
// expected shutdown pattern, and `server.HandleError` drops them silently.
// The task layer saw the same class and did the opposite — a multi-KB
// "Unhandled:" blob with a full stack on every occurrence — because the
// recover here runs BEFORE HandleError ever sees the panic, so that
// classification never applied. Keep the failure itself loud (the task still
// errors, reschedules, and names its cause) and drop only the stack for the
// already-benign class; V(1) restores it when someone is debugging.
func taskPanicError(r any) error {
	if server.IsDoneError(r) {
		if glog.V(1) {
			return fmt.Errorf("Interrupted: %s", server.ErrorJson(r, debug.Stack()))
		}
		return fmt.Errorf("Interrupted: %v", r)
	}
	return fmt.Errorf("Unhandled: %s", server.ErrorJson(r, debug.Stack()))
}

// orphanedRunPostCounter counts RunPost tasks whose finished_task row was
// reaped before the post ran. These used to reschedule forever; they now
// complete as a no-op, and this is the only remaining signal that post
// processing was skipped.
var orphanedRunPostCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "task",
		Name:      "run_post_orphaned_total",
		Help:      "RunPost tasks completed as a no-op because their finished_task row was reaped",
	},
)

func init() {
	prometheus.MustRegister(orphanedRunPostCounter)
}

// the task system captures work that needs to be done to advance the platform
// tasks have work and post-work that can atomically schedule new tasks
// important properties:
// - tasks are run as singletons, where a single worker will run a single task at a time.
//   this simplifies writing tasks so they do not have to assume potentially miltiple executions,
//   although in practice the implementation should still guard against
//   unrecoverable outcomes of parallel execution.
// - break work into small chunks so that code can be continuously deployed without
//   system interruption.
// - tasks are not lost
// - post tasks are not lost
// - errors are surfaced

// pattern for repeating tasks. Define three functions,
// ScheduleDo(schedule, ...)
// Do
// DoPost, calls ScheduleDo

// IMPORTANT: this is hard coded into the `db_migrations`
// IMPORTANT: if you change this number, you must also change the schema
const BlockSizeSeconds = 1

var DefaultMaxTime = 2 * time.Minute

// ReleaseTimeout is the heartbeat freshness window. Active workers refresh
// claim_time every ReleaseTimeout/3 so operators and monitors can distinguish a
// live long-running task from a dead owner.
var ReleaseTimeout = 30 * time.Second

// TaskLeaseTimeout is the maximum timestamp delay before a task can be
// reconsidered after its owner's PostgreSQL session disappears. It is
// deliberately independent of the task's declared MaxTime: MaxTime bounds
// execution, the session advisory lock prevents duplicate live execution, and
// this timeout bounds crash recovery. Five minutes comfortably covers routine
// scheduler/DB stalls without stranding a two-hour task for the full two hours.
var TaskLeaseTimeout = 5 * time.Minute

// A timestamp lease bounds recovery after a dead worker, while this
// session-scoped advisory lock prevents a live-but-starved worker from losing
// ownership when that timestamp expires. The lock is held on a direct postgres
// connection (never transaction-pooled PgBouncer), so PostgreSQL releases it
// automatically when the worker process/connection dies.
const taskAdvisoryLockNamespace = uint64(0x75726e7461736b31)

func taskAdvisoryLockKey(taskId server.Id) int64 {
	return int64(
		binary.BigEndian.Uint64(taskId[0:8]) ^
			binary.BigEndian.Uint64(taskId[8:16]) ^
			taskAdvisoryLockNamespace,
	)
}

type taskClaimGuard struct {
	conn        server.PgConn
	releaseOnce sync.Once
}

func (self *taskClaimGuard) ping(ctx context.Context) error {
	if self == nil || self.conn == nil {
		return errors.New("task claim guard is not active")
	}
	return self.conn.Ping(ctx)
}

func (self *taskClaimGuard) release() {
	if self == nil {
		return
	}
	self.releaseOnce.Do(func() {
		if self.conn == nil {
			return
		}

		ctx, cancel := context.WithTimeout(context.Background(), DefaultTaskFinalizeTimeout)
		_, err := self.conn.Exec(ctx, `SELECT pg_advisory_unlock_all()`)
		cancel()
		if err == nil {
			self.conn.Release()
		} else {
			// Never return a session with a possibly-held advisory lock to the
			// pool. Closing the physical connection makes PostgreSQL release it.
			pgxConn := self.conn.Hijack()
			closeCtx, closeCancel := context.WithTimeout(context.Background(), DefaultTaskFinalizeTimeout)
			_ = pgxConn.Close(closeCtx)
			closeCancel()
		}
		self.conn = nil
	})
}

// the reschedule time is uniformly chosen on [0, t] so the expected mean will be t/2
var RescheduleTimeout = 2 * BlockSizeSeconds * time.Second

// nominal cap for the exponential error-reschedule backoff. A task that keeps
// erroring retries at RescheduleTimeout * 2^reschedule_error_count, capped
// here. Saturated retries are jittered from half to one-and-a-half times this
// value, preserving the one-hour mean while dispersing a cohort over an hour.
// Without backoff a wedged task (e.g. an external 429 rate limit) retried every
// ~2s forever; 8k such payment tasks churned pending_task to ~94% dead tuples
// and made the poll query 39% of all db exec time. The count resets when the
// task completes (the pending row is deleted).
var RescheduleBackoffMaxTimeout = 1 * time.Hour

// clamp for the backoff exponent in the reschedule write (bounds power())
const rescheduleBackoffMaxExponent = 24

// exponent clamp for the version-skew retry: a target-not-found error
// usually means the task type exists only on the other build generation of a
// deploy overlap, so the full exponential backoff would push a brand-new
// chain out for no reason. Retries converge to
// RescheduleTimeout * 2^targetNotFoundBackoffMaxExponent (~16s) — negligible
// load, and a PERMANENTLY missing target stays loudly visible in
// has_reschedule_error instead of hiding behind an hour-long backoff.
const targetNotFoundBackoffMaxExponent = 3

// errorRescheduleDelay keeps the legacy short-retry behavior until the
// exponential backoff reaches its cap. At the cap, a two-second jitter is too
// small: tasks created by one outage retain the same wave forever and can rate
// limit their shared dependency once an hour. Proportional jitter spreads that
// wave over [cap/2, 3*cap/2), while its mean remains cap (plus the legacy
// half-base jitter). randomUnit is explicit so the distribution contract has
// deterministic synthetic tests; production passes math/rand.Float64().
func errorRescheduleDelay(
	base time.Duration,
	cap time.Duration,
	errorCount int,
	maxExponent int,
	randomUnit float64,
) time.Duration {
	if base <= 0 || cap <= 0 {
		return 0
	}
	if errorCount < 0 {
		errorCount = 0
	}
	if maxExponent < 0 {
		maxExponent = 0
	}
	exponent := min(errorCount, maxExponent)
	nominal := time.Duration(math.Min(
		float64(cap),
		float64(base)*math.Pow(2, float64(exponent)),
	))
	randomUnit = max(0, min(randomUnit, math.Nextafter(1, 0)))
	if nominal < cap {
		return nominal + time.Duration(randomUnit*float64(base))
	}
	return nominal/2 + time.Duration(randomUnit*float64(nominal)) + base/2
}

// ErrTargetNotFound tags a claimed task whose function has no registered
// target in this worker (deploy version skew, or a missing registration).
var ErrTargetNotFound = errors.New("Target not found")

// ErrDrained tags a task error caused by `Drain` canceling the task context.
// The reschedule write for these skips the error-count increment and the
// backoff (retry ~RescheduleTimeout later, claim released immediately), so a
// deploy never pushes a healthy chain toward the backoff cap.
var ErrDrained = errors.New("Drained")

type TaskPriority = int

const (
	TaskPriorityFastest TaskPriority = 20
	TaskPrioritySlowest TaskPriority = 0
)

var DefaultPriority = (TaskPriorityFastest + TaskPrioritySlowest) / 2

type TaskFunction[T any, R any] func(T, *session.ClientSession) (R, error)

type TaskPostFunction[T any, R any] func(T, R, *session.ClientSession, server.PgTx) error

// type ScheduleTaskFunction[T any, R any] func(TaskFunction[T, R], T, *session.ClientSession, ...any)

type RunAtOption struct {
	At time.Time
}

func RunAt(at time.Time) *RunAtOption {
	return &RunAtOption{
		At: at,
	}
}

// if the key is already scheduled, a new schedule will not be created
type RunOnceOption struct {
	Key []any
}

func RunOnce(key ...any) *RunOnceOption {
	return &RunOnceOption{
		Key: key,
	}
}

func (self *RunOnceOption) String() string {
	keyJson, err := json.Marshal(self.Key)
	if err != nil {
		panic(err)
	}
	return string(keyJson)
}

// FIXME RunReplace(key ...any)
//  remove all unclaimed tasks with same key, then add

type RunPriorityOption struct {
	Priority TaskPriority
}

func Priority(priority TaskPriority) *RunPriorityOption {
	return &RunPriorityOption{
		Priority: priority,
	}
}

type RunMaxTimeOption struct {
	MaxTime time.Duration
}

func MaxTime(maxTime time.Duration) *RunMaxTimeOption {
	return &RunMaxTimeOption{
		MaxTime: maxTime,
	}
}

func ScheduleTask[T any, R any](
	taskFunction TaskFunction[T, R],
	args T,
	clientSession *session.ClientSession,
	opts ...any,
) (taskId server.Id) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		taskId = ScheduleTaskInTx[T, R](tx, taskFunction, args, clientSession, opts...)
	})
	return
}

type preparedTask struct {
	taskId       server.Id
	functionName string
	argsJson     []byte
	// the peppered address hash + port (server.ClientIpHash) of the
	// scheduling session, never the raw ip:port. nil when the session has no
	// parseable address (local/internal schedulers).
	clientAddressHash []byte
	clientAddressPort int
	byJwtJson         *string
	runAt             time.Time
	runOnceKey        *string
	priority          TaskPriority
	maxTimeSeconds    int
}

func prepareTask[T any, R any](
	taskFunction TaskFunction[T, R],
	args T,
	clientSession *session.ClientSession,
	opts ...any,
) preparedTask {
	taskTarget := NewTaskTarget(taskFunction)

	argsJson, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}

	var byJwtJson *string
	if clientSession.ByJwt != nil {
		byJwtJsonBytes, err := json.Marshal(clientSession.ByJwt)
		if err != nil {
			panic(err)
		}
		byJwtJson_ := string(byJwtJsonBytes)
		byJwtJson = &byJwtJson_
	}

	runAt := &RunAtOption{
		At: server.NowUtc(),
	}
	var runOnce *RunOnceOption
	runPriority := &RunPriorityOption{
		Priority: DefaultPriority,
	}
	runMaxTime := &RunMaxTimeOption{
		MaxTime: DefaultMaxTime,
	}

	for _, opt := range opts {
		switch v := opt.(type) {
		case RunAtOption:
			runAt = &v
		case *RunAtOption:
			runAt = v
		case RunOnceOption:
			runOnce = &v
		case *RunOnceOption:
			runOnce = v
		case RunPriorityOption:
			runPriority = &v
		case *RunPriorityOption:
			runPriority = v
		case RunMaxTimeOption:
			runMaxTime = &v
		case *RunMaxTimeOption:
			runMaxTime = v
		}
	}

	var runOnceKey *string
	if runOnce != nil {
		runOnceKey_ := runOnce.String()
		runOnceKey = &runOnceKey_
	}

	// persist only the peppered hash of the scheduling address. the raw
	// ip:port used to be stored here verbatim and outlived the request in
	// pending_task (and finished_task for 24h); the 2024 hashing migration
	// missed this call site. an unparseable/absent address stores NULL, which
	// is also what sessions reconstructed from these rows carry.
	var clientAddressHash []byte
	clientAddressPort := 0
	if hash, port, err := clientSession.ClientAddressHashPort(); err == nil {
		clientAddressHash = hash[:]
		clientAddressPort = port
	}

	return preparedTask{
		taskId:            server.NewId(),
		functionName:      taskTarget.TargetFunctionName(),
		argsJson:          argsJson,
		clientAddressHash: clientAddressHash,
		clientAddressPort: clientAddressPort,
		byJwtJson:         byJwtJson,
		runAt:             runAt.At.UTC(),
		runOnceKey:        runOnceKey,
		priority:          runPriority.Priority,
		maxTimeSeconds:    int(runMaxTime.MaxTime / time.Second),
	}
}

func ScheduleTaskInTx[T any, R any](
	tx server.PgTx,
	taskFunction TaskFunction[T, R],
	args T,
	clientSession *session.ClientSession,
	opts ...any,
) (taskId server.Id) {
	p := prepareTask(taskFunction, args, clientSession, opts...)

	claimTime := time.Time{}

	server.RaisePgResult(tx.Exec(
		clientSession.Ctx,
		`
			INSERT INTO pending_task (
				task_id,
		        function_name,
		        args_json,
		        client_address,
		        client_address_hash,
		        client_address_port,
		        client_by_jwt_json,
		        run_at,
		        run_once_key,
		        run_priority,
		        run_max_time_seconds,
		        claim_time,
		        release_time
			) VALUES ($1, $2, $3, '', $4, $5, $6, $7, $8, $9, $10, $11, $11)
			ON CONFLICT (run_once_key) DO UPDATE SET
				run_at = LEAST(pending_task.run_at, $7),
				run_priority = LEAST(pending_task.run_priority, $9),
				run_max_time_seconds = GREATEST(pending_task.run_max_time_seconds, $10)
		`,
		p.taskId,
		p.functionName,
		p.argsJson,
		p.clientAddressHash,
		p.clientAddressPort,
		p.byJwtJson,
		p.runAt,
		p.runOnceKey,
		p.priority,
		p.maxTimeSeconds,
		claimTime,
	))
	return p.taskId
}

// ScheduleTaskInTxIfAbsent is like ScheduleTaskInTx but for callers that need
// an atomic "only schedule if not already pending under this key" guarantee,
// instead of RunOnce's merge-on-conflict semantics. RunOnce's
// `ON CONFLICT (run_once_key) DO UPDATE` only merges run_at/run_priority/
// run_max_time_seconds into an existing pending row -- crucially not
// args_json -- so if two different calls share a run_once key while the
// first is still pending, scheduling both would silently drop the second
// call's args while still reporting success. This does a single
// `INSERT ... ON CONFLICT (run_once_key) DO NOTHING` and reports via
// `scheduled` whether the row was actually inserted, so the caller can
// reject a duplicate outright -- atomically, in one round trip -- instead of
// a separate check-then-act that can itself race. runOnce is required (not
// optional via opts) since the whole point is a key-scoped guarantee.
func ScheduleTaskInTxIfAbsent[T any, R any](
	tx server.PgTx,
	taskFunction TaskFunction[T, R],
	args T,
	clientSession *session.ClientSession,
	runOnce *RunOnceOption,
	opts ...any,
) (scheduled bool, taskId server.Id) {
	if runOnce == nil {
		panic("ScheduleTaskInTxIfAbsent requires a non-nil runOnce key")
	}
	p := prepareTask(taskFunction, args, clientSession, append(opts, runOnce)...)

	claimTime := time.Time{}

	tag := server.RaisePgResult(tx.Exec(
		clientSession.Ctx,
		`
			INSERT INTO pending_task (
				task_id,
		        function_name,
		        args_json,
		        client_address,
		        client_address_hash,
		        client_address_port,
		        client_by_jwt_json,
		        run_at,
		        run_once_key,
		        run_priority,
		        run_max_time_seconds,
		        claim_time,
		        release_time
			) VALUES ($1, $2, $3, '', $4, $5, $6, $7, $8, $9, $10, $11, $11)
			ON CONFLICT (run_once_key) DO NOTHING
		`,
		p.taskId,
		p.functionName,
		p.argsJson,
		p.clientAddressHash,
		p.clientAddressPort,
		p.byJwtJson,
		p.runAt,
		p.runOnceKey,
		p.priority,
		p.maxTimeSeconds,
		claimTime,
	))
	scheduled = 0 < tag.RowsAffected()
	if !scheduled {
		return scheduled, server.Id{}
	}
	return scheduled, p.taskId
}

func ScheduleTaskIfAbsent[T any, R any](
	taskFunction TaskFunction[T, R],
	args T,
	clientSession *session.ClientSession,
	runOnce *RunOnceOption,
	opts ...any,
) (scheduled bool, taskId server.Id) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		scheduled, taskId = ScheduleTaskInTxIfAbsent[T, R](tx, taskFunction, args, clientSession, runOnce, opts...)
	})
	return
}

func GetTasks(ctx context.Context, taskIds ...server.Id) map[server.Id]*Task {
	if len(taskIds) == 0 {
		return map[server.Id]*Task{}
	}

	tasks := map[server.Id]*Task{}

	server.Tx(ctx, func(tx server.PgTx) {
		selectSql := `
    		SELECT
		    	pending_task.task_id,
		        pending_task.function_name,
		        pending_task.args_json,
		        pending_task.client_address,
		        pending_task.client_address_hash,
		        pending_task.client_address_port,
		        pending_task.client_by_jwt_json,
		        pending_task.run_at,
		        pending_task.run_once_key,
		        pending_task.run_priority,
		        pending_task.run_max_time_seconds,
		        pending_task.claim_time,
		        pending_task.release_time,
		        pending_task.reschedule_error,
		        pending_task.reschedule_error_count
		    FROM pending_task
		`

		var result server.PgResult
		var err error

		if len(taskIds) < 32 {
			// `task_id IN (...)` is more efficient than a temp table for small lists

			taskIdParams := []string{}
			for i := 0; i < len(taskIds); i += 1 {
				taskIdParams = append(taskIdParams, fmt.Sprintf("$%d", i+1))
			}

			taskIdValues := []any{}
			for _, taskId := range taskIds {
				taskIdValues = append(taskIdValues, taskId)
			}

			result, err = tx.Query(
				ctx,
				selectSql+`
				    WHERE task_id IN (`+strings.Join(taskIdParams, ",")+`)
			    `,
				taskIdValues...,
			)
		} else {
			server.CreateTempTableInTx(ctx, tx, "temp_task_ids(task_id uuid)", taskIds...)

			result, err = tx.Query(
				ctx,
				selectSql+`
				    INNER JOIN temp_task_ids ON temp_task_ids.task_id = pending_task.task_id
			    `,
			)
		}

		server.WithPgResult(result, err, func() {
			for result.Next() {
				task := &Task{}
				var byJwtJson *string
				var runOnceKey *string
				var rescheduleError *string
				server.Raise(result.Scan(
					&task.TaskId,
					&task.FunctionName,
					&task.ArgsJson,
					&task.ClientAddress,
					&task.ClientAddressHash,
					&task.ClientAddressPort,
					&byJwtJson,
					&task.RunAt,
					&runOnceKey,
					&task.RunPriority,
					&task.RunMaxTimeSeconds,
					&task.ClaimTime,
					&task.ReleaseTime,
					&rescheduleError,
					&task.RescheduleErrorCount,
				))
				if byJwtJson != nil {
					task.ClientByJwtJson = *byJwtJson
				}
				if runOnceKey != nil {
					task.RunOnceKey = *runOnceKey
				}
				if rescheduleError != nil {
					task.RescheduleError = *rescheduleError
				}
				tasks[task.TaskId] = task
			}
		})
	})

	return tasks
}

func GetFinishedTasks(ctx context.Context, taskIds ...server.Id) map[server.Id]*FinishedTask {
	finishedTasks := map[server.Id]*FinishedTask{}

	server.Tx(ctx, func(tx server.PgTx) {
		server.CreateTempTableInTx(ctx, tx, "temp_task_ids(task_id uuid)", taskIds...)

		result, err := tx.Query(
			ctx,
			`
			    SELECT
			    	finished_task.task_id,
		            finished_task.function_name,
		            finished_task.args_json,
		            finished_task.client_address,
		            finished_task.client_address_hash,
		            finished_task.client_address_port,
		            finished_task.client_by_jwt_json,
		            finished_task.run_at,
		            finished_task.run_once_key,
		            finished_task.run_priority,
		            finished_task.run_max_time_seconds,
		            finished_task.run_start_time,
		            finished_task.run_end_time,
		            finished_task.reschedule_error,
		            finished_task.result_json,
		            finished_task.post_error,
		            finished_task.post_completed
			    FROM finished_task
			    INNER JOIN temp_task_ids ON temp_task_ids.task_id = finished_task.task_id
		    `,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				finishedTask := &FinishedTask{}
				var byJwtJson *string
				var runOnceKey *string
				var rescheduleError *string
				var postError *string
				server.Raise(result.Scan(
					&finishedTask.TaskId,
					&finishedTask.FunctionName,
					&finishedTask.ArgsJson,
					&finishedTask.ClientAddress,
					&finishedTask.ClientAddressHash,
					&finishedTask.ClientAddressPort,
					&byJwtJson,
					&finishedTask.RunAt,
					&runOnceKey,
					&finishedTask.RunPriority,
					&finishedTask.RunMaxTimeSeconds,
					&finishedTask.RunStartTime,
					&finishedTask.RunEndTime,
					&rescheduleError,
					&finishedTask.ResultJson,
					&postError,
					&finishedTask.PostCompleted,
				))
				if byJwtJson != nil {
					finishedTask.ClientByJwtJson = *byJwtJson
				}
				if runOnceKey != nil {
					finishedTask.RunOnceKey = *runOnceKey
				}
				if rescheduleError != nil {
					finishedTask.RescheduleError = *rescheduleError
				}
				if postError != nil {
					finishedTask.PostError = *postError
				}
				finishedTasks[finishedTask.TaskId] = finishedTask
			}
		})
	})

	return finishedTasks
}

func ListPendingTasks(ctx context.Context) []server.Id {
	taskIds := []server.Id{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					task_id
				FROM pending_task
				ORDER BY run_at_block ASC, run_priority ASC, run_at ASC
			`,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var taskId server.Id
				server.Raise(result.Scan(&taskId))
				taskIds = append(taskIds, taskId)
			}
		})
	})

	return taskIds
}

// the task struct has the latest error attached to it
func ListRescheduledTasks(ctx context.Context) []server.Id {
	taskIds := []server.Id{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					task_id
				FROM pending_task
				WHERE has_reschedule_error
			`,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var taskId server.Id
				server.Raise(result.Scan(&taskId))
				taskIds = append(taskIds, taskId)
			}
		})
	})

	return taskIds
}

// CountAvailableByFunctionName reports how many pending_task rows targeting
// the given task function are currently available to run (run_at has
// passed), across all run_once keys -- this excludes rows scheduled for a
// future run_at, which are queued/waiting, not consuming any worker
// capacity yet. Callers use this to enforce a global concurrency cap on a
// specific background task type (e.g. capping how many networks can have a
// bulk operation actually in flight at once), independent of any single
// run_once key. Counting future-scheduled rows here would make a large
// backlog of merely-queued work block admission of brand new requests, even
// though nothing is actually running yet.
func CountAvailableByFunctionName[T any, R any](ctx context.Context, taskFunction TaskFunction[T, R]) int {
	functionName := NewTaskTarget(taskFunction).TargetFunctionName()
	// computed in Go, not `now()` in SQL: run_at is a naive `timestamp`
	// column holding UTC values, and comparing it against `now()`
	// (timestamptz) would force a timezone-dependent cast on the session's
	// TimeZone setting -- the same class of bug fixed in
	// model/account_action_rate_limit.go and avoided in
	// model.ReserveBulkClientRemovalSlot.
	asOf := server.NowUtc()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT COUNT(*) FROM pending_task WHERE function_name = $1 AND run_at <= $2`,
			functionName, asOf,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

func ListClaimedTasks(ctx context.Context) []server.Id {
	taskIds := []server.Id{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					task_id
				FROM pending_task
				WHERE $1 < release_time
			`,
			server.NowUtc(),
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var taskId server.Id
				server.Raise(result.Scan(&taskId))
				taskIds = append(taskIds, taskId)
			}
		})
	})

	return taskIds
}

func ListFinishedTasks(ctx context.Context) []server.Id {
	taskIds := []server.Id{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					task_id
				FROM finished_task
				ORDER BY run_end_time ASC
			`,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var taskId server.Id
				server.Raise(result.Scan(&taskId))
				taskIds = append(taskIds, taskId)
			}
		})
	})

	return taskIds
}

// FIXME update pending task
func RemovePendingTask(ctx context.Context, taskId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM pending_task
				WHERE task_id = $1
			`,
			taskId,
		))
	})
}

// RemovePendingTasksForFunctionInTx deletes every pending task targeting a
// function that has been removed from the codebase. Such rows can never run
// again: the runner fails them with ErrTargetNotFound and reschedules on the
// clamped skew backoff forever, because at claim time deploy version skew is
// indistinguishable from permanent removal. Call this from the startup
// seeding (taskworker InitTasks) for each deliberately removed target. The
// delete ignores claims on purpose — a row for a removed target is claimed
// and re-errored every few seconds, so a claim filter would race with that
// cycle. A rollback to a build that still schedules the function re-seeds
// it, and the next roll-forward cleans it again.
func RemovePendingTasksForFunctionInTx(ctx context.Context, tx server.PgTx, functionName string) (removedCount int64) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM pending_task
			WHERE function_name = $1
		`,
		functionName,
	))
	return tag.RowsAffected()
}

// ReleaseTask clears the claim lease on a pending task, making it claimable
// again per its run_at (release_time <= run_at puts available_block back on
// the run_at schedule). This is the operator recovery for a claim stranded
// by a killed worker, which otherwise blocks the task — and its RunOnce
// chain — until claim + max time passes; a deploy cannot heal it (the
// InitTasks upsert never touches claims). Releasing a task that is actually
// STILL RUNNING re-opens the duplicate-execution window the lease exists to
// prevent, so verify the claiming worker is really gone first.
func ReleaseTask(ctx context.Context, taskId server.Id) (released bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE pending_task
				SET
					claim_time = $2,
					release_time = $2
				WHERE task_id = $1
			`,
			taskId,
			time.Time{},
		))
		released = tag.RowsAffected() == 1
	})
	return
}

// KickTasks pulls the next run of the pending tasks matching a run-once key
// to now. The key matches both the raw form the Schedule* helpers use
// (e.g. "update_client_scores") and the exact stored json-encoded form.
// A claimed task still waits out its release_time (use ReleaseTask).
func KickTasks(ctx context.Context, runOnceKey string) (kickedCount int64) {
	// the stored key is the json-encoded RunOnce key list
	jsonKey := RunOnce(runOnceKey).String()
	now := server.NowUtc()
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE pending_task
				SET run_at = LEAST(run_at, $2)
				WHERE run_once_key IN ($1, $3)
			`,
			runOnceKey,
			now,
			jsonKey,
		))
		kickedCount = tag.RowsAffected()
	})
	return
}

// removes finished tasks older than `minTime` where the post was successfully
// run. Tasks whose post permanently errored are kept longer for debugging but
// still removed after `postErrorMinTime`, so they cannot strand forever.
func RemoveFinishedTasks(ctx context.Context, minTime time.Time, postErrorMinTime time.Time) (removeCount int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM finished_task
				WHERE
					(
						run_end_time < $1 AND
						(post_error IS NULL or post_completed)
					) OR
					run_end_time < $2
			`,
			minTime,
			postErrorMinTime,
		))

		removeCount = tag.RowsAffected()
	})

	return
}

type Task struct {
	TaskId               server.Id
	FunctionName         string
	ArgsJson             string
	ClientAddress        string
	ClientAddressHash    []byte
	ClientAddressPort    int
	ClientByJwtJson      string
	RunAt                time.Time
	RunOnceKey           string
	RunPriority          int
	RunMaxTimeSeconds    int
	ClaimTime            time.Time
	ReleaseTime          time.Time
	RescheduleError      string
	RescheduleErrorCount int
}

func (self *Task) ClientSession(ctx context.Context) (*session.ClientSession, error) {
	var byJwt *jwt.ByJwt
	if self.ClientByJwtJson != "" {
		byJwt = &jwt.ByJwt{}
		err := json.Unmarshal([]byte(self.ClientByJwtJson), byJwt)
		if err != nil {
			return nil, err
		}
	}

	// rows written since the hash migration carry only the peppered address
	// hash; reconstruct a session around it directly. legacy rows (written
	// before the migration, or by an old binary during a rolling deploy)
	// still carry the raw address and take the plain path until they drain.
	if len(self.ClientAddressHash) == 32 {
		clientSession := session.NewLocalClientSessionWithAddressHash(
			ctx,
			[32]byte(self.ClientAddressHash),
			self.ClientAddressPort,
			byJwt,
		)
		return clientSession, nil
	}

	clientSession := session.NewLocalClientSession(
		ctx,
		self.ClientAddress,
		byJwt,
	)

	return clientSession, nil
}

type FinishedTask struct {
	TaskId            server.Id
	FunctionName      string
	ArgsJson          string
	ClientAddress     string
	ClientAddressHash []byte
	ClientAddressPort int
	ClientByJwtJson   string
	RunAt             time.Time
	RunOnceKey        string
	RunPriority       int
	RunMaxTimeSeconds int
	RunStartTime      time.Time
	RunEndTime        time.Time
	RescheduleError   string
	ResultJson        string
	PostError         string
	PostCompleted     bool
}

func (self *FinishedTask) ClientSession(ctx context.Context) (*session.ClientSession, error) {
	var byJwt *jwt.ByJwt
	if self.ClientByJwtJson != "" {
		byJwt = &jwt.ByJwt{}
		err := json.Unmarshal([]byte(self.ClientByJwtJson), byJwt)
		if err != nil {
			return nil, err
		}
	}

	// rows written since the hash migration carry only the peppered address
	// hash; reconstruct a session around it directly. legacy rows (written
	// before the migration, or by an old binary during a rolling deploy)
	// still carry the raw address and take the plain path until they drain.
	if len(self.ClientAddressHash) == 32 {
		clientSession := session.NewLocalClientSessionWithAddressHash(
			ctx,
			[32]byte(self.ClientAddressHash),
			self.ClientAddressPort,
			byJwt,
		)
		return clientSession, nil
	}

	clientSession := session.NewLocalClientSession(
		ctx,
		self.ClientAddress,
		byJwt,
	)

	return clientSession, nil
}

type Target interface {
	TargetFunctionName() string
	// TargetFunction() TaskFunction[T, R]
	// PostFunction() TaskPostFunction[T, R]
	AlternateFunctionNames() []string
	Run(context.Context, *Task) (any, func(server.PgTx) error, error)
	RunPost(context.Context, *FinishedTask, server.PgTx) error
}

type TaskTarget[T any, R any] struct {
	targetFunctionName     string
	targetFunction         TaskFunction[T, R]
	postFunction           TaskPostFunction[T, R]
	alternateFunctionNames []string
}

func NewTaskTarget[T any, R any](
	targetFunction TaskFunction[T, R],
	alternateFunctionNames ...string,
) *TaskTarget[T, R] {
	return &TaskTarget[T, R]{
		targetFunctionName:     functionName(targetFunction),
		targetFunction:         targetFunction,
		alternateFunctionNames: alternateFunctionNames,
	}
}

func NewTaskTargetWithPost[T any, R any](
	targetFunction TaskFunction[T, R],
	postFunction TaskPostFunction[T, R],
	alternateFunctionNames ...string,
) *TaskTarget[T, R] {
	return &TaskTarget[T, R]{
		targetFunctionName:     functionName(targetFunction),
		targetFunction:         targetFunction,
		postFunction:           postFunction,
		alternateFunctionNames: alternateFunctionNames,
	}
}

func functionName[T any, R any](targetFunction TaskFunction[T, R]) string {
	targetFunctionName := runtime.FuncForPC(reflect.ValueOf(targetFunction).Pointer()).Name()
	// remove all /vXXXX paths in the canonical module
	return regexp.MustCompile("/v\\d+").ReplaceAllString(targetFunctionName, "")
}

func updateFunctionName(targetFunctionName string) string {
	// remove all /vXXXX paths in the canonical module
	return regexp.MustCompile("/v\\d+").ReplaceAllString(targetFunctionName, "")
}

func (self *TaskTarget[T, R]) TargetFunctionName() string {
	return self.targetFunctionName
}

//	func (self *TaskTarget[T, R]) TargetFunction() TaskFunction[T, R] {
//		return self.targetFunction
//	}
//
//	func (self *TaskTarget[T, R]) PostFunction() TaskPostFunction[T, R] {
//		return self.postFunction
//	}
func (self *TaskTarget[T, R]) AlternateFunctionNames() []string {
	return self.alternateFunctionNames
}

func (self *TaskTarget[T, R]) Run(ctx context.Context, task *Task) (
	result any,
	runPost func(server.PgTx) error,
	returnErr error,
) {
	return self.RunSpecific(ctx, task)
}

func (self *TaskTarget[T, R]) RunSpecific(ctx context.Context, task *Task) (
	result R,
	runPost func(server.PgTx) error,
	returnErr error,
) {
	var args T
	err := json.Unmarshal([]byte(task.ArgsJson), &args)
	if err != nil {
		returnErr = err
		return
	}

	clientSession, err := task.ClientSession(ctx)
	if err != nil {
		returnErr = err
		return
	}
	defer clientSession.Cancel()

	timeout := false

	go server.HandleError(func() {
		defer clientSession.Cancel()
		select {
		case <-clientSession.Ctx.Done():
		case <-time.After(max(
			time.Duration(task.RunMaxTimeSeconds)*time.Second,
			DefaultMaxTime,
		)):
			timeout = true
		}
	})

	defer func() {
		if r := recover(); r != nil {
			returnErr = taskPanicError(r)
		}
	}()

	result, returnErr = self.targetFunction(args, clientSession)
	if returnErr != nil {
		if timeout {
			returnErr = errors.Join(errors.New("Timeout"), returnErr)
		}
		return
	}
	if timeout {
		returnErr = errors.New("Timeout")
		return
	}

	runPost = func(tx server.PgTx) error {
		// the post runs in the finalize tx AFTER the function completed. It
		// must not be severed by the function's max-time/drain cancel (a
		// completed task's chain re-arm would strand into the RunPost retry
		// path), so it drops the function context's cancellation; the
		// finalize tx's own context still bounds the db work.
		postCtx, postCancel := context.WithTimeout(
			context.WithoutCancel(ctx),
			DefaultTaskFinalizeTimeout,
		)
		defer postCancel()
		clientSession, err := task.ClientSession(postCtx)
		if err != nil {
			return err
		}
		defer clientSession.Cancel()
		if self.postFunction == nil {
			return nil
		} else {
			return self.postFunction(args, result, clientSession, tx)
		}
	}

	return
}

func (self *TaskTarget[T, R]) RunPost(
	ctx context.Context,
	finishedTask *FinishedTask,
	tx server.PgTx,
) (returnErr error) {
	if self.postFunction == nil {
		returnErr = errors.New("No post")
		return
	}

	var args T
	err := json.Unmarshal([]byte(finishedTask.ArgsJson), &args)
	if err != nil {
		returnErr = err
		return
	}

	var result R
	err = json.Unmarshal([]byte(finishedTask.ResultJson), &result)
	if err != nil {
		returnErr = err
		return
	}

	clientSession, err := finishedTask.ClientSession(ctx)
	if err != nil {
		returnErr = err
		return
	}
	defer clientSession.Cancel()

	timeout := false

	go server.HandleError(func() {
		defer clientSession.Cancel()
		select {
		case <-clientSession.Ctx.Done():
		case <-time.After(max(
			time.Duration(finishedTask.RunMaxTimeSeconds)*time.Second,
			DefaultMaxTime,
		)):
			timeout = true
		}
	})

	defer func() {
		if r := recover(); r != nil {
			returnErr = taskPanicError(r)
		}
	}()

	returnErr = self.postFunction(args, result, clientSession, tx)
	if returnErr != nil {
		if timeout {
			returnErr = errors.Join(errors.New("Timeout"), returnErr)
		}
		return
	}
	if timeout {
		returnErr = errors.New("Timeout")
		return
	}

	return
}

type RunPostArgs struct {
	TaskId server.Id `json:"task_id"`
}

type RunPostResult struct {
}

func DefaultTaskWorkerSettings() *TaskWorkerSettings {
	return &TaskWorkerSettings{
		BatchSize:              4,
		RetryTimeoutAfterError: 30 * time.Second,
		PollTimeout:            5 * time.Second,
		DrainFinishTimeout:     60 * time.Second,
		DrainCancelTimeout:     30 * time.Second,
		FinalizeTimeout:        DefaultTaskFinalizeTimeout,
	}
}

const DefaultTaskFinalizeTimeout = 30 * time.Second

type TaskWorkerSettings struct {
	BatchSize              int
	RetryTimeoutAfterError time.Duration
	PollTimeout            time.Duration
	// how long `Drain` waits for in-flight tasks to finish naturally before
	// canceling their contexts
	DrainFinishTimeout time.Duration
	// how long `Drain` waits after the cancel for the canceled task
	// functions to unwind; a function that ignores its context keeps its
	// claim lease and rides to the process kill
	DrainCancelTimeout time.Duration
	// bounds the detached transaction that records completion/reschedule and
	// releases claims after task functions return. It deliberately outlives
	// the serving root context during shutdown.
	FinalizeTimeout time.Duration
}

type TaskWorker struct {
	ctx       context.Context
	cancel    context.CancelFunc
	runCtx    context.Context
	runCancel context.CancelFunc
	// canceled by `Drain` after DrainFinishTimeout to abort the in-flight
	// task function contexts (the eval/finalize machinery stays on ctx)
	drainCtx    context.Context
	drainCancel context.CancelFunc
	runWg       sync.WaitGroup
	targets     map[string]Target
	settings    *TaskWorkerSettings

	stateLock sync.Mutex
	draining  bool

	inflightCount      atomic.Int64
	drainCanceledCount atomic.Int64
}

func NewTaskWorkerWithDefaults(ctx context.Context) *TaskWorker {
	return NewTaskWorker(ctx, DefaultTaskWorkerSettings())
}

func NewTaskWorker(ctx context.Context, settings *TaskWorkerSettings) *TaskWorker {
	cancelCtx, cancel := context.WithCancel(ctx)
	runCtx, runCancel := context.WithCancel(cancelCtx)
	drainCtx, drainCancel := context.WithCancel(cancelCtx)

	taskWorker := &TaskWorker{
		ctx:         cancelCtx,
		cancel:      cancel,
		runCtx:      runCtx,
		runCancel:   runCancel,
		drainCtx:    drainCtx,
		drainCancel: drainCancel,
		targets:     map[string]Target{},
		settings:    settings,
	}

	taskWorker.AddTargets(
		NewTaskTargetWithPost(taskWorker.RunPost, taskWorker.RunPostPost),
	)

	return taskWorker
}

func (self *TaskWorker) Run() {
	if !self.enterRun() {
		return
	}
	defer self.runWg.Done()

	emptyCount := 0
	for {
		select {
		case <-self.runCtx.Done():
			return
		default:
		}

		finishedTaskIds, rescheduledTaskIds, postRescheduledTaskIds, err := self.EvalTasks(self.settings.BatchSize)
		if err != nil {
			glog.Infof("[taskworker]error running tasks: %s\n", err)
			select {
			case <-self.runCtx.Done():
				return
			case <-time.After(self.settings.RetryTimeoutAfterError):
			}
		} else if len(finishedTaskIds)+len(rescheduledTaskIds)+len(postRescheduledTaskIds) == 0 {
			emptyCount += 1
			if emptyCount%30 == 0 {
				glog.Infof("[taskworker]take(0)\n")
			}
			select {
			case <-self.runCtx.Done():
				return
			case <-time.After(self.settings.PollTimeout):
			}
		} else {
			emptyCount = 0
		}
	}
}

// enterRun registers a run loop with the drain wait group. Once `Drain` has
// started, run loops must not re-enter: a `runWg.Add` concurrent with the
// drain's `Wait` at counter zero is a WaitGroup reuse violation that panics
// and aborts the drain mid-flight. The taskworker main re-enters `Run` every
// second, so without this guard the race was real at the drain tail.
func (self *TaskWorker) enterRun() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.draining {
		return false
	}
	self.runWg.Add(1)
	return true
}

func (self *TaskWorker) setDraining() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.draining = true
}

func (self *TaskWorker) Draining() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.draining
}

// InflightCount is the number of claimed tasks currently executing in this
// worker.
func (self *TaskWorker) InflightCount() int {
	return int(self.inflightCount.Load())
}

// DrainCanceledCount is the number of task executions that errored under a
// drain cancel; each was rescheduled with its claim released for another
// worker to re-run immediately.
func (self *TaskWorker) DrainCanceledCount() int {
	return int(self.drainCanceledCount.Load())
}

// Drain stops the worker with a bounded wait (TASKDRAIN1 §2.1):
//  1. stop starting new batches and wait DrainFinishTimeout for in-flight
//     tasks to finish naturally (the common case — most tasks run seconds);
//  2. cancel the in-flight task function contexts. A canceled function
//     errors into the normal reschedule path, which releases its claim
//     immediately (release_time = now) for the new container or a sibling
//     block to re-run within seconds;
//  3. wait DrainCancelTimeout for the canceled functions to unwind. A
//     function that ignores its context keeps its lease and rides to the
//     process kill — logged, and the lease correctly prevents a duplicate
//     execution until it expires.
func (self *TaskWorker) Drain() {
	self.setDraining()
	self.runCancel()

	startTime := time.Now()
	elapsedSeconds := func() float32 {
		return float32(time.Since(startTime)/time.Millisecond) / 1000
	}

	if self.waitRunDone(self.settings.DrainFinishTimeout) {
		glog.Infof("[taskworker]drain finished cleanly in %.1fs\n", elapsedSeconds())
		return
	}

	glog.Infof(
		"[taskworker]drain canceling %d in-flight tasks after %.1fs\n",
		self.InflightCount(),
		elapsedSeconds(),
	)
	self.drainCancel()
	if self.waitRunDone(self.settings.DrainCancelTimeout) {
		glog.Infof(
			"[taskworker]drain finished after cancel in %.1fs (%d canceled and rescheduled)\n",
			elapsedSeconds(),
			self.DrainCanceledCount(),
		)
		return
	}

	glog.Infof(
		"[taskworker]drain gave up after %.1fs with %d tasks still running (claims release per task max time)\n",
		elapsedSeconds(),
		self.InflightCount(),
	)
}

// WaitFinalHandback keeps the process alive for one bounded finalization
// grace after Drain. It is immediate when the run loops already finished.
// When Drain gave up on a context-ignoring task, this lets that task unwind
// and run the detached claim handback before the taskworker CLI cancels its
// serving context and exits. A task that still has not returned at the end of
// the grace retains its lease, preserving the no-duplicate-execution rule.
func (self *TaskWorker) WaitFinalHandback() bool {
	timeout := self.settings.FinalizeTimeout
	if timeout <= 0 {
		timeout = DefaultTaskFinalizeTimeout
	}
	return self.waitRunDone(timeout)
}

// waitRunDone waits up to timeout for all run loops (and their in-flight
// batches) to complete. Multiple concurrent waiters are safe; `enterRun`
// guarantees no `Add` races the `Wait` once draining is set.
func (self *TaskWorker) waitRunDone(timeout time.Duration) bool {
	done := make(chan struct{})
	go server.HandleError(func() {
		defer close(done)
		self.runWg.Wait()
	})
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// HasTarget reports whether a function name resolves to a registered target
// (including alternate names). Like AddTargets, registration is expected to
// finish before Run, so this is for setup-time checks — e.g. guarding the
// removed-target reap list against a live function name.
func (self *TaskWorker) HasTarget(functionName string) bool {
	_, ok := self.targets[functionName]
	return ok
}

func (self *TaskWorker) AddTargets(taskTargets ...Target) {
	for _, taskTarget := range taskTargets {
		self.targets[taskTarget.TargetFunctionName()] = taskTarget
		for _, alternateFunctionNames := range taskTarget.AlternateFunctionNames() {
			self.targets[alternateFunctionNames] = taskTarget
		}
	}
}

// runs the post function for a finished taskId
func (self *TaskWorker) RunPost(
	runPost *RunPostArgs,
	clientSession *session.ClientSession,
) (runPostResult *RunPostResult, returnErr error) {
	finishedTasks := GetFinishedTasks(clientSession.Ctx, runPost.TaskId)
	finishedTask, ok := finishedTasks[runPost.TaskId]
	if !ok {
		// The finished_task row is gone, and it is never coming back:
		// RunPost is scheduled in the same tx that writes the row, so the
		// only way to observe its absence is `RemoveFinishedTasks` having
		// reaped it (which it does unconditionally past postErrorMinTime, so
		// a repeatedly-failing post "cannot strand forever"). Erroring here
		// rescheduled the orphan against a row that will never return, so the
		// reap traded a stranded finished_task for a pending_task that
		// retries until the end of time. Complete instead — there is no post
		// work to do — and count it. RunPostPost's UPDATE matches zero rows
		// and succeeds, so the orphan clears.
		orphanedRunPostCounter.Inc()
		if glog.V(1) {
			glog.Infof("[task]run post orphaned: finished task %s was reaped\n", runPost.TaskId)
		}
		return &RunPostResult{}, nil
	}

	// attach the finished task function name and args (%w keeps the error
	// class visible to the reschedule write, e.g. ErrTargetNotFound)
	defer func() {
		if returnErr != nil {
			returnErr = fmt.Errorf("%s(%s) = %w", finishedTask.FunctionName, finishedTask.ArgsJson, returnErr)
		}
	}()

	// update legacy function names
	finishedTask.FunctionName = updateFunctionName(finishedTask.FunctionName)

	if target, ok := self.targets[finishedTask.FunctionName]; ok {
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			if err := target.RunPost(clientSession.Ctx, finishedTask, tx); err == nil {
				runPostResult = &RunPostResult{}
				return
			} else {
				returnErr = err
				return
			}
		})
		return
	} else {
		returnErr = fmt.Errorf("%w (%s).", ErrTargetNotFound, finishedTask.FunctionName)
		return
	}
}

func (self *TaskWorker) RunPostPost(
	runPost *RunPostArgs,
	runPostResult *RunPostResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	_, err := tx.Exec(
		clientSession.Ctx,
		`
			UPDATE finished_task
			SET
				post_completed = true
			WHERE task_id = $1
		`,
		runPost.TaskId,
	)
	return err
}

// takes the n next available tasks, makes an initial timestamp claim, and
// returns the session guard that proves ownership while the tasks run.
func (self *TaskWorker) takeTasks(n int) (
	claimedTasks map[server.Id]*Task,
	claimGuard *taskClaimGuard,
	returnErr error,
) {
	if n <= 0 {
		return map[server.Id]*Task{}, nil, nil
	}

	// The advisory lock must live on a direct PostgreSQL session. A
	// transaction-pooled PgBouncer connection cannot safely own session state.
	conn, err := server.AcquireMaintenanceDbConn(self.ctx)
	if err != nil {
		return nil, nil, err
	}
	guard := &taskClaimGuard{conn: conn}
	retainGuard := false
	defer func() {
		if !retainGuard {
			guard.release()
		}
	}()

	tx, err := conn.Begin(self.ctx)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), DefaultTaskFinalizeTimeout)
		_ = tx.Rollback(rollbackCtx)
		rollbackCancel()
	}()

	// Select from the backlog as well as the current block. The extra candidates
	// let this worker step past expired timestamp leases that are still protected
	// by a live owner's advisory lock, rather than repeatedly sticking on the
	// queue head. Only n advisory locks are actually attempted successfully.
	type taskPriority struct {
		priority       int
		maxTimeSeconds int
	}
	type taskCandidate struct {
		taskId   server.Id
		priority taskPriority
	}

	nowBlock := server.NowUtc().Unix() / BlockSizeSeconds
	candidateLimit := n + 64
	result, err := tx.Query(
		self.ctx,
		`
			SELECT
				task_id,
				run_priority,
				run_max_time_seconds
			FROM pending_task
			WHERE available_block <= $1
			ORDER BY available_block, run_priority DESC, run_max_time_seconds DESC
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		`,
		nowBlock,
		candidateLimit,
	)
	if err != nil {
		return nil, nil, err
	}
	candidates := []taskCandidate{}
	for result.Next() {
		candidate := taskCandidate{}
		if err := result.Scan(
			&candidate.taskId,
			&candidate.priority.priority,
			&candidate.priority.maxTimeSeconds,
		); err != nil {
			result.Close()
			return nil, nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := result.Err(); err != nil {
		result.Close()
		return nil, nil, err
	}
	result.Close()

	taskIds := []server.Id{}
	taskIdPriorities := map[server.Id]taskPriority{}
	for _, candidate := range candidates {
		lockKey := taskAdvisoryLockKey(candidate.taskId)
		var acquired bool
		if err := tx.QueryRow(
			self.ctx,
			`SELECT pg_try_advisory_lock($1)`,
			lockKey,
		).Scan(&acquired); err != nil {
			return nil, nil, err
		}
		if !acquired {
			continue
		}
		taskIds = append(taskIds, candidate.taskId)
		taskIdPriorities[candidate.taskId] = candidate.priority
		if len(taskIds) == n {
			break
		}
	}

	mathrand.Shuffle(len(taskIds), func(i int, j int) {
		taskIds[i], taskIds[j] = taskIds[j], taskIds[i]
	})
	slices.SortStableFunc(taskIds, func(a server.Id, b server.Id) int {
		aPriority := taskIdPriorities[a]
		bPriority := taskIdPriorities[b]
		// descending
		if c := bPriority.priority - aPriority.priority; c != 0 {
			return c
		}
		// descending
		if c := bPriority.maxTimeSeconds - aPriority.maxTimeSeconds; c != 0 {
			return c
		}
		return 0
	})

	// Isolate higher-priority and longer-running work. Unlock any candidates
	// acquired speculatively but excluded by this existing batching rule.
	selectedCount := 0
	for k := min(n, len(taskIds)); selectedCount < k; {
		priority := taskIdPriorities[taskIds[selectedCount]]
		selectedCount += 1
		if DefaultPriority < priority.priority {
			break
		}
		if DefaultMaxTime < time.Duration(priority.maxTimeSeconds)*time.Second {
			break
		}
	}
	for _, taskId := range taskIds[selectedCount:] {
		var unlocked bool
		if err := tx.QueryRow(
			self.ctx,
			`SELECT pg_advisory_unlock($1)`,
			taskAdvisoryLockKey(taskId),
		).Scan(&unlocked); err != nil {
			return nil, nil, err
		}
		if !unlocked {
			return nil, nil, fmt.Errorf("task advisory lock was not held for %s", taskId)
		}
	}
	taskIds = taskIds[:selectedCount]

	claimTime := server.NowUtc()
	releaseTime := claimTime.Add(TaskLeaseTimeout)
	for _, taskId := range taskIds {
		// The short timestamp bounds crash recovery; the session advisory lock
		// above is the durable duplicate-execution guard for a live owner.
		if _, err := tx.Exec(
			self.ctx,
			`
				UPDATE pending_task
				SET
					claim_time = $2,
					release_time = $3
				WHERE task_id = $1
			`,
			taskId,
			claimTime,
			releaseTime,
		); err != nil {
			return nil, nil, err
		}
	}

	if err := tx.Commit(self.ctx); err != nil {
		return nil, nil, err
	}
	if len(taskIds) == 0 {
		return map[server.Id]*Task{}, nil, nil
	}

	claimedTasks = GetTasks(self.ctx, taskIds...)
	claimGuard = guard
	retainGuard = true
	return claimedTasks, claimGuard, nil
}

// return taskIds of the finished tasks, rescheduled tasks
func (self *TaskWorker) EvalTasks(n int) (
	finishedTaskIds []server.Id,
	rescheduledTaskIds []server.Id,
	postRescheduledTaskIds []server.Id,
	returnErr error,
) {
	tasks, claimGuard, err := self.takeTasks(n)
	if err != nil {
		returnErr = err
		return
	}
	if claimGuard != nil {
		defer claimGuard.release()
	}
	if len(tasks) == 0 {
		return
	}

	// Once tasks are claimed, their result collection and final handback must
	// survive cancellation of the process-serving context. Task functions
	// still receive root/drain cancellation below; this detached orchestration
	// context only keeps the collector alive long enough to finalize them.
	evalCtx, evalCancel := context.WithCancel(context.WithoutCancel(self.ctx))
	defer evalCancel()

	for _, task := range tasks {
		// update legacy function names
		task.FunctionName = updateFunctionName(task.FunctionName)
	}

	type finished struct {
		runStartTime time.Time
		runEndTime   time.Time
		resultJson   string
		runPost      func(server.PgTx) error
	}

	type result struct {
		task *Task
		err  error
		finished
	}

	taskCtx, taskCancel := context.WithCancel(evalCtx)
	results := make(chan *result)

	go server.HandleError(func() {
		defer func() {
			taskCancel()
			close(results)
		}()

		var wg sync.WaitGroup

		for _, task := range tasks {
			wg.Add(1)
			go server.HandleError(func() {
				defer wg.Done()

				r := &result{
					task: task,
					finished: finished{
						runStartTime: server.NowUtc(),
					},
				}
				if target, ok := self.targets[task.FunctionName]; ok {
					glog.V(1).Infof("[%s]eval start %s(%s)\n", task.TaskId, task.FunctionName, task.ArgsJson)
					r.runStartTime = server.NowUtc()
					var result any
					var err error
					func() {
						self.inflightCount.Add(1)
						defer self.inflightCount.Add(-1)

						// the function context additionally cancels when a
						// drain gives up waiting (`Drain` phase 2). The task
						// session derives from it, so the cancel aborts the
						// function's db work and surfaces as a normal task
						// error into the reschedule path below.
						fnCtx, fnCancel := context.WithCancel(evalCtx)
						defer fnCancel()
						stopAfterRoot := context.AfterFunc(self.ctx, fnCancel)
						defer stopAfterRoot()
						stopAfterDrain := context.AfterFunc(self.drainCtx, fnCancel)
						defer stopAfterDrain()

						defer func() {
							if r := recover(); r != nil {
								glog.Infof("Unexpected error: %s\n", server.ErrorJson(r, debug.Stack()))
								switch v := r.(type) {
								case error:
									err = v
								default:
									err = fmt.Errorf("%s", r)
								}
							}
						}()
						result, r.runPost, err = target.Run(fnCtx, task)
					}()

					if err == nil {
						var resultJsonBytes []byte
						resultJsonBytes, err = json.Marshal(result)
						if err == nil {
							r.resultJson = string(resultJsonBytes)
						}
					}
					if err != nil && self.drainCtx.Err() != nil {
						// errored while draining (usually the drain cancel
						// itself): tag so the reschedule skips the error
						// count and backoff
						err = fmt.Errorf("%w: %v", ErrDrained, err)
						self.drainCanceledCount.Add(1)
					}
					r.err = err
				} else {
					r.err = fmt.Errorf("%w (%s).", ErrTargetNotFound, task.FunctionName)
				}

				r.runEndTime = server.NowUtc()
				select {
				case results <- r:
				case <-taskCtx.Done():
					return
				}
			})
		}

		wg.Wait()
	})

	finishedTasks := map[server.Id]*finished{}
	rescheduledTasks := map[server.Id]error{}
	postRescheduledTasks := map[server.Id]error{}

	func() {
		defer taskCancel()

		startTime := time.Now()
		for {
			select {
			case <-taskCtx.Done():
				return
			case r, ok := <-results:
				if !ok {
					return
				}
				elapsedSeconds := float32(r.runEndTime.Sub(r.runStartTime)/time.Millisecond) / 1000
				if r.err == nil {
					glog.V(1).Infof("[%s]eval done(%.2fs) %s(%s) = %s\n", r.task.TaskId, elapsedSeconds, r.task.FunctionName, r.task.ArgsJson, string(r.resultJson))
					finishedTasks[r.task.TaskId] = &r.finished
				} else {
					glog.Infof("[%s]eval error(%.2fs) (reschedule) %s(%s) = %s\n", r.task.TaskId, elapsedSeconds, r.task.FunctionName, r.task.ArgsJson, r.err)
					rescheduledTasks[r.task.TaskId] = r.err
				}

			case <-time.After(ReleaseTimeout / 3):
				elapsedSeconds := float32(time.Now().Sub(startTime)/time.Millisecond) / 1000
				if 10 <= elapsedSeconds {
					for _, task := range tasks {
						glog.Infof("[%s]eval active(%.2fs) %s(%s)\n", task.TaskId, elapsedSeconds, task.FunctionName, task.ArgsJson)
					}
				}

				// A drain give-up can cancel the serving root while a
				// context-ignoring task is still unwinding. Keep its lease
				// heartbeat bounded but detached too; otherwise a canceled
				// heartbeat panics out of EvalTasks before the later result
				// can reach the detached finalization transaction.
				heartbeatTimeout := self.settings.FinalizeTimeout
				if heartbeatTimeout <= 0 {
					heartbeatTimeout = DefaultTaskFinalizeTimeout
				}
				heartbeatCtx, heartbeatCancel := context.WithTimeout(
					context.WithoutCancel(self.ctx),
					heartbeatTimeout,
				)
				// Keep the direct session carrying the advisory ownership lock
				// active and fail this evaluation if that ownership session is lost.
				server.Raise(claimGuard.ping(heartbeatCtx))
				server.Tx(heartbeatCtx, func(tx server.PgTx) {
					server.BatchInTx(heartbeatCtx, tx, func(batch server.PgBatch) {
						claimTime := server.NowUtc()
						releaseTime := claimTime.Add(TaskLeaseTimeout)

						for _, task := range tasks {
							// GREATEST prevents a backwards clock adjustment from
							// shortening an existing lease. Under a normal clock every
							// heartbeat advances the bounded recovery deadline.
							batch.Queue(
								`
									UPDATE pending_task
									SET
										claim_time = $2,
										release_time = GREATEST(release_time, $3)
									WHERE task_id = $1
								`,
								task.TaskId,
								claimTime,
								releaseTime,
							)
						}
					})
				})
				heartbeatCancel()
			}
		}
	}()

	for _, task := range tasks {
		_, rescheduled := rescheduledTasks[task.TaskId]
		_, finished := finishedTasks[task.TaskId]
		if !rescheduled && !finished {
			// this task was not recorded
			// treat it as rescheduled
			// LOG("Task not run.")

			rescheduledTasks[task.TaskId] = errors.New("Task not run.")
		}
	}

	finalizeTimeout := self.settings.FinalizeTimeout
	if finalizeTimeout <= 0 {
		finalizeTimeout = DefaultTaskFinalizeTimeout
	}
	finalizeCtx, finalizeCancel := context.WithTimeout(
		context.WithoutCancel(self.ctx),
		finalizeTimeout,
	)
	defer finalizeCancel()

	server.Tx(finalizeCtx, func(tx server.PgTx) {
		server.BatchInTx(finalizeCtx, tx, func(batch server.PgBatch) {
			for taskId, finished := range finishedTasks {
				batch.Queue(
					`
					INSERT INTO finished_task (
						task_id,
				        function_name,
				        args_json,
				        client_address,
				        client_address_hash,
				        client_address_port,
				        client_by_jwt_json,
				        run_at,
				        run_once_key,
				        run_priority,
				        run_max_time_seconds,

				        run_start_time,
				        run_end_time,
				        reschedule_error,
				        result_json
					)
					SELECT
						task_id,
				        function_name,
				        args_json,
				        client_address,
				        client_address_hash,
				        client_address_port,
				        client_by_jwt_json,
				        run_at,
				        run_once_key,
				        run_priority,
				        run_max_time_seconds,

				        $2 AS run_start_time,
				        $3 AS run_end_time,
				        reschedule_error,
				        $4 AS result_json
					
					FROM pending_task
					WHERE task_id = $1
					`,
					taskId,
					finished.runStartTime,
					finished.runEndTime,
					finished.resultJson,
				)

				batch.Queue(
					`
					DELETE FROM pending_task
					WHERE task_id = $1
					`,
					taskId,
				)
			}

			for taskId, err := range rescheduledTasks {
				now := server.NowUtc()
				// Exponential backoff is computed in Go so saturated retries can
				// receive proportional jitter. The first error retains the old
				// fast cadence (transient blips stay fast); a wedged cohort
				// converges to a one-hour mean while spreading retries across a
				// full hour instead of preserving an outage wave. The exponent
				// remains clamped to keep power() bounded.
				//
				// Two error classes adjust the backoff:
				// - drained (operator-caused): no error-count advance and a
				//   flat ~RescheduleTimeout retry; release_time = now below
				//   releases the claim so another worker re-runs immediately
				// - target not found (deploy version skew): the count still
				//   advances (visibility) but the exponent clamps low, so the
				//   retry converges to ~16s instead of the backoff cap
				errorCountDelta := 1
				backoffMaxExponent := rescheduleBackoffMaxExponent
				if errors.Is(err, ErrDrained) {
					errorCountDelta = 0
					backoffMaxExponent = 0
				} else if errors.Is(err, ErrTargetNotFound) {
					backoffMaxExponent = targetNotFoundBackoffMaxExponent
				}
				rescheduleTime := now.Add(errorRescheduleDelay(
					RescheduleTimeout,
					RescheduleBackoffMaxTimeout,
					tasks[taskId].RescheduleErrorCount,
					backoffMaxExponent,
					mathrand.Float64(),
				))
				batch.Queue(
					`
						UPDATE pending_task
						SET
							reschedule_error = $2,
							reschedule_error_count = pending_task.reschedule_error_count + $5,
							run_at = $3,
							release_time = $4
						WHERE task_id = $1
					`,
					taskId,
					err.Error(),
					rescheduleTime,
					now,
					errorCountDelta,
				)
			}
		})

		for taskId, finished := range finishedTasks {
			if err := finished.runPost(tx); err != nil {
				// record the post error

				postRescheduledTasks[taskId] = err

				tx.Exec(
					finalizeCtx,
					`
						UPDATE finished_task
						SET
							post_error = $2,
							post_completed = false
						WHERE task_id = $1
					`,
					taskId,
					err.Error(),
				)

				// re-run the post
				func() {
					now := server.NowUtc()
					rescheduleTime := now.Add(time.Second * time.Duration(mathrand.Intn(int(RescheduleTimeout/time.Second))))
					task := tasks[taskId]
					clientSession, err := task.ClientSession(finalizeCtx)
					if err != nil {
						panic(err)
					}
					defer clientSession.Cancel()
					ScheduleTaskInTx(
						tx,
						self.RunPost,
						&RunPostArgs{TaskId: taskId},
						clientSession,
						RunAt(rescheduleTime),
					)
				}()
			}
		}
	})

	for taskId, _ := range finishedTasks {
		if _, postRescheduled := postRescheduledTasks[taskId]; !postRescheduled {
			finishedTaskIds = append(finishedTaskIds, taskId)
		}
	}
	for taskId, _ := range rescheduledTasks {
		rescheduledTaskIds = append(rescheduledTaskIds, taskId)
	}
	for taskId, _ := range postRescheduledTasks {
		postRescheduledTaskIds = append(postRescheduledTaskIds, taskId)
	}

	return
}

func (self *TaskWorker) Close() {
	self.cancel()
}

// PERIODIC CLEANUP

type TaskCleanupArgs struct {
}

type TaskCleanupResult struct {
}

func ScheduleTaskCleanup(clientSession *session.ClientSession, tx server.PgTx) {
	ScheduleTaskInTx(
		tx,
		TaskCleanup,
		&TaskCleanupArgs{},
		clientSession,
		RunOnce("task_cleanup"),
		RunAt(time.Now().Add(1*time.Hour)),
	)
}

func TaskCleanup(
	taskCleanup *TaskCleanupArgs,
	clientSession *session.ClientSession,
) (*TaskCleanupResult, error) {
	minTime := time.Now().Add(-24 * time.Hour)
	postErrorMinTime := time.Now().Add(-7 * 24 * time.Hour)
	RemoveFinishedTasks(clientSession.Ctx, minTime, postErrorMinTime)
	return &TaskCleanupResult{}, nil
}

func TaskCleanupPost(
	taskCleanup *TaskCleanupArgs,
	taskCleanupResult *TaskCleanupResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleTaskCleanup(clientSession, tx)
	return nil
}
