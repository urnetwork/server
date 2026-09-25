// Independent blackhole admission uses real workers and synthetic barriers.
package work

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/operator-proxy/fleetprobe"
	"github.com/urnetwork/operator-proxy/ingest"
	"github.com/urnetwork/operator-proxy/prober"
	"github.com/urnetwork/server/model"
)

func TestBlackholeIndependentNoFullReusesTailSlots(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		h.pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) { return nil, nil }
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		if h.startedCount(250, 500) != 249 {
			t.Error("absent full work pinned idle blackhole workers behind a tail")
		}
		close(h.laterRelease)
		synctest.Wait()
		if len(h.submittedCohort(1)) != 250 || len(h.submittedCohort(0)) != 0 {
			t.Error("no-full successor publication waited for unrelated older tail")
		}
		h.finish()
		if h.err != nil || h.peak.Load() > 250 {
			t.Errorf("no-full independent owner: peak=%d err=%v", h.peak.Load(), h.err)
		}
	})
}

func TestBlackholeIndependentHealthyFullCompletionKeepsAdmission(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		admit := make(chan struct{})
		h.pass.runBlackhole = func(ctx context.Context, ps []prober.Provider, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			<-admit
			return fleetprobe.RunBlackhole(ctx, ps, options)
		}
		close(h.fullRelease)
		h.start()
		synctest.Wait()
		close(admit)
		synctest.Wait()
		if h.startedCount(0, 250) != 250 {
			t.Error("successful full completion truncated the independent initial cohort")
		}
		close(h.firstRelease)
		synctest.Wait()
		if h.startedCount(250, 500) != 249 {
			t.Error("successful full completion stopped healthy successor admission")
		}
		h.finish()
		if h.err != nil || h.peak.Load() > 250 {
			t.Errorf("healthy completed full owner: peak=%d err=%v", h.peak.Load(), h.err)
		}
	})
}

func TestBlackholeIndependentHealthyFullDuringLookupKeepsAdmission(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		entered, release := make(chan struct{}), make(chan struct{})
		h.lookup = func(call, limit int) ([]ingest.DueProvider, error) {
			if call == 2 {
				close(entered)
				<-release
			}
			return h.due[:min(limit, len(h.due))], nil
		}
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		select {
		case <-entered:
		default:
			t.Error("synthetic successor lookup was not entered")
		}
		close(h.fullRelease)
		synctest.Wait()
		close(release)
		synctest.Wait()
		if h.startedCount(250, 500) != 249 {
			t.Error("healthy full completion during lookup discarded ready successor work")
		}
		h.finish()
		if h.err != nil {
			t.Errorf("independent lookup: %v", h.err)
		}
	})
}

func TestBlackholeIndependentTwoTailsDoNotPinThirdCohort(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 3)
		secondTail := make(chan struct{})
		h.check = func(index int, p prober.Provider) fleetprobe.BlackholeResult {
			if index == 499 {
				select {
				case <-secondTail:
				case <-h.ctx.Done():
				}
			}
			return testBlackholeAdmissionPass(p)
		}
		close(h.laterRelease)
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		if h.startedCount(500, 750) != 250 || len(h.submittedCohort(2)) != 250 {
			t.Error("two stragglers pinned otherwise idle workers and third-cohort publication")
		}
		if len(h.submittedCohort(0)) != 0 || len(h.submittedCohort(1)) != 0 || h.peak.Load() > 250 {
			t.Error("third-cohort progress bypassed an original guard join or raised concurrency")
		}
		close(secondTail)
		h.finish()
		if h.err != nil {
			t.Errorf("independent original cohorts: %v", h.err)
		}
	})
}

func TestBlackholeIndependentFullErrorKeepsOnlyInitialMinimum(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		fullErr := errors.New("synthetic full owner failure")
		h.pass.runFull = func(context.Context, []prober.Provider, fleetprobe.FullOptions) (prober.Summary, error) {
			return prober.Summary{}, fullErr
		}
		admit := make(chan struct{})
		h.pass.runBlackhole = func(ctx context.Context, ps []prober.Provider, options fleetprobe.BlackholeOptions) (fleetprobe.BlackholeSummary, error) {
			<-admit
			return fleetprobe.RunBlackhole(ctx, ps, options)
		}
		h.start()
		synctest.Wait()
		close(admit)
		close(h.firstRelease)
		synctest.Wait()
		h.finish()
		if h.startedCount(0, 500) != args.DarkBatchGuardMinChecks || !errors.Is(h.err, fullErr) {
			t.Errorf("full failure bypassed initial-minimum/abort boundary: started=%d err=%v", h.startedCount(0, 500), h.err)
		}
	})
}

func TestBlackholeIndependentNoFullCancellationKeepsSafePrefix(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		h.pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) { return nil, nil }
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		h.cancel()
		h.finish()
		if !errors.Is(h.err, context.Canceled) || h.active.Load() != 0 || len(h.submittedCohort(0)) != 249 || len(h.submittedCohort(1)) != 0 {
			t.Errorf("no-full cancellation lost safe evidence or leaked checks: active=%d err=%v", h.active.Load(), h.err)
		}
	})
}

func TestBlackholeIndependentCreditFailureStopsSuccessor(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		h.pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) { return nil, nil }
		var reads atomic.Int32
		h.pass.readiness = &providerEgressProbeReadiness{minimum: 1, available: func(context.Context) (model.ByteCount, error) {
			if reads.Add(1) <= 2 {
				return 1, nil
			}
			return 0, nil
		}}
		h.start()
		synctest.Wait()
		close(h.firstRelease)
		synctest.Wait()
		h.finish()
		if !errors.Is(h.err, errProviderEgressProbeUnfunded) || h.startedCount(250, 500) != 0 || len(h.submittedCohort(0)) != 250 {
			t.Errorf("shared credit failure did not stop new work/preserve passes: err=%v", h.err)
		}
	})
}

func TestBlackholeIndependentSerialReserveAndStrictInitialCutoff(t *testing.T) {
	serial := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{serial: true, mixed: true})
	if serial.started != 8 || serial.fullStarted != 1 || serial.contextErr != nil || serial.result.BlackholeNotMeasured != 6 || serial.result.Dark != 1 {
		t.Fatalf("serial full reserve or short-sample pass/TLS policy changed: %+v", serial)
	}
	consumed := testBlackholeLeaseAdmissionRun(t, testBlackholeLeaseAdmissionCase{readinessConsumesAdmission: true})
	if consumed.started != 0 || consumed.submitCalls != 0 || !errors.Is(consumed.err, errProviderEgressBlackholeAdmissionBudget) {
		t.Fatalf("no-full strict cutoff was bypassed by a guard minimum: %+v", consumed)
	}
}

func TestBlackholeIndependentCutoffJoinsWithoutCancelingStartedChecks(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		h.pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) { return nil, nil }
		h.start()
		synctest.Wait()
		// Eight possible publications reserve4m;75m−32m45s−4m=38m15s.
		time.Sleep(39 * time.Minute)
		close(h.firstRelease)
		close(h.firstTail)
		synctest.Wait()
		h.finish()
		if h.startedCount(250, 500) != 0 || len(h.submittedCohort(0)) != 250 || h.err != nil {
			t.Errorf("admission cutoff canceled started checks or admitted an unfitting wave: %v", h.err)
		}
	})
}

func TestBlackholeIndependentInitialParallelWavesRespectCutoff(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	args.Blackhole.Concurrency = args.DarkBatchGuardMinChecks
	args.Full.Concurrency = 1
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 2)
		h.start()
		synctest.Wait()
		if h.startedCount(0, 500) != args.DarkBatchGuardMinChecks {
			t.Error("initial guard-sized cohort did not start")
		}
		// Advance only fake time: the first workers remain blocked while the
		// admission cutoff passes; later initial waves must not inherit a bypass.
		time.Sleep(39 * time.Minute)
		close(h.firstRelease)
		synctest.Wait()
		started := h.startedCount(0, 500)
		h.finish()
		if started != args.DarkBatchGuardMinChecks || h.result.Checked != args.DarkBatchGuardMinChecks || h.err != nil {
			t.Errorf("initial parallel cohort bypassed cutoff beyond minimum: started=%d checked=%d err=%v", started, h.result.Checked, h.err)
		}
	})
}

func TestBlackholeIndependentDecisionMetricsHaveFixedDomain(t *testing.T) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, decision := range []string{"pipeline_started", "lookup_started", "successor_selected", "cutoff", "partial_due", "no_unseen_due", "error", "cohort_cap", "canceled", "full_error", "full_finished", "no_full_serial", "serial_geometry"} {
		want[decision] = true
	}
	seen := map[string]bool{}
	for _, family := range families {
		if family.GetName() != "urnetwork_egress_probe_blackhole_pipeline_decisions_total" {
			continue
		}
		for _, metric := range family.Metric {
			if len(metric.Label) != 1 || metric.Label[0].GetName() != "decision" || !want[metric.Label[0].GetValue()] {
				t.Fatal("pipeline decision introduced an unbounded label/domain")
			}
			seen[metric.Label[0].GetValue()] = true
		}
		if strings.Contains(family.GetHelp(), "provider_id") || strings.Contains(family.GetHelp(), "task_id") {
			t.Error("identity appeared in diagnostic contract")
		}
	}
	if len(seen) != len(want) {
		t.Errorf("fixed preinitialized pipeline decisions missing: got%d want%d", len(seen), len(want))
	}
}

func TestBlackholeIndependentEightTailOwnersStayBoundedAndJoinCancellation(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 10)
		h.check = func(index int, p prober.Provider) fleetprobe.BlackholeResult {
			if index%250 == 249 {
				<-h.ctx.Done()
			}
			if index%250 == 0 {
				return testBlackholeAdmissionTls(p)
			}
			return testBlackholeAdmissionPass(p)
		}
		close(h.firstRelease)
		close(h.laterRelease)
		h.start()
		synctest.Wait()
		if h.startedCount(0, len(h.due)) != 2000 || h.peak.Load() > 250 || len(h.submissions) != 0 {
			t.Errorf("eight independent tails exceeded bounds or pinned admission: started=%d peak=%d ACKs=%d", h.startedCount(0, len(h.due)), h.peak.Load(), len(h.submissions))
		}
		h.cancel()
		h.finish()
		for cohort := range 8 {
			checks := h.submittedCohort(cohort)
			if len(checks) != 249 {
				t.Errorf("cohort%d lost completed evidence: %d", cohort, len(checks))
				continue
			}
			for index, check := range checks {
				if check.NotMeasured || (index == 0 && check.Failure != "tls_authentication_failed") || (index != 0 && !check.Ok) {
					t.Errorf("cohort%d cancellation changed completed pass/TLS evidence", cohort)
				}
			}
		}
		if !errors.Is(h.err, context.Canceled) || h.active.Load() != 0 || h.result.Checked != 8*249 || len(h.submissions) != 8 {
			t.Errorf("bounded tail cancellation lost retained passes or join: checked=%d ACKs=%d active=%d err=%v", h.result.Checked, len(h.submissions), h.active.Load(), h.err)
		}
	})
}

func TestBlackholeIndependentEightPendingCancellationBoundsPublication(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 8)
		h.check = func(index int, p prober.Provider) fleetprobe.BlackholeResult {
			if index%250 == 249 {
				<-h.ctx.Done()
			}
			if index%250 == 0 {
				return testBlackholeAdmissionTls(p)
			}
			return testBlackholeAdmissionPass(p)
		}
		var requests atomic.Int32
		h.pass.submitBlackholeChecks = func(ctx context.Context, checks []ingest.BlackholeCheck) error {
			deadline, bounded := ctx.Deadline()
			if ctx.Err() != nil || !bounded || time.Until(deadline) != 30*time.Second || len(checks) != 249 {
				t.Error("cancellation lost the independent publication bound or safe prefix")
			}
			for index, check := range checks {
				if check.NotMeasured || (index == 0 && check.Failure != "tls_authentication_failed") || (index != 0 && !check.Ok) {
					t.Error("cancellation fabricated a negative or lost authenticated evidence")
				}
			}
			requests.Add(1)
			<-ctx.Done()
			return ctx.Err()
		}
		close(h.firstRelease)
		close(h.laterRelease)
		h.start()
		synctest.Wait()
		if h.startedCount(0, 2000) != 2000 || requests.Load() != 0 {
			t.Error("publication-bound fixture did not retain eight independent tails")
		}
		canceledAt := time.Now()
		h.cancel()
		h.finish()
		if elapsed := time.Since(canceledAt); elapsed != 30*time.Second || requests.Load() != 8 || h.active.Load() != 0 ||
			!errors.Is(h.err, context.Canceled) || !errors.Is(h.err, context.DeadlineExceeded) {
			t.Errorf("eight pending finalizers did not join within one bounded unwind: elapsed=%s requests=%d active=%d err=%v", elapsed, requests.Load(), h.active.Load(), h.err)
		}
		if len(h.published) != 0 {
			t.Error("failed publication was claimed as acknowledged")
		}
	})
}

func TestBlackholeIndependentEightPendingGuardsRemainIsolated(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 8)
		releaseTails := make(chan struct{})
		h.check = func(index int, p prober.Provider) fleetprobe.BlackholeResult {
			position, cohort := index%250, index/250
			if position == 249 {
				<-releaseTails
				return testBlackholeAdmissionTls(p)
			}
			negativeCount := 40
			if cohort%2 == 0 {
				negativeCount = 51
			}
			if position < negativeCount {
				return testBlackholeAdmissionDark(p)
			}
			return testBlackholeAdmissionPass(p)
		}
		close(h.firstRelease)
		close(h.laterRelease)
		h.start()
		synctest.Wait()
		if h.startedCount(0, 2000) != 2000 || len(h.submissions) != 0 || h.peak.Load() > 250 {
			t.Error("eight original guard owners were not independently retained within the worker bound")
		}
		close(releaseTails)
		h.finish()
		for cohort := range 8 {
			checks := h.submittedCohort(cohort)
			held, tls := 0, 0
			for _, check := range checks {
				if check.NotMeasured {
					held++
				}
				if check.Failure == "tls_authentication_failed" {
					tls++
				}
			}
			wantHeld := 0
			if cohort%2 == 0 {
				wantHeld = 51
			}
			if len(checks) != 250 || held != wantHeld || tls != 1 {
				t.Errorf("cohort%d contaminated by sibling guard: checked=%d held=%d tls=%d", cohort, len(checks), held, tls)
			}
		}
		if h.err != nil || h.result.BlackholeGuardTripped != 4 {
			t.Errorf("guard cohorts were merged or lost: %+v err=%v", h.result, h.err)
		}
	})
}

func TestBlackholeIndependentEightPendingCreditLossKeepsOnlySafeEvidence(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		h := newTestBlackholePipeline(t, args, 10)
		h.pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) { return nil, nil }
		var depleted atomic.Bool
		h.pass.readiness = &providerEgressProbeReadiness{minimum: 1, available: func(context.Context) (model.ByteCount, error) {
			if depleted.Load() {
				return 0, nil
			}
			return 1, nil
		}}
		releaseTails := make(chan struct{})
		h.check = func(index int, p prober.Provider) fleetprobe.BlackholeResult {
			switch index % 250 {
			case 0:
				return testBlackholeAdmissionPass(p)
			case 1:
				return testBlackholeAdmissionTls(p)
			case 249:
				<-releaseTails
			}
			return testBlackholeAdmissionDark(p)
		}
		close(h.firstRelease)
		close(h.laterRelease)
		h.start()
		synctest.Wait()
		if h.startedCount(0, len(h.due)) != 2000 || len(h.submissions) != 0 {
			t.Error("credit-loss fixture did not reach eight buffered guard owners")
		}
		depleted.Store(true)
		close(releaseTails)
		h.finish()
		for cohort := range 8 {
			checks := h.submittedCohort(cohort)
			if len(checks) != 2 || !checks[0].Ok || checks[1].Failure != "tls_authentication_failed" || checks[1].NotMeasured {
				t.Errorf("cohort%d published credit-invalid negatives or lost pass/TLS evidence: count=%d", cohort, len(checks))
			}
		}
		if !errors.Is(h.err, errProviderEgressProbeUnfunded) || h.result.Checked != 16 || h.startedCount(2000, 2500) != 0 || h.active.Load() != 0 {
			t.Errorf("credit-loss failure/admission/join contract lost: checked=%d err=%v", h.result.Checked, h.err)
		}
	})
}

// Read only fixed labels; use before/after deltas rather than reset shared metrics.
func testBlackholeIndependentDecisionCounts(t *testing.T) map[string]float64 {
	counts := map[string]float64{}
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == "urnetwork_egress_probe_blackhole_pipeline_decisions_total" {
			for _, metric := range family.Metric {
				counts[metric.Label[0].GetValue()] = metric.GetCounter().GetValue()
			}
		}
	}
	return counts
}

func TestBlackholeIndependentDecisionsReportBeforeTailPublication(t *testing.T) {
	args := testProviderEgressParallelArgs(t)
	synctest.Test(t, func(t *testing.T) {
		before := testBlackholeIndependentDecisionCounts(t)
		h := newTestBlackholePipeline(t, args, 2)
		h.pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) { return nil, nil }
		h.start()
		synctest.Wait()
		selected := testBlackholeIndependentDecisionCounts(t)
		if selected["pipeline_started"]-before["pipeline_started"] != 1 || selected["lookup_started"]-before["lookup_started"] != 1 || selected["successor_selected"]-before["successor_selected"] != 1 {
			t.Error("fixed decisions did not expose selected successor while the initial cohort was active")
		}
		time.Sleep(39 * time.Minute)
		stopped := testBlackholeIndependentDecisionCounts(t)
		if stopped["cutoff"]-before["cutoff"] != 1 || len(h.submissions) != 0 || stopped["no_full_serial"] != before["no_full_serial"] || stopped["full_finished"] != before["full_finished"] {
			t.Error("no-full cutoff was hidden until ACK or mislabeled as full-finished/serial")
		}
		h.finish()
	})
}
