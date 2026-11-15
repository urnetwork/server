// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// onboardingStepTaskRunAt is the run time of the pending task for a network's
// campaign step, or nil when there is none.
func onboardingStepTaskRunAt(tb testing.TB, networkId server.Id, step string) *time.Time {
	tb.Helper()
	var runAt *time.Time
	server.Db(tb.Context(), func(conn server.PgConn) {
		result, err := conn.Query(tb.Context(), `SELECT run_at FROM pending_task WHERE run_once_key = $1`,
			onboardingStepRunOnce(networkId, step).String())
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var at time.Time
				server.Raise(result.Scan(&at))
				runAt = &at
			}
		})
	})
	return runAt
}

// EnrollNetworkOnboarding used to insert the row, upsert the Brevo contact and
// only then schedule E1, in a transaction of its own, all inside the
// post-primary timeout. When the context ended during the upsert (the 10 s
// deadline), the schedule failed, the panic was recovered, and the row kept
// next_step e1 with no task: its sequence never started (140 such rows on Main
// on 2026-11-15). Here the Brevo fixture cancels the request context during
// the upsert, deterministically; E1's task must already be committed with the
// row.
func TestEnrollNetworkOnboardingCommitsE1WithTheRow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		restore := model.Testing_SetOnboardingConfig(&model.OnboardingConfig{Enabled: true})
		defer restore()
		networkId, userId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-enroll-e1-"+networkId.String(), userId)
		model.AccountPreferencesSetForNetwork(ctx, networkId, true)

		// no client address: the fixture config has no GeoLite2 database for
		// the sign-up's IP lookup
		clientSession := session.NewLocalClientSession(ctx, "", &session.ByJwt{NetworkId: networkId, UserId: userId})
		defer clientSession.Cancel()
		upserts := 0
		newBrevoProtocolTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.Contains(r.URL.Path, "/contacts") {
				upserts += 1
				// the post-primary deadline runs out while Brevo answers
				clientSession.Cancel()
			}
			w.WriteHeader(http.StatusInternalServerError)
		}))

		EnrollNetworkOnboarding(clientSession, networkId, "synthetic-enroll-e1@example.com", false)

		if upserts == 0 {
			tb.Fatal("the fixture never saw the contact upsert, so it proved nothing")
		}
		row := model.GetNetworkOnboarding(ctx, networkId)
		if row == nil || row.Exited() || row.NextStep != onboarding.StepE1 || row.NextSendAt == nil {
			tb.Fatal("the campaign row was not created waiting for E1", row)
		}
		if runAt := onboardingStepTaskRunAt(tb, networkId, onboarding.StepE1); runAt == nil || !runAt.Equal(*row.NextSendAt) {
			tb.Fatal("E1 has no task at the row's send time: the sequence would never start", runAt, row.NextSendAt)
		}
	})
}

// The sweep repairs rows whose next step has no task: a lost E1 within the
// first step's age limit goes as E1, an older one joins at E2 without E1, and
// another lost step goes as itself, each at the next opening of the reader's
// send window. Rows with a pending task, rows not yet past the grace, and
// exited rows are left alone, and a second run finds nothing.
func TestOnboardingCampaignSweepReschedulesLostSteps(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()
		now := server.NowUtc()
		settings := DefaultOnboardingCampaignSweepSettings()
		settings.BatchSize = 1000

		create := func(name string, createdAgo time.Duration, step string, dueAgo time.Duration) server.Id {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "synthetic-sweep-"+name+"-"+networkId.String(), server.NewId())
			due := now.Add(-dueAgo)
			if !model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{
				NetworkId: networkId, CreatedAt: now.Add(-createdAgo), Email: true, TimeZone: "UTC",
				NextStep: step, NextSendAt: &due,
			}) {
				tb.Fatal("fixture row not created", name)
			}
			return networkId
		}
		recentE1 := create("recent-e1", 2*24*time.Hour, onboarding.StepE1, 24*time.Hour)
		oldE1 := create("old-e1", 10*24*time.Hour, onboarding.StepE1, 9*24*time.Hour)
		lostE3 := create("lost-e3", 6*24*time.Hour, onboarding.StepE3, 3*time.Hour)
		insideGrace := create("inside-grace", 2*24*time.Hour, onboarding.StepE1, 30*time.Minute)
		queued := create("queued", 2*24*time.Hour, onboarding.StepE2, 2*time.Hour)
		queuedAt := now.Add(time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			scheduleOnboardingCampaignStepInTx(tx, clientSession, queued, onboarding.StepE2, queuedAt)
		})
		exited := create("exited", 2*24*time.Hour, onboarding.StepE1, 24*time.Hour)
		model.ExitNetworkOnboarding(ctx, exited, onboarding.ExitOptOut, now)

		result := sweepOnboardingCampaign(clientSession, now, settings)
		if result.Resumed < 2 || result.SkippedE1 < 1 {
			tb.Fatal("sweep result", result)
		}

		// a recent lost E1 goes as E1, now (the UTC window is open or opens next)
		row := model.GetNetworkOnboarding(ctx, recentE1)
		runAt := onboardingStepTaskRunAt(tb, recentE1, onboarding.StepE1)
		if row.NextStep != onboarding.StepE1 || runAt == nil || !runAt.Equal(*row.NextSendAt) || runAt.Before(now) {
			tb.Fatal("recent E1 not resumed", row.NextStep, runAt, row.NextSendAt)
		}
		// an old lost E1 skips to E2, recorded as passed, never as sent
		row = model.GetNetworkOnboarding(ctx, oldE1)
		runAt = onboardingStepTaskRunAt(tb, oldE1, onboarding.StepE2)
		if row.NextStep != onboarding.StepE2 || row.LastStep != onboarding.StepE1 || row.E1SentAt != nil ||
			runAt == nil || !runAt.Equal(*row.NextSendAt) {
			tb.Fatal("old E1 did not join at E2", row.NextStep, row.LastStep, row.E1SentAt, runAt)
		}
		if onboardingStepTaskRunAt(tb, oldE1, onboarding.StepE1) != nil {
			tb.Fatal("old E1 was scheduled anyway")
		}
		// another lost step goes as itself
		row = model.GetNetworkOnboarding(ctx, lostE3)
		if runAt := onboardingStepTaskRunAt(tb, lostE3, onboarding.StepE3); row.NextStep != onboarding.StepE3 || runAt == nil {
			tb.Fatal("lost E3 not resumed", row.NextStep, runAt)
		}
		// left alone: inside the grace, a queued task, an exited row
		if onboardingStepTaskRunAt(tb, insideGrace, onboarding.StepE1) != nil {
			tb.Fatal("a step inside the grace was scheduled")
		}
		if runAt := onboardingStepTaskRunAt(tb, queued, onboarding.StepE2); runAt == nil || !runAt.Equal(queuedAt) {
			tb.Fatal("a queued task was moved", runAt)
		}
		if onboardingStepTaskRunAt(tb, exited, onboarding.StepE1) != nil {
			tb.Fatal("an exited row was scheduled")
		}

		// every repaired row has its task now: a second run finds none of them
		again := sweepOnboardingCampaign(clientSession, now, settings)
		for _, networkId := range []server.Id{recentE1, oldE1, lostE3} {
			row := model.GetNetworkOnboarding(ctx, networkId)
			keys := []string{onboardingStepRunOnce(networkId, row.NextStep).String()}
			if !task.PendingRunOnceKeys(ctx, keys)[keys[0]] {
				tb.Fatal("a repaired row lost its task", networkId)
			}
		}
		if again.Resumed+again.SkippedE1 != 0 {
			tb.Fatal("the second sweep re-scheduled rows", again)
		}
	})
}
