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

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
)

// Explicit shutdown signals fail and reschedule the task without an unexpected
// stack report. Database failures and cancellation-like diagnostic text retain
// their full report; only typed cancellation is benign.
func taskPanicError(r any) error {
	if server.IsDoneError(r) {
		message := fmt.Sprintf("Interrupted: %v", r)
		if glog.V(1) {
			message = fmt.Sprintf("Interrupted: %s", server.ErrorJson(r, debug.Stack()))
		}
		if cause, ok := r.(error); ok {
			return &taskInterruptedPanic{message: message, cause: cause}
		}
		return errors.New(message)
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

// Busy queue ownership or a nonparticipating row lock defers only the crash
// timestamp hint. The separately pinged execution guard still fences live work.
var taskTimestampLeaseRefreshSkippedCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "task",
		Name:      "timestamp_lease_refresh_skipped_total",
		Help:      "Task timestamp hints skipped because ownership was busy or the exact claim no longer matched",
	},
)

func init() {
	prometheus.MustRegister(taskTimestampLeaseRefreshSkippedCounter)
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
	conn              server.PgConn
	completionSession *server.PgOwnedSession
	releaseOnce       sync.Once
	admissionKVs      map[server.Id]*taskClaimReservation
	// Collector-owned exact identities also prevent reentrant session claims.
	taskIds        map[server.Id]bool
	groupKeyStates map[taskClaimGroupKey]*taskClaimGroupState
	taskGroupKeys  map[server.Id][]taskClaimGroupKey
}

func (self *taskClaimGuard) ping(ctx context.Context) error {
	if self == nil || self.conn == nil {
		return errors.New("task claim guard is not active")
	}
	if err := self.completionSessionError(); err != nil {
		return err
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

// Pending requests coalesce by key. A request after claim owns one successor
// after successful handback; it does not replace the existing invocation args.
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

	prepared := preparedTask{
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
	requirePreparedTaskOwnership(prepared, opts)
	return prepared
}

func ScheduleTaskInTx[T any, R any](
	tx server.PgTx,
	taskFunction TaskFunction[T, R],
	args T,
	clientSession *session.ClientSession,
	opts ...any,
) (taskId server.Id) {
	requireTaskPublicationBackend(tx, opts)
	p := prepareTask(taskFunction, args, clientSession, opts...)

	claimTime := time.Time{}

	var inserted bool
	server.Raise(tx.QueryRow(
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
				run_max_time_seconds = GREATEST(pending_task.run_max_time_seconds, $10),
				run_once_generation = pending_task.run_once_generation + 1,
				run_once_wake_at = LEAST(pending_task.run_once_wake_at, $7)
			RETURNING task_id=$1
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
	).Scan(&inserted))
	observeTaskSubmissionInTx(tx, inserted)
	return p.taskId
}

// ScheduleTaskInTxIfAbsent is like ScheduleTaskInTx but for callers that need
// an atomic "only schedule if not already pending under this key" guarantee,
// instead of RunOnce's merge-on-conflict semantics. RunOnce's
// `ON CONFLICT (run_once_key) DO UPDATE` merges scheduling metadata and records
// a wake generation, while preserving the existing args_json. If two different
// calls share a run_once key while the
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
	requireTaskPublicationBackend(tx, opts)
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
	observeTaskSubmissionInTx(tx, scheduled)
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
	return getTasks(ctx, false, taskIds...)
}

// A bounded, acknowledged Run claim reads its exact members with one fresh
// statement and no automatic retry, including cohorts above the legacy32 cutoff.
// Other large callers retain their existing transaction-local table policy.
func getTasks(ctx context.Context, exactClaim bool, taskIds ...server.Id) map[server.Id]*Task {
	if len(taskIds) == 0 {
		return map[server.Id]*Task{}
	}
	var tasks map[server.Id]*Task
	if exactClaim || len(taskIds) < 32 {
		server.Db(ctx, func(conn server.PgConn) {
			tasks = getTasksInConn(ctx, conn, exactClaim, taskIds...)
		}, server.OptNoRetry())
	} else {
		// Large public reads keep their transaction-local ID table and snapshot.
		server.Tx(ctx, func(tx server.PgTx) {
			server.CreateTempTableInTx(ctx, tx, "temp_task_ids(task_id uuid)", taskIds...)
			tasks = getTasksInConn(ctx, tx, false, taskIds...)
		})
	}
	return tasks
}

// Read through an existing owner without acquiring another PostgreSQL connection.
// The caller owns any transaction and keeps this query within its connection's
// serialized lifetime. Rows close before a claim can launch or heartbeat.
func getTasksInConn(ctx context.Context, query server.PgCanQuery, exactClaim bool, taskIds ...server.Id) map[server.Id]*Task {
	if len(taskIds) == 0 {
		return map[server.Id]*Task{}
	}
	tasks := map[server.Id]*Task{}
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
	        pending_task.run_once_generation,
	        pending_task.claim_generation,
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

	if exactClaim {
		result, err = query.Query(ctx, selectSql+` WHERE task_id=ANY($1::uuid[])`, taskIds)
	} else if len(taskIds) < 32 {
		// `task_id IN (...)` is more efficient than a temp table for small lists

		taskIdParams := []string{}
		for i := 0; i < len(taskIds); i += 1 {
			taskIdParams = append(taskIdParams, fmt.Sprintf("$%d", i+1))
		}

		taskIdValues := []any{}
		for _, taskId := range taskIds {
			taskIdValues = append(taskIdValues, taskId)
		}

		result, err = query.Query(
			ctx,
			selectSql+`
			    WHERE task_id IN (`+strings.Join(taskIdParams, ",")+`)
		    `,
			taskIdValues...,
		)
	} else {
		result, err = query.Query(
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
				&task.RunOnceGeneration,
				&task.ClaimGeneration,
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
	withPendingTaskQueueOwner(ctx, taskId, func(tx server.PgTx, key *string) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM pending_task
				WHERE task_id = $1 AND run_once_key IS NOT DISTINCT FROM $2::text
			`,
			taskId,
			key,
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
	withPendingTaskQueueOwner(ctx, taskId, func(tx server.PgTx, key *string) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE pending_task
				SET
					claim_time = $2,
					release_time = $2
				WHERE task_id = $1 AND run_once_key IS NOT DISTINCT FROM $3::text
			`,
			taskId,
			time.Time{},
			key,
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
	keys := []server.PgOwnershipKey{PendingTaskOwnershipKey(server.Id{}, &jsonKey)}
	if runOnceKey != "" {
		keys = append(keys, PendingTaskOwnershipKey(server.Id{}, &runOnceKey))
	}
	server.OwnedTx(ctx, keys, func(tx server.PgTx) {
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
	}, server.TxReadCommitted, server.OptNoRetry())
	return
}

// Remove aged finished owners in bounded, independently admitted groups. An
// old failed Post can still run, so age never substitutes for its finished-key
// owner. Busy groups/rows remain for a later sweep without blocking other groups.
func RemoveFinishedTasks(ctx context.Context, minTime time.Time, postErrorMinTime time.Time) (removeCount int64) {
	var afterTime *time.Time
	var afterId server.Id
	for {
		server.Raise(ctx.Err())
		ids := make([]server.Id, 0, taskCompletionBatchLimit)
		var removed int64
		server.Tx(ctx, func(tx server.PgTx) {
			rows, err := tx.Query(ctx, `SELECT task_id,run_end_time FROM finished_task
                WHERE run_end_time < GREATEST($1::timestamp,$2::timestamp)
                  AND ((run_end_time < $1 AND (post_error IS NULL OR post_completed)) OR run_end_time < $2)
                  AND ($3::timestamp IS NULL OR (run_end_time,task_id) > ($3,$4::uuid))
                ORDER BY run_end_time,task_id LIMIT $5`, minTime, postErrorMinTime, afterTime, afterId, taskCompletionBatchLimit)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					var endTime time.Time
					server.Raise(rows.Scan(&id, &endTime))
					ids = append(ids, id)
					afterTime, afterId = &endTime, id
				}
			})
			if len(ids) == 0 {
				return
			}
			keys := make([]server.PgOwnershipKey, len(ids))
			for index, id := range ids {
				keys[index] = taskFinishedOwnershipKey(id)
			}
			admitted, err := server.TryTxOwnership(ctx, tx, keys)
			server.Raise(err)
			if !admitted {
				return
			}
			tag := server.RaisePgResult(tx.Exec(ctx, `WITH removable AS (
                SELECT task_id FROM finished_task WHERE task_id=ANY($1::uuid[])
                  AND ((run_end_time < $2 AND (post_error IS NULL OR post_completed)) OR run_end_time < $3)
                ORDER BY task_id FOR UPDATE SKIP LOCKED
                ) DELETE FROM finished_task USING removable WHERE finished_task.task_id=removable.task_id`, ids, minTime, postErrorMinTime))
			removed = tag.RowsAffected()
		}, server.TxReadCommitted, server.OptNoRetry())
		removeCount += removed
		if len(ids) < taskCompletionBatchLimit {
			return
		}
	}
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
	RunOnceGeneration    int64
	ClaimGeneration      int64
	RunPriority          int
	RunMaxTimeSeconds    int
	ClaimTime            time.Time
	ReleaseTime          time.Time
	RescheduleError      string
	RescheduleErrorCount int
	runCohort            *taskRunCohort
}

func (self *Task) ClientSession(ctx context.Context) (*session.ClientSession, error) {
	var byJwt *session.ByJwt
	if self.ClientByJwtJson != "" {
		byJwt = &session.ByJwt{}
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
	var byJwt *session.ByJwt
	if self.ClientByJwtJson != "" {
		byJwt = &session.ByJwt{}
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
		// Capture the body's own cancellation authority before cleanup. A
		// locally canceled child racing a collector stop is not interrupted
		// by that collector, and max-time attribution takes precedence.
		bodyCancelCause := context.Cause(clientSession.Ctx)
		// Join before reading the timer's result, including recovered panics.
		// A canceled max-time context must not lose its timeout attribution.
		clientSession.Cancel()
		<-timerDone
		if timeout {
			returnErr = errors.Join(errors.New("Timeout"), returnErr)
			runPost = nil
		}
		if bodyCancelCause == errTaskCollectorInterrupted && taskCancellationOnly(returnErr) {
			returnErr = &taskCollectorInterruption{cause: returnErr}
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
	// Run and EvalTasks alternate indexed function lanes with the ordinary queue.
	// Owners enable this only after the matching migration is available.
	FairClaimFunctions bool
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

	// These production boundaries let tests inspect exact eligibility alarms,
	// heartbeats and refresh failures. Construction uses real clocks and I/O.
	heartbeatAfter             func(time.Duration) <-chan time.Time
	heartbeatNow               func() time.Time
	claimNow                   func() time.Time
	pollAfter                  func(time.Duration) <-chan time.Time
	refreshTaskTimestampLeases func(context.Context, map[server.Id]*Task)
	// Drain logs are best effort: a full stdout pipe must not hold shutdown.
	drainLogf func(string, ...any)
	// Test barriers sit at real claim boundaries, without changing production
	// ownership: nil leaves the direct PostgreSQL path untouched.
	claimBeforeQuery       func(server.PgTx) error
	claimCandidatesReady   func()
	claimQueueAdmission    func(server.Id, bool)
	claimCandidateLocked   func(server.Id)
	claimBeforeCommit      func(*taskClaimGuard) error
	claimAfterCommit       func(*taskClaimGuard)
	taskSlotEventPublished func()
	// A test can hold the collector until exact completed results are queued.
	// The queue remains bounded by this evaluator's already-claimed task count.
	completionResultPublished func()
	// A fixture can lose the finalizer's reply after its real commit returned.
	completionBatchCommitReturned func()

	stateLock          sync.Mutex
	draining           bool
	claimTargetCounts  map[string]int
	claimFunctionNames []string
	claimFunctionTurn  uint64

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
		claimNow:                   server.NowUtc,
		pollAfter:                  time.After,
		refreshTaskTimestampLeases: refreshTaskTimestampLeases,
		drainLogf:                  glog.Infof,
		claimTargetCounts:          map[string]int{},
	}

	taskWorker.AddTargets(
		&taskPostRetryTarget{Target: NewTaskTargetWithPost(taskWorker.RunPost, taskWorker.RunPostPost)},
	)

	return taskWorker
}

func (self *TaskWorker) Run() {
	if !self.enterRun() {
		return
	}
	defer self.runWg.Done()

	emptyCount := 0
	// Refill may defer an isolated lane to this Run's next initial claim.
	// The handoff cannot be shared with another concurrent Run caller.
	poll := &taskClaimPoll{}
	for {
		select {
		case <-self.runCtx.Done():
			return
		default:
		}

		worked, err := self.runTaskSlots(self.settings.BatchSize, poll)
		if err != nil {
			taskPollsTotal.WithLabelValues("error").Inc()
			glog.Infof("[taskworker]error running tasks: %s\n", err)
			select {
			case <-self.runCtx.Done():
				return
			case <-time.After(self.settings.RetryTimeoutAfterError):
			}
		} else if !worked {
			taskPollsTotal.WithLabelValues("empty").Inc()
			emptyCount += 1
			if emptyCount%30 == 0 {
				glog.Infof("[taskworker]take(0)\n")
			}
			select {
			case <-self.runCtx.Done():
				return
			case <-self.pollAfter(poll.delay(self.claimNow(), self.settings.PollTimeout)):
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
	// Registry mutation is setup-only. Include normalized retained aliases so
	// their queues receive the same indexed access as current target names.
	names := map[string]bool{}
	for name := range self.targets {
		names[updateFunctionName(name)] = true
	}
	self.claimFunctionNames = self.claimFunctionNames[:0]
	for name := range names {
		self.claimFunctionNames = append(self.claimFunctionNames, name)
	}
	slices.Sort(self.claimFunctionNames)
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
		// reaped it (which remains eligible past postErrorMinTime, so
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
	storedFunctionName := finishedTask.FunctionName
	finishedTask.FunctionName = updateFunctionName(finishedTask.FunctionName)

	if target, ok := self.targets[finishedTask.FunctionName]; ok {
		var commitPosts []server.PostFunction
		queued := &Task{TaskId: finishedTask.TaskId, FunctionName: finishedTask.FunctionName,
			ArgsJson: finishedTask.ArgsJson, RunOnceKey: finishedTask.RunOnceKey}
		keys, owned, err := taskCompletionOwnershipKeys(target, queued, finishedTask.ResultJson, true)
		if err != nil {
			return nil, err
		}
		run := func(tx server.PgTx) {
			// a rerun callback starts over: neither the outcome nor the work
			// of a rolled-back attempt may outlive it
			commitPosts = nil
			runPostResult = nil
			returnErr = nil
			if owned {
				present, err := validateFinishedTaskPostOwner(clientSession.Ctx, tx, finishedTask, storedFunctionName)
				if err != nil {
					returnErr = err
					return
				}
				if !present {
					orphanedRunPostCounter.Inc()
					runPostResult = &RunPostResult{}
					return
				}
			}
			if posts, err := target.RunPost(clientSession.Ctx, finishedTask, tx); err == nil {
				commitPosts = posts
				runPostResult = &RunPostResult{}
				return
			} else {
				returnErr = err
				return
			}
		}
		if owned {
			server.OwnedTx(clientSession.Ctx, keys, run, server.TxReadCommitted, server.OptNoRetry())
		} else {
			server.Tx(clientSession.Ctx, run)
		}
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
func (self *TaskWorker) takeTasks(n int) (map[server.Id]*Task, *taskClaimGuard, error) {
	tasks, guard, _, err := self.takeTasksWithGuard(self.ctx, n, nil, taskClaimOptions{})
	return tasks, guard, err
}

// Run cancellation stops new claims; already-committed identities still need
// their bounded read/handback. Finite EvalTasks retains its original context.
type taskClaimOptions struct {
	ordinaryOnly        bool
	detachCommittedRead bool
	poll                *taskClaimPoll
	runCohorts          bool
}

// Claim at most n free slots. Only the Run collector touches a reused guard.
func (self *TaskWorker) takeTasksWithGuard(ctx context.Context, n int, guard *taskClaimGuard, options taskClaimOptions) (
	claimedTasks map[server.Id]*Task,
	claimGuard *taskClaimGuard,
	isolatedPending bool,
	returnErr error,
) {
	if err := guard.completionSessionError(); err != nil {
		return nil, guard, false, err
	}
	if options.poll != nil {
		options.poll.availableAt = time.Time{}
	}
	if n <= 0 {
		return map[server.Id]*Task{}, guard, false, nil
	}

	// Refills reuse the one direct ownership session. On a failed refill its
	// new reservations/locks stay with this guard until all live siblings join;
	// an ambiguous claim must never unwind their ownership or be replayed.
	createdGuard := guard == nil
	if createdGuard {
		conn, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			return nil, nil, false, err
		}
		guard = &taskClaimGuard{
			conn:         conn,
			admissionKVs: map[server.Id]*taskClaimReservation{},
			taskIds:      map[server.Id]bool{},
		}
	}
	retainGuard := !createdGuard
	defer func() {
		if !retainGuard {
			guard.release()
			claimGuard = nil
		}
	}()

	tx, err := guard.conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: server.TxReadCommitted})
	if err != nil {
		return nil, guard, false, err
	}
	defer func() {
		rollbackCtx, rollbackCancel := context.WithTimeout(context.Background(), DefaultTaskFinalizeTimeout)
		_ = tx.Rollback(rollbackCtx)
		rollbackCancel()
	}()
	if err := server.ValidatePgTaskClaimTransaction(ctx, guard.conn, tx); err != nil {
		return nil, guard, false, err
	}
	if self.claimBeforeQuery != nil {
		if err := self.claimBeforeQuery(tx); err != nil {
			return nil, guard, false, err
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
		taskId         server.Id
		functionName   string
		priority       taskPriority
		argsJson       string
		runOnceKey     *string
		availableBlock int64
	}

	now := self.claimNow()
	nowBlock := now.Unix() / BlockSizeSeconds
	throughBlock := nowBlock
	if options.poll != nil && self.settings.PollTimeout > 0 {
		throughBlock = now.Add(self.settings.PollTimeout).Unix() / BlockSizeSeconds
	}
	candidateLimit := n + 64
	includeGroupArgs := self.hasTaskClaimGroups()
	claimFunction := self.nextClaimFunction(options)
	openCursor := func() error {
		bound := throughBlock
		if claimFunction != "" {
			// Only ordinary discovery supplies a future eligibility alarm.
			bound = nowBlock
		}
		query, queryArgs := self.taskFunctionCandidatesQuery(bound, candidateLimit, true, claimFunction, includeGroupArgs)
		_, err := tx.Exec(ctx, `DECLARE pending_task_claim_candidates NO SCROLL CURSOR FOR `+query, queryArgs...)
		return err
	}
	// Keep the queue name visible in FETCH for the existing query monitors.
	if err := openCursor(); err != nil {
		return nil, guard, false, err
	}

	taskIds := []server.Id{}
	taskIdPriorities := map[server.Id]taskPriority{}
	passGroupKeys := map[taskClaimGroupKey]bool{}
	cohorts := map[taskRunCohortKey]*taskRunCohort{}
	taskCohorts := map[server.Id]*taskRunCohort{}
	claimedSlots := 0
	cohortCapacity := func() int {
		capacity := 0
		for _, cohort := range cohorts {
			capacity = max(capacity, cohort.limit-cohort.count)
		}
		return capacity
	}
claimCandidates:
	for candidateCount := 0; (claimedSlots < n || cohortCapacity() > 0) && candidateCount < candidateLimit; {
		// An explicit cohort may fill its remaining member capacity in this
		// same ordered cursor. The original n+64 candidate ceiling remains.
		fetchCount := min(max(n-claimedSlots, cohortCapacity()), candidateLimit-candidateCount)
		// A forward cursor preserves one scan and snapshot across refusals.
		// Discovery is unlocked. Queue admission precedes the exact locking
		// recheck below; transaction end closes this non-holdable cursor.
		// A pooled session caches FETCH result formats by SQL text. The
		// registry's optional args column must have a distinct cache identity.
		result, err := tx.Query(
			ctx,
			fmt.Sprintf(`FETCH FORWARD %d FROM pending_task_claim_candidates /* group_args=%t */`, fetchCount, includeGroupArgs),
		)
		if err != nil {
			return nil, guard, false, err
		}
		candidates := make([]taskCandidate, 0, fetchCount)
		for result.Next() {
			candidate := taskCandidate{}
			columns := []any{
				&candidate.taskId,
				&candidate.functionName,
				&candidate.priority.priority,
				&candidate.priority.maxTimeSeconds,
			}
			if includeGroupArgs {
				columns = append(columns, &candidate.argsJson)
			}
			columns = append(columns, &candidate.runOnceKey, &candidate.availableBlock)
			if err := result.Scan(columns...); err != nil {
				result.Close()
				return nil, guard, false, err
			}
			candidates = append(candidates, candidate)
		}
		if err := result.Err(); err != nil {
			result.Close()
			return nil, guard, false, err
		}
		result.Close()
		if candidateCount == 0 && self.claimCandidatesReady != nil {
			self.claimCandidatesReady()
		}
		candidateCount += len(candidates)
		for _, candidate := range candidates {
			// The same bounded cursor can retain an eligibility wake hint.
			// Future rows take no execution, queue, group or business owner.
			if candidate.availableBlock > nowBlock {
				if options.poll != nil {
					options.poll.availableAt = time.Unix(candidate.availableBlock*BlockSizeSeconds, 0)
				}
				break claimCandidates
			}
			// Session advisory locks are reentrant: a reset timestamp must not
			// let this same Run start a second execution of its live task.
			if guard.taskIds[candidate.taskId] {
				continue
			}
			var cohort *taskRunCohort
			if options.runCohorts {
				key, limit, err := self.claimRunCohort(candidate.functionName, candidate.argsJson,
					candidate.priority.priority, candidate.priority.maxTimeSeconds)
				if err != nil {
					return nil, guard, false, err
				}
				if limit != 0 {
					cohort = cohorts[key]
					if cohort == nil {
						cohort = &taskRunCohort{key: key, limit: limit}
					} else if cohort.limit != limit || cohort.count >= cohort.limit {
						continue
					}
				}
			}
			if claimedSlots >= n && (cohort == nil || cohort.count == 0) {
				// Never skip an earlier ordinary candidate to expand a later
				// cohort, and never take an owner for an unneeded new slot.
				break claimCandidates
			}
			reservation, admitted := self.reserveTaskClaim(candidate.functionName)
			if !admitted {
				continue
			}
			if reservation != nil {
				guard.admissionKVs[candidate.taskId] = reservation
			}
			guard.taskIds[candidate.taskId] = true
			lockKey := taskAdvisoryLockKey(candidate.taskId)
			acquired, queueAdmitted, err := server.TryPgTaskClaimSessionAndQueueOwnership(
				ctx, tx, lockKey, PendingTaskOwnershipKey(candidate.taskId, candidate.runOnceKey))
			if err != nil {
				return nil, guard, false, err
			}
			if !acquired {
				delete(guard.taskIds, candidate.taskId)
				guard.releaseAdmission(candidate.taskId)
				continue
			}
			if self.claimQueueAdmission != nil {
				self.claimQueueAdmission(candidate.taskId, queueAdmitted)
			}
			if !queueAdmitted {
				if err := guard.retireTaskWithQuery(ctx, tx, candidate.taskId); err != nil {
					return nil, guard, false, err
				}
				continue
			}
			grouped, err := self.reserveTaskClaimGroups(ctx, tx, guard, candidate.taskId, candidate.functionName, candidate.argsJson, passGroupKeys)
			if err != nil {
				return nil, guard, false, err
			}
			if !grouped {
				if err := guard.retireTaskWithQuery(ctx, tx, candidate.taskId); err != nil {
					return nil, guard, false, err
				}
				continue
			}
			var expectedArgs *string
			if includeGroupArgs {
				expectedArgs = &candidate.argsJson
			}
			// RC observes the current lease/key after admission. Neither a stale
			// discovery snapshot nor a nonparticipating writer can make this
			// exact recheck wait on a business row or claim a changed identity.
			// Evaluate the bound at execution too: planning a primary-key
			// recheck must not probe the queue's live histogram endpoints.
			err = tx.QueryRow(ctx, `SELECT run_priority,run_max_time_seconds
				FROM pending_task WHERE task_id=$1 AND available_block <= (SELECT $2::bigint)
				AND run_once_key IS NOT DISTINCT FROM $3::text AND function_name=$4
				AND ($5::text IS NULL OR args_json=$5)
				FOR UPDATE SKIP LOCKED`, candidate.taskId, nowBlock,
				candidate.runOnceKey, candidate.functionName, expectedArgs).Scan(
				&candidate.priority.priority, &candidate.priority.maxTimeSeconds)
			if errors.Is(err, pgx.ErrNoRows) {
				if err := guard.retireTaskWithQuery(ctx, tx, candidate.taskId); err != nil {
					return nil, guard, false, err
				}
				continue
			}
			if err != nil {
				return nil, guard, false, err
			}
			if cohort != nil && (candidate.priority.priority != DefaultPriority || DefaultMaxTime < time.Duration(candidate.priority.maxTimeSeconds)*time.Second) {
				// The locked recheck is authoritative if an external writer
				// changed isolation after discovery. It cannot borrow a spare
				// cohort position for another physical execution owner.
				cohort = nil
				if claimedSlots >= n {
					if err := guard.retireTaskWithQuery(ctx, tx, candidate.taskId); err != nil {
						return nil, guard, false, err
					}
					break claimCandidates
				}
			}
			if self.claimCandidateLocked != nil {
				self.claimCandidateLocked(candidate.taskId)
			}
			taskIds = append(taskIds, candidate.taskId)
			taskIdPriorities[candidate.taskId] = candidate.priority
			if cohort == nil {
				claimedSlots++
			} else {
				if cohort.count == 0 {
					claimedSlots++
					cohorts[cohort.key] = cohort
				}
				cohort.count++
				taskCohorts[candidate.taskId] = cohort
			}
		}
		if len(candidates) < fetchCount || candidateCount >= candidateLimit {
			if claimFunction != "" && len(taskIds) == 0 {
				// An empty, busy or stale lane cannot spend ordinary capacity.
				// The second cursor has its original independent n+64 bound.
				if _, err := tx.Exec(ctx, `CLOSE pending_task_claim_candidates`); err != nil {
					return nil, guard, false, err
				}
				claimFunction = ""
				candidateCount = 0
				if err := openCursor(); err != nil {
					return nil, guard, false, err
				}
				continue
			}
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
	selectedIsolated := false
	selectionLimit := min(n, len(taskIds))
	if options.runCohorts {
		selectionLimit = len(taskIds)
	}
	for k := selectionLimit; selectedCount < k; {
		priority := taskIdPriorities[taskIds[selectedCount]]
		selectedCount += 1
		if DefaultPriority < priority.priority {
			selectedIsolated = true
			break
		}
		if DefaultMaxTime < time.Duration(priority.maxTimeSeconds)*time.Second {
			selectedIsolated = true
			break
		}
	}
	if options.ordinaryOnly && selectedIsolated {
		// Honor the existing isolation boundary without skipping that work
		// forever behind a recurring ordinary task in the other slot.
		isolatedPending = true
		selectedCount = 0
		if claimFunction != "" && options.poll != nil {
			// Retain no row or guard here. The same Run freshly revalidates
			// this registered lane after every live sibling has joined.
			options.poll.isolatedFunction = claimFunction
		}
	}
	for _, taskId := range taskIds[selectedCount:] {
		if err := guard.retireTaskWithQuery(ctx, tx, taskId); err != nil {
			return nil, guard, false, err
		}
	}
	taskIds = taskIds[:selectedCount]

	claimTime := server.NowUtc()
	releaseTime := claimTime.Add(TaskLeaseTimeout)
	claimedWakeGenerations := map[server.Id]int64{}
	claimedGenerations := map[server.Id]int64{}
	if len(taskIds) != 0 {
		// The short timestamp bounds crash recovery; the session advisory lock
		// above is the durable duplicate-execution guard for a live owner. All
		// exact selected IDs already hold both row and advisory ownership, so
		// their common lease can be published by one statement in this claim Tx.
		rows, err := tx.Query(
			ctx,
			`
				UPDATE pending_task
				SET
					claim_time = $2,
					release_time = $3,
					run_once_wake_at = NULL,
					claim_generation = pending_task.claim_generation + 1
				WHERE task_id = ANY($1)
				RETURNING task_id, run_once_generation, claim_generation
			`,
			taskIds,
			claimTime,
			releaseTime,
		)
		if err != nil {
			return nil, guard, false, err
		}
		for rows.Next() {
			var taskId server.Id
			var wakeGeneration, claimGeneration int64
			if err := rows.Scan(&taskId, &wakeGeneration, &claimGeneration); err != nil {
				rows.Close()
				return nil, guard, false, err
			}
			claimedWakeGenerations[taskId] = wakeGeneration
			claimedGenerations[taskId] = claimGeneration
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, guard, false, err
		}
		if len(claimedGenerations) != len(taskIds) {
			return nil, guard, false, errors.New("task claim generation ownership missing")
		}
	}

	if self.claimBeforeCommit != nil {
		if err := self.claimBeforeCommit(guard); err != nil {
			return nil, guard, false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, guard, false, err
	}
	if len(taskIds) == 0 {
		if createdGuard {
			return map[server.Id]*Task{}, nil, isolatedPending, nil
		}
		return map[server.Id]*Task{}, guard, isolatedPending, nil
	}

	if self.claimAfterCommit != nil {
		self.claimAfterCommit(guard)
	}
	readCtx := ctx
	if options.detachCommittedRead {
		// A drain phase1 may stop admission immediately after COMMIT. Its
		// already-owned rows must not become a collector panic that cancels
		// unrelated functions before the configured drain finish deadline.
		timeout := self.settings.FinalizeTimeout
		if timeout <= 0 {
			timeout = DefaultTaskFinalizeTimeout
		}
		var cancel context.CancelFunc
		readCtx, cancel = context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
	}
	// COMMIT ends the claim snapshot, but this collector still owns its direct
	// session. A fresh exact read must not wait for another pool connection
	// while these committed execution guards remain held.
	claimedTasks = getTasksInConn(readCtx, guard.conn, true, taskIds...)
	for _, taskId := range taskIds {
		if queued := claimedTasks[taskId]; queued != nil {
			// A producer may commit between claim and this exact-ID read. Its
			// generation belongs to a successor, not this already-owned run.
			queued.RunOnceGeneration = claimedWakeGenerations[taskId]
			queued.ClaimGeneration = claimedGenerations[taskId]
			queued.runCohort = taskCohorts[taskId]
		} else {
			// A deleted row has no execution to retire its owner. Continuous
			// refill must not accumulate such absent claims beside a live task.
			if err := guard.retireTask(readCtx, taskId); err != nil {
				return nil, guard, false, err
			}
		}
	}
	claimGuard = guard
	retainGuard = true
	return claimedTasks, claimGuard, isolatedPending, nil
}

// refreshTaskTimestampLeases writes the short crash-recovery timestamps through
// the ordinary pooled DB path. server.Tx raises DB errors, so callers must keep
// this operation behind tryRefreshTaskTimestampLeases's narrow recovery
// boundary. The direct taskClaimGuard session remains the ownership authority.
func refreshTaskTimestampLeases(
	ctx context.Context,
	tasks map[server.Id]*Task,
) {
	if len(tasks) == 0 {
		return
	}
	ids := make([]server.Id, 0, len(tasks))
	claims := make([]int64, 0, len(tasks))
	keys := make([]server.PgOwnershipKey, 0, len(tasks))
	for _, task := range tasks {
		ids = append(ids, task.TaskId)
		claims = append(claims, task.ClaimGeneration)
		keys = append(keys, taskQueueOwnershipKey(task.TaskId, task.RunOnceKey))
	}
	refreshed := int64(0)
	server.Tx(ctx, func(tx server.PgTx) {
		admitted, err := server.TryTxOwnership(ctx, tx, keys)
		server.Raise(err)
		if !admitted {
			return
		}
		claimTime := server.NowUtc()
		releaseTime := claimTime.Add(TaskLeaseTimeout)
		tag := server.RaisePgResult(tx.Exec(ctx, `WITH owned AS MATERIALIZED (
			SELECT pending_task.task_id FROM pending_task
			JOIN unnest($1::uuid[],$4::bigint[]) AS expected(task_id,claim_generation)
			USING(task_id,claim_generation)
			ORDER BY pending_task.task_id FOR UPDATE OF pending_task SKIP LOCKED
		)
		UPDATE pending_task SET claim_time=$2,release_time=GREATEST(release_time,$3)
		FROM owned WHERE pending_task.task_id=owned.task_id`, ids, claimTime, releaseTime, claims))
		refreshed = tag.RowsAffected()
	}, server.TxReadCommitted, server.OptNoRetry())
	taskTimestampLeaseRefreshSkippedCounter.Add(float64(int64(len(tasks)) - refreshed))
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
	// Only the collector's explicit cancellation cause grants a short retry.
	collectorInterrupted bool
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
	evalCtx, evalCancel := context.WithCancelCause(context.WithoutCancel(self.ctx))
	defer evalCancel(nil)

	for _, task := range tasks {
		// update legacy function names
		task.FunctionName = updateFunctionName(task.FunctionName)
	}
	executionTargets := self.prepareTaskBatchTargets(tasks)

	// Execution cancellation must not drop the results that prove each claimed
	// sibling joined. Only the collector closes the result-delivery context.
	taskCtx, taskCancel := context.WithCancel(context.WithoutCancel(evalCtx))
	results := make(chan *taskExecutionResult, len(tasks))
	executionAdmissions := claimGuard.retainExecutionAdmissions(tasks)

	go server.HandleError(func() {
		defer executionAdmissions.releaseUnlaunched()
		// The collector drains every buffered result before closing its context.
		// Canceling it here could race the final acknowledged result handbacks.
		defer close(results)

		var wg sync.WaitGroup

		for _, task := range tasks {
			wg.Add(1)
			reservation := executionAdmissions.take(task.TaskId)
			go server.HandleError(func() {
				defer wg.Done()
				if reservation != nil {
					defer reservation.release()
				}
				r := self.executeTask(evalCtx, task, executionTargets[task.FunctionName])
				select {
				case results <- r:
					if self.completionResultPublished != nil {
						self.completionResultPublished()
					}
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
		evalCancel(errTaskCollectorInterrupted)
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
		posts, postRescheduled := self.finalizeTaskWithGuard(r, claimGuard)
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
			if claimGuard.completionSessionError() != nil {
				// A quarantined borrowed scope cannot perform more direct SQL.
				// Keep its session until all sibling results and posts join.
				nextHeartbeatTime = self.heartbeatNow().Add(ReleaseTimeout / 3)
				return
			}
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
		finalizeReady := func(ready []*taskExecutionResult) {
			for _, r := range ready {
				elapsedSeconds := float32(r.runEndTime.Sub(r.runStartTime)/time.Millisecond) / 1000
				if r.err == nil {
					glog.V(1).Infof("[%s]eval done(%.2fs) %s(%s) = %s\n", r.task.TaskId, elapsedSeconds, r.task.FunctionName, ArgumentsForLog(r.task.ArgsJson), r.resultJson)
				} else {
					glog.Infof("[%s]eval error(%.2fs) (reschedule) %s(%s) = %s\n", r.task.TaskId, elapsedSeconds, r.task.FunctionName, ArgumentsForLog(r.task.ArgsJson), r.err)
				}
				delete(unreceivedTasks, r.task.TaskId)
			}
			if len(ready) == 1 {
				finalize(ready[0])
				return
			}
			retrySingles, err := self.finalizeTaskBatchWithGuard(ready, claimGuard)
			switch {
			case err == nil:
				for _, r := range ready {
					delete(heartbeatTasks, r.task.TaskId)
					finishedTaskIds = append(finishedTaskIds, r.task.TaskId)
				}
				taskFinalizationsTotal.WithLabelValues("succeeded").Add(float64(len(ready)))
			case retrySingles:
				for _, r := range ready {
					if !self.heartbeatNow().Before(nextHeartbeatTime) {
						heartbeat()
					}
					finalize(r)
				}
			default:
				if finalizePanic == nil {
					finalizePanic = err
				}
				glog.Infof("[taskworker]task completion batch remains unacknowledged: %v\n", err)
			}
		}
		var deferredResult *taskExecutionResult
		collect := func(r *taskExecutionResult) {
			// Take only results already available. A slow or held sibling never
			// becomes a prerequisite for a completed owner's handback. Ordinary
			// results keep their heartbeat opportunity between every handback.
			ready := []*taskExecutionResult{r}
			if self.canBatchTaskCompletion(r) {
			collectReady:
				for len(ready) < taskCompletionBatchLimit {
					select {
					case next, ok := <-results:
						if !ok {
							break collectReady
						}
						if !self.canBatchTaskCompletion(next) {
							deferredResult = next
							break collectReady
						}
						ready = append(ready, next)
					default:
						break collectReady
					}
				}
			}
			finalizeReady(ready)
		}
		for {
			if err := claimGuard.completionSessionError(); err != nil {
				if finalizePanic == nil {
					finalizePanic = err
				}
				evalCancel(errTaskCollectorInterrupted)
			}
			// Finalizing ready results must not reset the lease clock. Check
			// an overdue heartbeat between bounded handbacks even when result
			// delivery is continuously ready; the guard connection stays serial.
			heartbeatDelay := nextHeartbeatTime.Sub(self.heartbeatNow())
			if heartbeatDelay <= 0 {
				heartbeat()
				continue
			}
			if deferredResult != nil {
				r := deferredResult
				deferredResult = nil
				collect(r)
				continue
			}
			select {
			case <-taskCtx.Done():
				return
			case r, ok := <-results:
				if !ok {
					return
				}
				collect(r)

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
