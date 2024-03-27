package task

import (
	"context"
	// "net/http"
	// "strings"
	// "strconv"
	// "encoding/base64"
	"encoding/json"
	"errors"
	"time"
	"runtime"
	"reflect"
	"fmt"
	mathrand "math/rand"
	"slices"

	"golang.org/x/exp/maps"

	"bringyour.com/bringyour/jwt"
	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour"
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



type TaskFunction[T any, R any] func(T, *session.ClientSession)(R, error)

type TaskPostFunction[T any, R any] func(T, R, *session.ClientSession, bringyour.PgTx)(error)

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
	bringyour.Tx(clientSession.Ctx, func(tx bringyour.PgTx) {
		ScheduleTaskInTx[T, R](tx, taskFunction, args, clientSession, opts...)
	})
}


func ScheduleTaskInTx[T any, R any](
	tx bringyour.PgTx,
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

	taskId := bringyour.NewId()

	bringyour.RaisePgResult(tx.Exec(
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
		runAt.At,
		runOnceKey,
		runPriority.Priority,
		maxTimeSeconds,
		claimTime,
	))
}


func GetTasks(ctx context.Context, taskIds ...bringyour.Id) map[bringyour.Id]*Task {
    tasks := map[bringyour.Id]*Task{}

    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
    	bringyour.CreateTempTableInTx(ctx, tx, "temp_task_ids(task_id uuid)", taskIds...)

    	result, err := tx.Query(
    		ctx,
    		`
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
			    INNER JOIN temp_task_ids ON temp_task_ids.task_id = pending_task.task_id
		    `,
    	)

    	bringyour.WithPgResult(result, err, func() {
    		for result.Next() {
    			task := &Task{}
    			var byJwtJson *string
    			var runOnceKey *string
    			var rescheduleError *string
    			bringyour.Raise(result.Scan(
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

func GetFinishedTasks(ctx context.Context, taskIds ...bringyour.Id) map[bringyour.Id]*FinishedTask {
	finishedTasks := map[bringyour.Id]*FinishedTask{}

    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
    	bringyour.CreateTempTableInTx(ctx, tx, "temp_task_ids(task_id uuid)", taskIds...)

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

    	bringyour.WithPgResult(result, err, func() {
    		for result.Next() {
    			finishedTask := &FinishedTask{}
    			var byJwtJson *string
    			var runOnceKey *string
    			var rescheduleError *string
    			var postError *string
    			bringyour.Raise(result.Scan(
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


func ListPendingTasks(ctx context.Context) []bringyour.Id {
	taskIds := []bringyour.Id{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					task_id
				FROM pending_task
				ORDER BY run_at_block ASC, run_priority ASC, run_at ASC
			`,
		)

		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var taskId bringyour.Id
				bringyour.Raise(result.Scan(&taskId))
				taskIds = append(taskIds, taskId)
			}
		})
	})

	return taskIds
}


// the task struct has the latest error attached to it
func ListRescheduledTasks(ctx context.Context) []bringyour.Id {
	taskIds := []bringyour.Id{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					task_id
				FROM pending_task
				WHERE has_reschedule_error
			`,
		)

		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var taskId bringyour.Id
				bringyour.Raise(result.Scan(&taskId))
				taskIds = append(taskIds, taskId)
			}
		})
	})

	return taskIds
}


func ListClaimedTasks(ctx context.Context) []bringyour.Id {
	taskIds := []bringyour.Id{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					task_id
				FROM pending_task
				WHERE $1 < release_time
			`,
			bringyour.NowUtc(),
		)

		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var taskId bringyour.Id
				bringyour.Raise(result.Scan(&taskId))
				taskIds = append(taskIds, taskId)
			}
		})
	})

	return taskIds
}


func ListFinishedTasks(ctx context.Context) []bringyour.Id {
	taskIds := []bringyour.Id{}

	bringyour.Db(ctx, func(conn bringyour.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					task_id
				FROM finished_task
				ORDER BY run_end_time ASC
			`,
		)

		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var taskId bringyour.Id
				bringyour.Raise(result.Scan(&taskId))
				taskIds = append(taskIds, taskId)
			}
		})
	})

	return taskIds
}


// removed finished tasks older than `minTime` where the post was successfully run
func RemoveFinishedTasks(ctx context.Context, minTime time.Time) (removeCount int64) {
	bringyour.Tx(ctx, func(tx bringyour.PgTx) {
		tag := bringyour.RaisePgResult(tx.Exec(
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
	TaskId bringyour.Id
	FunctionName string
	ArgsJson string
	ClientAddress string
	ClientByJwtJson string
	RunAt time.Time
	RunOnceKey string
	RunPriority int
	RunMaxTimeSeconds int
	ClaimTime time.Time
	ReleaseTime time.Time
	RescheduleError string
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
	TaskId bringyour.Id
	FunctionName string
	ArgsJson string
	ClientAddress string
	ClientByJwtJson string
	RunAt time.Time
	RunOnceKey string
	RunPriority int
	RunMaxTimeSeconds int
	RunStartTime time.Time
	RunEndTime time.Time
	RescheduleError string
	ResultJson string
	PostError string
	PostCompleted bool
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
	Run(context.Context, *Task)(any, func(bringyour.PgTx)(error), error)
	RunPost(context.Context, *FinishedTask, bringyour.PgTx)(error)
}


type TaskTarget[T any, R any] struct {
	targetFunctionName string
	targetFunction TaskFunction[T, R]
	postFunction TaskPostFunction[T, R]
	alternateFunctionNames []string
}

func NewTaskTarget[T any, R any](
	targetFunction TaskFunction[T, R],
	alternateFunctionNames ...string,
) *TaskTarget[T, R] {
	return &TaskTarget[T, R]{
		targetFunctionName: functionName(targetFunction),
		targetFunction: targetFunction,
		alternateFunctionNames: alternateFunctionNames,
	}
}

func NewTaskTargetWithPost[T any, R any](
	targetFunction TaskFunction[T, R],
	postFunction TaskPostFunction[T, R],
	alternateFunctionNames ...string,
) *TaskTarget[T, R] {
	return &TaskTarget[T, R]{
		targetFunctionName: functionName(targetFunction),
		targetFunction: targetFunction,
		postFunction: postFunction,
		alternateFunctionNames: alternateFunctionNames,
	}
}

func functionName[T any, R any](targetFunction TaskFunction[T, R]) string {
	targetFunctionName := runtime.FuncForPC(reflect.ValueOf(targetFunction).Pointer()).Name()
	return targetFunctionName
}

func (self *TaskTarget[T, R]) TargetFunctionName() string {
	return self.targetFunctionName
}
// func (self *TaskTarget[T, R]) TargetFunction() TaskFunction[T, R] {
// 	return self.targetFunction
// }
// func (self *TaskTarget[T, R]) PostFunction() TaskPostFunction[T, R] {
// 	return self.postFunction
// }
func (self *TaskTarget[T, R]) AlternateFunctionNames() []string {
	return self.alternateFunctionNames
}

func (self *TaskTarget[T, R]) Run(ctx context.Context, task *Task) (
	result any,
	runPost func(bringyour.PgTx)(error),
	returnErr error,
) {
	return self.RunSpecific(ctx, task)
}

func (self *TaskTarget[T, R]) RunSpecific(ctx context.Context, task *Task) (
	result R,
	runPost func(bringyour.PgTx)(error),
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
		case <- clientSession.Ctx.Done():
		case <- time.After(time.Duration(task.RunMaxTimeSeconds) * time.Second):
			timeout = true
		}
		clientSession.Cancel()
	}()

	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%s", r)
			}
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

	runPost = func(tx bringyour.PgTx)(error) {
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
	tx bringyour.PgTx,
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
		case <- clientSession.Ctx.Done():
		case <- time.After(time.Duration(finishedTask.RunMaxTimeSeconds) * time.Second):
			timeout = true
		}
		clientSession.Cancel()
	}()

	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%s", r)
			}
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
	ctx context.Context
	targets map[string]Target
}

func NewTaskWorker(ctx context.Context) *TaskWorker {
	taskWorker := &TaskWorker{
		ctx: ctx,
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
	TaskId bringyour.Id `json:"task_id"`
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
		bringyour.Tx(clientSession.Ctx, func(tx bringyour.PgTx) {
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
		returnErr = errors.New("Target not found.")
		return
	}
}

func (self *TaskWorker) RunPostPost(
	runPost *RunPostArgs,
	runPostResult *RunPostResult,
	clientSession *session.ClientSession,
	tx bringyour.PgTx,
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
func (self *TaskWorker) takeTasks(n int) (map[bringyour.Id]*Task, error) {
	// select from the backlog as well as the current block
	// choose the top N by priority
	// this makes sure the current block doesn't get starved by the backlog

	var taskIds []bringyour.Id

	bringyour.Tx(self.ctx, func(tx bringyour.PgTx) {

		now := bringyour.NowUtc()
		nowBlock := now.Unix() / BlockSizeSeconds


		taskIdPriorities := map[bringyour.Id]int{}

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


		bringyour.WithPgResult(result, err, func() {
			for result.Next() {
				var taskId bringyour.Id
				var priority int
				bringyour.Raise(result.Scan(
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
	    slices.SortStableFunc(taskIds, func(a bringyour.Id, b bringyour.Id)(int) {
	    	aPriority := taskIdPriorities[a]
	    	bPriority := taskIdPriorities[b]
	    	return aPriority - bPriority
	    })

	    // keep up to n
	    taskIds = taskIds[0:min(n, len(taskIds))]

	    claimTime := bringyour.NowUtc()
	    releaseTime := claimTime.Add(ReleaseTimeout)

	    bringyour.BatchInTx(self.ctx, tx, func(batch bringyour.PgBatch) {
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
	}, bringyour.TxReadCommitted)

    return GetTasks(self.ctx, taskIds...), nil
}

// return taskIds of the finished tasks, rescheduled tasks
func (self *TaskWorker) EvalTasks(n int) (
	finishedTaskIds []bringyour.Id,
	rescheduledTaskIds []bringyour.Id,
	postRescheduledTaskIds []bringyour.Id,
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
		runEndTime time.Time
		resultJson string
		runPost func(bringyour.PgTx)(error)
	}

	finishedTasks := map[bringyour.Id]*Finished{}
	rescheduledTasks := map[bringyour.Id]error{}
	postRescheduledTasks := map[bringyour.Id]error{}

	go func() {
		defer close(done)
		for _, task := range tasks {
			if target, ok := self.targets[task.FunctionName]; ok {
				runStartTime := bringyour.NowUtc()
				if result, runPost, err := target.Run(self.ctx, task); err == nil {
					runEndTime := bringyour.NowUtc()
					if resultJson, err := json.Marshal(result); err == nil {
						finishedTasks[task.TaskId] = &Finished{
							runStartTime: runStartTime,
							runEndTime: runEndTime,
							resultJson: string(resultJson),
							runPost: runPost,
						}
					} else {
						// error with converting result to json
						rescheduledTasks[task.TaskId] = err
					}
				} else {
					rescheduledTasks[task.TaskId] = err
				}
			} else {
				rescheduledTasks[task.TaskId] = errors.New("Target not found.")
			}
		}
	}()

	Wait:
	for {
		select {
		case <- done:
			break Wait
		case <- time.After(ReleaseTimeout / 3):
			bringyour.Tx(self.ctx, func(tx bringyour.PgTx) {
				bringyour.BatchInTx(self.ctx, tx, func(batch bringyour.PgBatch) {
					claimTime := bringyour.NowUtc()
					releaseTime := claimTime.Add(ReleaseTimeout)

					for _, task := range tasks {
						batch.Queue(
							`
								UPDATE pending_task
								SET
									claim_time = $2
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

	bringyour.Tx(self.ctx, func(tx bringyour.PgTx) {
		bringyour.BatchInTx(self.ctx, tx, func(batch bringyour.PgBatch) {
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
				now := bringyour.NowUtc()
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
					now := bringyour.NowUtc()
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

