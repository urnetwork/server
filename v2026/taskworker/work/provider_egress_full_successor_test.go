package work

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

// The real pass and blackhole worker own scheduling, guarding, publication and
// joining. Only the network full-run and bounded due-source boundaries are fake.
// No wall-clock lower bound, production identity, endpoint or extra goroutine
// label is needed to expose the full lane waiting behind the blackhole tail.
type testFullSuccessorOwner struct {
	t                 *testing.T
	args              *ProviderEgressProbeArgs
	ctx               context.Context
	cancel            context.CancelFunc
	pass              *providerEgressProbePass
	inner             *recordingEgressProbeIngest
	blackholeRelease  chan struct{}
	done              chan struct{}
	fullCalls         atomic.Int32
	dueCalls          atomic.Int32
	blackholeCalls    atomic.Int32
	blackholeDueCalls atomic.Int32
	result            *ProviderEgressProbeResult
	err               error
}

func newTestFullSuccessorOwner(t *testing.T, lease time.Duration) *testFullSuccessorOwner {
	t.Helper()
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	args.Full.Limit, args.Full.Concurrency = 1, 1
	args.Blackhole.Limit, args.Blackhole.Concurrency = 2, 2
	args.DarkBatchGuardMinChecks = 2
	ctx, cancel := context.WithTimeout(context.Background(), lease)
	h := &testFullSuccessorOwner{
		t: t, args: args, ctx: ctx, cancel: cancel,
		inner:            newRecordingEgressProbeIngest(),
		blackholeRelease: make(chan struct{}), done: make(chan struct{}),
	}
	h.pass = &providerEgressProbePass{
		blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) {
			if h.blackholeDueCalls.Add(1) != 1 {
				return nil, nil
			}
			return testDueProviders("synthetic-blackhole-a", "synthetic-blackhole-b"), nil
		},
		fullDue: func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			if limit != args.Full.Limit {
				t.Errorf("due query broadened: limit=%d want=%d", limit, args.Full.Limit)
			}
			switch h.dueCalls.Add(1) {
			case 1:
				return testDueProviders("synthetic-full-a"), nil
			case 2:
				return testDueProviders("synthetic-full-b"), nil
			case 3:
				return nil, nil
			default:
				return nil, errors.New("synthetic repeated empty due query")
			}
		},
		loadPins: func(context.Context) (map[string][]string, error) { return nil, nil },
		fullSink: testFullBatchSink(h.inner),
		blackholeOptions: fleetprobe.BlackholeOptions{
			CheckOne: func(checkCtx context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
				h.blackholeCalls.Add(1)
				select {
				case <-h.blackholeRelease:
				case <-checkCtx.Done():
				}
				return testBlackholeAdmissionPass(provider)
			},
		},
		runBlackhole:          fleetprobe.RunBlackhole,
		submitBlackholeChecks: func(context.Context, []ingest.BlackholeCheck) error { return nil },
	}
	h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
		h.fullCalls.Add(1)
		return testFullSuccessorMeasure(ctx, providers, options, true)
	}
	return h
}

func testFullSuccessorMeasure(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions, healthy bool) (prober.Summary, error) {
	for _, provider := range providers {
		ok := 9
		if !healthy {
			ok = 5
		}
		run := testHealthRun(10, ok)
		run.ExitIp = "203.0.113.7"
		if err := options.HealthResults.SubmitEgressHealth(ctx, provider.ClientId, run); err != nil {
			return prober.Summary{}, err
		}
		if err := options.Submit.Submit(ctx, provider.ClientId, run.ExitIp, time.Now()); err != nil {
			return prober.Summary{}, err
		}
		if err := options.Attempts.ReportAttempt(ctx, provider.ClientId, ""); err != nil {
			return prober.Summary{}, err
		}
	}
	return prober.Summary{Attempted: len(providers), Submitted: len(providers)}, nil
}

func (h *testFullSuccessorOwner) start() {
	go func() {
		h.result, h.err = h.pass.run(h.ctx, h.args)
		close(h.done)
	}()
}

func (h *testFullSuccessorOwner) finish() {
	close(h.blackholeRelease)
	<-h.done
	h.cancel()
	synctest.Wait()
}

func TestFullSuccessorAdvancesBeforeBlackholeDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.start()
		synctest.Wait()
		if h.fullCalls.Load() != 2 || h.dueCalls.Load() != 3 {
			t.Errorf("full successor stayed behind blackhole drain: runs=%d due_reads=%d", h.fullCalls.Load(), h.dueCalls.Load())
		}
		select {
		case <-h.done:
			t.Error("active blackhole owner was not joined")
		default:
		}
		h.finish()
		if h.err != nil || h.result.FullDue != 2 || h.result.Attempted != 2 || h.result.Submitted != 2 || h.result.Checked != 2 {
			t.Errorf("independent lane result not accumulated: result=%+v err=%v", h.result, h.err)
		}
	})
}

func TestFullSuccessorPartialBatchesDoNotInventSaturation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.args.Full.Limit, h.args.Full.Concurrency = 2, 2
		h.args.Blackhole.Limit, h.args.Blackhole.Concurrency = 3, 3
		h.start()
		synctest.Wait()
		h.finish()
		if h.err != nil || h.result.FullDue != 2 || h.result.Submitted != 2 {
			t.Errorf("partial full successors did not complete: %+v err=%v", h.result, h.err)
		}
		if h.result.Full {
			t.Error("cumulative partial full batches fabricated saturated-backlog scheduling")
		}
	})
}

func TestFullSuccessorDeduplicatesBoundedDueRows(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.args.Full.Limit, h.args.Full.Concurrency = 3, 3
		h.args.Blackhole.Concurrency, h.args.Blackhole.Limit = 4, 4
		h.pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			if limit != 3 {
				t.Error("dedup broadened the due query")
			}
			switch h.dueCalls.Add(1) {
			case 1:
				return testDueProviders("synthetic-full-a"), nil
			case 2:
				return testDueProviders("synthetic-full-a", "synthetic-full-b", "synthetic-full-b"), nil
			case 3:
				return testDueProviders("synthetic-full-a", "synthetic-full-b"), nil
			default:
				return nil, errors.New("synthetic duplicate-only busy retry")
			}
		}
		h.start()
		synctest.Wait()
		if h.fullCalls.Load() != 2 || h.dueCalls.Load() != 3 {
			t.Errorf("unique successor missing or due loop unbounded: runs=%d reads=%d", h.fullCalls.Load(), h.dueCalls.Load())
		}
		h.finish()
		if h.err != nil || h.result.Attempted != 2 || h.result.FullDue != 2 || len(h.inner.health) != 2 {
			t.Errorf("duplicate full admission: result=%+v err=%v health=%d", h.result, h.err, len(h.inner.health))
		}
	})
}

// A retained/renewedly-due prefix was already measured by this task; it must
// not hide unseen providers immediately behind it for the remaining lease.
func TestFullSuccessorAdvancesPastRetainedDuePrefix(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		before := egressProbeFullProgress.snapshot().selections
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.args.Full.Limit, h.args.Full.Concurrency = 2, 2
		h.args.Blackhole.Limit, h.args.Blackhole.Concurrency = 3, 3
		all := testDueProviders("synthetic-prefix-a", "synthetic-prefix-b", "synthetic-prefix-c", "synthetic-prefix-d", "synthetic-prefix-e")
		maxLookup := 0
		h.pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			h.dueCalls.Add(1)
			maxLookup = max(maxLookup, limit)
			return slices.Clone(all[:min(len(all), limit)]), nil
		}
		admitted := map[string]int{}
		h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			h.fullCalls.Add(1)
			if len(providers) > h.args.Full.Limit {
				t.Error("lookahead widened probe admission/guard cohort")
			}
			for _, provider := range providers {
				admitted[provider.ClientId]++
			}
			return testFullSuccessorMeasure(ctx, providers, options, true)
		}
		h.start()
		synctest.Wait()
		h.finish()
		if h.err != nil || h.result.Attempted != 5 || h.result.Submitted != 5 || len(admitted) != 5 {
			t.Fatalf("seen prefix stranded unseen due: result=%+v error=%v admitted=%d", h.result, h.err, len(admitted))
		}
		for _, attempts := range admitted {
			if attempts != 1 {
				t.Error("lookahead re-probed already seen provider")
			}
		}
		if maxLookup > len(all)+h.args.Full.Limit || h.dueCalls.Load() > 7 {
			t.Fatalf("unbounded prefix scan: largest=%d queries=%d", maxLookup, h.dueCalls.Load())
		}
		want := [3]uint64{before[0] + 3, before[1] + 2, before[2] + 1}
		if got := egressProbeFullProgress.snapshot().selections; got != want {
			t.Errorf("prefix selection diagnostic=%v want=%v", got, want)
		}
	})
}

func TestFullSuccessorPrefixLookupFailureRemainsVisible(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		want := errors.New("synthetic prefix lookup failure")
		h.pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			h.dueCalls.Add(1)
			if limit > 1 {
				return nil, want
			}
			return testDueProviders("synthetic-full-a"), nil
		}
		h.start()
		synctest.Wait()
		h.finish()
		if !errors.Is(h.err, want) || h.result.Submitted != 1 || h.fullCalls.Load() != 1 || h.dueCalls.Load() != 3 {
			t.Fatalf("prefix source failure hidden: result=%+v error=%v queries=%d", h.result, h.err, h.dueCalls.Load())
		}
	})
}

func TestFullSuccessorPrefixLookaheadRejectsOversizedReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			h.dueCalls.Add(1)
			if limit == 1 {
				return testDueProviders("synthetic-full-a"), nil
			}
			providers := testDueProviders("synthetic-full-a")
			for index := range limit {
				providers = append(providers, ingest.DueProvider{ClientId: fmt.Sprintf("synthetic-extra-%d", index)})
			}
			return providers, nil
		}
		h.start()
		synctest.Wait()
		h.finish()
		if h.err == nil || h.result.Submitted != 1 || h.fullCalls.Load() != 1 || h.dueCalls.Load() != 3 {
			t.Fatalf("oversized prefix response admitted: result=%+v error=%v queries=%d", h.result, h.err, h.dueCalls.Load())
		}
	})
}

// A clamped/duplicate-only source is not authority to scan the whole fleet or
// retry the same providers. One failed lookahead ends this owner normally.
func TestFullSuccessorPrefixLookaheadStopsAfterOneBoundedReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			if h.dueCalls.Add(1) > 3 || limit > 2 {
				return nil, errors.New("synthetic unbounded prefix lookup")
			}
			return testDueProviders("synthetic-full-a"), nil
		}
		h.start()
		synctest.Wait()
		h.finish()
		if h.err != nil || h.result.Submitted != 1 || h.fullCalls.Load() != 1 || h.dueCalls.Load() != 3 {
			t.Fatalf("bounded-prefix control: result=%+v error=%v queries=%d", h.result, h.err, h.dueCalls.Load())
		}
	})
}

func TestFullSuccessorPrefixLookaheadRechecksCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.pass.fullDue = func(_ context.Context, limit int) ([]ingest.DueProvider, error) {
			h.dueCalls.Add(1)
			if limit == 1 {
				return testDueProviders("synthetic-full-a"), nil
			}
			h.cancel()
			return testDueProviders("synthetic-full-a", "synthetic-full-b"), nil
		}
		h.start()
		synctest.Wait()
		h.finish()
		if !errors.Is(h.err, context.Canceled) || h.result.Attempted != 1 || h.fullCalls.Load() != 1 || h.dueCalls.Load() != 3 {
			t.Fatalf("late-prefix reply admitted: result=%+v error=%v queries=%d", h.result, h.err, h.dueCalls.Load())
		}
	})
}

func TestFullSuccessorJoinsAdmittedFullAfterBlackholeEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		releaseFull := make(chan struct{})
		h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			if h.fullCalls.Add(1) == 2 {
				<-releaseFull
				if ctx.Err() != nil {
					t.Error("blackhole completion canceled admitted full work")
				}
			}
			return testFullSuccessorMeasure(ctx, providers, options, true)
		}
		h.start()
		synctest.Wait()
		if h.fullCalls.Load() != 2 {
			t.Error("second full owner was not admitted")
		}
		close(h.blackholeRelease)
		synctest.Wait()
		select {
		case <-h.done:
			t.Error("pass returned before admitted full owner joined")
		default:
		}
		close(releaseFull)
		<-h.done
		h.cancel()
		synctest.Wait()
		if h.err != nil || h.result.Attempted != 2 {
			t.Errorf("joined full result: %+v err=%v", h.result, h.err)
		}
	})
}

func TestFullSuccessorFirstErrorKeepsInitialBlackholeCohort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		want := errors.New("synthetic first full failure")
		h.pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			h.fullCalls.Add(1)
			return prober.Summary{}, want
		}
		h.start()
		synctest.Wait()
		h.finish()
		if !errors.Is(h.err, want) || h.result.Checked != 2 || h.fullCalls.Load() != 1 || h.dueCalls.Load() != 1 {
			t.Errorf("first failure suppressed independent work or retried: %+v err=%v runs=%d reads=%d", h.result, h.err, h.fullCalls.Load(), h.dueCalls.Load())
		}
	})
}

func TestFullSuccessorSecondErrorRetainsFirstResult(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		want := errors.New("synthetic second full failure")
		h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			if h.fullCalls.Add(1) == 2 {
				return prober.Summary{}, want
			}
			return testFullSuccessorMeasure(ctx, providers, options, true)
		}
		h.start()
		synctest.Wait()
		h.finish()
		if !errors.Is(h.err, want) || h.result.Attempted != 1 || h.result.Submitted != 1 || h.result.FullDue != 2 || h.dueCalls.Load() != 2 {
			t.Errorf("successor error lost or first result overwritten: %+v err=%v reads=%d", h.result, h.err, h.dueCalls.Load())
		}
	})
}

func TestFullSuccessorDueErrorIsNotEmptyHealthy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		want := errors.New("synthetic due lookup failure")
		h.pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			if h.dueCalls.Add(1) == 1 {
				return testDueProviders("synthetic-full-a"), nil
			}
			return nil, want
		}
		h.start()
		synctest.Wait()
		h.finish()
		if !errors.Is(h.err, want) || h.result.Submitted != 1 || h.fullCalls.Load() != 1 {
			t.Errorf("due error collapsed into healthy exhaustion: %+v err=%v", h.result, h.err)
		}
	})
}

func TestFullSuccessorCancellationJoinsOwnerCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		releaseCleanup := make(chan struct{})
		h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			if h.fullCalls.Add(1) == 2 {
				<-ctx.Done()
				<-releaseCleanup
				return prober.Summary{}, ctx.Err()
			}
			return testFullSuccessorMeasure(ctx, providers, options, true)
		}
		h.start()
		synctest.Wait()
		if h.fullCalls.Load() != 2 {
			t.Error("cancel control has no successor owner")
		}
		h.cancel()
		synctest.Wait()
		select {
		case <-h.done:
			t.Error("canceled successor cleanup was not joined")
		default:
		}
		close(releaseCleanup)
		h.finish()
		if !errors.Is(h.err, context.Canceled) || h.dueCalls.Load() != 2 {
			t.Errorf("cancellation identity or no-retry boundary lost: err=%v reads=%d", h.err, h.dueCalls.Load())
		}
	})
}

func TestFullSuccessorInsufficientLeaseKeepsFirstBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, time.Minute)
		h.start()
		synctest.Wait()
		h.finish()
		if h.err != nil || h.result.Submitted != 1 || h.fullCalls.Load() != 1 || h.dueCalls.Load() != 1 {
			t.Errorf("lease reserve changed the original first batch: %+v err=%v", h.result, h.err)
		}
	})
}

func TestFullSuccessorRechecksLeaseAfterDueLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		releaseDue := make(chan struct{})
		h.pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			if h.dueCalls.Add(1) == 1 {
				return testDueProviders("synthetic-full-a"), nil
			}
			<-releaseDue
			return testDueProviders("synthetic-full-b"), nil
		}
		h.start()
		synctest.Wait()
		if h.dueCalls.Load() != 2 {
			t.Error("successor due lookup was not reached")
		}
		time.Sleep(74 * time.Minute) // synctest virtual time, never a wall-clock sleep
		close(releaseDue)
		synctest.Wait()
		h.finish()
		if h.err != nil || h.fullCalls.Load() != 1 {
			t.Errorf("late due result admitted without publication reserve: runs=%d err=%v", h.fullCalls.Load(), h.err)
		}
	})
}

func TestFullSuccessorBlackholeEndsDuringDueLookup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		releaseDue := make(chan struct{})
		h.pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			if h.dueCalls.Add(1) == 1 {
				return testDueProviders("synthetic-full-a"), nil
			}
			<-releaseDue
			return testDueProviders("synthetic-full-b"), nil
		}
		h.start()
		synctest.Wait()
		if h.dueCalls.Load() != 2 {
			t.Error("successor due lookup was not reached")
		}
		close(h.blackholeRelease)
		synctest.Wait()
		close(releaseDue)
		<-h.done
		h.cancel()
		synctest.Wait()
		if h.err != nil || h.fullCalls.Load() != 1 || h.result.FullDue != 1 {
			t.Errorf("blackhole drain raced extra full network admission: %+v err=%v", h.result, h.err)
		}
	})
}

func TestFullSuccessorGuardRemainsPerBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.args.Full.Limit, h.args.Full.Concurrency = 3, 3
		h.args.Blackhole.Limit, h.args.Blackhole.Concurrency = 4, 4
		h.pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) {
			switch h.dueCalls.Add(1) {
			case 1:
				return testDueProviders("synthetic-bad-a", "synthetic-bad-b", "synthetic-bad-c"), nil
			case 2:
				return testDueProviders("synthetic-good-a", "synthetic-good-b", "synthetic-good-c"), nil
			default:
				return nil, nil
			}
		}
		h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			return testFullSuccessorMeasure(ctx, providers, options, h.fullCalls.Add(1) == 2)
		}
		h.start()
		synctest.Wait()
		h.finish()
		if h.err != nil || !h.result.FullGuardTripped || h.result.Attempted != 6 || h.result.Failed != 3 || h.result.Submitted != 3 {
			t.Errorf("guard scope/results crossed batch boundary: %+v err=%v", h.result, h.err)
		}
		for _, id := range []string{"synthetic-bad-a", "synthetic-bad-b", "synthetic-bad-c"} {
			if h.inner.health[id] != nil {
				t.Error("guarded ordinary negative was submitted")
			}
		}
		if len(h.inner.health) != 3 {
			t.Errorf("independent good successor was not retained: count=%d", len(h.inner.health))
		}
	})
}

func TestFullSuccessorNoDeadlineCannotInventLease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.cancel()
		h.ctx, h.cancel = context.WithCancel(context.Background())
		h.start()
		synctest.Wait()
		h.finish()
		if h.err != nil || h.fullCalls.Load() != 1 || h.dueCalls.Load() != 1 {
			t.Errorf("missing task lease allowed unbounded successors: runs=%d reads=%d err=%v", h.fullCalls.Load(), h.dueCalls.Load(), h.err)
		}
	})
}

func TestFullSuccessorSerialPathStaysOneBatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.args.Blackhole.Concurrency = 1
		h.start()
		synctest.Wait()
		h.finish()
		if h.err != nil || h.fullCalls.Load() != 1 || h.dueCalls.Load() != 1 {
			t.Errorf("serial pass unexpectedly repeated full work: runs=%d reads=%d err=%v", h.fullCalls.Load(), h.dueCalls.Load(), h.err)
		}
	})
}

// A sink sees the release context at the actual reporter boundary. Recording
// stays inside the one serialized full owner; tests inspect only after join.
type testFullSuccessorDeadlineSink struct {
	*recordingEgressProbeIngest
	observe func(context.Context, string)
}

func (s *testFullSuccessorDeadlineSink) SubmitEgressHealth(ctx context.Context, id string, run *egresshealth.Result) error {
	s.observe(ctx, id)
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.recordingEgressProbeIngest.SubmitEgressHealth(ctx, id, run)
}

func (s *testFullSuccessorDeadlineSink) Submit(ctx context.Context, id string, exitIp string, measuredAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.recordingEgressProbeIngest.Submit(ctx, id, exitIp, measuredAt)
}

func (s *testFullSuccessorDeadlineSink) ReportAttempt(ctx context.Context, id string, failure string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.recordingEgressProbeIngest.ReportAttempt(ctx, id, failure)
}

func TestFullSuccessorPublicationReserveIsNotRunOnlyBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.cancel()
		// One wave plus3x30s sequential full API requests and30s blackhole
		// publication leaves no slack. The source must not reserve just run time.
		h.ctx, h.cancel = context.WithTimeout(context.Background(), providerEgressFullRunBudget(h.args)+120*time.Second)
		h.start()
		synctest.Wait()
		h.finish()
		if h.err != nil || h.fullCalls.Load() != 1 || h.dueCalls.Load() != 1 {
			t.Errorf("publication reserve was not retained: runs=%d reads=%d err=%v", h.fullCalls.Load(), h.dueCalls.Load(), h.err)
		}
	})
}

func TestFullSuccessorReleaseDeadlineExpiryIsObservableError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		h.pass.fullSink = testFullBatchSink(&testFullSuccessorDeadlineSink{
			recordingEgressProbeIngest: h.inner,
			observe: func(ctx context.Context, id string) {
				if id != "synthetic-full-b" {
					return
				}
				if _, bounded := ctx.Deadline(); !bounded {
					t.Error("successor finalization has no deadline")
					return
				}
				<-ctx.Done()
			},
		})
		h.start()
		synctest.Wait()
		h.finish()
		if !errors.Is(h.err, context.DeadlineExceeded) || h.result.Submitted != 1 || h.result.Failed != 1 || h.inner.health["synthetic-full-b"] != nil {
			t.Errorf("expired publication reported healthy or duplicated evidence: result=%+v err=%v", h.result, h.err)
		}
	})
}

func TestFullSuccessorNoCircularAdmissionAfterBlackholeEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		releaseFull := make(chan struct{})
		h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			h.fullCalls.Add(1)
			<-releaseFull
			return testFullSuccessorMeasure(ctx, providers, options, true)
		}
		h.start()
		synctest.Wait()
		close(h.blackholeRelease)
		synctest.Wait()
		close(releaseFull)
		<-h.done
		h.cancel()
		synctest.Wait()
		if h.err != nil || h.fullCalls.Load() != 1 || h.dueCalls.Load() != 1 || h.blackholeDueCalls.Load() > 2 {
			t.Errorf("finished siblings restarted each other: result=%+v err=%v full_reads=%d blackhole_reads=%d", h.result, h.err, h.dueCalls.Load(), h.blackholeDueCalls.Load())
		}
	})
}

func TestFullSuccessorReleaseRetainsDeadlineButDetachesCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newTestFullSuccessorOwner(t, 75*time.Minute)
		deadline, _ := h.ctx.Deadline()
		var observed []string
		h.pass.fullSink = testFullBatchSink(&testFullSuccessorDeadlineSink{
			recordingEgressProbeIngest: h.inner,
			observe: func(ctx context.Context, id string) {
				got, bounded := ctx.Deadline()
				if id == "synthetic-full-a" && bounded {
					t.Error("original first-batch release policy changed")
				}
				if id == "synthetic-full-b" {
					if !bounded || !got.Equal(deadline) {
						t.Error("successor release lost the task deadline")
					}
					if ctx.Err() != nil {
						t.Error("completed successor evidence inherited task cancellation")
					}
				}
				observed = append(observed, id)
			},
		})
		h.pass.runFull = func(ctx context.Context, providers []prober.Provider, options fleetprobe.FullOptions) (prober.Summary, error) {
			call := h.fullCalls.Add(1)
			summary, err := testFullSuccessorMeasure(ctx, providers, options, true)
			if call == 2 {
				h.cancel()
			}
			return summary, err
		}
		h.start()
		synctest.Wait()
		h.finish()
		if !slices.Equal(observed, []string{"synthetic-full-a", "synthetic-full-b"}) || h.result.Submitted != 2 {
			t.Errorf("completed evidence lost/duplicated before reserve expired: observed=%v result=%+v", observed, h.result)
		}
	})
}
