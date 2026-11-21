// The announce batcher's queue (transport_announce_batch.go): what it takes
// into a batch, what each caller gets back, and how a caller's end withdraws
// or waits for its item. No database or Redis: each queue runs a test batch.
package connect

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// A queue of ints whose batch returns each item doubled, except as
// outcomeFor says; it records every batch it ran.
type announceBatchQueueForTest struct {
	queue *announceBatchQueue[int, int]

	stateLock sync.Mutex
	batches   [][]int
}

func newAnnounceBatchQueueForTest(
	maxBatchCount int,
	outcomeFor func(item int) announceBatchOutcome[int],
) *announceBatchQueueForTest {
	settings := DefaultConnectionAnnounceBatchSettings()
	settings.MaxBatchCount = maxBatchCount
	fixture := &announceBatchQueueForTest{}
	fixture.queue = newAnnounceBatchQueue(
		announceBatchKindSyncRead,
		settings,
		func(ctx context.Context, items []int) []announceBatchOutcome[int] {
			func() {
				fixture.stateLock.Lock()
				defer fixture.stateLock.Unlock()
				fixture.batches = append(fixture.batches, slices.Clone(items))
			}()
			outcomes := make([]announceBatchOutcome[int], len(items))
			for i, item := range items {
				if outcomeFor != nil {
					outcomes[i] = outcomeFor(item)
				} else {
					outcomes[i] = announceBatchOutcome[int]{result: 2 * item}
				}
			}
			return outcomes
		},
	)
	return fixture
}

func (self *announceBatchQueueForTest) ranBatches() [][]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.batches)
}

// Holds the queue's driver before its takes until release closes.
func (self *announceBatchQueueForTest) holdTakes() (release func()) {
	gate := make(chan struct{})
	self.queue.beforeTakeForTest = func() {
		<-gate
	}
	return sync.OnceFunc(func() {
		close(gate)
	})
}

// Waits until count items wait in the queue.
func (self *announceBatchQueueForTest) awaitWaiting(t testing.TB, count int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		waiting := func() int {
			self.queue.stateLock.Lock()
			defer self.queue.stateLock.Unlock()
			return len(self.queue.waiting)
		}()
		if waiting == count {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d items wait, want %d", waiting, count)
		}
		time.Sleep(time.Millisecond)
	}
}

// Submits item and returns its result, or what it raised.
func submitForTest(ctx context.Context, queue *announceBatchQueue[int, int], item int) (result int, raised any) {
	defer func() {
		raised = recover()
	}()
	return queue.submit(ctx, item), nil
}

// Items that wait while the driver is busy share one batch, and each caller
// gets its own item's result.
func TestAnnounceBatchQueueGathersWaitingItemsIntoOneBatch(t *testing.T) {
	fixture := newAnnounceBatchQueueForTest(512, nil)
	release := fixture.holdTakes()
	defer release()
	const itemCount = 32
	results := make([]int, itemCount)
	var callers sync.WaitGroup
	for item := range itemCount {
		callers.Go(func() {
			result, raised := submitForTest(context.Background(), fixture.queue, item)
			if raised != nil {
				t.Errorf("item %d raised %v", item, raised)
			}
			results[item] = result
		})
	}
	fixture.awaitWaiting(t, itemCount)
	release()
	callers.Wait()
	batches := fixture.ranBatches()
	if len(batches) != 1 || len(batches[0]) != itemCount {
		t.Fatalf("%d waiting items ran in batches %v, want one batch of all", itemCount, batches)
	}
	for item, result := range results {
		if result != 2*item {
			t.Fatalf("item %d got %d, want its own result %d", item, result, 2*item)
		}
	}
}

// A lone item runs at once, in a batch of its own: no timer holds it for
// company.
func TestAnnounceBatchQueueRunsALoneItemAtOnce(t *testing.T) {
	fixture := newAnnounceBatchQueueForTest(512, nil)
	done := make(chan int, 1)
	go func() {
		result, _ := submitForTest(context.Background(), fixture.queue, 21)
		done <- result
	}()
	select {
	case result := <-done:
		if result != 42 {
			t.Fatalf("the lone item got %d, want 42", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a lone item waited for other items")
	}
	if batches := fixture.ranBatches(); len(batches) != 1 || !slices.Equal(batches[0], []int{21}) {
		t.Fatalf("the lone item ran in batches %v, want one of its own", batches)
	}
}

// One item's failure is that item's alone: its caller gets the raise, and
// every other caller in its batch its result.
func TestAnnounceBatchQueueIsolatesAnItemFailure(t *testing.T) {
	itemErr := errors.New("test: this item's command failed")
	fixture := newAnnounceBatchQueueForTest(512, func(item int) announceBatchOutcome[int] {
		if item == 3 {
			return announceBatchOutcome[int]{raised: itemErr}
		}
		return announceBatchOutcome[int]{result: 2 * item}
	})
	release := fixture.holdTakes()
	defer release()
	const itemCount = 8
	raises := make([]any, itemCount)
	results := make([]int, itemCount)
	var callers sync.WaitGroup
	for item := range itemCount {
		callers.Go(func() {
			results[item], raises[item] = submitForTest(context.Background(), fixture.queue, item)
		})
	}
	fixture.awaitWaiting(t, itemCount)
	release()
	callers.Wait()
	for item := range itemCount {
		if item == 3 {
			if raises[item] != itemErr {
				t.Fatalf("the failed item raised %v, want its own error", raises[item])
			}
			continue
		}
		if raises[item] != nil || results[item] != 2*item {
			t.Fatalf("item %d of the failed item's batch got %d and raised %v", item, results[item], raises[item])
		}
	}
}

// A batch that raises, an outage, fails every item in it with the raise, and
// the queue goes on to the next batch.
func TestAnnounceBatchQueueRaiseFailsEveryItem(t *testing.T) {
	outageErr := errors.New("test: the batch's statement failed")
	failing := true
	var failingLock sync.Mutex
	settings := DefaultConnectionAnnounceBatchSettings()
	queue := newAnnounceBatchQueue(
		announceBatchKindSyncRead,
		settings,
		func(ctx context.Context, items []int) []announceBatchOutcome[int] {
			failingLock.Lock()
			defer failingLock.Unlock()
			if failing {
				panic(outageErr)
			}
			outcomes := make([]announceBatchOutcome[int], len(items))
			for i, item := range items {
				outcomes[i].result = 2 * item
			}
			return outcomes
		},
	)
	gate := make(chan struct{})
	queue.beforeTakeForTest = func() {
		<-gate
	}
	const itemCount = 4
	raises := make([]any, itemCount)
	var callers sync.WaitGroup
	for item := range itemCount {
		callers.Go(func() {
			_, raises[item] = submitForTest(context.Background(), queue, item)
		})
	}
	for {
		waiting := func() int {
			queue.stateLock.Lock()
			defer queue.stateLock.Unlock()
			return len(queue.waiting)
		}()
		if waiting == itemCount {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(gate)
	callers.Wait()
	for item, raised := range raises {
		if raised != outageErr {
			t.Fatalf("item %d of a failed batch raised %v, want the batch's raise", item, raised)
		}
	}
	func() {
		failingLock.Lock()
		defer failingLock.Unlock()
		failing = false
	}()
	if result, raised := submitForTest(context.Background(), queue, 5); raised != nil || result != 10 {
		t.Fatalf("the batch after a failed one got %d and raised %v", result, raised)
	}
}

// An item whose caller ends before a batch takes it is withdrawn: the caller
// gets its context's error, and no batch runs the item.
func TestAnnounceBatchQueueWithdrawsAnUntakenItem(t *testing.T) {
	fixture := newAnnounceBatchQueueForTest(512, nil)
	release := fixture.holdTakes()
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	withdrawn := make(chan any, 1)
	go func() {
		_, raised := submitForTest(ctx, fixture.queue, 7)
		withdrawn <- raised
	}()
	fixture.awaitWaiting(t, 1)
	cancel()
	select {
	case raised := <-withdrawn:
		if raised != context.Canceled {
			t.Fatalf("the withdrawn item raised %v, want its context's error", raised)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("an untaken item's caller kept waiting after its end")
	}
	kept := make(chan int, 1)
	go func() {
		result, _ := submitForTest(context.Background(), fixture.queue, 8)
		kept <- result
	}()
	fixture.awaitWaiting(t, 1)
	release()
	if result := <-kept; result != 16 {
		t.Fatalf("the next item got %d, want 16", result)
	}
	if batches := fixture.ranBatches(); len(batches) != 1 || !slices.Equal(batches[0], []int{8}) {
		t.Fatalf("the batches ran %v, want only the item that stayed", batches)
	}
}

// An item a batch has taken is waited for when its caller ends: the caller
// returns only after the batch, with the item's result, so none of the item's
// I/O runs after its call returns.
func TestAnnounceBatchQueueWaitsForATakenItem(t *testing.T) {
	entered := make(chan struct{})
	finish := make(chan struct{})
	settings := DefaultConnectionAnnounceBatchSettings()
	queue := newAnnounceBatchQueue(
		announceBatchKindSyncRead,
		settings,
		func(ctx context.Context, items []int) []announceBatchOutcome[int] {
			close(entered)
			<-finish
			return []announceBatchOutcome[int]{{result: 2 * items[0]}}
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	type submitted struct {
		result int
		raised any
	}
	returned := make(chan submitted, 1)
	go func() {
		result, raised := submitForTest(ctx, queue, 9)
		returned <- submitted{result, raised}
	}()
	<-entered
	cancel()
	select {
	case <-returned:
		t.Fatal("the caller of a taken item returned while its batch ran")
	case <-time.After(100 * time.Millisecond):
	}
	close(finish)
	select {
	case got := <-returned:
		if got.raised != nil || got.result != 18 {
			t.Fatalf("the taken item returned %d and raised %v, want its result", got.result, got.raised)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the caller of a taken item did not return after its batch")
	}
}

// A batch takes at most MaxBatchCount items, in the order they came; the
// others wait for the next.
func TestAnnounceBatchQueueBoundsItsBatches(t *testing.T) {
	fixture := newAnnounceBatchQueueForTest(3, nil)
	release := fixture.holdTakes()
	defer release()
	var callers sync.WaitGroup
	for item := range 7 {
		// in order: each waits before the next is submitted
		callers.Go(func() {
			submitForTest(context.Background(), fixture.queue, item)
		})
		fixture.awaitWaiting(t, item+1)
	}
	release()
	callers.Wait()
	batches := fixture.ranBatches()
	want := [][]int{{0, 1, 2}, {3, 4, 5}, {6}}
	if !slices.EqualFunc(batches, want, slices.Equal[[]int]) {
		t.Fatalf("seven items ran in batches %v, want %v", batches, want)
	}
}

// With nothing waiting the queue has no driver, and the next item starts one.
func TestAnnounceBatchQueueStopsItsDriverWhenIdle(t *testing.T) {
	fixture := newAnnounceBatchQueueForTest(512, nil)
	driving := func() bool {
		fixture.queue.stateLock.Lock()
		defer fixture.queue.stateLock.Unlock()
		return fixture.queue.driving
	}
	for round := range 3 {
		if result, raised := submitForTest(context.Background(), fixture.queue, round); raised != nil || result != 2*round {
			t.Fatalf("round %d got %d and raised %v", round, result, raised)
		}
		deadline := time.Now().Add(10 * time.Second)
		for driving() {
			if time.Now().After(deadline) {
				t.Fatalf("the driver ran on with nothing waiting after round %d", round)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// submitWithoutRaise returns from a raised item and from a withdrawn one.
func TestAnnounceBatchQueueSubmitWithoutRaise(t *testing.T) {
	failing := func(item int) announceBatchOutcome[int] {
		return announceBatchOutcome[int]{raised: errors.New("test: best-effort write failed")}
	}
	newAnnounceBatchQueueForTest(512, failing).queue.submitWithoutRaise(context.Background(), 1)
	fixture := newAnnounceBatchQueueForTest(512, failing)
	release := fixture.holdTakes()
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		fixture.queue.submitWithoutRaise(ctx, 2)
	}()
	fixture.awaitWaiting(t, 1)
	cancel()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("a withdrawn best-effort item's caller kept waiting")
	}
}

// A batch runs under BatchTimeout, and a non-positive timeout leaves it
// unbounded rather than expired at once.
func TestAnnounceBatchQueueBatchTimeout(t *testing.T) {
	for _, testCase := range []struct {
		timeout     time.Duration
		wantBounded bool
	}{
		{timeout: time.Minute, wantBounded: true},
		{timeout: 0, wantBounded: false},
		{timeout: -time.Second, wantBounded: false},
	} {
		settings := DefaultConnectionAnnounceBatchSettings()
		settings.BatchTimeout = testCase.timeout
		queue := newAnnounceBatchQueue(
			announceBatchKindSyncRead,
			settings,
			func(ctx context.Context, items []int) []announceBatchOutcome[int] {
				if err := ctx.Err(); err != nil {
					panic(err)
				}
				_, bounded := ctx.Deadline()
				outcome := announceBatchOutcome[int]{}
				if bounded {
					outcome.result = 1
				}
				return []announceBatchOutcome[int]{outcome}
			},
		)
		result, raised := submitForTest(context.Background(), queue, 1)
		if raised != nil {
			t.Fatalf("timeout %s: the batch raised %v", testCase.timeout, raised)
		}
		if bounded := result == 1; bounded != testCase.wantBounded {
			t.Fatalf("timeout %s: the batch had a deadline %t, want %t", testCase.timeout, bounded, testCase.wantBounded)
		}
	}
}
