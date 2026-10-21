package task

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
)

// Execute one owned task using the same cancellation, result, and metrics
// policy for finite EvalTasks and the continuous Run collector.
func (self *TaskWorker) executeTask(evalCtx context.Context, task *Task, target Target) *taskExecutionResult {
	metricName := self.metricName(task.FunctionName)
	attribution := taskMetricAttribution(task)
	taskExecutionInflight.WithLabelValues(metricName, attribution).Inc()
	defer taskExecutionInflight.WithLabelValues(metricName, attribution).Dec()

	r := &taskExecutionResult{
		task:         task,
		runStartTime: server.NowUtc(),
	}
	if target != nil {
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

			// Inspect before fnCancel cleanup can supply a cancellation of its
			// own. Only the collector can set this private ancestor cause.
			defer func() {
				r.collectorInterrupted = taskCollectorInterrupted(fnCtx, err)
			}()
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
	return r
}

// Keep ordinary completion/error logs identical across both collectors.
func logTaskExecutionResult(r *taskExecutionResult) {
	elapsedSeconds := float32(r.runEndTime.Sub(r.runStartTime)/time.Millisecond) / 1000
	if r.err == nil {
		glog.V(1).Infof("[%s]eval done(%.2fs) %s(%s) = %s\n", r.task.TaskId, elapsedSeconds, r.task.FunctionName, ArgumentsForLog(r.task.ArgsJson), string(r.resultJson))
	} else {
		glog.Infof("[%s]eval error(%.2fs) (reschedule) %s(%s) = %s\n", r.task.TaskId, elapsedSeconds, r.task.FunctionName, ArgumentsForLog(r.task.ArgsJson), r.err)
	}
}
