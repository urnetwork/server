package task

import (
	"context"
	// "net/http"
	"strings"
	// "strconv"
	// "encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand"
	"reflect"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"sync"
	"time"

	"golang.org/x/exp/maps"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

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
var ReleaseTimeout = 30 * time.Second

// the reschedule time is uniformly chosen on [0, t] so the expected mean will be t/2
var RescheduleTimeout = 2 * BlockSizeSeconds * time.Second

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

func ScheduleTaskInTx[T any, R any](
	tx server.PgTx,
	taskFunction TaskFunction[T, R],
	args T,
	clientSession *session.ClientSession,
	opts ...any,
) (taskId server.Id) {
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
	maxTimeSeconds := int(runMaxTime.MaxTime / time.Second)

	claimTime := time.Time{}

	taskId = server.NewId()

	server.RaisePgResult(tx.Exec(
		clientSession.Ctx,
		`
			INSERT INTO pending_task (
				task_id,
		        function_name,
		        args_json,
		        client_address,
		        client_by_jwt_json,
		        run_at,
		        run_once_key,
		        run_priority,
		        run_max_time_seconds,
		        claim_time,
		        release_time
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
			ON CONFLICT (run_once_key) DO UPDATE SET
				run_at = LEAST(pending_task.run_at, $6),
				run_priority = LEAST(pending_task.run_priority, $8),
				run_max_time_seconds = GREATEST(pending_task.run_max_time_seconds, $9)
		`,
		taskId,
		taskTarget.TargetFunctionName(),
		argsJson,
		clientSession.ClientAddress,
		byJwtJson,
		runAt.At.UTC(),
		runOnceKey,
		runPriority.Priority,
		maxTimeSeconds,
		claimTime,
	))
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
		        pending_task.client_by_jwt_json,
		        pending_task.run_at,
		        pending_task.run_once_key,
		        pending_task.run_priority,
		        pending_task.run_max_time_seconds,
		        pending_task.claim_time,
		        pending_task.release_time,
		        pending_task.reschedule_error
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
					&byJwtJson,
					&task.RunAt,
					&runOnceKey,
					&task.RunPriority,
					&task.RunMaxTimeSeconds,
					&task.ClaimTime,
					&task.ReleaseTime,
					&rescheduleError,
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

// removed finished tasks older than `minTime` where the post was successfully run
func RemoveFinishedTasks(ctx context.Context, minTime time.Time) (removeCount int64) {
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM finished_task
				WHERE
					run_end_time < $1 AND 
					(post_error IS NULL or post_completed)
			`,
			minTime,
		))

		removeCount = tag.RowsAffected()
	})

	return
}

type Task struct {
	TaskId            server.Id
	FunctionName      string
	ArgsJson          string
	ClientAddress     string
	ClientByJwtJson   string
	RunAt             time.Time
	RunOnceKey        string
	RunPriority       int
	RunMaxTimeSeconds int
	ClaimTime         time.Time
	ReleaseTime       time.Time
	RescheduleError   string
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

	go func() {
		select {
		case <-clientSession.Ctx.Done():
		case <-time.After(max(
			time.Duration(task.RunMaxTimeSeconds)*time.Second,
			DefaultMaxTime,
		)):
			timeout = true
		}
		clientSession.Cancel()
	}()

	defer func() {
		if r := recover(); r != nil {
			returnErr = fmt.Errorf("Unhandled: %s", server.ErrorJson(r, debug.Stack()))
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
		clientSession, err := task.ClientSession(ctx)
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

	go func() {
		select {
		case <-clientSession.Ctx.Done():
		case <-time.After(max(
			time.Duration(finishedTask.RunMaxTimeSeconds)*time.Second,
			DefaultMaxTime,
		)):
			timeout = true
		}
		clientSession.Cancel()
	}()

	defer func() {
		if r := recover(); r != nil {
			returnErr = fmt.Errorf("Unhandled: %s", server.ErrorJson(r, debug.Stack()))
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
	}
}

type TaskWorkerSettings struct {
	BatchSize              int
	RetryTimeoutAfterError time.Duration
	PollTimeout            time.Duration
}

type TaskWorker struct {
	ctx       context.Context
	cancel    context.CancelFunc
	runCtx    context.Context
	runCancel context.CancelFunc
	runWg     sync.WaitGroup
	targets   map[string]Target
	settings  *TaskWorkerSettings
}

func NewTaskWorkerWithDefaults(ctx context.Context) *TaskWorker {
	return NewTaskWorker(ctx, DefaultTaskWorkerSettings())
}

func NewTaskWorker(ctx context.Context, settings *TaskWorkerSettings) *TaskWorker {
	cancelCtx, cancel := context.WithCancel(ctx)
	runCtx, runCancel := context.WithCancel(cancelCtx)

	taskWorker := &TaskWorker{
		ctx:       cancelCtx,
		cancel:    cancel,
		runCtx:    runCtx,
		runCancel: runCancel,
		targets:   map[string]Target{},
		settings:  settings,
	}

	taskWorker.AddTargets(
		NewTaskTargetWithPost(taskWorker.RunPost, taskWorker.RunPostPost),
	)

	return taskWorker
}

func (self *TaskWorker) Run() {
	self.runWg.Add(1)
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

func (self *TaskWorker) Drain() {
	self.runCancel()

	self.runWg.Wait()
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
		returnErr = errors.New("Finished task not found.")
		return
	}

	// attach the finished task function name and args
	defer func() {
		if returnErr != nil {
			returnErr = fmt.Errorf("%s(%s) = %s", finishedTask.FunctionName, finishedTask.ArgsJson, returnErr.Error())
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
		returnErr = fmt.Errorf("Target not found (%s).", finishedTask.FunctionName)
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

// takes the n next available tasks and makes an initial claim
func (self *TaskWorker) takeTasks(n int) (map[server.Id]*Task, error) {
	// select from the backlog as well as the current block
	// run the current block first so that the current block doesn't get starved by the backlog

	type taskPriority struct {
		priority       int
		maxTimeSeconds int
	}

	var taskIds []server.Id

	server.Tx(self.ctx, func(tx server.PgTx) {

		now := server.NowUtc()
		nowBlock := now.Unix() / BlockSizeSeconds

		taskIdPriorities := map[server.Id]taskPriority{}

		result, err := tx.Query(
			self.ctx,
			`
			    SELECT
			    	task_id,
			    	run_priority,
			    	run_max_time_seconds
			    FROM pending_task
			    WHERE
			        available_block <= $1
			    ORDER BY available_block, run_priority DESC, run_max_time_seconds DESC
			    LIMIT $2
			    FOR UPDATE SKIP LOCKED
		    `,
			nowBlock,
			n,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var taskId server.Id
				var priority taskPriority
				server.Raise(result.Scan(
					&taskId,
					&priority.priority,
					&priority.maxTimeSeconds,
				))
				taskIdPriorities[taskId] = priority
			}
		})

		taskIds = maps.Keys(taskIdPriorities)
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

		// isolate higher priority and longer running tasks
		// this ensures that they don't block or get blocked with rescheduling
		i := 0
		for k := min(n, len(taskIds)); i < k; {
			priority := taskIdPriorities[taskIds[i]]
			i += 1
			if DefaultPriority < priority.priority {
				break
			}
			if DefaultMaxTime < time.Duration(priority.maxTimeSeconds)*time.Second {
				break
			}
		}
		taskIds = taskIds[0:i]

		claimTime := server.NowUtc()
		releaseTime := claimTime.Add(ReleaseTimeout)

		server.BatchInTx(self.ctx, tx, func(batch server.PgBatch) {
			for _, taskId := range taskIds {
				batch.Queue(
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
				)
			}
		})
	}, server.TxReadCommitted)

	return GetTasks(self.ctx, taskIds...), nil
}

// return taskIds of the finished tasks, rescheduled tasks
func (self *TaskWorker) EvalTasks(n int) (
	finishedTaskIds []server.Id,
	rescheduledTaskIds []server.Id,
	postRescheduledTaskIds []server.Id,
	returnErr error,
) {
	tasks, err := self.takeTasks(n)
	if err != nil {
		returnErr = err
		return
	}
	if len(tasks) == 0 {
		return
	}

	evalCtx, evalCancel := context.WithCancel(self.ctx)
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

	go func() {
		defer func() {
			taskCancel()
			close(results)
		}()

		var wg sync.WaitGroup

		for _, task := range tasks {
			wg.Add(1)
			go func() {
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
						result, r.runPost, err = target.Run(evalCtx, task)
					}()

					if err == nil {
						var resultJsonBytes []byte
						resultJsonBytes, err = json.Marshal(result)
						if err == nil {
							r.resultJson = string(resultJsonBytes)
						}
					}
					r.err = err
				} else {
					r.err = fmt.Errorf("Target not found (%s).", task.FunctionName)
				}

				r.runEndTime = server.NowUtc()
				select {
				case results <- r:
				case <-taskCtx.Done():
					return
				}
			}()
		}

		wg.Wait()
	}()

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

				server.Tx(self.ctx, func(tx server.PgTx) {
					server.BatchInTx(self.ctx, tx, func(batch server.PgBatch) {
						claimTime := server.NowUtc()
						releaseTime := claimTime.Add(ReleaseTimeout)

						for _, task := range tasks {
							batch.Queue(
								`
									UPDATE pending_task
									SET
										claim_time = $2,
										release_time = $3
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

	server.Tx(self.ctx, func(tx server.PgTx) {
		server.BatchInTx(self.ctx, tx, func(batch server.PgBatch) {
			for taskId, finished := range finishedTasks {
				batch.Queue(
					`
					INSERT INTO finished_task (
						task_id,
				        function_name,
				        args_json,
				        client_address,
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
				rescheduleTime := now.Add(time.Second * time.Duration(mathrand.Intn(int(RescheduleTimeout/time.Second))))
				batch.Queue(
					`
						UPDATE pending_task
						SET
							reschedule_error = $2,
							run_at = $3,
							release_time = $4
						WHERE task_id = $1
					`,
					taskId,
					err.Error(),
					rescheduleTime,
					now,
				)
			}
		})

		for taskId, finished := range finishedTasks {
			if err := finished.runPost(tx); err != nil {
				// record the post error

				postRescheduledTasks[taskId] = err

				tx.Exec(
					self.ctx,
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
					clientSession, err := task.ClientSession(self.ctx)
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
	RemoveFinishedTasks(clientSession.Ctx, minTime)
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
