// Real fleet workers and pass guards, driven by synthetic providers and barriers.
package work

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"
)

// One test task; channels hold checks, never real network operations. Published
// IDs disappear from the synthetic due source exactly as a successful API ACK.
type testBlackholePipeline struct {
	args                                               *ProviderEgressProbeArgs
	ctx                                                context.Context
	cancel                                             context.CancelFunc
	pass                                               *providerEgressProbePass
	done                                               chan struct{}
	firstRelease, firstTail, laterRelease, fullRelease chan struct{}
	firstHeld                                          int
	due                                                []ingest.DueProvider
	check                                              func(int, prober.Provider) fleetprobe.BlackholeResult
	lookup                                             func(int, int) ([]ingest.DueProvider, error)
	submitError                                        error
	stateLock                                          sync.Mutex
	started                                            map[string]int
	published                                          map[string]bool
	submissions                                        [][]ingest.BlackholeCheck
	dueReads, maxLookup                                int
	active, peak                                       atomic.Int32
	result                                             *ProviderEgressProbeResult
	err                                                error
}

func newTestBlackholePipeline(t *testing.T, args *ProviderEgressProbeArgs, cohorts int) *testBlackholePipeline {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	h := &testBlackholePipeline{
		args: args, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		firstRelease: make(chan struct{}), firstTail: make(chan struct{}),
		laterRelease: make(chan struct{}), fullRelease: make(chan struct{}),
		firstHeld: 1, started: map[string]int{}, published: map[string]bool{},
	}
	for index := range cohorts * args.Blackhole.Limit {
		h.due = append(h.due, ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-cohort-%04d", index)})
	}
	fullReads := 0
	h.pass = &providerEgressProbePass{
		blackholeDue: func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			h.stateLock.Lock()
			h.dueReads++
			call := h.dueReads
			h.maxLookup = max(h.maxLookup, limit)
			if lookup := h.lookup; lookup != nil {
				// Injected lookups may wait on a test barrier. Never hold the
				// accounting lock across that independently controlled wait.
				h.stateLock.Unlock()
				return lookup(call, limit)
			}
			selected := make([]ingest.DueProvider, 0, limit)
			for _, provider := range h.due {
				if !h.published[provider.ClientId] {
					selected = append(selected, provider)
					if len(selected) == limit {
						break
					}
				}
			}
			h.stateLock.Unlock()
			return selected, nil
		},
		fullDue: func(context.Context, int) ([]ingest.DueProvider, error) {
			fullReads++
			if fullReads == 1 {
				return testDueProviders("synthetic-full-owner"), nil
			}
			return nil, nil
		},
		loadPins: func(context.Context) (map[string][]string, error) { return nil, nil },
		runFull: func(ctx context.Context, _ []prober.Provider, _ fleetprobe.FullOptions) (prober.Summary, error) {
			select {
			case <-h.fullRelease:
				return prober.Summary{Attempted: 1, Submitted: 1}, nil
			case <-ctx.Done():
				return prober.Summary{}, ctx.Err()
			}
		},
		blackholeOptions: fleetprobe.BlackholeOptions{CheckOne: func(ctx context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
			index := 0
			if _, err := fmt.Sscanf(provider.ClientId, "synthetic-cohort-%04d", &index); err != nil {
				t.Error("invalid synthetic key")
			}
			h.stateLock.Lock()
			h.started[provider.ClientId]++
			h.stateLock.Unlock()
			active := h.active.Add(1)
			for previous := h.peak.Load(); previous < active && !h.peak.CompareAndSwap(previous, active); previous = h.peak.Load() {
			}
			defer h.active.Add(-1)
			wait := func(ch <-chan struct{}) {
				select {
				case <-ch:
				case <-ctx.Done():
				}
			}
			if index < args.Blackhole.Limit {
				wait(h.firstRelease)
				if args.Blackhole.Limit-h.firstHeld <= index {
					wait(h.firstTail)
				}
			} else {
				wait(h.laterRelease)
			}
			if h.check != nil {
				return h.check(index, provider)
			}
			return testBlackholeAdmissionPass(provider)
		}},
		runBlackhole: fleetprobe.RunBlackhole,
		submitBlackholeChecks: func(ctx context.Context, checks []ingest.BlackholeCheck) error {
			if ctx.Err() != nil {
				t.Error("completed cohort used canceled submission context")
			}
			h.stateLock.Lock()
			defer h.stateLock.Unlock()
			h.submissions = append(h.submissions, append([]ingest.BlackholeCheck(nil), checks...))
			if h.submitError != nil && strings.HasSuffix(checks[0].ClientId, "0000") {
				return h.submitError
			}
			for _, check := range checks {
				h.published[check.ClientId] = true
			}
			return nil
		},
	}
	return h
}

func (h *testBlackholePipeline) start() {
	go func() { h.result, h.err = h.pass.run(h.ctx, h.args); close(h.done) }()
}

func testBlackholePipelineRelease(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (h *testBlackholePipeline) finish() {
	testBlackholePipelineRelease(h.fullRelease)
	testBlackholePipelineRelease(h.firstRelease)
	testBlackholePipelineRelease(h.firstTail)
	testBlackholePipelineRelease(h.laterRelease)
	<-h.done
	h.cancel()
}

func (h *testBlackholePipeline) startedCount(first, end int) int {
	h.stateLock.Lock()
	defer h.stateLock.Unlock()
	count := 0
	for index := first; index < end; index++ {
		count += h.started[h.due[index].ClientId]
	}
	return count
}

func (h *testBlackholePipeline) submittedCohort(cohort int) []ingest.BlackholeCheck {
	h.stateLock.Lock()
	defer h.stateLock.Unlock()
	for _, checks := range h.submissions {
		if 0 < len(checks) && checks[0].ClientId == h.due[cohort*h.args.Blackhole.Limit].ClientId {
			return append([]ingest.BlackholeCheck(nil), checks...)
		}
	}
	return nil
}

func TestProviderEgressBlackholePipelineReusesTailSlotsBeforeFirstAck(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		h.start()
		defer h.finish()
		synctest.Wait()
		if got := h.startedCount(0, 250); got != 250 {
			t.Errorf("initial oldest cohort started=%d", got)
		}
		close(h.firstRelease)
		synctest.Wait()
		if got := h.startedCount(250, 500); got != 249 {
			t.Errorf("straggler pinned free slots: successor started=%d, want249", got)
		}
		if h.peak.Load() != 250 {
			t.Errorf("active worker peak=%d, want250", h.peak.Load())
		}
		close(h.laterRelease)
		synctest.Wait()
		if len(h.submittedCohort(1)) != 250 || len(h.submittedCohort(0)) != 0 {
			t.Error("completed successor cohort waited for unrelated first-cohort tail/ACK")
		}
		if h.maxLookup > 750 {
			t.Errorf("unbounded due lookahead=%d", h.maxLookup)
		}
	})
}

func TestProviderEgressBlackholePipelineKeepsOriginalGuardCohorts(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		h.check = func(index int, p prober.Provider) fleetprobe.BlackholeResult {
			if index < 51 || 250 <= index && index < 290 {
				return testBlackholeAdmissionDark(p)
			}
			if index == 249 {
				return testBlackholeAdmissionTls(p)
			}
			return testBlackholeAdmissionPass(p)
		}
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		close(h.laterRelease)
		synctest.Wait()
		second := h.submittedCohort(1)
		if len(second) != 250 {
			t.Error("healthy successor's independent guard/ACK did not run before first tail")
		}
		for _, check := range second {
			if check.NotMeasured {
				t.Error("first cohort's guard contaminated second cohort")
			}
		}
		h.finish()
		first := h.submittedCohort(0)
		held, tls := 0, 0
		for _, check := range first {
			if check.NotMeasured {
				held++
			}
			if check.Failure == egresshealth.FailureTlsAuthentication {
				tls++
			}
		}
		if len(first) != 250 || held != 51 || tls != 1 || h.err != nil {
			t.Errorf("first original guard lost: checked=%d held=%d tls=%d err=%v", len(first), held, tls, h.err)
		}
	})
}

func TestProviderEgressBlackholePipelinePartialStopRetainsMinimumGuard(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	args.Blackhole.Limit, args.Blackhole.Concurrency = 10, 10
	args.Full.Concurrency = 1
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		h.firstHeld = 7
		h.check = func(index int, p prober.Provider) fleetprobe.BlackholeResult {
			if index == 10 {
				return testBlackholeAdmissionPass(p)
			}
			if index == 11 {
				return testBlackholeAdmissionTls(p)
			}
			if 12 <= index {
				return testBlackholeAdmissionDark(p)
			}
			return testBlackholeAdmissionPass(p)
		}
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		if h.startedCount(10, 20) != 3 {
			t.Error("three available slots did not admit the successor prefix")
		}
		close(h.fullRelease)
		synctest.Wait()
		close(h.laterRelease)
		synctest.Wait()
		checks := h.submittedCohort(1)
		held, pass, tls := 0, 0, 0
		for _, check := range checks {
			if check.NotMeasured {
				held++
			}
			if check.Ok {
				pass++
			}
			if check.Failure == egresshealth.FailureTlsAuthentication {
				tls++
			}
		}
		if len(checks) != 3 || held != 1 || pass != 1 || tls != 1 || h.startedCount(10, 20) != 3 {
			t.Errorf("partial successor bypassed guard or admitted after full finish: checks=%d pass/tls/held=%d/%d/%d", len(checks), pass, tls, held)
		}
		h.finish()
	})
}

func TestProviderEgressBlackholePipelineCancellationJoinsSafeEvidence(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		h.cancel()
		h.finish()
		if !errors.Is(h.err, context.Canceled) || h.active.Load() != 0 {
			t.Errorf("canceled owners not joined: active=%d err=%v", h.active.Load(), h.err)
		}
		if len(h.submittedCohort(0)) != 249 || len(h.submittedCohort(1)) != 0 {
			t.Error("cancellation lost completed passes or manufactured post-cancel evidence")
		}
		if h.peak.Load() > 250 {
			t.Error("independent cohorts exceeded owning250 pool")
		}
	})
}

func TestProviderEgressBlackholePipelineSubmitErrorStopsPendingNotStarted(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 3)
		h.submitError = errors.New("synthetic cohort submission unavailable")
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		if h.startedCount(250, 500) != 249 {
			t.Error("submit-error control never admitted speculative successor")
		}
		close(h.firstTail)
		synctest.Wait()
		// The released slot may start one more check before the submission
		// error becomes observable. Only admission after that boundary is wrong.
		admittedBeforeFailure := h.startedCount(250, 500)
		close(h.laterRelease)
		synctest.Wait()
		h.finish()
		if !errors.Is(h.err, h.submitError) {
			t.Errorf("first submission failure lost: %v", h.err)
		}
		if got := h.startedCount(250, 500); got != admittedBeforeFailure || h.startedCount(500, 750) != 0 {
			t.Errorf("submission failure allowed queued successor or lost started work: %d", got)
		}
		if len(h.submittedCohort(1)) != admittedBeforeFailure {
			t.Error("already-started independent passing cohort was not finalized")
		}
	})
}

func TestProviderEgressBlackholePipelineRepeatedDueStopsWithoutDuplicate(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 1)
		h.lookup = func(call, limit int) ([]ingest.DueProvider, error) {
			if call > 2 {
				return nil, errors.New("synthetic no-progress hot-loop stop")
			}
			return append([]ingest.DueProvider(nil), h.due...), nil
		}
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		close(h.firstTail)
		synctest.Wait()
		h.finish()
		if h.dueReads != 2 || h.startedCount(0, 250) != 250 || h.err != nil {
			t.Errorf("stale due/ACK retried the same cohort: reads=%d started=%d err=%v", h.dueReads, h.startedCount(0, 250), h.err)
		}
	})
}

func TestProviderEgressBlackholePipelineFastCohortContinues(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		// Exercise the completion branch without relying on a progress wake
		// winning the race against an immediately completed runner.
		h.pass.runBlackhole = func(ctx context.Context, ps []prober.Provider, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			options.ObserveProgress = nil
			return fleetprobe.RunBlackhole(ctx, ps, options)
		}
		close(h.firstRelease)
		close(h.firstTail)
		close(h.laterRelease)
		h.start()
		synctest.Wait()
		if h.startedCount(0, 500) != 500 || len(h.submittedCohort(1)) != 250 {
			t.Error("fast saturated first completion skipped successor lookup")
		}
		h.finish()
		if h.err != nil {
			t.Errorf("fast cohorts: %v", h.err)
		}
	})
}

func TestProviderEgressBlackholePipelineFullStopsDuringLookup(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		entered, release := make(chan struct{}), make(chan struct{})
		h.lookup = func(call, limit int) ([]ingest.DueProvider, error) {
			if call == 1 {
				return h.due[:250], nil
			}
			close(entered)
			<-release
			return h.due[:limit], nil
		}
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Error("lookahead did not overlap the first tail")
		}
		close(h.fullRelease)
		synctest.Wait()
		close(release)
		synctest.Wait()
		if h.startedCount(250, 500) != 0 {
			t.Error("lookup crossed full-finished admission edge")
		}
		h.finish()
		if h.err != nil || len(h.submittedCohort(1)) != 0 {
			t.Errorf("closed lookup manufactured an ACK/error: %v", h.err)
		}
	})
}

func TestProviderEgressBlackholePipelineFullStopsAtWorkerBoundary(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		fullFinished := make(chan struct{})
		reached := false
		h.pass.runBlackhole = func(ctx context.Context, ps []prober.Provider, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			if ps[0].ClientId == h.due[250].ClientId {
				reached = true
				if options.AdmissionDone != fullFinished {
					t.Error("worker full-finish check depends on asynchronous forwarding")
				}
				close(fullFinished)
			}
			return fleetprobe.RunBlackhole(ctx, ps, options)
		}
		go func() {
			out := h.pass.drainBlackhole(h.ctx, args, nil, nil, 250, h.due[:250], fullFinished)
			h.err = out.err
			close(h.done)
		}()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		if !reached || h.startedCount(250, 500) != 0 {
			t.Error("post-lookup full close admitted a successor worker")
		}
		h.finish()
		if h.err != nil || len(h.submittedCohort(1)) != 0 {
			t.Errorf("zero-admission stop became healthy ACK or task failure: %v", h.err)
		}
	})
}

func TestProviderEgressBlackholePipelineLookupErrorJoinsActiveChecks(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		readErr := errors.New("synthetic lookahead unavailable")
		h.lookup = func(call, _ int) ([]ingest.DueProvider, error) {
			if call == 1 {
				return h.due[:250], nil
			}
			return nil, readErr
		}
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		select {
		case <-h.done:
			t.Error("lookup error failed to join active first tail")
		default:
		}
		h.finish()
		if !errors.Is(h.err, readErr) || h.startedCount(250, 500) != 0 || len(h.submittedCohort(0)) != 250 {
			t.Errorf("lookup error lost completed evidence or admitted new work: %v", h.err)
		}
	})
}

func TestProviderEgressBlackholePipelineRejectsOversizeLookahead(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 3)
		h.lookup = func(call, limit int) ([]ingest.DueProvider, error) {
			if call == 1 {
				return h.due[:250], nil
			}
			return h.due[:limit+1], nil
		}
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		h.finish()
		if h.err == nil || !strings.Contains(h.err.Error(), "exceeds requested lookahead") || h.startedCount(250, 750) != 0 {
			t.Errorf("oversized response accepted: %v", h.err)
		}
	})
}

func TestProviderEgressBlackholePipelineDeduplicatesExpandedResponse(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		h.lookup = func(call, _ int) ([]ingest.DueProvider, error) {
			if call == 1 {
				return h.due[:250], nil
			}
			rows := append([]ingest.DueProvider(nil), h.due[:375]...)
			return append(rows, h.due[250:375]...), nil
		}
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		close(h.laterRelease)
		synctest.Wait()
		if len(h.submittedCohort(1)) != 125 || h.startedCount(250, 375) != 125 || h.startedCount(375, 500) != 0 {
			t.Error("repeated lookahead IDs inflated guard or work")
		}
		h.finish()
		if h.err != nil || h.dueReads != 2 {
			t.Errorf("partial unique tail did not stop boundedly: reads=%d err=%v", h.dueReads, h.err)
		}
	})
}

func TestProviderEgressBlackholePipelineCutoffDoesNotDelayReadyWork(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		h.start()
		synctest.Wait()
		// Virtual time crosses only the successor admission reserve, not the
		// live check's75m task context. No sleep controls scheduling assertions.
		time.Sleep(43 * time.Minute)
		close(h.firstRelease)
		close(h.firstTail)
		synctest.Wait()
		if h.startedCount(250, 500) != 0 || len(h.submittedCohort(0)) != 250 {
			t.Error("cutoff canceled existing evidence or admitted an unfitting successor")
		}
		h.finish()
		if h.err != nil {
			t.Errorf("admission-only cutoff canceled the task: %v", h.err)
		}
	})
}

func TestProviderEgressBlackholePipelineBoundsSelectedBookkeeping(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 12)
		close(h.firstRelease)
		close(h.firstTail)
		close(h.laterRelease)
		h.start()
		synctest.Wait()
		h.finish()
		if h.startedCount(0, len(h.due)) != 2000 || h.peak.Load() > 250 || h.result == nil || !h.result.Full || h.err != nil {
			t.Errorf("per-task selected work not bounded/rearmed: started=%d peak=%d result=%+v err=%v", h.startedCount(0, len(h.due)), h.peak.Load(), h.result, h.err)
		}
	})
}

func TestProviderEgressBlackholePipelineOlderAdmissionPrecedesNextCohort(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	args.Blackhole.Limit, args.Blackhole.Concurrency, args.Full.Concurrency = 10, 10, 1
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 3)
		h.firstHeld = 7
		releaseRest := make(chan struct{})
		h.pass.runBlackhole = func(ctx context.Context, ps []prober.Provider, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			if ps[0].ClientId != h.due[10].ClientId {
				return fleetprobe.RunBlackhole(ctx, ps, options)
			}
			prefix, err := fleetprobe.RunBlackhole(ctx, ps[:3], options)
			if err != nil {
				return prefix, err
			}
			<-releaseRest
			rest, err := fleetprobe.RunBlackhole(ctx, ps[3:], options)
			prefix.Checks = append(prefix.Checks, rest.Checks...)
			return prefix, err
		}
		close(h.laterRelease)
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		close(h.firstTail)
		synctest.Wait()
		if h.startedCount(10, 20) != 3 || h.startedCount(20, 30) != 0 || h.dueReads != 2 {
			t.Error("new cohort overtook older undispatched checks")
		}
		close(releaseRest)
		synctest.Wait()
		if h.startedCount(0, 30) != 30 || h.peak.Load() > 10 {
			t.Error("ordered cohorts failed to reuse the fixed worker pool")
		}
		h.finish()
	})
}

func TestProviderEgressBlackholePipelineMetricKeepsFixedSelectedContract(t *testing.T) {
	descriptions := make(chan *prometheus.Desc, 1)
	egressProbePassDue.Describe(descriptions)
	description := (<-descriptions).String()
	if !strings.Contains(description, "selected from the most recent due response") ||
		!strings.Contains(description, "excludes locally selected IDs") ||
		!strings.Contains(description, "variableLabels: {schedule}") {
		t.Fatalf("selected-cohort gauge lost fixed-label/source meaning: %s", description)
	}
	for _, forbidden := range []string{"provider_id", "client_id", "task_id", "cohort_id", "token", "url"} {
		if strings.Contains(description, forbidden) {
			t.Errorf("private/high-cardinality label in gauge: %s", forbidden)
		}
	}
}

func TestProviderEgressBlackholePipelinePinnedOldestPrefixDoesNotHideBacklog(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 4)
		// Model a due source which still returns acknowledged NotMeasured or
		// equal-time rows. Admission must not mistake the selected prefix for
		// exhaustion of the unselected rows behind it.
		h.lookup = func(_ int, limit int) ([]ingest.DueProvider, error) {
			return h.due[:min(limit, len(h.due))], nil
		}
		close(h.firstRelease)
		close(h.firstTail)
		close(h.laterRelease)
		h.start()
		synctest.Wait()
		h.finish()
		if h.startedCount(0, len(h.due)) != 1000 || h.err != nil {
			t.Errorf("selected oldest prefix hid unseen backlog: started=%d err=%v", h.startedCount(0, len(h.due)), h.err)
		}
		if h.maxLookup > 1250 || h.dueReads > 5 || h.peak.Load() > 250 {
			t.Errorf("prefix escape exceeded per-invocation bounds: lookup=%d reads=%d peak=%d", h.maxLookup, h.dueReads, h.peak.Load())
		}
	})
}

func TestProviderEgressBlackholePipelineSubmissionFailureDuringLookupStopsWorkers(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	args.Blackhole.Limit, args.Blackhole.Concurrency, args.Full.Concurrency = 10, 10, 1
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 3)
		entered, release := make(chan struct{}), make(chan struct{})
		h.lookup = func(call, limit int) ([]ingest.DueProvider, error) {
			if call == 1 {
				return h.due[:10], nil
			}
			if call == 2 {
				return h.due[:20], nil
			}
			if call == 3 {
				close(entered)
				<-release
				return h.due[10:30], nil
			}
			return nil, nil
		}
		submitErr := errors.New("synthetic second cohort publication failure")
		var admissionDone <-chan struct{}
		h.pass.runBlackhole = func(ctx context.Context, ps []prober.Provider, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			if ps[0].ClientId == h.due[10].ClientId {
				admissionDone = options.AdditionalAdmissionDone
			}
			return fleetprobe.RunBlackhole(ctx, ps, options)
		}
		submit := h.pass.submitBlackholeChecks
		h.pass.submitBlackholeChecks = func(ctx context.Context, checks []ingest.BlackholeCheck) error {
			if checks[0].ClientId == h.due[10].ClientId {
				return submitErr
			}
			return submit(ctx, checks)
		}
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		close(h.firstTail)
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Error("third lookup did not overlap second active cohort")
		}
		close(h.laterRelease)
		synctest.Wait()
		select {
		case <-admissionDone:
		default:
			t.Error("publication failure left successor admission open during blocked lookup")
		}
		close(release)
		synctest.Wait()
		if h.startedCount(20, 30) != 0 || len(h.submittedCohort(2)) != 0 {
			t.Error("known publication failure admitted a fresh cohort after blocked lookup")
		}
		h.finish()
		if !errors.Is(h.err, submitErr) || len(h.submittedCohort(0)) != 10 {
			t.Errorf("publication failure or independent first ACK lost: %v", h.err)
		}
	})
}
