// A transient due-read timeout must not strand this shard's cheap lane while
// its independent full lane owns a long-running task. No database or network.
package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

func TestBlackholeDueInitialTimeoutDoesNotLeaveFullOnlyShard(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
		var reads atomic.Int32
		cheapStarted, fullStarted, releaseFull, done := make(chan struct{}, 1), make(chan struct{}), make(chan struct{}), make(chan struct{})
		pass := &providerEgressProbePass{
			blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) {
				if reads.Add(1) == 1 {
					return nil, fmt.Errorf("synthetic due transport: %w", context.DeadlineExceeded)
				}
				return testDueProviders("synthetic-cheap"), nil
			},
			fullDue: func(context.Context, int) ([]ingest.DueProvider, error) {
				return testDueProviders("synthetic-full"), nil
			},
			loadPins:              func(context.Context) (map[string][]string, error) { return nil, nil },
			submitBlackholeChecks: func(context.Context, []ingest.BlackholeCheck) error { return nil },
			runBlackhole: func(context.Context, []prober.Provider, fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
				cheapStarted <- struct{}{}
				return fleetprobe.BlackholeSummary{Checks: []ingest.BlackholeCheck{{ClientId: "synthetic-cheap", Ok: true, CheckedAt: time.Now()}}}, nil
			},
			runFull: func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
				close(fullStarted)
				<-releaseFull
				return prober.Summary{Attempted: 1}, nil
			},
		}
		var result *ProviderEgressProbeResult
		var runErr error
		go func() {
			defer close(done)
			result, runErr = pass.run(t.Context(), args)
		}()
		<-fullStarted
		synctest.Wait()
		select {
		case <-cheapStarted:
		default:
			t.Error("transient initial due timeout left all cheap capacity idle behind held Full work")
		}
		close(releaseFull)
		<-done
		if runErr != nil || reads.Load() != 2 || result.Checked != 1 {
			t.Errorf("recovered due read did not advance this same shard: reads=%d checked=%d error=%v", reads.Load(), result.Checked, runErr)
		}
	})
}

// Each attempt owns one existing operator deadline. No detached lookup may
// overlap its successor; an unavailable service gets at most three reads.
func TestBlackholeDueTimeoutRetriesHaveFiniteJoinedBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		reads, active, peak := 0, 0, 0
		before := testutil.ToFloat64(egressProbeBlackholeDueReads.WithLabelValues("timeout"))
		pass := &providerEgressProbePass{blackholeDue: func(ctx context.Context, limit int) ([]ingest.DueProvider, error) {
			reads++
			active++
			peak = max(peak, active)
			defer func() { active-- }()
			deadline, bounded := ctx.Deadline()
			if !bounded || time.Until(deadline) != providerEgressControlPlaneTimeout || limit != 250 {
				t.Error("due read lost its instance deadline or selected-row limit")
			}
			<-ctx.Done()
			return nil, &url.Error{Op: "Get", URL: "https://synthetic.example/due", Err: ctx.Err()}
		}}
		due, err := pass.blackholeDueWithRetry(t.Context(), 250, nil)
		elapsed := time.Since(started)
		if len(due) != 0 || !errors.Is(err, context.DeadlineExceeded) || reads != 3 || active != 0 || peak != 1 {
			t.Errorf("unbounded or unjoined retries: reads=%d active=%d peak=%d error=%v", reads, active, peak, err)
		}
		if elapsed < 90*time.Second+500*time.Millisecond || 91*time.Second <= elapsed {
			t.Errorf("three read deadlines plus bounded jitter took %s", elapsed)
		}
		if got := testutil.ToFloat64(egressProbeBlackholeDueReads.WithLabelValues("timeout")) - before; got != 3 {
			t.Errorf("timeout metric counted %g attempts, want3", got)
		}
	})
}

// Endpoint and decode failures are not transport transients, even if a joined
// error happens to carry a timeout. Preserve the exact error for the task owner.
func TestBlackholeDuePermanentFailuresNeverRetry(t *testing.T) {
	testCases := []struct {
		name string
		err  error
		want providerEgressBlackholeDueReadResult
	}{
		{name: "auth", err: errors.Join(ingest.ErrUnauthorized, context.DeadlineExceeded), want: blackholeDueReadUnauthorized},
		{name: "unsupported", err: ingest.ErrBlackholeUnsupported, want: blackholeDueReadUnsupported},
		{name: "rejected", err: errors.Join(ingest.ErrRejected, context.DeadlineExceeded), want: blackholeDueReadRejected},
		{name: "syntax", err: &json.SyntaxError{Offset: 3}, want: blackholeDueReadDecode},
		{name: "type", err: &json.UnmarshalTypeError{Value: "synthetic"}, want: blackholeDueReadDecode},
		{name: "empty", err: io.EOF, want: blackholeDueReadDecode},
		{name: "truncated", err: errors.Join(io.ErrUnexpectedEOF, context.DeadlineExceeded), want: blackholeDueReadDecode},
		{name: "canceled", err: fmt.Errorf("synthetic cancellation: %w", context.Canceled), want: blackholeDueReadCanceled},
		{name: "other", err: errors.New("synthetic rejected connection"), want: blackholeDueReadOther},
	}
	for _, testCase := range testCases {
		reads := 0
		before := testutil.ToFloat64(egressProbeBlackholeDueReads.WithLabelValues(testCase.want.label()))
		pass := &providerEgressProbePass{blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) {
			reads++
			return nil, testCase.err
		}}
		_, err := pass.blackholeDueWithRetry(t.Context(), 250, nil)
		if reads != 1 || err != testCase.err || providerEgressBlackholeDueReadClass(t.Context(), err) != testCase.want {
			t.Errorf("%s was retried, relabeled or discarded: reads=%d error=%v", testCase.name, reads, err)
		}
		if got := testutil.ToFloat64(egressProbeBlackholeDueReads.WithLabelValues(testCase.want.label())) - before; got != 1 {
			t.Errorf("%s metric counted %g attempts, want1", testCase.name, got)
		}
	}
}

// A shorter owning deadline ends both the in-flight read and retry loop. It is
// cancellation of this admission owner, not a retryable transport timeout.
func TestBlackholeDueOwnerDeadlineStopsInFlightRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		reads := 0
		started := time.Now()
		pass := &providerEgressProbePass{blackholeDue: func(ctx context.Context, _ int) ([]ingest.DueProvider, error) {
			reads++
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		_, err := pass.blackholeDueWithRetry(ctx, 250, nil)
		if !errors.Is(err, context.DeadlineExceeded) || reads != 1 || time.Since(started) != 3*time.Second {
			t.Errorf("owner deadline permitted another read: reads=%d error=%v", reads, err)
		}
		if providerEgressBlackholeDueReadClass(ctx, err) != blackholeDueReadCanceled {
			t.Error("owning deadline was classified as a retryable timeout")
		}
	})
}

// Explicit barriers park the loop in its jitter wait, then cancel its owner.
// No timer or admission race may cause a second request after cancellation.
func TestBlackholeDueCancellationDuringRetryWaitStartsNoRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		reads := 0
		pass := &providerEgressProbePass{blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) {
			reads++
			return nil, context.DeadlineExceeded
		}}
		done := make(chan struct{})
		var err error
		go func() { _, err = pass.blackholeDueWithRetry(ctx, 250, nil); close(done) }()
		synctest.Wait()
		cancel()
		<-done
		if reads != 1 || !errors.Is(err, context.Canceled) {
			t.Errorf("canceled retry wait started another read: reads=%d error=%v", reads, err)
		}
	})
}

// Healthy full completion ends serial successor admission without turning a
// transient lookup failure into failure of already-published cheap work.
func TestBlackholeDueAdmissionStopDuringRetryWaitStartsNoRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stop, done := make(chan struct{}), make(chan struct{})
		reads := 0
		pass := &providerEgressProbePass{blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) {
			reads++
			return nil, context.DeadlineExceeded
		}}
		var err error
		go func() { _, err = pass.blackholeDueWithRetry(t.Context(), 250, stop); close(done) }()
		synctest.Wait()
		close(stop)
		<-done
		if reads != 1 || err != nil {
			t.Errorf("ended admission started another read: reads=%d error=%v", reads, err)
		}
	})
}

// A stop that precedes admission emits no read event at all. Empty successful
// responses, by contrast, are real successful reads and must not be retried.
func TestBlackholeDueClosedAdmissionAndEmptyResponseStayDistinct(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	reads := 0
	before := testutil.ToFloat64(egressProbeBlackholeDueReads.WithLabelValues("ok"))
	pass := &providerEgressProbePass{blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) {
		reads++
		return nil, nil
	}}
	if _, err := pass.blackholeDueWithRetry(t.Context(), 250, stop); err != nil || reads != 0 {
		t.Errorf("closed admission issued a read: reads=%d error=%v", reads, err)
	}
	if _, err := pass.blackholeDueWithRetry(t.Context(), 250, nil); err != nil || reads != 1 {
		t.Errorf("empty successful response was retried: reads=%d error=%v", reads, err)
	}
	if got := testutil.ToFloat64(egressProbeBlackholeDueReads.WithLabelValues("ok")) - before; got != 1 {
		t.Errorf("synthetic stopped read counted as an empty success: successes=%g", got)
	}
}

// Scrapes distinguish every known terminal read class from a missing metric.
// Even an invalid internal enum collapses to one fixed unknown child.
func TestBlackholeDueReadMetricHasFixedPreinitializedDomain(t *testing.T) {
	metric := newProviderEgressBlackholeDueReadMetrics()
	metric.WithLabelValues(providerEgressBlackholeDueReadResult(255).label()).Inc()
	registry := prometheus.NewRegistry()
	registry.MustRegister(metric)
	families, err := registry.Gather()
	if err != nil || len(families) != 1 {
		t.Fatalf("fixed metric gather: families=%d error=%v", len(families), err)
	}
	want := map[string]bool{"ok": true, "timeout": true, "canceled": true, "unauthorized": true, "unsupported": true, "rejected": true, "unavailable": true, "decode": true, "error_or_unknown": true}
	if got := len(families[0].Metric); got != len(want) {
		t.Fatalf("due-read metric cardinality=%d want%d", got, len(want))
	}
	for _, metric := range families[0].Metric {
		if len(metric.Label) != 1 || metric.Label[0].GetName() != "result" || !want[metric.Label[0].GetValue()] {
			t.Fatal("due-read metric contains a nonfixed class or identity label")
		}
		result := metric.Label[0].GetValue()
		value := metric.GetCounter().GetValue()
		if result == "error_or_unknown" && value != 1 || result != "error_or_unknown" && value != 0 {
			t.Errorf("preseed/unknown mismatch for %s: %g", result, value)
		}
	}
}

// A retried lookahead may return the whole still-due prefix. The existing
// cohort owner, not the request attempt, owns deduplication and the guard.
func TestBlackholeDuePipelineRetryPreservesSeenPrefixAndWorkerBound(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		recovered := make(chan struct{})
		h.lookup = func(call, limit int) ([]ingest.DueProvider, error) {
			switch call {
			case 1:
				return h.due[:250], nil
			case 2:
				return nil, context.DeadlineExceeded
			case 3:
				close(recovered)
				return h.due[:min(limit, len(h.due))], nil
			default:
				return nil, nil
			}
		}
		h.start()
		defer h.finish()
		<-recovered
		close(h.firstRelease)
		synctest.Wait()
		if h.startedCount(0, 250) != 250 || h.startedCount(250, 500) != 249 || h.peak.Load() != 250 {
			t.Error("due retry duplicated the retained prefix or expanded worker concurrency")
		}
		h.finish()
		if h.err != nil || h.result.Checked != 500 || h.dueReads != 4 {
			t.Errorf("recovered successor lost a cohort: reads=%d checked=%d error=%v", h.dueReads, h.result.Checked, h.err)
		}
		for _, count := range h.started {
			if count != 1 {
				t.Error("one provider was executed more than once after due-read retry")
			}
		}
	})
}

// A retryable in-flight read is bounded by admission, not by the task's much
// later measurement/publication deadline. Expiry must join the started checks
// with their original context and must not turn them into unknown results.
func TestBlackholeDuePipelineAdmissionDeadlineKeepsStartedChecks(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		h.cancel()
		reserve := providerEgressBlackholeCheckBudget(args) + providerEgressBlackholePublicationBudget(args.Blackhole.Limit)
		h.ctx, h.cancel = context.WithTimeout(t.Context(), reserve+2*time.Second)
		reads := 0
		entered, returned := make(chan struct{}), make(chan struct{})
		h.pass.blackholeDue = func(ctx context.Context, _ int) ([]ingest.DueProvider, error) {
			reads++
			if reads == 1 {
				return h.due[:250], nil
			}
			close(entered)
			<-ctx.Done()
			close(returned)
			return nil, ctx.Err()
		}
		before := testutil.ToFloat64(egressProbeBlackholePipelineDecisions.WithLabelValues("cutoff"))
		h.start()
		defer h.finish()
		<-entered
		<-returned
		synctest.Wait()
		if h.ctx.Err() != nil || h.active.Load() != 250 || h.startedCount(250, 500) != 0 {
			t.Error("lookup admission deadline canceled started checks or admitted a successor")
		}
		h.finish()
		if reads != 2 || h.err != nil || h.result.Checked != 250 || h.result.BlackholeNotMeasured != 0 {
			t.Errorf("normal cutoff became failed measurement: reads=%d checked=%d not_measured=%d error=%v", reads, h.result.Checked, h.result.BlackholeNotMeasured, h.err)
		}
		if got := testutil.ToFloat64(egressProbeBlackholePipelineDecisions.WithLabelValues("cutoff")) - before; got != 1 {
			t.Errorf("cutoff diagnostic counted %g stops, want1", got)
		}
	})
}

// The compatibility serial drain has its own due-read callsite. A transport
// timeout there uses the same finite policy, without re-executing the first
// cohort or keeping admission alive after the full sibling has finished.
func TestBlackholeDueSerialSuccessorRetriesOnlySelection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		args := testProviderEgressParallelArgs(t)
		args.Blackhole.Limit = 1
		args.Blackhole.Concurrency = 1
		fullFinished := make(chan struct{})
		reads := 0
		started := map[string]int{}
		pass := &providerEgressProbePass{
			blackholeDue: func(context.Context, int) ([]ingest.DueProvider, error) {
				reads++
				switch reads {
				case 1:
					return nil, &net.DNSError{Name: "synthetic.example", IsTimeout: true}
				case 2:
					return testDueProviders("synthetic-next"), nil
				default:
					close(fullFinished)
					return nil, context.DeadlineExceeded
				}
			},
			runBlackhole: func(_ context.Context, providers []prober.Provider, _ fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
				started[providers[0].ClientId]++
				return fleetprobe.BlackholeSummary{Checks: []ingest.BlackholeCheck{{ClientId: providers[0].ClientId, Ok: true, CheckedAt: time.Now()}}}, nil
			},
			submitBlackholeChecks: func(context.Context, []ingest.BlackholeCheck) error { return nil },
		}
		outcome := pass.drainBlackholeSerial(t.Context(), args, nil, nil, 1, testDueProviders("synthetic-first"), fullFinished)
		if outcome.err != nil || outcome.checked != 2 || reads != 3 || len(started) != 2 || started["synthetic-first"] != 1 || started["synthetic-next"] != 1 {
			t.Errorf("serial retry escaped selection ownership: reads=%d checked=%d starts=%v error=%v", reads, outcome.checked, started, outcome.err)
		}
	})
}
