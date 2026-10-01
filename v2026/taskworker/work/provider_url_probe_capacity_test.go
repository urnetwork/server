// Synthetic stage delays exercise the real task owner without external targets.
package work

import (
	"container/heap"
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

type simulatedUrlProvider struct {
	id              string
	index           int
	next            time.Time
	started         time.Time
	attempts        int
	outcomes        int
	acceptedAttempt int
	successes       []time.Time
	jitter          float64
	turns           int
	errors          int
	localFailures   int
	waited          time.Duration
	maxWait         time.Duration
	running         bool
}

type simulatedUrlHeap []*simulatedUrlProvider

func (self simulatedUrlHeap) Len() int { return len(self) }
func (self simulatedUrlHeap) Less(i, j int) bool {
	if self[i].next.Equal(self[j].next) {
		return self[i].id < self[j].id
	}
	return self[i].next.Before(self[j].next)
}
func (self simulatedUrlHeap) Swap(i, j int) {
	self[i], self[j] = self[j], self[i]
	self[i].index = i
	self[j].index = j
}
func (self *simulatedUrlHeap) Push(value any) {
	provider := value.(*simulatedUrlProvider)
	provider.index = len(*self)
	*self = append(*self, provider)
}
func (self *simulatedUrlHeap) Pop() any {
	old := *self
	provider := old[len(old)-1]
	*self = old[:len(old)-1]
	provider.index = -1
	return provider
}

type simulatedUrlFleet struct {
	*recordingEgressProbeIngest
	mu            sync.Mutex
	queue         simulatedUrlHeap
	providers     map[string]*simulatedUrlProvider
	token         time.Time
	active        int
	peak          int
	attempted     int
	successes     int
	errors        int
	localFailures int
	occupied      time.Duration
}

func newSimulatedUrlFleet(count int) *simulatedUrlFleet {
	fleet := &simulatedUrlFleet{recordingEgressProbeIngest: newRecordingEgressProbeIngest(), providers: map[string]*simulatedUrlProvider{}, token: time.Now()}
	for index := range count {
		provider := &simulatedUrlProvider{id: fmt.Sprintf("synthetic-%04d", index), next: fleet.token,
			jitter: 0.9 + float64((index*997)%2001)/10000, attempts: index % 20}
		fleet.providers[provider.id] = provider
		heap.Push(&fleet.queue, provider)
	}
	return fleet
}

func (self *simulatedUrlFleet) due(_ context.Context, limit int) ([]ingest.DueProvider, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	var due []ingest.DueProvider
	for len(due) < limit && self.queue.Len() > 0 && !self.queue[0].next.After(time.Now()) {
		provider := self.queue[0]
		waited := time.Since(provider.next)
		provider.waited += waited
		provider.maxWait = max(provider.maxWait, waited)
		provider.next = time.Now().Add(15 * time.Minute)
		provider.started = time.Now()
		provider.attempts++
		provider.turns++
		provider.running = true
		heap.Fix(&self.queue, provider.index)
		self.active++
		self.peak = max(self.peak, self.active)
		self.attempted++
		due = append(due, ingest.DueProvider{ClientId: provider.id, CycleStartedAt: self.token, OutcomeCount: provider.outcomes})
	}
	return due, nil
}

func simulatedUrlDelay(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

func (self *simulatedUrlFleet) SubmitEgressHealth(ctx context.Context, id string, result *egresshealth.Result) error {
	if err := simulatedUrlDelay(ctx, 100*time.Millisecond); err != nil {
		return err
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	provider := self.providers[id]
	if provider.acceptedAttempt == provider.attempts {
		return nil
	}
	provider.acceptedAttempt = provider.attempts
	provider.outcomes++
	now := time.Now()
	for len(provider.successes) > 0 && !provider.successes[0].After(now.Add(-4*time.Hour)) {
		provider.successes = provider.successes[1:]
	}
	interval := time.Minute
	if result.OkCount == 1 {
		self.successes++
		provider.successes = append(provider.successes, now)
		interval = 20 * time.Minute
	} else {
		self.errors++
		provider.errors++
	}
	provider.next = now.Add(time.Duration(float64(interval) * provider.jitter))
	if len(provider.successes) >= 10 {
		provider.next = provider.successes[len(provider.successes)-10].Add(4 * time.Hour)
	}
	heap.Fix(&self.queue, provider.index)
	return nil
}

func (self *simulatedUrlFleet) ReportAttempt(ctx context.Context, id, failure string) error {
	if err := simulatedUrlDelay(ctx, 100*time.Millisecond); err != nil {
		return err
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	provider := self.providers[id]
	self.active--
	provider.running = false
	self.occupied += time.Since(provider.started)
	if failure != "" {
		self.localFailures++
		provider.localFailures++
		provider.next = time.Now().Add(time.Duration(float64(time.Minute) * provider.jitter))
		heap.Fix(&self.queue, provider.index)
	}
	return nil
}

func (self *simulatedUrlFleet) run(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
	provider := providers[0]
	self.mu.Lock()
	outcome := self.providers[provider.ClientId].attempts % 20
	self.mu.Unlock()
	// Every provider experiences the same repeating mix, with different phases:
	// 75% success, 20% measured DNS/URL failure, 5% local setup failure.
	setup := 2 * time.Second
	if outcome == 19 {
		setup = 60 * time.Second
	}
	if err := simulatedUrlDelay(ctx, setup); err != nil {
		return prober.Summary{}, err
	}
	if outcome == 19 {
		if err := options.Attempts.ReportAttempt(ctx, provider.ClientId, prober.FailureTunnel); err != nil {
			return prober.Summary{}, err
		}
		return prober.Summary{Attempted: 1, Failed: 1}, nil
	}
	urlDelay, ok := time.Second, 1
	if outcome >= 15 {
		urlDelay = 30 * time.Second
		ok = 0
	}
	if err := simulatedUrlDelay(ctx, urlDelay); err != nil {
		return prober.Summary{}, err
	}
	result := testHealthRun(1, ok)
	result.CycleStartedAt = provider.CycleStartedAt
	if err := options.HealthResults.SubmitEgressHealth(ctx, provider.ClientId, result); err != nil {
		return prober.Summary{}, err
	}
	if err := options.Attempts.ReportAttempt(ctx, provider.ClientId, ""); err != nil {
		return prober.Summary{}, err
	}
	return prober.Summary{Attempted: 1, Submitted: 1}, nil
}

// The same task owner runs many durable passes. Only injected stage latency is
// simulated: this is a scheduler/capacity regression, not a live-network claim.
func TestUrlProbeFourHourSyntheticCapacity(t *testing.T) {
	for _, concurrency := range []int{2, 8, 16, 32} {
		t.Run(fmt.Sprintf("workers_%d", concurrency), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fleet := newSimulatedUrlFleet(512)
				pass, args, _ := testUrlProbePass()
				args.UrlProbe.Limit, args.UrlProbe.Concurrency = concurrency, concurrency
				pass.fullDue = fleet.due
				pass.runFull = fleet.run
				pass.fullSink = testFullBatchSink(fleet)
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan struct{})
				go func() {
					defer close(done)
					for ctx.Err() == nil {
						passCtx, stop := context.WithTimeout(ctx, 15*time.Minute)
						_, err := pass.run(passCtx, args)
						stop()
						if err != nil && ctx.Err() == nil {
							t.Errorf("synthetic URL task failed: %v", err)
							return
						}
						if simulatedUrlDelay(ctx, 5*time.Second) != nil {
							return
						}
					}
				}()
				time.Sleep(4 * time.Hour)
				fleet.mu.Lock()
				complete := 0
				deficits := map[int]int{}
				var waited, maxWait time.Duration
				for _, provider := range fleet.providers {
					recent := 0
					for _, at := range provider.successes {
						if at.After(time.Now().Add(-4 * time.Hour)) {
							recent++
						}
					}
					if recent >= 10 {
						complete++
					} else {
						deficits[10-recent]++
						if concurrency >= 8 {
							first, last := time.Duration(0), time.Duration(0)
							if len(provider.successes) > 0 {
								first = provider.successes[0].Sub(fleet.token)
								last = provider.successes[len(provider.successes)-1].Sub(fleet.token)
							}
							t.Logf("incomplete %s: successes%d turns%d errors%d local_failure%d jitter%.4f queue_wait%s max_wait%s first_success%s last_success%s next_due%s in_flight%t",
								provider.id, recent, provider.turns, provider.errors, provider.localFailures, provider.jitter, provider.waited, provider.maxWait,
								first, last, provider.next.Sub(fleet.token), provider.running)
						}
					}
					waited += provider.waited
					maxWait = max(maxWait, provider.maxWait)
				}
				attempted, successes, errors, localFailures, peak, occupied := fleet.attempted, fleet.successes, fleet.errors, fleet.localFailures, fleet.peak, fleet.occupied
				fleet.mu.Unlock()
				cancel()
				<-done
				t.Logf("4h synthetic512providers concurrency%d: complete%d attempted%d success%d measured_error%d local_failure%d peak%d mean_occupancy%s worker_utilization%.4f mean_queue_wait%s max_queue_wait%s deficits%v", concurrency,
					complete, attempted, successes, errors, localFailures, peak, occupied/time.Duration(successes+errors+localFailures),
					float64(occupied)/float64(4*time.Hour)/float64(concurrency), waited/time.Duration(attempted), maxWait, deficits)
				if peak > concurrency || localFailures == 0 || errors == 0 {
					t.Fatal("synthetic run lost bounded workers or negative-path controls")
				}
				if concurrency == 2 && complete >= 256 {
					t.Fatalf("underprovisioned negative control claimed implausible capacity: complete%d", complete)
				}
				if concurrency == 8 && complete < 500 {
					t.Fatalf("paced scheduler failed the supplied capacity envelope: complete%d", complete)
				}
				if concurrency >= 16 && complete != len(fleet.providers) {
					t.Fatalf("headroom control left reachable eligible providers below rolling quota: complete%d want%d", complete, len(fleet.providers))
				}
			})
		})
	}
}
