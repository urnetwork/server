package prober

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// Tests of the scheduler: concurrency, cancellation, the success cache and its
// pruning, failure counting, and capped error logging.

// Returns a prober whose every probe succeeds, counting probes in probed and
// tracking the peak of concurrently open tunnels in maxSeen, under stateLock.
func okProber(probed *int32, stateLock *sync.Mutex, inflight *int32, maxSeen *int32) *Prober {
	return &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			cur := atomic.AddInt32(inflight, 1)
			stateLock.Lock()
			if *maxSeen < cur {
				*maxSeen = cur
			}
			stateLock.Unlock()
			time.Sleep(10 * time.Millisecond)
			return &http.Client{}, func() error { atomic.AddInt32(inflight, -1); return nil }, nil
		},
		Health: func(context.Context, *http.Client, egresshealth.Place) (*egresshealth.Result, error) {
			atomic.AddInt32(probed, 1)
			return exitResult(), nil
		},
		Submit: &stubSubmitter{},
	}
}

// A batch of providers with no place, the way the enumeration
// fallback and a place-less due list hand them over.
func providersOf(ids ...string) []Provider {
	out := make([]Provider, 0, len(ids))
	for _, id := range ids {
		out = append(out, Provider{ClientId: id})
	}
	return out
}

// No more than Concurrency tunnels are open at once.
func TestSchedulerRespectsConcurrencyCap(t *testing.T) {
	var probed, inflight, maxSeen int32
	var stateLock sync.Mutex
	s := &Scheduler{Prober: okProber(&probed, &stateLock, &inflight, &maxSeen), Concurrency: 2, CacheTtl: time.Hour}

	ids := []string{"a", "b", "c", "d", "e", "f"}
	sum := s.Run(context.Background(), providersOf(ids...))

	if sum.Attempted != len(ids) {
		t.Fatalf("attempted = %d, want %d", sum.Attempted, len(ids))
	}
	if sum.Submitted != len(ids) {
		t.Fatalf("submitted = %d, want %d", sum.Submitted, len(ids))
	}
	stateLock.Lock()
	peak := maxSeen
	stateLock.Unlock()
	if 2 < peak {
		t.Fatalf("peak concurrency = %d, want <= 2", peak)
	}
}

// A SIGTERM mid-pass cancels the
// run's context, and the spawn loop must stop there. providertunnel.Open
// constructs a full netstack before any context check, so without the stop
// every remaining provider in the batch still got a real tunnel built and
// torn down just so its probe could fail instantly on the dead context --
// a 500-provider batch reported hundreds of spurious failures, and
// single-shot mode exited 1 blaming the providers, when the truth was that
// the operator pressed Ctrl-C.
func TestSchedulerStopsSpawningWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var opens atomic.Int32
	p := &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			opens.Add(1)
			// the operator's signal lands while the first probe is in flight
			cancel()
			return nil, nil, ctx.Err()
		},
		Health: func(ctx context.Context, c *http.Client, place egresshealth.Place) (*egresshealth.Result, error) {
			return nil, ctx.Err()
		},
		Submit: &stubSubmitter{},
	}
	var logBuf bytes.Buffer
	origWriter := log.Writer()
	log.SetOutput(&logBuf)
	defer log.SetOutput(origWriter)

	s := &Scheduler{Prober: p, Concurrency: 1, CacheTtl: time.Hour}
	ids := []string{"a", "b", "c", "d", "e"}
	sum := s.Run(ctx, providersOf(ids...))

	// The in-flight probe is legitimately attempted, and one more may race
	// the cancellation through the semaphore; anything beyond that means the
	// loop is not watching the context.
	if got := opens.Load(); 2 < got {
		t.Fatalf("%d tunnels were opened after the run was cancelled, want at most 2 (the in-flight probe plus at most one race)", got)
	}
	if sum.Skipped < len(ids)-2 {
		t.Fatalf("skipped = %d, want at least %d: the unspawned remainder must be accounted as skipped, not silently dropped", sum.Skipped, len(ids)-2)
	}
	if sum.Attempted+sum.Skipped != len(ids) {
		t.Fatalf("attempted (%d) + skipped (%d) != %d: every id in the batch must be accounted for", sum.Attempted, sum.Skipped, len(ids))
	}
}

// A provider probed successfully is not probed again within CacheTtl.
func TestSchedulerCachesWithinTtl(t *testing.T) {
	var probed, inflight, maxSeen int32
	var stateLock sync.Mutex
	s := &Scheduler{Prober: okProber(&probed, &stateLock, &inflight, &maxSeen), Concurrency: 2, CacheTtl: time.Hour}

	s.Run(context.Background(), providersOf("a", "b"))
	first := atomic.LoadInt32(&probed)
	sum := s.Run(context.Background(), providersOf("a", "b"))

	if atomic.LoadInt32(&probed) != first {
		t.Fatal("a second run within the ttl must not re-probe")
	}
	if sum.Skipped != 2 {
		t.Fatalf("skipped = %d, want 2", sum.Skipped)
	}
}

// A provider is probed again once CacheTtl has passed.
func TestSchedulerReprobesAfterTtl(t *testing.T) {
	var probed, inflight, maxSeen int32
	var stateLock sync.Mutex
	now := time.Now()
	s := &Scheduler{
		Prober:      okProber(&probed, &stateLock, &inflight, &maxSeen),
		Concurrency: 2,
		CacheTtl:    time.Hour,
		Now:         func() time.Time { return now },
	}
	s.Run(context.Background(), providersOf("a"))
	now = now.Add(2 * time.Hour)
	s.Run(context.Background(), providersOf("a"))

	if atomic.LoadInt32(&probed) != 2 {
		t.Fatalf("probed = %d, want 2 (ttl expired)", probed)
	}
}

// The M2 regression test: probed must
// not grow unboundedly across the life of a long-running Scheduler. An
// entry past CacheTtl no longer affects recentlyProbed's decision either
// way, but it must still be evicted from the map so memory does not
// accumulate one entry per provider ever probed, forever, in a process
// that runs an unbounded number of passes (cmd/egress-prober's main loop).
// This drives Run with an empty id list on the second call specifically to
// isolate pruning from re-probing: nothing is attempted or skipped, so any
// change to probed's size can only be prune's doing.
func TestSchedulerPrunesProbedAfterTtl(t *testing.T) {
	var probed, inflight, maxSeen int32
	var stateLock sync.Mutex
	now := time.Now()
	s := &Scheduler{
		Prober:      okProber(&probed, &stateLock, &inflight, &maxSeen),
		Concurrency: 2,
		CacheTtl:    time.Hour,
		Now:         func() time.Time { return now },
	}
	s.Run(context.Background(), providersOf("a", "b"))

	s.stateLock.Lock()
	before := len(s.probed)
	s.stateLock.Unlock()
	if before != 2 {
		t.Fatalf("probed entries after first run = %d, want 2", before)
	}

	now = now.Add(2 * time.Hour)
	s.Run(context.Background(), nil)

	s.stateLock.Lock()
	after := len(s.probed)
	s.stateLock.Unlock()
	if after != 0 {
		t.Fatalf("probed entries after TTL elapsed = %d, want 0 (stale entries must be pruned)", after)
	}
}

// A failed probe is counted and not cached, so the next run retries it.
func TestSchedulerCountsFailuresAndDoesNotCache(t *testing.T) {
	p := &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			return nil, nil, errors.New("boom")
		},
		Health: answers(exitResult()),
		Submit: &stubSubmitter{},
	}
	s := &Scheduler{Prober: p, Concurrency: 1, CacheTtl: time.Hour}
	sum := s.Run(context.Background(), providersOf("a"))
	if sum.Failed != 1 {
		t.Fatalf("failed = %d, want 1", sum.Failed)
	}
	// a failure must not be cached; the next run retries
	sum2 := s.Run(context.Background(), providersOf("a"))
	if sum2.Attempted != 1 {
		t.Fatal("a failed probe must be retried on the next run")
	}
}

// A probe whose run measured
// nothing -- its tunnel died and could not be re-created -- is a failure of
// the pass, and is counted apart from the providers' own failures so a pass
// full of lost tunnels reads as what it is.
func TestSchedulerCountsRunsThatMeasuredNothing(t *testing.T) {
	unmeasured := &egresshealth.Result{
		Checks:      []egresshealth.CheckResult{{Name: "google", Class: egresshealth.ClassSite, NotMeasured: true}},
		NotMeasured: 1,
		ByClass:     map[egresshealth.Class]egresshealth.ClassSummary{},
	}
	p := &Prober{
		Open: okOpen,
		Health: func(ctx context.Context, c *http.Client, place egresshealth.Place) (*egresshealth.Result, error) {
			return unmeasured, nil
		},
		Submit: &stubSubmitter{},
	}
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	sum := (&Scheduler{Prober: p, Concurrency: 2}).Run(context.Background(), providersOf("a", "b"))
	if sum.Attempted != 2 || sum.Failed != 2 || sum.NotMeasured != 2 || sum.Submitted != 0 {
		t.Fatalf("summary = %+v, want 2 attempted, 2 failed, both not measured", sum)
	}
}

// The I2 regression test: each
// provider here fails with a distinct error message (so a naive
// once-per-message dedupe against a single global error would not
// coalesce them), and there are more of them than
// MaxLoggedDistinctErrors. Run must log detail for only the first
// MaxLoggedDistinctErrors distinct messages, plus exactly one suppression
// notice once the cap is hit -- not flood the log with all of them, and
// not silently drop all detail either. sum.Failed must still count every
// failure regardless of how many were logged in detail.
func TestSchedulerLogsCappedDistinctErrors(t *testing.T) {
	var buf bytes.Buffer
	orig := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(orig)

	p := &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			return nil, nil, fmt.Errorf("boom-%s", id) // distinct per provider
		},
		Health: answers(exitResult()),
		Submit: &stubSubmitter{},
	}

	maxLoggedDistinctErrors := p.maxLoggedDistinctErrors()
	n := maxLoggedDistinctErrors + 5
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("p%d", i)
	}

	s := &Scheduler{Prober: p, Concurrency: 4, CacheTtl: time.Hour}
	sum := s.Run(context.Background(), providersOf(ids...))

	if sum.Failed != n {
		t.Fatalf("failed = %d, want %d", sum.Failed, n)
	}

	logged := buf.String()
	detailLines := strings.Count(logged, "probe failed provider=")
	if detailLines != maxLoggedDistinctErrors {
		t.Fatalf("detail lines logged = %d, want %d (the cap)", detailLines, maxLoggedDistinctErrors)
	}
	if strings.Count(logged, "suppressing further per-error detail") != 1 {
		t.Fatal("want exactly one suppression notice once the distinct-error cap was hit")
	}
}

// The scheduler's recentlyProbed only becomes true once
// a probe completes, so a due batch containing the same provider twice used
// to open two tunnels to it at the same moment and pay the contract cost
// twice. The enumeration path de-duplicates before Run sees it; the due list
// is whatever the server sent.
func TestSchedulerProbesADuplicateIdOnce(t *testing.T) {
	var opens atomic.Int32
	p := &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			opens.Add(1)
			time.Sleep(10 * time.Millisecond)
			return &http.Client{}, func() error { return nil }, nil
		},
		Health: answers(exitResult()),
		Submit: &stubSubmitter{},
	}
	s := &Scheduler{Prober: p, Concurrency: 4, CacheTtl: time.Hour}
	sum := s.Run(context.Background(), providersOf("a", "a", "a"))

	if got := opens.Load(); got != 1 {
		t.Fatalf("opened %d tunnels for one repeated id, want 1", got)
	}
	if sum.Attempted != 1 || sum.Skipped != 2 {
		t.Fatalf("attempted = %d skipped = %d, want 1 and 2", sum.Attempted, sum.Skipped)
	}
}
