// Deterministic local ownership tests complement the PostgreSQL claim tests.
// Barriers hold successful reservations while every competing caller tries.
package task

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// No database or task goroutine is needed to construct an admission owner.
func newClaimAdmissionTestWorker(t testing.TB, limit int) (*TaskWorker, string) {
	t.Helper()
	target := NewTaskTarget(claimProfileAllowed, "fixture.example/tasks.LegacyLimited")
	settings := DefaultTaskWorkerSettings()
	settings.TargetClaimLimits = map[string]int{target.TargetFunctionName(): limit}
	worker := NewTaskWorker(context.Background(), settings)
	worker.AddTargets(target)
	t.Cleanup(worker.Close)
	return worker, target.TargetFunctionName()
}

// The generic scheduler remains unlimited unless its owner explicitly opts in.
func TestTaskClaimAdmissionDefaultIsUnlimited(t *testing.T) {
	worker := NewTaskWorkerWithDefaults(t.Context())
	defer worker.Close()
	for range 128 {
		reservation, admitted := worker.reserveTaskClaim("fixture.example/tasks.Unlimited")
		if !admitted || reservation != nil {
			t.Fatal("ordinary target acquired a new admission budget")
		}
	}
	if len(worker.saturatedClaimFunctionNames()) != 0 {
		t.Fatal("default scheduler filtered ordinary work")
	}
}

// Instances snapshot settings and hold independent capacity, even when callers
// supplied the very same settings pointer and map to their constructors.
func TestTaskClaimAdmissionInstanceAndSettingsIsolation(t *testing.T) {
	name := NewTaskTarget(claimProfileAllowed).TargetFunctionName()
	settings := DefaultTaskWorkerSettings()
	settings.TargetClaimLimits = map[string]int{name: 1}
	first := NewTaskWorker(t.Context(), settings)
	second := NewTaskWorker(t.Context(), settings)
	defer first.Close()
	defer second.Close()
	settings.TargetClaimLimits[name] = 100
	settings.BatchSize = 100
	for _, worker := range []*TaskWorker{first, second} {
		reservation, admitted := worker.reserveTaskClaim(name)
		if !admitted || reservation == nil {
			t.Fatal("another instance consumed this owner's capacity")
		}
		defer reservation.release()
		if extra, admitted := worker.reserveTaskClaim(name); admitted {
			if extra != nil {
				extra.release()
			}
			t.Fatal("caller mutation changed an existing admission limit")
		}
		if worker.settings.BatchSize == 100 {
			t.Fatal("worker retained mutable caller settings")
		}
	}
}

// All contenders cross a start barrier, and the winner cannot retire before
// every contender's result is collected. Exactly one reservation may exist.
func TestTaskClaimAdmissionConcurrentReservationBound(t *testing.T) {
	worker, name := newClaimAdmissionTestWorker(t, 1)
	start := make(chan struct{})
	release := make(chan struct{})
	var contenders, owners sync.WaitGroup
	var admittedCount atomic.Int32
	for range 64 {
		contenders.Add(1)
		owners.Add(1)
		go func() {
			defer owners.Done()
			<-start
			reservation, admitted := worker.reserveTaskClaim(name)
			if admitted {
				admittedCount.Add(1)
			}
			contenders.Done()
			if reservation != nil {
				<-release
				reservation.release()
			}
		}()
	}
	close(start)
	contenders.Wait()
	if admittedCount.Load() != 1 {
		t.Errorf("concurrent claim callers admitted %d owners, want one", admittedCount.Load())
	}
	close(release)
	owners.Wait()
	if names := worker.saturatedClaimFunctionNames(); len(names) != 0 {
		t.Errorf("completed reservation leaked capacity: %v", names)
	}
}

// Both guard cleanup and actual execution completion are mandatory; neither
// ordering may make a replacement visible before the second boundary.
func TestTaskClaimAdmissionRequiresHandbackAndExecutionCompletion(t *testing.T) {
	for _, executionFirst := range []bool{false, true} {
		worker, name := newClaimAdmissionTestWorker(t, 1)
		reservation, _ := worker.reserveTaskClaim(name)
		taskId := server.NewId()
		guard := &taskClaimGuard{admissionKVs: map[server.Id]*taskClaimReservation{taskId: reservation}}
		finishExecution := guard.retainExecutionAdmissions(map[server.Id]*Task{taskId: nil}).take(taskId).release
		first, second := guard.release, finishExecution
		if executionFirst {
			first, second = second, first
		}
		first()
		if extra, admitted := worker.reserveTaskClaim(name); admitted {
			if extra != nil {
				extra.release()
			}
			t.Error("only one owner completed, but replacement admission became possible")
		}
		second()
		guard.release()
		if len(worker.saturatedClaimFunctionNames()) != 0 {
			t.Error("joined execution and handback retained local capacity")
		}
	}
}

// A launcher panic returns every untaken execution reference, but cannot
// retire a reference already handed to a still-unwinding task goroutine.
func TestTaskClaimAdmissionLauncherPanicReleasesOnlyUnlaunched(t *testing.T) {
	worker, name := newClaimAdmissionTestWorker(t, 2)
	first, _ := worker.reserveTaskClaim(name)
	second, _ := worker.reserveTaskClaim(name)
	firstId, secondId := server.NewId(), server.NewId()
	guard := &taskClaimGuard{admissionKVs: map[server.Id]*taskClaimReservation{firstId: first, secondId: second}}
	executions := guard.retainExecutionAdmissions(map[server.Id]*Task{firstId: nil, secondId: nil})
	launched := executions.take(firstId)
	func() {
		defer func() { _ = recover() }()
		defer executions.releaseUnlaunched()
		panic("synthetic launch failure")
	}()
	guard.release()
	if first.owners.Load() != 1 || second.owners.Load() != 0 || worker.claimTargetCounts[name] != 1 {
		t.Fatal("launcher failure freed a launched owner or leaked an unlaunched one")
	}
	launched.release()
	if worker.claimTargetCounts[name] != 0 {
		t.Fatal("last launched owner did not return its capacity")
	}
}

// Operator deletion between commit and GetTasks must not retain an execution
// reference for a row that the launcher can no longer see.
func TestTaskClaimAdmissionMissingRowsRetainNoExecutionReference(t *testing.T) {
	worker, name := newClaimAdmissionTestWorker(t, 1)
	reservation, _ := worker.reserveTaskClaim(name)
	guard := &taskClaimGuard{admissionKVs: map[server.Id]*taskClaimReservation{server.NewId(): reservation}}
	executions := guard.retainExecutionAdmissions(nil)
	guard.release()
	executions.releaseUnlaunched()
	if reservation.owners.Load() != 0 || worker.claimTargetCounts[name] != 0 {
		t.Fatal("missing row leaked a never-launched execution reservation")
	}
}

// Alias and version normalization is identical at SQL exclusion and local
// reservation. Post-only wrappers remain outside the limited target's budget.
func TestTaskClaimAdmissionAliasesAndPostRemainDistinct(t *testing.T) {
	worker, name := newClaimAdmissionTestWorker(t, 1)
	versioned := strings.Replace(name, "/server/", "/server/v73/", 1)
	reservation, admitted := worker.reserveTaskClaim(versioned)
	if !admitted || reservation == nil {
		t.Fatal("versioned canonical target did not reserve its owner's capacity")
	}
	defer reservation.release()
	want := []string{name, "fixture.example/tasks.LegacyLimited"}
	slices.Sort(want)
	if got := worker.saturatedClaimFunctionNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("saturation omitted an alias: got=%v want=%v", got, want)
	}
	if _, admitted := worker.reserveTaskClaim("fixture.example/tasks.LegacyLimited"); admitted {
		t.Fatal("an alias bypassed its canonical target's capacity")
	}
	if post, admitted := worker.reserveTaskClaim(functionName(worker.RunPost)); !admitted || post != nil {
		t.Fatal("post-only retry borrowed the original task's transport budget")
	}
}

// Drain closes fresh limited admission but must not clear a token still held
// by an unwinding function. Ordinary target policy remains unchanged.
func TestTaskClaimAdmissionDrainPreservesExistingReservation(t *testing.T) {
	worker, name := newClaimAdmissionTestWorker(t, 1)
	reservation, _ := worker.reserveTaskClaim(name)
	worker.setDraining()
	if worker.claimTargetCounts[name] != 1 {
		t.Fatal("drain cleared still-owned capacity")
	}
	reservation.release()
	if _, admitted := worker.reserveTaskClaim(name); admitted {
		t.Fatal("draining owner admitted a fresh limited task")
	}
	if ordinary, admitted := worker.reserveTaskClaim("fixture.example/tasks.Ordinary"); !admitted || ordinary != nil {
		t.Fatal("limited admission changed ordinary target policy")
	}
}
