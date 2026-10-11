// Explicit target limits belong to one worker, across all of its polling
// loops. Reservations precede advisory claims and outlive both handback and
// task execution; unrelated workers and targets never share this budget.
package task

import (
	"fmt"
	"maps"
	"slices"
	"sync/atomic"

	"github.com/urnetwork/server/v2026"
)

// Freeze caller-owned configuration before any run loop can observe it.
// Invalid explicit bounds fail at construction, never by hiding queue rows.
func snapshotTaskWorkerSettings(settings *TaskWorkerSettings) *TaskWorkerSettings {
	if settings == nil {
		settings = DefaultTaskWorkerSettings()
	}
	snapshot := *settings
	snapshot.TargetClaimLimits = maps.Clone(settings.TargetClaimLimits)
	for functionName, limit := range snapshot.TargetClaimLimits {
		if functionName == "" || limit <= 0 {
			panic(fmt.Errorf("task claim limit requires a canonical target name and a positive bound"))
		}
	}
	return &snapshot
}

// One reference belongs to the claim guard; execution adds a second before
// launching. A canceled/panicking collector cannot admit a replacement while
// its task function still unwinds. Each owner releases exactly once.
type taskClaimReservation struct {
	owners         atomic.Int32
	returnCapacity func()
}

// Execution retains its reference before the claim guard can retire.
func (self *taskClaimReservation) retain() {
	self.owners.Add(1)
}

// The last owner returns capacity without any database work under its lock.
func (self *taskClaimReservation) release() {
	if self.owners.Add(-1) == 0 {
		self.returnCapacity()
	}
}

// Resolve the same normalized aliases as dispatch. Registration, as with
// AddTargets, is immutable once polling or direct EvalTasks calls begin.
func (self *TaskWorker) claimTargetName(storedName string) string {
	name := updateFunctionName(storedName)
	if target := self.targets[name]; target != nil {
		return target.TargetFunctionName()
	}
	return name
}

// This is a zero-wait local reservation, not a claimed task parked behind a
// semaphore. A concurrent claim can lose admission after its SQL snapshot.
func (self *TaskWorker) reserveTaskClaim(storedName string) (*taskClaimReservation, bool) {
	if len(self.settings.TargetClaimLimits) == 0 {
		return nil, true
	}
	name := self.claimTargetName(storedName)
	limit := self.settings.TargetClaimLimits[name]
	if limit == 0 {
		return nil, true
	}
	admitted := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.draining || self.runCtx.Err() != nil || limit <= self.claimTargetCounts[name] {
			return false
		}
		self.claimTargetCounts[name]++
		return true
	}()
	if !admitted {
		return nil, false
	}
	reservation := &taskClaimReservation{returnCapacity: func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.claimTargetCounts[name]--
		if self.claimTargetCounts[name] == 0 {
			delete(self.claimTargetCounts, name)
		}
	}}
	reservation.owners.Store(1)
	return reservation, true
}

// Exclude a saturated target and all its aliases before the SQL candidate
// limit, so its old queue head cannot starve ordinary work. RunPost is its own
// target: post-only retries do not own the original probe's transport budget.
func (self *TaskWorker) saturatedClaimFunctionNames() []string {
	saturated := func() map[string]bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		names := map[string]bool{}
		for name, limit := range self.settings.TargetClaimLimits {
			if self.draining || self.runCtx.Err() != nil || limit <= self.claimTargetCounts[name] {
				names[name] = true
			}
		}
		return names
	}()
	if len(saturated) == 0 {
		return nil
	}
	for name, target := range self.targets {
		if saturated[target.TargetFunctionName()] {
			saturated[name] = true
		}
	}
	names := slices.Collect(maps.Keys(saturated))
	slices.Sort(names)
	return names
}

// Speculatively acquired candidates rejected by priority batching return their
// local slot immediately, after their advisory lock was released.
func (self *taskClaimGuard) releaseAdmission(taskId server.Id) {
	if reservation := self.admissionKVs[taskId]; reservation != nil {
		delete(self.admissionKVs, taskId)
		reservation.release()
	}
}

// Only the launcher owns this map. Taking an entry transfers one execution
// reference to that task's goroutine; launcher unwind releases untaken entries.
type taskExecutionAdmissions map[server.Id]*taskClaimReservation

// Transfer immediately before the go statement, with no fallible work between.
func (self taskExecutionAdmissions) take(taskId server.Id) *taskClaimReservation {
	reservation := self[taskId]
	delete(self, taskId)
	return reservation
}

// No launched goroutine shares this map, so panic cleanup cannot retire its
// reference. Missing/deleted task rows were never retained for execution.
func (self taskExecutionAdmissions) releaseUnlaunched() {
	for taskId, reservation := range self {
		delete(self, taskId)
		reservation.release()
	}
}

// Copy reservations before execution starts; guard cleanup can then run in
// parallel with per-task completion without sharing a mutable map.
func (self *taskClaimGuard) retainExecutionAdmissions(tasks map[server.Id]*Task) taskExecutionAdmissions {
	reservations := make(taskExecutionAdmissions, len(self.admissionKVs))
	for taskId := range tasks {
		if reservation := self.admissionKVs[taskId]; reservation != nil {
			reservation.retain()
			reservations[taskId] = reservation
		}
	}
	return reservations
}
