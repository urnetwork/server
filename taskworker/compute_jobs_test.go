package taskworker

// The compute jobs' schedule (CSRV-M12-05; DESIGN-COMPUTE §14.5): each design job's name bound, a chain started for
// each linked job of a minute or more and nothing for an unlinked group, a daily job at its UTC minute, an unbuilt job
// failing and waiting with its chain standing, a run continued where it stopped, and compute_settle a loop a shard.
// Pure: the registry, the clock and the jobs' logic are the test's.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/work"
)

// A task registry recording what is bound and queued.
type testComputeJobTasks struct {
	stateLock sync.Mutex
	functions map[string]sidecar.TaskFunction
	requests  []*sidecar.TaskRequest
}

func newTestComputeJobTasks() *testComputeJobTasks {
	return &testComputeJobTasks{functions: map[string]sidecar.TaskFunction{}}
}

func (self *testComputeJobTasks) Register(name string, function sidecar.TaskFunction) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.functions[name]; ok {
		panic("a second function for " + name)
	}
	self.functions[name] = function
}

func (self *testComputeJobTasks) Schedule(ctx context.Context, request *sidecar.TaskRequest) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	copied := *request
	self.requests = append(self.requests, &copied)
	return nil
}

func (self *testComputeJobTasks) function(name string) sidecar.TaskFunction {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.functions[name]
}

func (self *testComputeJobTasks) queued() []*sidecar.TaskRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

// A runtime over the test's registry, clock, groups and jobs.
func newTestComputeJobRuntime(clock sidecar.Clock, linked func(group string) bool, jobs map[string]*work.Job) (*computeJobRuntime, *testComputeJobTasks) {
	tasks := newTestComputeJobTasks()
	runtime := &computeJobRuntime{
		settings: work.DefaultComputeJobSettings(),
		tasks:    tasks,
		clock:    clock,
		linked:   linked,
		job: func(name string) *work.Job {
			return jobs[name]
		},
	}
	return runtime, tasks
}

// Every design job's task name is bound whatever the binary links; a binary that links no group starts nothing but
// the queued mirror, which starts nothing either.
func TestComputeJobsScheduleNothingWithoutTheirGroup(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	runtime, tasks := newTestComputeJobRuntime(clock, func(group string) bool { return group == "" }, map[string]*work.Job{})
	runtime.register()
	for _, job := range work.ComputeJobDesign(nil) {
		if tasks.function(work.ComputeJobTaskName(job.Name)) == nil {
			t.Errorf("%s is not bound", job.Name)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	var lifetime sync.WaitGroup
	runtime.start(ctx, &lifetime)
	cancel()
	lifetime.Wait()
	if queued := tasks.queued(); len(queued) != 0 {
		t.Fatalf("an unlinked binary queued %+v", queued[0])
	}
	if pending := clock.PendingTimers(); len(pending) != 0 {
		t.Fatalf("an unlinked binary started %d loops", len(pending))
	}
	// a run queued elsewhere fails and waits
	if err := tasks.function(work.ComputeJobTaskName(work.ComputeJobOpRecover))(context.Background(), nil); !errors.Is(err, errComputeJobGroupUnlinked) {
		t.Fatalf("a run in an unlinked binary: %v", err)
	}
}

// A linked job of a minute or more starts its chain at once, once-keyed by its task name; an unbuilt one's run queues
// the next link an interval on and fails with errComputeJobNotBuilt, which server/task's backoff retries, so the chain
// stands until the logic lands, and then the next run works.
func TestComputeUnbuiltJobFailsAndRetries(t *testing.T) {
	start := time.Date(2026, 11, 26, 1, 30, 0, 0, time.UTC)
	clock := sidecartest.NewFakeClock(start)
	jobs := map[string]*work.Job{}
	// the ledger's group alone, so no loop reads the jobs while the test changes them
	runtime, tasks := newTestComputeJobRuntime(clock, func(group string) bool { return group == work.ComputeJobGroupLedger }, jobs)
	runtime.settings.Ledger.SettleShardCount = 0
	runtime.register()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var lifetime sync.WaitGroup
	runtime.start(ctx, &lifetime)
	defer func() {
		cancel()
		lifetime.Wait()
	}()

	taskName := work.ComputeJobTaskName(work.ComputeJobSuspend)
	queued := map[string]*sidecar.TaskRequest{}
	for _, request := range tasks.queued() {
		queued[request.Name] = request
	}
	first := queued[taskName]
	if first == nil || first.OnceKey != taskName || !first.RunAt.Equal(start) || len(first.Args) != 0 {
		t.Fatalf("compute_suspend's chain started as %+v", first)
	}
	if queued[work.ComputeJobTaskName(work.ComputeJobEnclaveMeter)] != nil {
		t.Fatal("an unlinked group's job started its chain")
	}
	if queued[work.ComputeJobTaskName(work.ComputeJobModelMirror)] != nil {
		t.Fatal("the queued mirror started a chain")
	}

	before := len(tasks.queued())
	err := tasks.function(taskName)(ctx, nil)
	if !errors.Is(err, errComputeJobNotBuilt) {
		t.Fatalf("an unbuilt run answered %v", err)
	}
	after := tasks.queued()
	if len(after) != before+1 {
		t.Fatalf("the run queued %d requests", len(after)-before)
	}
	next := after[len(after)-1]
	if next.Name != taskName || next.OnceKey != taskName || !next.RunAt.Equal(start.Add(time.Minute)) {
		t.Fatalf("the chain's next link is %+v", next)
	}

	// the logic lands: the next run works
	runCount := 0
	jobs[work.ComputeJobSuspend] = &work.Job{Name: work.ComputeJobSuspend, Run: func(ctx context.Context, args []byte) ([]byte, error) {
		runCount += 1
		return nil, nil
	}}
	if err := tasks.function(taskName)(ctx, nil); err != nil || runCount != 1 {
		t.Fatalf("a built run: %v, ran %d", err, runCount)
	}
}

// A daily job starts its chain at its next UTC minute, not at once, and each run queues the next day's: 02:00 for
// compute_rollup_day and 04:00 for compute_reconcile, a run at the minute itself queueing the next day's.
func TestComputeDailyJobsRunAtTheirMinute(t *testing.T) {
	start := time.Date(2026, 11, 26, 3, 0, 0, 0, time.UTC)
	clock := sidecartest.NewFakeClock(start)
	runtime, tasks := newTestComputeJobRuntime(clock, func(group string) bool { return true }, map[string]*work.Job{})
	runtime.settings.Ledger.SettleShardCount = 0
	runtime.settings.GammaWatchPollInterval = 0
	runtime.register()
	ctx, cancel := context.WithCancel(context.Background())
	var lifetime sync.WaitGroup
	runtime.start(ctx, &lifetime)
	cancel()
	lifetime.Wait()
	queued := map[string]*sidecar.TaskRequest{}
	for _, request := range tasks.queued() {
		queued[request.Name] = request
	}
	rollupName := work.ComputeJobTaskName(work.ComputeJobRollupDay)
	if request := queued[rollupName]; request == nil || !request.RunAt.Equal(time.Date(2026, 11, 27, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("compute_rollup_day starts at %+v", request)
	}
	if request := queued[work.ComputeJobTaskName(work.ComputeJobReconcile)]; request == nil || !request.RunAt.Equal(time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)) {
		t.Fatalf("compute_reconcile starts at %+v", request)
	}

	// the run at 02:00 the next day queues the day after's, before its (unbuilt) logic fails
	clock.AdvanceTo(time.Date(2026, 11, 27, 2, 0, 0, 0, time.UTC))
	before := len(tasks.queued())
	if err := tasks.function(rollupName)(context.Background(), nil); !errors.Is(err, errComputeJobNotBuilt) {
		t.Fatalf("the run: %v", err)
	}
	after := tasks.queued()
	if len(after) != before+1 || !after[len(after)-1].RunAt.Equal(time.Date(2026, 11, 28, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("the next link is %+v", after[len(after)-1])
	}

	for _, c := range []struct {
		now  time.Time
		want time.Time
	}{
		{now: time.Date(2026, 11, 26, 1, 59, 59, 0, time.UTC), want: time.Date(2026, 11, 26, 2, 0, 0, 0, time.UTC)},
		{now: time.Date(2026, 11, 26, 2, 0, 0, 0, time.UTC), want: time.Date(2026, 11, 27, 2, 0, 0, 0, time.UTC)},
		{now: time.Date(2026, 12, 31, 23, 0, 0, 0, time.FixedZone("UTC+3", 3*60*60)), want: time.Date(2027, 1, 1, 2, 0, 0, 0, time.UTC)},
	} {
		if got := computeJobNextDaily(c.now, 2*time.Hour); !got.Equal(c.want) {
			t.Errorf("after %v: %v, want %v", c.now, got, c.want)
		}
	}
}

// A run that stops partway queues its continuation with the args it answered, at once, or after the settings' retry
// when it failed there, and the run itself then succeeds, so the backoff does not run it again from its start; a
// continuation's run queues no chain link.
func TestComputeJobContinuesWhereItStopped(t *testing.T) {
	start := time.Date(2026, 11, 26, 2, 0, 0, 0, time.UTC)
	clock := sidecartest.NewFakeClock(start)
	stopErr := errors.New("a batch failed")
	answers := []struct {
		args []byte
		err  error
	}{{args: []byte("cursor-1")}, {args: []byte("cursor-2"), err: stopErr}, {}}
	seenArgs := [][]byte{}
	jobs := map[string]*work.Job{
		work.ComputeJobRollupDay: {Name: work.ComputeJobRollupDay, Run: func(ctx context.Context, args []byte) ([]byte, error) {
			seenArgs = append(seenArgs, args)
			answer := answers[0]
			answers = answers[1:]
			return answer.args, answer.err
		}},
	}
	runtime, tasks := newTestComputeJobRuntime(clock, func(group string) bool { return true }, jobs)
	runtime.register()
	function := tasks.function(work.ComputeJobTaskName(work.ComputeJobRollupDay))
	taskName := work.ComputeJobTaskName(work.ComputeJobRollupDay)

	if err := function(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	queued := tasks.queued()
	if len(queued) != 2 || queued[0].OnceKey != taskName || queued[1].OnceKey != taskName+".continue" || string(queued[1].Args) != "cursor-1" || !queued[1].RunAt.Equal(start) {
		t.Fatalf("after a run that stopped at its bound: %+v", queued)
	}
	if err := function(context.Background(), queued[1].Args); err != nil {
		t.Fatalf("a continuation that failed partway answered %v", err)
	}
	queued = tasks.queued()
	if len(queued) != 3 || string(queued[2].Args) != "cursor-2" || !queued[2].RunAt.Equal(start.Add(runtime.settings.ContinueRetryAfter)) {
		t.Fatalf("after a continuation that failed: %+v", queued)
	}
	if err := function(context.Background(), queued[2].Args); err != nil {
		t.Fatal(err)
	}
	if queued := tasks.queued(); len(queued) != 3 {
		t.Fatalf("the finished continuation queued %+v", queued[len(queued)-1])
	}
	if len(seenArgs) != 3 || len(seenArgs[0]) != 0 || string(seenArgs[1]) != "cursor-1" || string(seenArgs[2]) != "cursor-2" {
		t.Fatalf("the runs saw %q", seenArgs)
	}
}

// compute_settle runs as a loop a shard, each with its shard's args, every interval on the clock, and never as a chain;
// the loops end with the lifetime.
func TestComputeSettleRunsALoopAShard(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	stateLock := sync.Mutex{}
	shardRunCounts := map[string]int{}
	jobs := map[string]*work.Job{
		work.ComputeJobSettle: {Name: work.ComputeJobSettle, Run: func(ctx context.Context, args []byte) ([]byte, error) {
			stateLock.Lock()
			defer stateLock.Unlock()
			shardRunCounts[string(args)] += 1
			return nil, nil
		}},
	}
	runtime, tasks := newTestComputeJobRuntime(clock, func(group string) bool { return group == work.ComputeJobGroupLedger }, jobs)
	runtime.settings.Ledger.SettleShardCount = 3
	runtime.register()
	ctx, cancel := context.WithCancel(context.Background())
	var lifetime sync.WaitGroup
	runtime.start(ctx, &lifetime)
	defer func() {
		cancel()
		lifetime.Wait()
	}()
	if err := clock.AwaitTimers(ctx, 3); err != nil {
		t.Fatal(err)
	}
	clock.Advance(runtime.settings.Ledger.SettleInterval)
	if err := clock.AwaitTimers(ctx, 3); err != nil {
		t.Fatal(err)
	}
	stateLock.Lock()
	counts := map[string]int{}
	for shardArgs, count := range shardRunCounts {
		counts[shardArgs] = count
	}
	stateLock.Unlock()
	for shard := range 3 {
		if counts[string(work.ComputeSettleJobArgs(int16(shard)))] != 2 {
			t.Errorf("shard %d ran %d times", shard, counts[string(work.ComputeSettleJobArgs(int16(shard)))])
		}
	}
	if len(counts) != 3 {
		t.Errorf("runs of %d shards", len(counts))
	}
	for _, request := range tasks.queued() {
		if request.Name == work.ComputeJobTaskName(work.ComputeJobSettle) {
			t.Fatalf("compute_settle was queued as a chain: %+v", request)
		}
	}
}
