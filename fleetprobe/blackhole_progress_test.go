package fleetprobe

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"
)

type testBlackholeProgressEvents struct {
	mu     sync.Mutex
	events []BlackholeProgress
}

func (self *testBlackholeProgressEvents) observe(event BlackholeProgress) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.events = append(self.events, event)
}

func (self *testBlackholeProgressEvents) snapshot() []BlackholeProgress {
	self.mu.Lock()
	defer self.mu.Unlock()
	return append([]BlackholeProgress(nil), self.events...)
}

func testBlackholeProgressPassing(provider prober.Provider) BlackholeResult {
	return BlackholeResult{Check: ingest.BlackholeCheck{ClientId: provider.ClientId, Ok: true}}
}

func TestBlackholeProgressActualWorkerOrdering(t *testing.T) {
	events := &testBlackholeProgressEvents{}
	summary, err := RunBlackhole(context.Background(), ProvidersFromClientIds([]string{"worker.example"}), BlackholeOptions{
		Timeout: time.Second, Concurrency: 1, ObserveProgress: events.observe,
		CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
			if !reflect.DeepEqual(events.snapshot(), []BlackholeProgress{BlackholeStarted}) {
				t.Error("worker started without its start event")
			}
			return testBlackholeProgressPassing(provider)
		},
	})
	if err != nil || len(summary.Checks) != 1 || !reflect.DeepEqual(events.snapshot(), []BlackholeProgress{BlackholeStarted, BlackholeCompleted}) {
		t.Fatalf("worker event ordering: events=%v checks=%d err=%v", events.snapshot(), len(summary.Checks), err)
	}
}

func TestBlackholeProgressBufferedBeforeBatchBarrier(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		events := &testBlackholeProgressEvents{}
		release := make(chan struct{})
		done := make(chan struct{})
		var summary BlackholeSummary
		go func() {
			defer close(done)
			summary, _ = RunBlackhole(context.Background(), ProvidersFromClientIds([]string{"fast.example", "tail.example"}), BlackholeOptions{
				Timeout: time.Second, Concurrency: 2, ObserveProgress: events.observe,
				CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
					if provider.ClientId == "tail.example" {
						<-release
					}
					return testBlackholeProgressPassing(provider)
				},
			})
		}()
		synctest.Wait()
		counts := map[BlackholeProgress]int{}
		for _, event := range events.snapshot() {
			counts[event]++
		}
		if counts[BlackholeStarted] != 2 || counts[BlackholeCompleted] != 1 {
			t.Errorf("in-flight completed worker is invisible: %v", counts)
		}
		select {
		case <-done:
			t.Error("batch barrier returned early")
		default:
		}
		close(release)
		<-done
		if len(summary.Checks) != 2 {
			t.Fatal("worker instrumentation changed retained results")
		}
	})
}

func TestBlackholeProgressCancellationDiscardsStartedResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := &testBlackholeProgressEvents{}
	summary, err := RunBlackhole(ctx, ProvidersFromClientIds([]string{"cancel.example"}), BlackholeOptions{
		Timeout: time.Second, ObserveProgress: events.observe,
		CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
			cancel()
			return testBlackholeProgressPassing(provider)
		},
	})
	if err != nil || len(summary.Checks) != 0 || !reflect.DeepEqual(events.snapshot(), []BlackholeProgress{BlackholeStarted, BlackholeCanceled}) {
		t.Fatalf("canceled worker gained retained progress: events=%v checks=%d err=%v", events.snapshot(), len(summary.Checks), err)
	}
}

func TestBlackholeProgressCanceledBeforeAdmissionHasNoEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	events := &testBlackholeProgressEvents{}
	summary, err := RunBlackhole(ctx, ProvidersFromClientIds([]string{"never.example"}), BlackholeOptions{
		Timeout: time.Second, ObserveProgress: events.observe,
		CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
			t.Error("canceled work started")
			return testBlackholeProgressPassing(provider)
		},
	})
	if err != nil || len(summary.Checks) != 0 || len(events.snapshot()) != 0 {
		t.Fatal("pre-admission cancellation invented work")
	}
}

func TestBlackholeProgressAdmissionStopKeepsInitialCohort(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	events := &testBlackholeProgressEvents{}
	summary, err := RunBlackhole(context.Background(), ProvidersFromClientIds([]string{"first.example", "second.example", "queued.example"}), BlackholeOptions{
		Timeout: time.Second, Concurrency: 1, AdmissionDone: stop, MinimumAdmission: 2, ObserveProgress: events.observe,
		CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
			return testBlackholeProgressPassing(provider)
		},
	})
	want := []BlackholeProgress{BlackholeStarted, BlackholeCompleted, BlackholeStarted, BlackholeCompleted}
	if err != nil || len(summary.Checks) != 2 || !reflect.DeepEqual(events.snapshot(), want) {
		t.Fatalf("admission stop progress: events=%v checks=%d err=%v", events.snapshot(), len(summary.Checks), err)
	}
}

func TestBlackholeProgressEmptyResultIsNotBuffered(t *testing.T) {
	events := &testBlackholeProgressEvents{}
	summary, err := RunBlackhole(context.Background(), ProvidersFromClientIds([]string{"empty.example"}), BlackholeOptions{
		Timeout: time.Second, ObserveProgress: events.observe,
		CheckOne: func(context.Context, prober.Provider) BlackholeResult { return BlackholeResult{} },
	})
	if err != nil || len(summary.Checks) != 0 || !reflect.DeepEqual(events.snapshot(), []BlackholeProgress{BlackholeStarted, BlackholeDiscarded}) {
		t.Fatalf("non-retained result became completed-buffered: %v", events.snapshot())
	}
}

func TestBlackholeProgressNilObserverPreservesHealthyWork(t *testing.T) {
	summary, err := RunBlackhole(context.Background(), ProvidersFromClientIds([]string{"healthy.example"}), BlackholeOptions{
		Timeout: time.Second,
		CheckOne: func(_ context.Context, provider prober.Provider) BlackholeResult {
			return testBlackholeProgressPassing(provider)
		},
	})
	if err != nil || len(summary.Checks) != 1 || !summary.Checks[0].Ok {
		t.Fatal("optional observer changed healthy work")
	}
}
