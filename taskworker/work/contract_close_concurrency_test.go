// The background closer's owner batches and steals are its concurrent writers
// on the primary. contract_close.yml lowers either limit for an operator who
// needs headroom on a saturated primary; nothing else may change.
package work

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestContractCloseConcurrencyLimitsKeepTheCompiledLimitsWithoutConfig(t *testing.T) {
	deadlineCloseParallel, stealConcurrency := contractCloseConcurrencyLimits()
	if deadlineCloseParallel != contractDeadlineCloseParallel || stealConcurrency != scheduledCloseStealConcurrency {
		t.Fatalf("limits without config = (%d, %d), want the compiled (%d, %d)",
			deadlineCloseParallel, stealConcurrency, contractDeadlineCloseParallel, scheduledCloseStealConcurrency)
	}
}

func TestContractCloseConcurrencyLimitsOnlyLowerTheCompiledLimits(t *testing.T) {
	cases := []struct {
		config                    string
		wantDeadlineCloseParallel int
		wantStealConcurrency      int
	}{
		{config: "deadline_close_parallel: 3\nsteal_concurrency: 2\n", wantDeadlineCloseParallel: 3, wantStealConcurrency: 2},
		{config: "deadline_close_parallel: 1\nsteal_concurrency: 1\n", wantDeadlineCloseParallel: 1, wantStealConcurrency: 1},
		{config: "steal_concurrency: 4\n", wantDeadlineCloseParallel: contractDeadlineCloseParallel, wantStealConcurrency: 4},
		{config: "deadline_close_parallel: 0\nsteal_concurrency: -2\n", wantDeadlineCloseParallel: contractDeadlineCloseParallel, wantStealConcurrency: scheduledCloseStealConcurrency},
		{config: "deadline_close_parallel: 64\nsteal_concurrency: 9\n", wantDeadlineCloseParallel: contractDeadlineCloseParallel, wantStealConcurrency: scheduledCloseStealConcurrency},
		{config: "deadline_close_parallel: four\n", wantDeadlineCloseParallel: contractDeadlineCloseParallel, wantStealConcurrency: scheduledCloseStealConcurrency},
		{config: "deadline_close_parallel: [2\n", wantDeadlineCloseParallel: contractDeadlineCloseParallel, wantStealConcurrency: scheduledCloseStealConcurrency},
	}
	for _, c := range cases {
		func() {
			defer server.Config.PushSimpleResource(contractCloseConcurrencyResourceName, []byte(c.config))()
			deadlineCloseParallel, stealConcurrency := contractCloseConcurrencyLimits()
			if deadlineCloseParallel != c.wantDeadlineCloseParallel || stealConcurrency != c.wantStealConcurrency {
				t.Errorf("config %q: limits = (%d, %d), want (%d, %d)", c.config,
					deadlineCloseParallel, stealConcurrency, c.wantDeadlineCloseParallel, c.wantStealConcurrency)
			}
		}()
	}
}

// Every owner is held at a barrier until the test releases it, so the active
// count each owner sees on entry is exact.
func TestContractOwnerBatchesRunAtMostTheConfiguredParallel(t *testing.T) {
	const ownerCount = 6
	const parallel = 2
	owners := []int{}
	for owner := range ownerCount {
		owners = append(owners, owner)
	}
	entered := make(chan int, ownerCount)
	releases := map[int]chan struct{}{}
	for _, owner := range owners {
		releases[owner] = make(chan struct{})
	}
	var stateLock sync.Mutex
	active := 0
	maxActive := 0
	finished := 0
	done := make(chan bool)
	go func() {
		done <- runContractOwnerBatches(owners, parallel, func() bool { return true }, func(owner int) {
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				active++
				maxActive = max(maxActive, active)
			}()
			entered <- owner
			<-releases[owner]
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				active--
				finished++
			}()
		})
	}()

	running := []int{<-entered, <-entered}
	for released := 0; released < ownerCount; released++ {
		next := running[0]
		running = running[1:]
		close(releases[next])
		if released+parallel < ownerCount {
			running = append(running, <-entered)
		}
	}
	if complete := <-done; !complete {
		t.Fatalf("every owner started, but the batches report incomplete")
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	if maxActive != parallel {
		t.Fatalf("at most %d owners at once, observed %d", parallel, maxActive)
	}
	if finished != ownerCount {
		t.Fatalf("%d of %d owners finished", finished, ownerCount)
	}
}

func TestContractOwnerBatchesStopStartingWhenTheBudgetEnds(t *testing.T) {
	owners := []int{0, 1, 2, 3, 4}
	starts := 0
	more := func() bool {
		starts++
		return starts <= 3
	}
	var stateLock sync.Mutex
	ran := []int{}
	complete := runContractOwnerBatches(owners, 1, more, func(owner int) {
		stateLock.Lock()
		defer stateLock.Unlock()
		ran = append(ran, owner)
	})
	if complete {
		t.Fatalf("the budget ended before every owner started, but the batches report complete")
	}
	if len(ran) != 3 {
		t.Fatalf("ran %v, want the first 3 owners", ran)
	}
}

func TestScheduledCloseStealAdmissionHonorsTheLimit(t *testing.T) {
	slots := make(chan struct{}, scheduledCloseStealConcurrency)
	releaseFirst, ok := admitScheduledCloseSteal(slots, 2)
	if !ok {
		t.Fatalf("first steal refused under a limit of 2")
	}
	releaseSecond, ok := admitScheduledCloseSteal(slots, 2)
	if !ok {
		t.Fatalf("second steal refused under a limit of 2")
	}
	if _, ok := admitScheduledCloseSteal(slots, 2); ok {
		t.Fatalf("third steal admitted under a limit of 2")
	}
	if len(slots) != 2 {
		t.Fatalf("a refused steal kept its slot: %d held, want 2", len(slots))
	}
	releaseFirst()
	releaseThird, ok := admitScheduledCloseSteal(slots, 2)
	if !ok {
		t.Fatalf("steal refused after a release under a limit of 2")
	}
	releaseSecond()
	releaseThird()
	if len(slots) != 0 {
		t.Fatalf("%d slots held after every release", len(slots))
	}

	// the compiled capacity still bounds a limit at the compiled value
	releases := []func(){}
	for range scheduledCloseStealConcurrency {
		release, ok := admitScheduledCloseSteal(slots, scheduledCloseStealConcurrency)
		if !ok {
			t.Fatalf("steal refused below the compiled capacity")
		}
		releases = append(releases, release)
	}
	if _, ok := admitScheduledCloseSteal(slots, scheduledCloseStealConcurrency); ok {
		t.Fatalf("steal admitted beyond the compiled capacity")
	}
	for _, release := range releases {
		release()
	}
}

// Root cause: the steal concurrency was fixed at 8 per process, so an operator
// could not shed the closer's write load on a saturated primary. With
// steal_concurrency: 1 and one steal already running, another steal must refuse
// before it reads a single candidate. The first read of a steal chooses its
// sample slice, which this test replaces with a tripwire.
func TestScheduledCloseStealOverTheConfiguredLimitReadsNothing(t *testing.T) {
	defer server.Config.PushSimpleResource(contractCloseConcurrencyResourceName, []byte("steal_concurrency: 1\n"))()

	// one steal already holds a slot
	scheduledCloseStealSlots <- struct{}{}
	defer func() { <-scheduledCloseStealSlots }()

	sampled := false
	sliceStart := scheduledCloseRecentSliceStart
	scheduledCloseRecentSliceStart = func(time.Time, time.Duration) time.Time {
		sampled = true
		panic("steal admitted beyond the configured limit")
	}
	defer func() { scheduledCloseRecentSliceStart = sliceStart }()

	closed := stealScheduledCloseOwner(context.Background(), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), server.Id{}, nil)
	if sampled {
		t.Fatalf("a second steal ran under steal_concurrency: 1")
	}
	if closed != 0 {
		t.Fatalf("a refused steal closed %d contracts", closed)
	}
	if len(scheduledCloseStealSlots) != 1 {
		t.Fatalf("%d steal slots held, want only the running steal's", len(scheduledCloseStealSlots))
	}
}
