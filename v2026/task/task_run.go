// A Run loop refills retired execution slots without borrowing another loop's
// concurrency budget or ownership connection. Direct EvalTasks stays finite.
package task

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
)

// A slot sends one function result, then (only when it has external commit
// work) one retirement. At most BatchSize slots can retain such work at once.
type taskSlotEvent struct {
	taskId        server.Id
	result        *taskExecutionResult
	cohortResults []*taskExecutionResult
}

// Retire one exact owner, keeping all live sibling locks on the same session.
// A failure leaves the guard responsible for cleanup after its executions join.
func (self *taskClaimGuard) retireTask(ctx context.Context, taskId server.Id) error {
	return self.retireTaskWithQuery(ctx, self.conn, taskId)
}

// Stream ordinary work through n slots until the owned cohort drains. A slot
// includes finalization and committed posts, not only the task function. All
// claim, unlock, and heartbeat calls on the direct session are serialized here.
func (self *TaskWorker) runTaskSlots(n int, poll *taskClaimPoll) (worked bool, returnErr error) {
	tasks, guard, _, err := self.takeTasksWithGuard(self.runCtx, n, nil, taskClaimOptions{detachCommittedRead: true, poll: poll, runCohorts: true})
	if err != nil {
		return false, err
	}
	if guard != nil {
		defer guard.release()
	}
	if len(tasks) == 0 {
		return false, nil
	}
	if guard == nil {
		return false, errors.New("nonempty task claim has no advisory ownership guard")
	}

	evalCtx, evalCancel := context.WithCancel(context.WithoutCancel(self.ctx))
	defer evalCancel()
	active := map[server.Id]*Task{}
	activeSlots := map[server.Id]*taskRunSlot{}
	taskSlots := map[server.Id]*taskRunSlot{}
	heartbeatTasks := map[server.Id]*Task{}
	events := make(chan taskSlotEvent, n)
	publishEvent := func(event taskSlotEvent) {
		events <- event
		if self.taskSlotEventPublished != nil {
			self.taskSlotEventPublished()
		}
	}
	var firstPanic any
	var claimErr error
	isolated := false
	for _, task := range tasks {
		if DefaultPriority < task.RunPriority || DefaultMaxTime < time.Duration(task.RunMaxTimeSeconds)*time.Second {
			isolated = true
		}
	}
	stopped := isolated
	refill := false
	readyHandbacks := 0
	retiredIds := make([]server.Id, 0, n)
	nextPoll := self.heartbeatNow().Add(poll.delay(self.claimNow(), self.settings.PollTimeout))
	nextHeartbeat := self.heartbeatNow().Add(ReleaseTimeout / 3)

	boundedContext := func() (context.Context, context.CancelFunc) {
		timeout := self.settings.FinalizeTimeout
		if timeout <= 0 {
			timeout = DefaultTaskFinalizeTimeout
		}
		return context.WithTimeout(context.WithoutCancel(self.ctx), timeout)
	}
	launch := func(claimed map[server.Id]*Task) {
		for _, task := range claimed {
			task.FunctionName = updateFunctionName(task.FunctionName)
		}
		slots := taskRunSlots(claimed)
		ordinary := map[server.Id]*Task{}
		for _, slot := range slots {
			if len(slot.tasks) == 1 {
				ordinary[slot.id] = slot.tasks[0]
			}
		}
		ordinaryTargets := self.prepareTaskBatchTargets(ordinary)
		reservations := guard.retainExecutionAdmissions(claimed)
		defer reservations.releaseUnlaunched()
		for _, slot := range slots {
			targets := ordinaryTargets
			if len(slot.tasks) > 1 {
				members := make(map[server.Id]*Task, len(slot.tasks))
				for _, queued := range slot.tasks {
					members[queued.TaskId] = queued
				}
				// Preparation may not couple another physical slot to this
				// owner's deadline, account transaction or result publication.
				targets = self.prepareTaskBatchTargets(members)
			}
			if len(activeSlots) >= n {
				panic("Run claim exceeded its physical execution slots")
			}
			activeSlots[slot.id] = slot
			admissions := taskExecutionAdmissions{}
			for _, queued := range slot.tasks {
				active[queued.TaskId] = queued
				heartbeatTasks[queued.TaskId] = queued
				taskSlots[queued.TaskId] = slot
				if reservation := reservations.take(queued.TaskId); reservation != nil {
					admissions[queued.TaskId] = reservation
				}
			}
			go func() {
				defer admissions.releaseUnlaunched()
				if len(slot.tasks) > 1 {
					var results []*taskExecutionResult
					failure := server.HandleError(func() {
						results = self.executeTaskRunCohort(evalCtx, slot, targets, admissions)
					})
					if len(results) != len(slot.tasks) {
						// No partial event may strand a member or publish success
						// for an invocation whose ordinary owner did not return.
						cause := errors.New("Run cohort execution did not join every member")
						if err, ok := failure.(error); ok {
							cause = errors.Join(cause, err)
						}
						results = make([]*taskExecutionResult, 0, len(slot.tasks))
						for _, queued := range slot.tasks {
							results = append(results, &taskExecutionResult{task: queued, err: cause})
						}
					}
					publishEvent(taskSlotEvent{taskId: slot.id, cohortResults: results})
					return
				}
				task := slot.tasks[0]
				var result *taskExecutionResult
				server.HandleError(func() {
					result = self.executeTask(evalCtx, task, targets[task.FunctionName])
				})
				if result == nil {
					result = &taskExecutionResult{task: task, err: errors.New("Task not run.")}
				}
				// The guard's reservation remains until durable handback and
				// every committed post retires, even after execution unwinds.
				if reservation := admissions[task.TaskId]; reservation != nil {
					reservation.release()
					delete(admissions, task.TaskId)
				}
				publishEvent(taskSlotEvent{taskId: task.TaskId, result: result})
			}()
		}
	}
	removeActive := func(taskId server.Id) {
		if active[taskId] == nil {
			return
		}
		delete(active, taskId)
		slot := taskSlots[taskId]
		delete(taskSlots, taskId)
		slot.remaining--
		if slot.remaining == 0 {
			delete(activeSlots, slot.id)
		}
	}
	retire := func(taskId server.Id) {
		// Keep the exact lock and reservation until the next claim. If all
		// ready slots retire, ordinary cohort release can unlock them once.
		// No further event can arrive for this member. At most n slots, each
		// containing at most 64 members, retire before the next bounded flush.
		removeActive(taskId)
		retiredIds = append(retiredIds, taskId)
		refill = true
	}
	flushRetired := func() {
		ctx, cancel := boundedContext()
		defer cancel()
		for _, taskId := range retiredIds {
			// A partial failure leaves the remaining IDs guard-owned. It
			// unwinds this collector, so ambiguous unlocks are never replayed.
			server.Raise(guard.retireTask(ctx, taskId))
		}
		retiredIds = retiredIds[:0]
	}
	handleCohortEvent := func(event taskSlotEvent) {
		slot := activeSlots[event.taskId]
		if slot == nil {
			panic("Run cohort result lost its physical slot")
		}
		retained := true
		defer func() {
			if retained {
				// Every function already joined. An ambiguous/fatal handback
				// retains all keys and heartbeats until the other slots join.
				for _, queued := range slot.tasks {
					removeActive(queued.TaskId)
				}
			}
		}()
		if len(event.cohortResults) != len(slot.tasks) {
			panic("Run cohort result lost a durable member")
		}
		for index, result := range event.cohortResults {
			if result == nil || result.task != slot.tasks[index] {
				panic("Run cohort result changed its exact member")
			}
			logTaskExecutionResult(result)
		}
		if err := self.finalizeTaskRunCohort(event.cohortResults); err != nil {
			if firstPanic == nil {
				firstPanic = err
			}
			stopped = true
			return
		}
		for _, result := range event.cohortResults {
			delete(heartbeatTasks, result.task.TaskId)
			retire(result.task.TaskId)
			outcome := "succeeded"
			if result.err != nil {
				outcome = "rescheduled"
			}
			taskFinalizationsTotal.WithLabelValues(outcome).Inc()
		}
		retained = false
	}
	handleSingleEvent := func(event taskSlotEvent) {
		if event.cohortResults != nil {
			handleCohortEvent(event)
			return
		}
		if event.result == nil {
			retire(event.taskId)
			return
		}
		r := event.result
		logTaskExecutionResult(r)
		var posts []server.PostFunction
		var postRescheduled bool
		failed := false
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					if firstPanic == nil {
						firstPanic = recovered
					}
					failed = true
					stopped = true
					glog.Infof("[%s]task finalization failed: %v\n", r.task.TaskId, recovered)
				}
			}()
			posts, postRescheduled = self.finalizeTask(r)
		}()
		if failed {
			// Keep ambiguous/failed ownership and its heartbeat until every
			// other slot joins; never replay a possibly committed finalization.
			removeActive(event.taskId)
			return
		}
		// Transfer committed work before any later collector operation can
		// fail. The deferred handoff also runs while that collector unwinds.
		defer func() {
			if len(posts) == 0 {
				retire(event.taskId)
				return
			}
			go func() {
				defer func() { publishEvent(taskSlotEvent{taskId: event.taskId}) }()
				server.HandleError(func() {
					server.RunPosts(context.WithoutCancel(evalCtx), posts...)
				})
			}()
		}()
		delete(heartbeatTasks, event.taskId)
		switch {
		case r.err != nil:
			taskFinalizationsTotal.WithLabelValues("rescheduled").Inc()
		case postRescheduled:
			taskFinalizationsTotal.WithLabelValues("post_rescheduled").Inc()
		default:
			taskFinalizationsTotal.WithLabelValues("succeeded").Inc()
		}

	}
	heartbeat := func() {
		for _, task := range heartbeatTasks {
			elapsedSeconds := float32(self.heartbeatNow().Sub(task.ClaimTime)/time.Millisecond) / 1000
			if elapsedSeconds >= 10 {
				glog.Infof("[%s]eval active(%.2fs) %s(%s)\n", task.TaskId, elapsedSeconds, task.FunctionName, ArgumentsForLog(task.ArgsJson))
			}
		}
		ctx, cancel := boundedContext()
		defer cancel()
		server.Raise(guard.ping(ctx))
		if err := tryRefreshTaskTimestampLeases(ctx, heartbeatTasks, self.refreshTaskTimestampLeases); err != nil {
			taskTimestampLeaseRefreshErrorCounter.Inc()
			glog.Infof("[taskworker]timestamp lease refresh failed while advisory ownership remained healthy: %v\n", err)
		}
		nextHeartbeat = self.heartbeatNow().Add(ReleaseTimeout / 3)
	}
	var deferredEvent *taskSlotEvent
	collectEvent := func(event taskSlotEvent) int {
		if event.result == nil {
			handleSingleEvent(event)
			return 1
		}
		ready := []*taskExecutionResult{event.result}
		owned := true
		defer func() {
			if recovered := recover(); recovered != nil {
				if owned {
					// These functions already returned. A collector failure must
					// not replay their possibly committed handback or await a
					// second event. Their guards remain owned through sibling join.
					for _, r := range ready {
						removeActive(r.task.TaskId)
					}
				}
				panic(recovered)
			}
		}()
		if self.canBatchTaskCompletion(event.result) {
		collectReady:
			for len(ready) < min(n, taskCompletionBatchLimit) {
				select {
				case next := <-events:
					// Retain an ordinary result/post retirement before inspecting
					// it, so an interrupted collector still owns its cleanup.
					deferredEvent = &next
					if !self.canBatchTaskCompletion(next.result) {
						break collectReady
					}
					ready = append(ready, next.result)
					deferredEvent = nil
				default:
					break collectReady
				}
			}
		}
		if len(ready) == 1 {
			owned = false
			handleSingleEvent(event)
			return 1
		}
		retrySingles, err := self.finalizeTaskBatch(ready)
		if retrySingles {
			for _, r := range ready {
				if !self.heartbeatNow().Before(nextHeartbeat) {
					heartbeat()
				}
				handleSingleEvent(taskSlotEvent{taskId: r.task.TaskId, result: r})
			}
			return len(ready)
		}
		for _, r := range ready {
			logTaskExecutionResult(r)
		}
		if err != nil {
			if firstPanic == nil {
				firstPanic = err
			}
			stopped = true
			for _, r := range ready {
				removeActive(r.task.TaskId)
			}
			glog.Infof("[taskworker]task completion batch remains unacknowledged: %v\n", err)
			return len(ready)
		}
		for _, r := range ready {
			delete(heartbeatTasks, r.task.TaskId)
			retire(r.task.TaskId)
		}
		taskFinalizationsTotal.WithLabelValues("succeeded").Add(float64(len(ready)))
		return len(ready)
	}

	func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				if firstPanic == nil {
					firstPanic = recovered
				}
				evalCancel()
			}
		}()
		launch(tasks)
		runDone := self.runCtx.Done()
		for len(active) != 0 {
			if self.runCtx.Err() != nil {
				stopped = true
				runDone = nil
			}
			// A ready result stream cannot postpone the ownership heartbeat.
			if !self.heartbeatNow().Before(nextHeartbeat) {
				heartbeat()
				continue
			}
			if deferredEvent != nil {
				event := *deferredEvent
				deferredEvent = nil
				readyHandbacks++
				handleSingleEvent(event)
				continue
			}
			free := n - len(activeSlots)
			if !stopped && free > 0 && (refill || !self.heartbeatNow().Before(nextPoll)) {
				// Combine already-ready retirements into the next bounded claim.
				// Never wait for another result. With no launches during this
				// drain, each of n slots can publish at most result + post join.
				// Re-enter the loop between handbacks for cancellation/heartbeat.
				if readyHandbacks < 2*n {
					select {
					case event := <-events:
						readyHandbacks += collectEvent(event)
						continue
					default:
					}
				}
				readyHandbacks = 0
				flushRetired()
				refill = false
				timeout := self.settings.FinalizeTimeout
				if timeout <= 0 {
					timeout = DefaultTaskFinalizeTimeout
				}
				claimCtx, cancel := context.WithTimeout(self.runCtx, timeout)
				claimed, _, needsIsolation, err := self.takeTasksWithGuard(claimCtx, free, guard, taskClaimOptions{ordinaryOnly: true, detachCommittedRead: true, poll: poll, runCohorts: true})
				cancel()
				if err != nil {
					claimErr = err
					stopped = true
				} else if needsIsolation {
					stopped = true
				} else if len(claimed) != 0 {
					taskPollsTotal.WithLabelValues("claimed").Inc()
					launch(claimed)
				} else {
					taskPollsTotal.WithLabelValues("empty").Inc()
				}
				nextPoll = self.heartbeatNow().Add(poll.delay(self.claimNow(), self.settings.PollTimeout))
				// Check heartbeat again after the bounded claim transaction.
				continue
			}
			var pollTick <-chan time.Time
			if !stopped && free > 0 {
				pollTick = self.pollAfter(nextPoll.Sub(self.heartbeatNow()))
			}
			select {
			case <-runDone:
				stopped = true
				runDone = nil
			case event := <-events:
				readyHandbacks += collectEvent(event)
			case <-self.heartbeatAfter(nextHeartbeat.Sub(self.heartbeatNow())):
				heartbeat()
			case <-pollTick:
				refill = true
			}
		}
	}()

	// Unexpected collector/ownership failures cancel functions but must still
	// join their results and detached committed work before releasing any owner.
	// Use the ordinary timer here so a failing injected collector hook is not
	// called a second time during cleanup.
	for len(active) != 0 {
		if !self.heartbeatNow().Before(nextHeartbeat) {
			server.HandleError(heartbeat)
			// A dead ownership session must not create a hot cleanup loop.
			nextHeartbeat = self.heartbeatNow().Add(ReleaseTimeout / 3)
			continue
		}
		if deferredEvent != nil {
			event := *deferredEvent
			deferredEvent = nil
			if recovered := server.HandleError(func() { handleSingleEvent(event) }); recovered != nil && firstPanic == nil {
				firstPanic = recovered
			}
			continue
		}
		select {
		case event := <-events:
			if recovered := server.HandleError(func() { handleSingleEvent(event) }); recovered != nil && firstPanic == nil {
				firstPanic = recovered
			}
		case <-time.After(nextHeartbeat.Sub(self.heartbeatNow())):
			server.HandleError(heartbeat)
			nextHeartbeat = self.heartbeatNow().Add(ReleaseTimeout / 3)
		}
	}

	if firstPanic != nil {
		panic(firstPanic)
	}
	if self.runCtx.Err() != nil {
		return true, nil
	}
	return true, claimErr
}
