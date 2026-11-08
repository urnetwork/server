// The zero-carry judge (model/provider_zero_carry.go): every few minutes, one
// page of connected providers is judged on what their contracts carried, and
// a provider whose contracts from several client networks carried nothing,
// idle sessions left out, and that failed every URL probe leaves every
// bucket, answer and count until it carries again. Each run first clears the verdicts
// that no longer hold: a URL probe succeeded, the device reports a new app
// version, or the verdict expired. Each run exports the verdicts in force by
// country and when it ran, so the providers dashboard charts what the newest
// run left, and a stopped judge shows as a run timestamp that stops moving
// while its verdicts lapse at their expiry.
package work

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// How often the judge runs. A run reads model.DefaultProviderZeroCarrySettings'
// page of connected providers, so a pass over Main's fleet takes about an
// hour, well inside the day of contracts each verdict reads and the three
// days a verdict lasts.
const providerZeroCarryJudgeInterval = 2 * time.Minute

// The bound on one run: a page's key lookups and the contract reads of its
// candidates take seconds.
const providerZeroCarryJudgeMaxTime = 5 * time.Minute

var providerZeroCarryProviders = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "provider_zero_carry",
	Name:      "providers",
	Help:      "Connected providers with a zero-carry verdict in force, by published country, after this process's newest judge run (urnetwork_provider_zero_carry_judge_last_run_timestamp_seconds): contracts from at least three client networks carried nothing, idle sessions left out, and every URL probe failed, so they are out of every bucket, answer and provider-locations count",
}, []string{"country_code"})

var providerZeroCarryVerdicts = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "provider_zero_carry",
	Name:      "verdicts",
	Help:      "Zero-carry verdicts in force after this process's newest judge run, connected providers or not",
})

var providerZeroCarryChangesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "provider_zero_carry",
	Name:      "changes_total",
	Help:      "Zero-carry verdicts the judge made (created) or cleared, by why: expired, url_success (a URL probe succeeded after the verdict), upgrade (the device reported another app version), gone (the client is gone or inactive)",
}, []string{"change"})

var providerZeroCarryReadsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "provider_zero_carry",
	Name:      "reads_total",
	Help:      "What the zero-carry judge read: connected providers paged (providers), those the URL condition left (candidates), those whose contracts it read (evaluated), those it read again to confirm a verdict (confirmed), and the contracts both reads read (contracts), the judge's load on transfer_contract",
}, []string{"kind"})

var providerZeroCarryConfirmationsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "provider_zero_carry",
	Name:      "confirmations_total",
	Help:      "Candidates the zero-carry judge read again before a verdict, by outcome: verdict (confirmed), older_carried (the read stopped at its limit and a contract closed earlier in the day carried bytes), idle (with the sessions their clients reported idle left out, they fell below the rule)",
}, []string{"outcome"})

var providerZeroCarryJudgeLastRunTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "provider_zero_carry",
	Name:      "judge_last_run_timestamp_seconds",
	Help:      "Unix time of this process's newest zero-carry judge run, 0 until one has run. The fleet's newest run is the maximum over Taskworker processes; the providers and verdicts gauges are that process's",
})

var providerZeroCarryJudgeLastPassTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork",
	Subsystem: "provider_zero_carry",
	Name:      "judge_last_pass_timestamp_seconds",
	Help:      "Unix time this process's zero-carry judge last finished a pass over every connected provider, 0 until one has",
})

var providerZeroCarryChanges = []string{"created"}

var providerZeroCarryReadKinds = []string{"providers", "candidates", "evaluated", "confirmed", "contracts"}

var providerZeroCarryConfirmationOutcomes = []string{"verdict", "older_carried", "idle"}

func init() {
	for _, change := range append(append([]string{}, providerZeroCarryChanges...), model.ProviderZeroCarryClearReasons...) {
		providerZeroCarryChangesTotal.WithLabelValues(change)
	}
	for _, kind := range providerZeroCarryReadKinds {
		providerZeroCarryReadsTotal.WithLabelValues(kind)
	}
	for _, outcome := range providerZeroCarryConfirmationOutcomes {
		providerZeroCarryConfirmationsTotal.WithLabelValues(outcome)
	}
	prometheus.MustRegister(
		providerZeroCarryProviders,
		providerZeroCarryVerdicts,
		providerZeroCarryChangesTotal,
		providerZeroCarryReadsTotal,
		providerZeroCarryConfirmationsTotal,
		providerZeroCarryJudgeLastRunTimestamp,
		providerZeroCarryJudgeLastPassTimestamp,
	)
}

// Where the next run starts: the last connected provider the previous run
// read, the zero id to start a pass.
type JudgeProviderZeroCarryArgs struct {
	Cursor server.Id `json:"cursor"`
}

// What one run did; the same numbers are logged.
type JudgeProviderZeroCarryResult struct {
	Cursor             server.Id      `json:"cursor"`
	PassComplete       bool           `json:"pass_complete"`
	Providers          int            `json:"providers"`
	Candidates         int            `json:"candidates"`
	Evaluated          int            `json:"evaluated"`
	Confirmed          int            `json:"confirmed"`
	IdleContracts      int            `json:"idle_contracts"`
	KeptOlderCarried   int            `json:"kept_older_carried"`
	KeptIdle           int            `json:"kept_idle"`
	Contracts          int            `json:"contracts"`
	Created            int            `json:"created"`
	Cleared            map[string]int `json:"cleared,omitempty"`
	Verdicts           int            `json:"verdicts"`
	ConnectedByCountry map[string]int `json:"connected_by_country,omitempty"`
}

// Schedules a run now, coalescing with a pending one, whose cursor it keeps.
func ScheduleJudgeProviderZeroCarry(clientSession *session.ClientSession, tx server.PgTx) {
	scheduleJudgeProviderZeroCarryAt(clientSession, tx, &JudgeProviderZeroCarryArgs{}, server.NowUtc())
}

func scheduleJudgeProviderZeroCarryAt(
	clientSession *session.ClientSession,
	tx server.PgTx,
	args *JudgeProviderZeroCarryArgs,
	runAt time.Time,
) {
	task.ScheduleTaskInTx(
		tx,
		JudgeProviderZeroCarry,
		args,
		clientSession,
		task.RunOnce("judge_provider_zero_carry"),
		task.RunAt(runAt),
		task.MaxTime(providerZeroCarryJudgeMaxTime),
	)
}

// One run of the judge.
func JudgeProviderZeroCarry(
	args *JudgeProviderZeroCarryArgs,
	clientSession *session.ClientSession,
) (*JudgeProviderZeroCarryResult, error) {
	return judgeProviderZeroCarry(clientSession.Ctx, server.NowUtc(), args, model.DefaultProviderZeroCarrySettings()), nil
}

// Schedules the next run one interval out, from where this one stopped.
func JudgeProviderZeroCarryPost(
	args *JudgeProviderZeroCarryArgs,
	result *JudgeProviderZeroCarryResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	next := &JudgeProviderZeroCarryArgs{}
	if result != nil {
		next.Cursor = result.Cursor
	} else if args != nil {
		next.Cursor = args.Cursor
	}
	scheduleJudgeProviderZeroCarryAt(clientSession, tx, next, server.NowUtc().Add(providerZeroCarryJudgeInterval))
	return nil
}

// One run at now: judge, export what is in force, and report.
func judgeProviderZeroCarry(
	ctx context.Context,
	now time.Time,
	args *JudgeProviderZeroCarryArgs,
	settings *model.ProviderZeroCarrySettings,
) *JudgeProviderZeroCarryResult {
	var cursor server.Id
	if args != nil {
		cursor = args.Cursor
	}
	judgement := model.JudgeProviderZeroCarry(ctx, now, cursor, settings)

	providerZeroCarryChangesTotal.WithLabelValues("created").Add(float64(judgement.Created))
	for reason, count := range judgement.Cleared {
		providerZeroCarryChangesTotal.WithLabelValues(reason).Add(float64(count))
	}
	providerZeroCarryReadsTotal.WithLabelValues("providers").Add(float64(judgement.PageCount))
	providerZeroCarryReadsTotal.WithLabelValues("candidates").Add(float64(judgement.CandidateCount))
	providerZeroCarryReadsTotal.WithLabelValues("evaluated").Add(float64(judgement.EvaluatedCount))
	providerZeroCarryReadsTotal.WithLabelValues("confirmed").Add(float64(judgement.ConfirmedCount))
	providerZeroCarryConfirmationsTotal.WithLabelValues("verdict").Add(float64(judgement.ConfirmedCount - judgement.KeptOlderCarriedCount - judgement.KeptIdleCount))
	providerZeroCarryConfirmationsTotal.WithLabelValues("older_carried").Add(float64(judgement.KeptOlderCarriedCount))
	providerZeroCarryConfirmationsTotal.WithLabelValues("idle").Add(float64(judgement.KeptIdleCount))
	providerZeroCarryReadsTotal.WithLabelValues("contracts").Add(float64(judgement.ContractRowCount))
	// the countries of this run alone: one whose last verdict cleared leaves
	providerZeroCarryProviders.Reset()
	for countryCode, count := range judgement.CountryCounts {
		if countryCode == "" {
			countryCode = "none"
		}
		providerZeroCarryProviders.WithLabelValues(countryCode).Add(float64(count))
	}
	providerZeroCarryVerdicts.Set(float64(judgement.ActiveCount))
	providerZeroCarryJudgeLastRunTimestamp.Set(float64(now.UnixMilli()) / 1000)
	if judgement.PassComplete {
		providerZeroCarryJudgeLastPassTimestamp.Set(float64(now.UnixMilli()) / 1000)
	}

	cleared := 0
	for _, count := range judgement.Cleared {
		cleared += count
	}
	if 0 < judgement.Created || 0 < cleared || 0 < judgement.KeptOlderCarriedCount+judgement.KeptIdleCount {
		glog.Infof(
			"[zero-carry]judged %d providers (%d candidates, %d evaluated, %d confirmed, %d contracts read): %d verdicts made, %d kept by an older carrying contract, %d kept with %d idle contracts left out, cleared %v; %d in force, connected by country: %s\n",
			judgement.PageCount, judgement.CandidateCount, judgement.EvaluatedCount, judgement.ConfirmedCount, judgement.ContractRowCount,
			judgement.Created, judgement.KeptOlderCarriedCount, judgement.KeptIdleCount, judgement.IdleContractCount,
			judgement.Cleared, judgement.ActiveCount, judgement.CountrySummary(),
		)
	} else if glog.V(1) {
		glog.Infof(
			"[zero-carry]judged %d providers (%d candidates, %d evaluated, %d contracts read): no change; %d in force\n",
			judgement.PageCount, judgement.CandidateCount, judgement.EvaluatedCount, judgement.ContractRowCount,
			judgement.ActiveCount,
		)
	}

	return &JudgeProviderZeroCarryResult{
		Cursor:             judgement.Cursor,
		PassComplete:       judgement.PassComplete,
		Providers:          judgement.PageCount,
		Candidates:         judgement.CandidateCount,
		Evaluated:          judgement.EvaluatedCount,
		Confirmed:          judgement.ConfirmedCount,
		IdleContracts:      judgement.IdleContractCount,
		KeptOlderCarried:   judgement.KeptOlderCarriedCount,
		KeptIdle:           judgement.KeptIdleCount,
		Contracts:          judgement.ContractRowCount,
		Created:            judgement.Created,
		Cleared:            judgement.Cleared,
		Verdicts:           judgement.ActiveCount,
		ConnectedByCountry: judgement.CountryCounts,
	}
}
