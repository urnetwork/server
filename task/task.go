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
	"time"

	"golang.org/x/exp/maps"

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
const BlockSizeSeconds = 300

var DefaultMaxTime = 120 * time.Second
var ReleaseTimeout = 30 * time.Second
var RescheduleTimeout = 60 * time.Second

type TaskPriority = int

const (
	TaskPriorityFastest TaskPriority = 0
	TaskPrioritySlowest TaskPriority = 20
)

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

type RunPriorityOption struct {
	Priority TaskPriority
}

type RunMaxTimeOption struct {
	MaxTime time.Duration
}

func ScheduleTask[T any, R any](
	taskFunction TaskFunction[T, R],
	args T,
	clientSession *session.ClientSession,
	opts ...any,
) {
	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		ScheduleTaskInTx[T, R](tx, taskFunction, args, clientSession, opts...)
	})
}

func ScheduleTaskInTx[T any, R any](
	tx server.PgTx,
	taskFunction TaskFunction[T, R],
	args T,
	clientSession *session.ClientSession,
	opts ...any,
) {
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
		At: time.Time{},
	}
	var runOnce *RunOnceOption
	runPriority := &RunPriorityOption{
		Priority: (TaskPriorityFastest + TaskPrioritySlowest) / 2,
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

	taskId := server.NewId()

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
			ON CONFLICT (run_once_key) DO NOTHING
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
		case <-time.After(time.Duration(task.RunMaxTimeSeconds) * time.Second):
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
		case <-time.After(time.Duration(finishedTask.RunMaxTimeSeconds) * time.Second):
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
		return
	}
	if timeout {
		returnErr = errors.New("Timeout")
		return
	}

	return
}

type TaskWorker struct {
	ctx     context.Context
	targets map[string]Target
}

func NewTaskWorker(ctx context.Context) *TaskWorker {
	taskWorker := &TaskWorker{
		ctx:     ctx,
		targets: map[string]Target{},
	}

	taskWorker.AddTargets(
		NewTaskTargetWithPost(taskWorker.RunPost, taskWorker.RunPostPost),
	)

	return taskWorker
}

func (self *TaskWorker) AddTargets(taskTargets ...Target) {
	for _, taskTarget := range taskTargets {
		self.targets[taskTarget.TargetFunctionName()] = taskTarget
		for _, alternateFunctionNames := range taskTarget.AlternateFunctionNames() {
			self.targets[alternateFunctionNames] = taskTarget
		}
	}
}

type RunPostArgs struct {
	TaskId server.Id `json:"task_id"`
}

type RunPostResult struct {
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
	// choose the top N by priority
	// this makes sure the current block doesn't get starved by the backlog

	var taskIds []server.Id

	server.Tx(self.ctx, func(tx server.PgTx) {

		now := server.NowUtc()
		nowBlock := now.Unix() / BlockSizeSeconds

		taskIdPriorities := map[server.Id]int{}

		result, err := tx.Query(
			self.ctx,
			`
				SELECT
					task_id,
					run_priority
				FROM (
				    SELECT
				    	task_id,
				    	run_priority
				    FROM pending_task
				    WHERE
				        run_at_block < $2 AND
				        run_at <= $1 AND
				        release_time <= $1
				    ORDER BY run_at_block ASC, run_priority ASC
				    LIMIT $3
				    FOR UPDATE SKIP LOCKED
				) a

				UNION ALL

				SELECT
					b.task_id,
					b.run_priority
				FROM (
				    SELECT
				    	task_id,
				    	run_priority
				    FROM pending_task
				    WHERE
				        run_at_block = $2 AND
				        run_at <= $1 AND
				        release_time <= $1
				    ORDER BY run_priority ASC
				    LIMIT $3
				    FOR UPDATE SKIP LOCKED
				) b
		    `,
			now,
			nowBlock,
			n,
		)

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var taskId server.Id
				var priority int
				server.Raise(result.Scan(
					&taskId,
					&priority,
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
			return aPriority - bPriority
		})

		// keep up to n
		taskIds = taskIds[0:min(n, len(taskIds))]

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

	done := make(chan struct{})

	type Finished struct {
		runStartTime time.Time
		runEndTime   time.Time
		resultJson   string
		runPost      func(server.PgTx) error
	}

	finishedTasks := map[server.Id]*Finished{}
	rescheduledTasks := map[server.Id]error{}
	postRescheduledTasks := map[server.Id]error{}

	go func() {
		defer close(done)
		for _, task := range tasks {
			if target, ok := self.targets[task.FunctionName]; ok {
				runStartTime := server.NowUtc()
				if result, runPost, err := target.Run(self.ctx, task); err == nil {
					runEndTime := server.NowUtc()
					if resultJson, err := json.Marshal(result); err == nil {
						finishedTasks[task.TaskId] = &Finished{
							runStartTime: runStartTime,
							runEndTime:   runEndTime,
							resultJson:   string(resultJson),
							runPost:      runPost,
						}
					} else {
						// error with converting result to json
						rescheduledTasks[task.TaskId] = err
					}
				} else {
					rescheduledTasks[task.TaskId] = err
				}
			} else {
				rescheduledTasks[task.TaskId] = fmt.Errorf("Target not found (%s).", task.FunctionName)
			}
		}
	}()

Wait:
	for {
		select {
		case <-done:
			break Wait
		case <-time.After(ReleaseTimeout / 3):
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
					FOR UPDATE
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
					now.Add(RescheduleTimeout),
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
						RunAt(now.Add(RescheduleTimeout)),
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
