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

// Explicit shutdown signals fail and reschedule the task without an unexpected
// stack report. Database failures and cancellation-like diagnostic text retain
// their full report; only typed cancellation is benign.
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

// taskTimestampLeaseRefreshErrorCounter counts timestamp-heartbeat writes that
// failed while the direct PostgreSQL session still proved advisory ownership.
// The advisory lock is the duplicate-execution guard; this timestamp is only
// the bounded crash-recovery hint, so a pooled write stall must stay visible
// without canceling live work and releasing its ownership session.
var taskTimestampLeaseRefreshErrorCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "task",
		Name:      "timestamp_lease_refresh_errors_total",
		Help:      "Task timestamp lease refreshes that failed while advisory ownership remained healthy",
	},
)

func init() {
	prometheus.MustRegister(orphanedRunPostCounter)
	prometheus.MustRegister(taskTimestampLeaseRefreshErrorCounter)
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
	conn         server.PgConn
	releaseOnce  sync.Once
	admissionKVs map[server.Id]*taskClaimReservation
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
		defer func() {
			for _, reservation := range self.admissionKVs {
				reservation.release()
			}
		}()
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

// A post that also returns work for after the finishing transaction commits.
// External side effects (account messages, outbound calls) belong in that
// work, not in the transaction: the transaction can roll back or rerun its
// callback, and it holds its connection and row locks until the commit. The
// worker runs the work once per committed finish, after `server.Tx` returns,
// and waits for it; a post that returns an error, or a finish that rolls back,
// drops it, and a crash between the commit and the work loses it. The post's
// client session is canceled by then, so the work must not use its context.
type TaskCommitPostFunction[T any, R any] func(T, R, *session.ClientSession, server.PgTx) ([]server.PostFunction, error)

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

// The post hooks run in the caller's transaction and return the work to run
// after it commits (see `TaskCommitPostFunction`).
type Target interface {
	TargetFunctionName() string
	// TargetFunction() TaskFunction[T, R]
	// PostFunction() TaskPostFunction[T, R]
	AlternateFunctionNames() []string
	Run(context.Context, *Task) (any, func(server.PgTx) ([]server.PostFunction, error), error)
	RunPost(context.Context, *FinishedTask, server.PgTx) ([]server.PostFunction, error)
}

type TaskTarget[T any, R any] struct {
	targetFunctionName     string
	targetFunction         TaskFunction[T, R]
	postFunction           TaskCommitPostFunction[T, R]
	alternateFunctionNames []string
	// An instance-local timer boundary permits deterministic max-time controls.
	runAfter func(time.Duration) <-chan time.Time
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

// The post has no work for after the commit.
func NewTaskTargetWithPost[T any, R any](
	targetFunction TaskFunction[T, R],
	postFunction TaskPostFunction[T, R],
	alternateFunctionNames ...string,
) *TaskTarget[T, R] {
	var commitPostFunction TaskCommitPostFunction[T, R]
	if postFunction != nil {
		commitPostFunction = func(args T, result R, clientSession *session.ClientSession, tx server.PgTx) ([]server.PostFunction, error) {
			return nil, postFunction(args, result, clientSession, tx)
		}
	}
	return NewTaskTargetWithCommitPost(targetFunction, commitPostFunction, alternateFunctionNames...)
}

// The post can return work for after the finishing transaction commits.
func NewTaskTargetWithCommitPost[T any, R any](
	targetFunction TaskFunction[T, R],
	postFunction TaskCommitPostFunction[T, R],
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
	runPost func(server.PgTx) ([]server.PostFunction, error),
	returnErr error,
) {
	return self.RunSpecific(ctx, task)
}

func (self *TaskTarget[T, R]) RunSpecific(ctx context.Context, task *Task) (
	result R,
	runPost func(server.PgTx) ([]server.PostFunction, error),
	returnErr error,
) {
	ctx = withExecutionIdentity(ctx, task.TaskId)
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
	timerDone := make(chan struct{})
	after := self.runAfter
	if after == nil {
		after = time.After
	}

	go server.HandleError(func() {
		defer close(timerDone)
		defer clientSession.Cancel()
		select {
		case <-clientSession.Ctx.Done():
		case <-after(max(
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
		// Join before reading the timer's result, including recovered panics.
		// A canceled max-time context must not lose its timeout attribution.
		clientSession.Cancel()
		<-timerDone
		if timeout {
			returnErr = errors.Join(errors.New("Timeout"), returnErr)
			runPost = nil
		}
	}()

	result, returnErr = self.targetFunction(args, clientSession)
	if returnErr != nil {
		if clientSession.Ctx.Err() != nil {
			returnErr = withoutTaskRetryArgs(returnErr)
		}
		return
	}

	runPost = func(tx server.PgTx) ([]server.PostFunction, error) {
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
			return nil, err
		}
		defer clientSession.Cancel()
		if self.postFunction == nil {
			return nil, nil
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
) (commitPosts []server.PostFunction, returnErr error) {
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
	timerDone := make(chan struct{})
	after := self.runAfter
	if after == nil {
		after = time.After
	}

	go server.HandleError(func() {
		defer close(timerDone)
		defer clientSession.Cancel()
		select {
		case <-clientSession.Ctx.Done():
		case <-after(max(
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
		clientSession.Cancel()
		<-timerDone
		if timeout {
			returnErr = errors.Join(errors.New("Timeout"), returnErr)
		}
		if returnErr != nil {
			// a failed post is retried whole, with its work
			commitPosts = nil
		}
	}()

	commitPosts, returnErr = self.postFunction(args, result, clientSession, tx)

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
	// ClaimRegisteredTargetsOnly leaves other workloads' pending rows untouched.
	// Filtering precedes the candidate limit, including the owner of a RunPost
	// retry, so an unrelated backlog cannot starve registered work.
	ClaimRegisteredTargetsOnly bool
	// Opt-in per-instance limits, keyed by canonical target function name.
	// Aliases share their target's limit; RunPost wrappers do not borrow it.
	// Nil leaves every target unlimited. Construction snapshots this map.
	TargetClaimLimits map[string]int
	// how long `Drain` waits for in-flight tasks to finish naturally before
	// canceling their contexts
	DrainFinishTimeout time.Duration
	// how long `Drain` waits after the cancel for the canceled task
	// functions to unwind; a function that ignores its context keeps its
	// claim lease and rides to the process kill
	DrainCancelTimeout time.Duration
	// bounds each detached transaction that records a returned task's
	// completion/reschedule. It deliberately outlives the serving root context
	// during shutdown; batch advisory ownership stays until all tasks join.
	FinalizeTimeout time.Duration
}

type TaskWorker struct {
	ctx       context.Context
	cancel    context.CancelFunc
	runCtx    context.Context
	runCancel context.CancelFunc
	// canceled by `Drain` after DrainFinishTimeout to abort the in-flight
	// task function contexts (the eval/finalize machinery stays on ctx)
	drainCtx          context.Context
	drainCancel       context.CancelFunc
	runWg             sync.WaitGroup
	targets           map[string]Target
	targetMetricNames map[string]string
	settings          *TaskWorkerSettings

	// These production-boundary functions are fields so tests can trigger a
	// heartbeat and a pooled refresh panic with explicit barriers. Every worker
	// constructed through NewTaskWorker receives the real clock and DB write.
	heartbeatAfter             func(time.Duration) <-chan time.Time
	heartbeatNow               func() time.Time
	refreshTaskTimestampLeases func(context.Context, map[server.Id]*Task)
	// Drain logs are best effort: a full stdout pipe must not hold shutdown.
	drainLogf func(string, ...any)
	// Test barriers sit at real claim boundaries, without changing production
	// ownership: nil leaves the direct PostgreSQL path untouched.
	claimBeforeQuery     func(server.PgTx) error
	claimCandidatesReady func()
	claimBeforeCommit    func(*taskClaimGuard) error

	stateLock         sync.Mutex
	draining          bool
	claimTargetCounts map[string]int

	inflightCount      atomic.Int64
	drainCanceledCount atomic.Int64
}

func NewTaskWorkerWithDefaults(ctx context.Context) *TaskWorker {
	return NewTaskWorker(ctx, DefaultTaskWorkerSettings())
}

func NewTaskWorker(ctx context.Context, settings *TaskWorkerSettings) *TaskWorker {
	settings = snapshotTaskWorkerSettings(settings)
	cancelCtx, cancel := context.WithCancel(ctx)
	runCtx, runCancel := context.WithCancel(cancelCtx)
	drainCtx, drainCancel := context.WithCancel(cancelCtx)

	taskWorker := &TaskWorker{
		ctx:                        cancelCtx,
		cancel:                     cancel,
		runCtx:                     runCtx,
		runCancel:                  runCancel,
		drainCtx:                   drainCtx,
		drainCancel:                drainCancel,
		targets:                    map[string]Target{},
		targetMetricNames:          map[string]string{},
		settings:                   settings,
		heartbeatAfter:             time.After,
		heartbeatNow:               time.Now,
		refreshTaskTimestampLeases: refreshTaskTimestampLeases,
		drainLogf:                  glog.Infof,
		claimTargetCounts:          map[string]int{},
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
			taskPollsTotal.WithLabelValues("error").Inc()
			glog.Infof("[taskworker]error running tasks: %s\n", err)
			select {
			case <-self.runCtx.Done():
				return
			case <-time.After(self.settings.RetryTimeoutAfterError):
			}
		} else if len(finishedTaskIds)+len(rescheduledTaskIds)+len(postRescheduledTaskIds) == 0 {
			taskPollsTotal.WithLabelValues("empty").Inc()
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
			taskPollsTotal.WithLabelValues("claimed").Inc()
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
		self.logDrainAsync("[taskworker]drain finished cleanly in %.1fs\n", elapsedSeconds())
		return
	}

	self.logDrainAsync(
		"[taskworker]drain canceling %d in-flight tasks after %.1fs\n",
		self.InflightCount(),
		elapsedSeconds(),
	)
	self.drainCancel()
	if self.waitRunDone(self.settings.DrainCancelTimeout) {
		self.logDrainAsync(
			"[taskworker]drain finished after cancel in %.1fs (%d canceled and rescheduled)\n",
			elapsedSeconds(),
			self.DrainCanceledCount(),
		)
		return
	}

	self.logDrainAsync(
		"[taskworker]drain gave up after %.1fs with %d tasks still running (claims release per task max time)\n",
		elapsedSeconds(),
		self.InflightCount(),
	)
}

// Logging is not a shutdown gate. A stalled stdout/journal consumer can block
// glog indefinitely; the drain deadlines and task cancellation must still run.
func (self *TaskWorker) logDrainAsync(format string, args ...any) {
	if self.drainLogf != nil {
		go self.drainLogf(format, args...)
	}
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
		metricName := taskMetricName(taskTarget.TargetFunctionName())
		self.targets[taskTarget.TargetFunctionName()] = taskTarget
		self.targetMetricNames[taskTarget.TargetFunctionName()] = metricName
		for _, alternateFunctionNames := range taskTarget.AlternateFunctionNames() {
			self.targets[alternateFunctionNames] = taskTarget
			self.targetMetricNames[alternateFunctionNames] = metricName
		}
	}
}

// metricName resolves only registered targets and aliases; an arbitrary stale
// database function never becomes a Prometheus label.
func (self *TaskWorker) metricName(functionName string) string {
	if metricName, ok := self.targetMetricNames[functionName]; ok {
		return metricName
	}
	return "unregistered"
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
			returnErr = fmt.Errorf("%s(%s) = %w", finishedTask.FunctionName, ArgumentsForLog(finishedTask.ArgsJson), returnErr)
		}
	}()

	// update legacy function names
	finishedTask.FunctionName = updateFunctionName(finishedTask.FunctionName)

	if target, ok := self.targets[finishedTask.FunctionName]; ok {
		var commitPosts []server.PostFunction
		server.Tx(clientSession.Ctx, func(tx server.PgTx) {
			// a rerun callback starts over: neither the outcome nor the work
			// of a rolled-back attempt may outlive it
			commitPosts = nil
			runPostResult = nil
			returnErr = nil
			if posts, err := target.RunPost(clientSession.Ctx, finishedTask, tx); err == nil {
				commitPosts = posts
				runPostResult = &RunPostResult{}
				return
			} else {
				returnErr = err
				return
			}
		})
		// the post's transaction committed
		server.RunPosts(clientSession.Ctx, commitPosts...)
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
	// a failed update raises: a returned error would leave the finalize
	// transaction to commit a rollback
	server.RaisePgResult(tx.Exec(
		clientSession.Ctx,
		`
			UPDATE finished_task
			SET
				post_completed = true
			WHERE task_id = $1
		`,
		runPost.TaskId,
	))
	return nil
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
	guard := &taskClaimGuard{conn: conn, admissionKVs: map[server.Id]*taskClaimReservation{}}
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
	if self.claimBeforeQuery != nil {
		if err := self.claimBeforeQuery(tx); err != nil {
			return nil, nil, err
		}
	}

	// Keep the existing ordered fallback window, but fetch only enough rows to
	// fill the batch. Unneeded fallback rows must remain unlocked for other
	// evaluators, and their saturated-target suffix must not be scanned eagerly.
	type taskPriority struct {
		priority       int
		maxTimeSeconds int
	}
	type taskCandidate struct {
		taskId       server.Id
		functionName string
		priority     taskPriority
	}

	nowBlock := server.NowUtc().Unix() / BlockSizeSeconds
	candidateLimit := n + 64
	query, queryArgs := self.claimCandidatesQuery(nowBlock, candidateLimit)
	// Keep the queue name visible in FETCH for the existing query monitors.
	_, err = tx.Exec(
		self.ctx,
		`DECLARE pending_task_claim_candidates NO SCROLL CURSOR FOR `+query,
		queryArgs...,
	)
	if err != nil {
		return nil, nil, err
	}

	taskIds := []server.Id{}
	taskIdPriorities := map[server.Id]taskPriority{}
	for candidateCount := 0; len(taskIds) < n && candidateCount < candidateLimit; {
		fetchCount := min(n-len(taskIds), candidateLimit-candidateCount)
		// A forward cursor preserves one scan and snapshot across refusals.
		// PostgreSQL locks FOR UPDATE rows only when FETCH returns them; the
		// transaction closes this non-holdable cursor on commit or rollback.
		result, err := tx.Query(
			self.ctx,
			fmt.Sprintf(`FETCH FORWARD %d FROM pending_task_claim_candidates`, fetchCount),
		)
		if err != nil {
			return nil, nil, err
		}
		candidates := make([]taskCandidate, 0, fetchCount)
		for result.Next() {
			candidate := taskCandidate{}
			if err := result.Scan(
				&candidate.taskId,
				&candidate.functionName,
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
		if candidateCount == 0 && self.claimCandidatesReady != nil {
			self.claimCandidatesReady()
		}
		candidateCount += len(candidates)
		for _, candidate := range candidates {
			reservation, admitted := self.reserveTaskClaim(candidate.functionName)
			if !admitted {
				continue
			}
			if reservation != nil {
				guard.admissionKVs[candidate.taskId] = reservation
			}
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
				guard.releaseAdmission(candidate.taskId)
				continue
			}
			taskIds = append(taskIds, candidate.taskId)
			taskIdPriorities[candidate.taskId] = candidate.priority
		}
		if len(candidates) < fetchCount {
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
		guard.releaseAdmission(taskId)
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

	if self.claimBeforeCommit != nil {
		if err := self.claimBeforeCommit(guard); err != nil {
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

// refreshTaskTimestampLeases writes the short crash-recovery timestamps through
// the ordinary pooled DB path. server.Tx raises DB errors, so callers must keep
// this operation behind tryRefreshTaskTimestampLeases's narrow recovery
// boundary. The direct taskClaimGuard session remains the ownership authority.
func refreshTaskTimestampLeases(
	ctx context.Context,
	tasks map[server.Id]*Task,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
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
}

// tryRefreshTaskTimestampLeases converts only the pooled timestamp-refresh
// operation's panic contract into an error. Losing that recovery hint is
// observable but nonfatal while claimGuard.ping has just proved the direct
// advisory-lock session healthy.
func tryRefreshTaskTimestampLeases(
	ctx context.Context,
	tasks map[server.Id]*Task,
	refresh func(context.Context, map[server.Id]*Task),
) (returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = fmt.Errorf("refresh task timestamp leases: %w", err)
			} else {
				returnErr = fmt.Errorf("refresh task timestamp leases: %v", recovered)
			}
		}
	}()
	refresh(ctx, tasks)
	return nil
}

// One completed execution; its durable handback is independent of the other
// task functions claimed by the same evaluator.
type taskExecutionResult struct {
	task         *Task
	err          error
	runStartTime time.Time
	runEndTime   time.Time
	resultJson   string
	runPost      func(server.PgTx) ([]server.PostFunction, error)
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
	if claimGuard == nil {
		returnErr = errors.New("nonempty task claim has no advisory ownership guard")
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
	executionTargets := self.prepareTaskBatchTargets(tasks)

	taskCtx, taskCancel := context.WithCancel(evalCtx)
	results := make(chan *taskExecutionResult)
	executionAdmissions := claimGuard.retainExecutionAdmissions(tasks)

	go server.HandleError(func() {
		defer executionAdmissions.releaseUnlaunched()
		defer func() {
			taskCancel()
			close(results)
		}()

		var wg sync.WaitGroup

		for _, task := range tasks {
			wg.Add(1)
			reservation := executionAdmissions.take(task.TaskId)
			go server.HandleError(func() {
				defer wg.Done()
				if reservation != nil {
					defer reservation.release()
				}
				metricName := self.metricName(task.FunctionName)
				attribution := taskMetricAttribution(task)
				taskExecutionInflight.WithLabelValues(metricName, attribution).Inc()
				defer taskExecutionInflight.WithLabelValues(metricName, attribution).Dec()

				r := &taskExecutionResult{
					task:         task,
					runStartTime: server.NowUtc(),
				}
				if target, ok := executionTargets[task.FunctionName]; ok {
					glog.V(1).Infof("[%s]eval start %s(%s)\n", task.TaskId, task.FunctionName, ArgumentsForLog(task.ArgsJson))
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
				recordTaskExecution(
					metricName,
					attribution,
					len(task.ArgsJson),
					len(r.resultJson),
					r.runEndTime.Sub(r.runStartTime),
					r.err,
				)
				select {
				case results <- r:
				case <-taskCtx.Done():
					return
				}
			})
		}

		wg.Wait()
	})

	// The launcher concurrently ranges tasks. These collector-owned maps track
	// receipt and durable handback separately, including an ambiguous commit.
	unreceivedTasks := make(map[server.Id]*Task, len(tasks))
	heartbeatTasks := make(map[server.Id]*Task, len(tasks))
	for taskId, task := range tasks {
		unreceivedTasks[taskId] = task
		heartbeatTasks[taskId] = task
	}
	var commitPosts []server.PostFunction
	defer func() {
		// A later collector/ownership failure must not discard work from an
		// already committed finish. Cancel live functions before external work,
		// keeping that handback detached and the batch guard until it returns.
		evalCancel()
		server.RunPosts(context.WithoutCancel(evalCtx), commitPosts...)
	}()
	var finalizePanic any
	finalize := func(r *taskExecutionResult) {
		// A failed handback must not unwind the batch guard while unrelated
		// task functions are still executing. Do not replay an unknown commit.
		defer func() {
			if recovered := recover(); recovered != nil {
				if finalizePanic == nil {
					finalizePanic = recovered
				}
				glog.Infof("[%s]task finalization failed: %v\n", r.task.TaskId, recovered)
			}
		}()
		posts, postRescheduled := self.finalizeTask(r)
		delete(heartbeatTasks, r.task.TaskId)
		commitPosts = append(commitPosts, posts...)
		switch {
		case r.err != nil:
			rescheduledTaskIds = append(rescheduledTaskIds, r.task.TaskId)
			taskFinalizationsTotal.WithLabelValues("rescheduled").Inc()
		case postRescheduled:
			postRescheduledTaskIds = append(postRescheduledTaskIds, r.task.TaskId)
			taskFinalizationsTotal.WithLabelValues("post_rescheduled").Inc()
		default:
			finishedTaskIds = append(finishedTaskIds, r.task.TaskId)
			taskFinalizationsTotal.WithLabelValues("succeeded").Inc()
		}
	}

	func() {
		defer taskCancel()

		startTime := self.heartbeatNow()
		nextHeartbeatTime := startTime.Add(ReleaseTimeout / 3)
		heartbeat := func() {
			elapsedSeconds := float32(self.heartbeatNow().Sub(startTime)/time.Millisecond) / 1000
			if 10 <= elapsedSeconds {
				for _, task := range heartbeatTasks {
					glog.Infof("[%s]eval active(%.2fs) %s(%s)\n", task.TaskId, elapsedSeconds, task.FunctionName, ArgumentsForLog(task.ArgsJson))
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
			func() {
				defer heartbeatCancel()
				server.Raise(claimGuard.ping(heartbeatCtx))
				if err := tryRefreshTaskTimestampLeases(
					heartbeatCtx,
					heartbeatTasks,
					self.refreshTaskTimestampLeases,
				); err != nil {
					taskTimestampLeaseRefreshErrorCounter.Inc()
					glog.Infof(
						"[taskworker]timestamp lease refresh failed while advisory ownership remained healthy: %v\n",
						err,
					)
				}
			}()
			nextHeartbeatTime = self.heartbeatNow().Add(ReleaseTimeout / 3)
		}
		for {
			// Finalizing ready results must not reset the lease clock. Check
			// an overdue heartbeat between bounded handbacks even when result
			// delivery is continuously ready; the guard connection stays serial.
			heartbeatDelay := nextHeartbeatTime.Sub(self.heartbeatNow())
			if heartbeatDelay <= 0 {
				heartbeat()
				continue
			}
			select {
			case <-taskCtx.Done():
				return
			case r, ok := <-results:
				if !ok {
					return
				}
				elapsedSeconds := float32(r.runEndTime.Sub(r.runStartTime)/time.Millisecond) / 1000
				if r.err == nil {
					glog.V(1).Infof("[%s]eval done(%.2fs) %s(%s) = %s\n", r.task.TaskId, elapsedSeconds, r.task.FunctionName, ArgumentsForLog(r.task.ArgsJson), string(r.resultJson))
				} else {
					glog.Infof("[%s]eval error(%.2fs) (reschedule) %s(%s) = %s\n", r.task.TaskId, elapsedSeconds, r.task.FunctionName, ArgumentsForLog(r.task.ArgsJson), r.err)
				}

				delete(unreceivedTasks, r.task.TaskId)
				finalize(r)

			case <-self.heartbeatAfter(heartbeatDelay):
				heartbeat()
			}
		}
	}()

	for _, task := range unreceivedTasks {
		finalize(&taskExecutionResult{task: task, err: errors.New("Task not run.")})
	}
	if finalizePanic != nil {
		panic(finalizePanic)
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
