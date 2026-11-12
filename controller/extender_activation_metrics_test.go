package controller

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/connect"
)

// The activation metrics (extender_activation_metrics.go). Before them the
// operator counted no activation by how it ended, so a run of refusals of one
// host, one family or one step left no trace beside the request count
// (connect#230).

// Both metrics are registered in the process, as every other application
// metric is, so the api serves them.
func TestExtenderActivationMetricsAreRegistered(t *testing.T) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, family := range families {
		names = append(names, family.GetName())
	}
	for _, name := range []string{
		"urnetwork_extender_activations_total",
		"urnetwork_extender_activation_step_seconds",
	} {
		if !slices.Contains(names, name) {
			t.Fatalf("%s is not registered", name)
		}
	}
}

// The step histogram of one step and outcome.
func testActivationStepHistogram(t testing.TB, metrics *extenderActivationMetrics, step string, outcome string) *dto.Histogram {
	t.Helper()
	metric := &dto.Metric{}
	observer, err := metrics.stepSeconds.GetMetricWithLabelValues(step, outcome)
	if err != nil {
		t.Fatal(err)
	}
	if err := observer.(prometheus.Metric).Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetHistogram()
}

// Every series an activation can end in exists at zero from the start, and
// there are no others: a request refusal by each family and an unreadable
// one, and every other stage's outcomes by each family -- a busy api and each
// budget among them (connect#230). Every step is timed by step and outcome
// alone.
func TestExtenderActivationMetricsAreFixedCardinality(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newExtenderActivationMetrics(registry)
	// main's 37 and the webrtc stage's four outcomes (error, timeout,
	// other_family, cap) by two families, and the webrtc step's three
	// outcomes, the required path of connect/EXTENDER.md S8
	connect.AssertEqual(t, testutil.CollectAndCount(metrics.activations), 45)
	connect.AssertEqual(t, testutil.CollectAndCount(metrics.stepSeconds), 15)
	for stage, outcomes := range extenderActivationStageOutcomes {
		for _, outcome := range outcomes {
			for _, family := range []string{extenderActivationFamilyIpv4, extenderActivationFamilyIpv6} {
				if value := testutil.ToFloat64(metrics.activations.WithLabelValues(stage, outcome, family)); value != 0 {
					t.Fatalf("%s %s %s starts at %v", stage, outcome, family, value)
				}
			}
		}
	}
	// the cells above are the ones that already existed
	connect.AssertEqual(t, testutil.CollectAndCount(metrics.activations), 45)
}

// One activation is counted once, where it ended, and each step it probed is
// timed in its own step and outcome.
func TestExtenderActivationMetricsCountOneActivation(t *testing.T) {
	metrics := newExtenderActivationMetrics(prometheus.NewRegistry())
	metrics.observe(
		extenderActivationStageForward,
		extenderActivationOutcomeTimeout,
		extenderActivationFamilyIpv6,
		[]*ExtenderProbeStep{
			{Name: connect.ExtenderCarrierTcp, IpVersion: 6, Port: 443, ElapsedMs: 9500, BudgetLeftMs: 10000, Outcome: extenderProbeStepOutcomeOk},
			{Name: extenderProbeStepForward, IpVersion: 6, Port: 443, ElapsedMs: 500, BudgetLeftMs: 500, Outcome: extenderProbeStepOutcomeTimeout},
		},
	)
	connect.AssertEqual(t, testutil.ToFloat64(metrics.activations.WithLabelValues("forward", "timeout", "ipv6")), float64(1))
	connect.AssertEqual(t, testutil.ToFloat64(metrics.activations.WithLabelValues("forward", "timeout", "ipv4")), float64(0))
	tcpHistogram := testActivationStepHistogram(t, metrics, "tcp", "ok")
	connect.AssertEqual(t, tcpHistogram.GetSampleCount(), uint64(1))
	connect.AssertEqual(t, tcpHistogram.GetSampleSum(), 9.5)
	forwardHistogram := testActivationStepHistogram(t, metrics, "forward", "timeout")
	connect.AssertEqual(t, forwardHistogram.GetSampleCount(), uint64(1))
	connect.AssertEqual(t, forwardHistogram.GetSampleSum(), 0.5)

	// nil counts nothing, which is what settings built by hand get
	var noMetrics *extenderActivationMetrics
	noMetrics.observe(extenderActivationStageStore, extenderActivationOutcomeActivated, extenderActivationFamilyIpv4, nil)
}

// A refusal is counted as its stage ends: a request refuses, the operator's
// configuration or storing fails, a step of the required test ends as its
// probe did, a busy api names why it was busy, and a budget refusal names the
// budget (connect#230).
func TestExtenderActivationRefusalOutcomeByStage(t *testing.T) {
	for _, c := range []struct {
		stage        string
		stageOutcome string
		want         string
	}{
		{stage: extenderActivationStageRequest, want: "refused"},
		{stage: extenderActivationStageConfig, want: "error"},
		{stage: extenderActivationStageOwner, want: "refused"},
		{stage: extenderActivationStageBusy, stageOutcome: extenderActivationOutcomeProbes, want: "probes"},
		{stage: extenderActivationStageBusy, stageOutcome: extenderActivationOutcomeBudgetLock, want: "budget_lock"},
		{stage: extenderActivationStageBudget, stageOutcome: extenderActivationOutcomeClient, want: "client"},
		{stage: extenderActivationStageBudget, stageOutcome: extenderActivationOutcomeAddress, want: "address"},
		{stage: extenderActivationStageBudget, stageOutcome: extenderActivationOutcomeProvenAddress, want: "proven_address"},
		{stage: extenderActivationStageBudget, stageOutcome: extenderActivationOutcomeAccount, want: "account"},
		{stage: extenderActivationStageBudget, stageOutcome: "unproven_account", want: "unproven_account"},
		{stage: extenderActivationStageBudget, stageOutcome: extenderActivationOutcomeError, want: "error"},
		{stage: extenderActivationStageTcp, stageOutcome: extenderProbeStepOutcomeError, want: "error"},
		{stage: extenderActivationStageTcp, stageOutcome: extenderProbeStepOutcomeTimeout, want: "timeout"},
		{stage: extenderActivationStageForward, stageOutcome: extenderProbeRefusalOtherFamily, want: "other_family"},
		{stage: extenderActivationStageStore, want: "error"},
	} {
		if got := extenderActivationRefusalOutcome(c.stage, c.stageOutcome); got != c.want {
			t.Fatalf("a refusal at %s %s is counted as %s, want %s", c.stage, c.stageOutcome, got, c.want)
		}
	}
}

// A budget refusal is counted under the budget that refused, by its action:
// the calling client's, the caller address's, or the account's (connect#230).
func TestExtenderActivationBudgetOutcomeNamesTheBudget(t *testing.T) {
	for _, ipVersion := range []int{4, 6} {
		clientAction, addressAction, accountAction := extenderActivateRateLimitActions(ipVersion)
		for action, want := range map[string]string{
			clientAction:  "client",
			addressAction: "address",
			extenderActivateProvenAddressRateLimitAction(ipVersion): "proven_address",
			accountAction: "account",
		} {
			if got := extenderActivationBudgetOutcome(action); got != want {
				t.Errorf("a refusal by %s is counted as %s, want %s", action, got, want)
			}
		}
	}
}

// A refusal is counted under the budget that refused, or as the error it is,
// never under the account's for want of a better label
// (research extender-health-2026-11-11 §1.2). A refusal by a budget the
// activation never spends, and a budget check that failed rather than
// refused, were both counted as budget/account, the label the account's own
// refusals of about 96,000 posts a day are read from; each is now budget/error,
// a series of its own.
func TestExtenderActivationBudgetOutcomeCountsNoOtherBudgetAsTheAccount(t *testing.T) {
	for _, action := range []string{
		"",
		"ping_report",
		"extender_block_report",
		"extender_activate_v5",
	} {
		if got := extenderActivationBudgetOutcome(action); got != "error" {
			t.Errorf("a refusal by %q is counted as %s, want error", action, got)
		}
	}
	outcomes := extenderActivationStageOutcomes[extenderActivationStageBudget]
	for _, want := range []string{"error", "unproven_account"} {
		if !slices.Contains(outcomes, want) {
			t.Errorf("the budget stage has no %s series: %v", want, outcomes)
		}
	}
}

// Each way the required test refuses an activation is a stage and outcome the
// count holds a series for, so no probe refusal is counted under a label the
// dashboard does not read.
func TestExtenderActivationProbeRefusalsAreCountedStages(t *testing.T) {
	setups := []func(probes *testActivationProbes){
		func(probes *testActivationProbes) {
			probes.carrierErrs[testTimingTcpProbe] = errors.New("connection refused")
		},
		func(probes *testActivationProbes) {
			probes.answer = func(ctx context.Context, probe string) error {
				return probes.clock.WaitOut(ctx)
			}
		},
		func(probes *testActivationProbes) {
			probes.forwardErr = errors.New("extender hello answered with status 502")
		},
		func(probes *testActivationProbes) {
			probes.forwardAnswer = func(ctx context.Context) error {
				return probes.clock.WaitOut(ctx)
			}
		},
		func(probes *testActivationProbes) {
			probes.forwardClientAddress = "[2001:db8::10]:443"
		},
	}
	counted := map[string]bool{}
	for _, setup := range setups {
		probes := newTestActivationProbes()
		setup(probes)
		outcome := probes.judge(context.Background(), connect.ExtenderCarrierTcp)
		if outcome.refusal == "" {
			t.Fatal("the activation was not refused")
		}
		outcomes, ok := extenderActivationStageOutcomes[outcome.refusalStep]
		if !ok || !slices.Contains(outcomes, outcome.refusalOutcome) {
			t.Fatalf("the refusal %q is counted at %s %s, which has no series", outcome.refusal, outcome.refusalStep, outcome.refusalOutcome)
		}
		counted[outcome.refusalStep+" "+outcome.refusalOutcome] = true
	}
	connect.AssertEqual(t, len(counted), len(setups))
}

// The family label is the caller address's family, an ipv4-mapped address
// counting as ipv4, and unknown when the address cannot be read.
func TestExtenderActivationFamilyLabel(t *testing.T) {
	for clientAddress, want := range map[string]string{
		"192.0.2.10:54321":          "ipv4",
		"[::ffff:192.0.2.10]:54321": "ipv4",
		"[2001:db8::10]:54321":      "ipv6",
		"":                          "unknown",
		"not an address":            "unknown",
	} {
		if got := extenderActivationFamily(clientAddress); got != want {
			t.Fatalf("the family of %q is %s, want %s", clientAddress, got, want)
		}
	}
}

// The step histogram's buckets reach the 10 s probe budget and no further, so
// a step that ran out of the budget lands in the last bucket.
func TestExtenderActivationStepBucketsEndAtTheProbeBudget(t *testing.T) {
	metrics := newExtenderActivationMetrics(prometheus.NewRegistry())
	metrics.observe(
		extenderActivationStageTcp,
		extenderActivationOutcomeTimeout,
		extenderActivationFamilyIpv4,
		[]*ExtenderProbeStep{
			{Name: connect.ExtenderCarrierTcp, IpVersion: 4, Port: 443, ElapsedMs: ExtenderActivateProbeTimeout.Milliseconds(), BudgetLeftMs: ExtenderActivateProbeTimeout.Milliseconds(), Outcome: extenderProbeStepOutcomeTimeout},
		},
	)
	histogram := testActivationStepHistogram(t, metrics, "tcp", "timeout")
	buckets := histogram.GetBucket()
	lastBucket := buckets[len(buckets)-1]
	connect.AssertEqual(t, lastBucket.GetUpperBound(), ExtenderActivateProbeTimeout.Seconds())
	connect.AssertEqual(t, lastBucket.GetCumulativeCount(), uint64(1))
	if secondLast := buckets[len(buckets)-2]; secondLast.GetCumulativeCount() != 0 || secondLast.GetUpperBound() >= (ExtenderActivateProbeTimeout-time.Millisecond).Seconds() {
		t.Fatalf("the bucket below the budget, %v s, holds %d", secondLast.GetUpperBound(), secondLast.GetCumulativeCount())
	}
}
