// The overload judge (model/provider_egress_url_overload.go): about once a
// minute the fleet's scored URL outcomes of the recent buckets of measured time
// are tallied and judged, and a bucket of an overload of our own platform is
// discounted: its outcomes stay recorded in the history but leave every score
// and pool decision. Each run counts the outcomes it newly discounted, logs
// each bucket that enters or leaves the discount, and exports when it ran, so a
// stopped judge shows as a run timestamp that stops moving. A stopped judge
// discounts nothing: overloads count against providers again, as before it.
package work

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

var providerEgressUrlDiscountedOutcomesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_url",
	Name:      "discounted_outcomes_total",
	Help:      "Scored URL outcomes the overload judge discounted, by outcome (success or error): measured in a bucket of five minutes in which the whole fleet's outcomes failed far above their baseline, and still did with the bucket's worst country or worst network left out, so the failures were the platform's (an overload of Main), not the providers'. They stay recorded in the history and the rolling quota and leave every score and pool decision. Each is counted once, by the taskworker whose judge run first found it in a discounted bucket",
}, []string{"outcome"})

var providerEgressUrlOverloadJudgeLastRunTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "egress_url",
	Name:      "overload_judge_last_run_timestamp_seconds",
	Help:      "Unix time of this process's newest overload judge run, 0 until one has run. The fleet's newest run is the maximum over Taskworker processes; while no taskworker runs the judge, overloads count against providers",
})

func init() {
	for _, outcome := range []string{"success", "error"} {
		providerEgressUrlDiscountedOutcomesTotal.WithLabelValues(outcome)
	}
	prometheus.MustRegister(
		providerEgressUrlDiscountedOutcomesTotal,
		providerEgressUrlOverloadJudgeLastRunTimestamp,
	)
}

type JudgeProviderEgressUrlOverloadArgs struct {
}

// What one run did; the same is logged and counted.
type JudgeProviderEgressUrlOverloadResult struct {
	// buckets tallied, and those of them discounted
	Buckets    int `json:"buckets"`
	Discounted int `json:"discounted"`
	// buckets that entered the discount, and that left it
	Entered  int `json:"entered"`
	Released int `json:"released"`
	// outcomes newly discounted, and the failures among them
	DiscountedOutcomes int   `json:"discounted_outcomes"`
	DiscountedErrors   int   `json:"discounted_errors"`
	Removed            int64 `json:"removed"`
}

// Schedules a run now, coalescing with a pending one.
func ScheduleJudgeProviderEgressUrlOverload(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleJudgeProviderEgressUrlOverloadAt(clientSession, tx, server.NowUtc(), model.DefaultProviderEgressUrlOverloadSettings())
}

func scheduleJudgeProviderEgressUrlOverloadAt(
	clientSession *session.ClientSession,
	tx server.PgTx,
	runAt time.Time,
	settings *model.ProviderEgressUrlOverloadSettings,
) {
	task.ScheduleTaskInTx(
		tx,
		JudgeProviderEgressUrlOverload,
		&JudgeProviderEgressUrlOverloadArgs{},
		clientSession,
		task.RunOnce("judge_provider_egress_url_overload"),
		task.RunAt(runAt),
		task.MaxTime(settings.JudgeMaxTime),
	)
}

// One run of the judge.
func JudgeProviderEgressUrlOverload(
	args *JudgeProviderEgressUrlOverloadArgs,
	clientSession *session.ClientSession,
) (*JudgeProviderEgressUrlOverloadResult, error) {
	return judgeProviderEgressUrlOverload(clientSession.Ctx, server.NowUtc(), model.DefaultProviderEgressUrlOverloadSettings()), nil
}

// Schedules the next run one interval out, whatever this one did.
func JudgeProviderEgressUrlOverloadPost(
	args *JudgeProviderEgressUrlOverloadArgs,
	result *JudgeProviderEgressUrlOverloadResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	settings := model.DefaultProviderEgressUrlOverloadSettings()
	scheduleJudgeProviderEgressUrlOverloadAt(clientSession, tx, server.NowUtc().Add(settings.JudgeInterval), settings)
	return nil
}

// One run at now: judge, then count and report.
func judgeProviderEgressUrlOverload(ctx context.Context, now time.Time, settings *model.ProviderEgressUrlOverloadSettings) *JudgeProviderEgressUrlOverloadResult {
	return exportProviderEgressUrlOverloadJudgement(now, model.JudgeProviderEgressUrlOverload(ctx, now, settings))
}

// Counts what the run newly discounted, records when it ran, logs each bucket
// that entered or left the discount, and reports.
func exportProviderEgressUrlOverloadJudgement(now time.Time, judgement *model.ProviderEgressUrlOverloadJudgement) *JudgeProviderEgressUrlOverloadResult {
	providerEgressUrlDiscountedOutcomesTotal.WithLabelValues("success").Add(float64(max(0, judgement.DiscountedOutcomeCount-judgement.DiscountedErrorCount)))
	providerEgressUrlDiscountedOutcomesTotal.WithLabelValues("error").Add(float64(max(0, judgement.DiscountedErrorCount)))
	providerEgressUrlOverloadJudgeLastRunTimestamp.Set(float64(now.UnixMilli()) / 1000)

	for _, bucket := range judgement.Entered {
		// alert class: the fleet's URL outcomes fail far above their baseline
		// in every country and network, an overload of the platform
		glog.Errorf(
			"[egress]URL overload: the outcomes measured %s to %s are discounted, recorded but not scored: %d of %d failed (%.0f%%) against a baseline of %.0f%% over %d; without the worst country %d of %d, without the worst network %d of %d\n",
			bucket.BucketStart.UTC().Format(time.RFC3339),
			bucket.BucketEnd.UTC().Format(time.RFC3339),
			bucket.ErrorCount,
			bucket.OutcomeCount,
			100*bucket.ErrorShare(),
			100*bucket.BaselineErrorShare(),
			bucket.BaselineOutcomeCount,
			bucket.ErrorCount-bucket.CountryErrorCount,
			bucket.OutcomeCount-bucket.CountryOutcomeCount,
			bucket.ErrorCount-bucket.NetworkErrorCount,
			bucket.OutcomeCount-bucket.NetworkOutcomeCount,
		)
	}
	for _, bucket := range judgement.Released {
		glog.Infof(
			"[egress]URL overload released: the outcomes measured %s to %s are scored again: %d of %d failed (%.0f%%) against a baseline of %.0f%%\n",
			bucket.BucketStart.UTC().Format(time.RFC3339),
			bucket.BucketEnd.UTC().Format(time.RFC3339),
			bucket.ErrorCount,
			bucket.OutcomeCount,
			100*bucket.ErrorShare(),
			100*bucket.BaselineErrorShare(),
		)
	}

	result := &JudgeProviderEgressUrlOverloadResult{
		Buckets:            len(judgement.Buckets),
		Entered:            len(judgement.Entered),
		Released:           len(judgement.Released),
		DiscountedOutcomes: judgement.DiscountedOutcomeCount,
		DiscountedErrors:   judgement.DiscountedErrorCount,
		Removed:            judgement.RemovedCount,
	}
	for _, bucket := range judgement.Buckets {
		if bucket.Discounted {
			result.Discounted++
		}
	}
	return result
}
