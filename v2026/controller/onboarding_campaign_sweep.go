// SPDX-License-Identifier: MPL-2.0

package controller

// The onboarding campaign sweep: re-schedules running rows whose next step
// has no task. Every step after E1 is scheduled in its predecessor's
// completion transaction, so in practice only E1 was lost:
// EnrollNetworkOnboarding inserted the row, upserted the Brevo contact, then
// scheduled E1 in a transaction of its own, all inside the 10 s post-primary
// timeout, and a failure or an expired context after the insert left a row
// whose sequence never started (140 running rows on Main on 2026-11-15, the
// oldest from 2026-09-11). E1 now commits with the row; the sweep repairs the
// rows left before that, and any step lost another way.
//
// A row lost at E1 within FirstStepMaxAge of its creation starts its sequence
// with E1; an older one skips E1, whose copy is for a new account, and joins
// at E2 (owner, 2026-11-15). Any other lost step is re-scheduled as it is. Each
// goes at the next opening of the reader's send window, and the step decides
// at send time as always (a deleted, Pro or unsubscribed network exits then).
// The first run after the taskworker starts is the one-time repair of the
// rows left before the fix; later runs find a lost step within the grace.

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/onboarding"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// OnboardingCampaignSweepSettings are the sweep's tunables.
type OnboardingCampaignSweepSettings struct {
	// between runs
	Interval time.Duration
	// a step due this long ago with no task is lost: well past the scheduler's
	// own lag, so a step about to run is never doubled
	Grace time.Duration
	// rows read per run, oldest due first
	BatchSize int
	// a lost E1 still goes when its row is at most this old; an older row
	// skips to E2
	FirstStepMaxAge time.Duration
}

func DefaultOnboardingCampaignSweepSettings() *OnboardingCampaignSweepSettings {
	return &OnboardingCampaignSweepSettings{
		Interval:        15 * time.Minute,
		Grace:           1 * time.Hour,
		BatchSize:       500,
		FirstStepMaxAge: 7 * 24 * time.Hour,
	}
}

// The sweep's actions.
const (
	onboardingSweepResumed   = "resumed"
	onboardingSweepSkippedE1 = "skipped_e1"
)

var onboardingSweepRescheduledTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "onboarding",
	Name:      "sweep_rescheduled_total",
	Help:      "Running onboarding campaign rows whose next step had no task, re-scheduled by the sweep, by the step scheduled and the action: resumed (the lost step itself) or skipped_e1 (an E1 lost past the first step's age limit, joined at E2).",
}, []string{"step", "action"})

func init() {
	prometheus.MustRegister(onboardingSweepRescheduledTotal)
}

const onboardingCampaignSweepTaskKey = "onboarding_campaign_sweep"

type OnboardingCampaignSweepArgs struct {
}

type OnboardingCampaignSweepResult struct {
	// rows due past the grace with no pending task for their next step
	Lost      int `json:"lost"`
	Resumed   int `json:"resumed"`
	SkippedE1 int `json:"skipped_e1"`
}

// ScheduleOnboardingCampaignSweep schedules the fleet's one sweep at `at`.
func ScheduleOnboardingCampaignSweep(clientSession *session.ClientSession, tx server.PgTx, at time.Time) {
	task.ScheduleTaskInTx(
		tx,
		OnboardingCampaignSweep,
		&OnboardingCampaignSweepArgs{},
		clientSession,
		task.RunOnce(onboardingCampaignSweepTaskKey),
		task.RunAt(at),
		task.MaxTime(10*time.Minute),
	)
}

func OnboardingCampaignSweep(args *OnboardingCampaignSweepArgs, clientSession *session.ClientSession) (*OnboardingCampaignSweepResult, error) {
	return sweepOnboardingCampaign(clientSession, server.NowUtc(), DefaultOnboardingCampaignSweepSettings()), nil
}

func OnboardingCampaignSweepPost(
	args *OnboardingCampaignSweepArgs,
	result *OnboardingCampaignSweepResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleOnboardingCampaignSweep(clientSession, tx, server.NowUtc().Add(DefaultOnboardingCampaignSweepSettings().Interval))
	return nil
}

// sweepOnboardingCampaign re-schedules the lost steps of the rows due before
// now minus the grace, at most BatchSize rows a run. A row is lost when no
// pending task (queued or running) holds its next step's run-once key. Each
// row moves and gets its task in one transaction, and only while its next
// step is still the one read, so a row that moved on meanwhile is left alone.
func sweepOnboardingCampaign(clientSession *session.ClientSession, now time.Time, settings *OnboardingCampaignSweepSettings) *OnboardingCampaignSweepResult {
	ctx := clientSession.Ctx
	result := &OnboardingCampaignSweepResult{}
	rows := model.ListOnboardingRowsDueBefore(ctx, now.Add(-settings.Grace), settings.BatchSize)
	keys := make([]string, 0, len(rows))
	for _, row := range rows {
		keys = append(keys, onboardingStepRunOnce(row.NetworkId, row.NextStep).String())
	}
	pending := task.PendingRunOnceKeys(ctx, keys)
	for i, row := range rows {
		if pending[keys[i]] {
			continue
		}
		result.Lost += 1
		step, lastStep, action := row.NextStep, "", onboardingSweepResumed
		if row.NextStep == onboarding.StepE1 && settings.FirstStepMaxAge < now.Sub(row.CreatedAt) {
			step, lastStep, action = onboarding.StepE2, onboarding.StepE1, onboardingSweepSkippedE1
		}
		zone := onboardingMailReader(ctx, row).SendZone
		at := onboarding.DueSendAt(now, zone.Loc(), zone.Window(onboardingSendWindow()))
		resumed := false
		server.Tx(ctx, func(tx server.PgTx) {
			resumed = model.ResumeNetworkOnboardingInTx(tx, ctx, row.NetworkId, row.NextStep, lastStep, step, at)
			if resumed {
				scheduleOnboardingCampaignStepInTx(tx, clientSession, row.NetworkId, step, at)
			}
		})
		if !resumed {
			continue
		}
		onboardingSweepRescheduledTotal.WithLabelValues(step, action).Inc()
		if action == onboardingSweepSkippedE1 {
			result.SkippedE1 += 1
		} else {
			result.Resumed += 1
		}
	}
	return result
}
