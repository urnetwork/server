package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/onboarding"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// Lose task completion after the real body advances the campaign. A retry
// must rebuild the successor from that durable state, without another send.
func TestOnboardingCampaignTaskRecoversCommittedSuccessorOnReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-campaign-replay", server.NewId())
		now := server.NowUtc()
		if !model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{NetworkId: networkId, CreatedAt: now,
			Email: true, TimeZone: "UTC", NextStep: onboarding.StepE1, NextSendAt: &now}) {
			t.Fatal("campaign fixture missing")
		}
		// The real E1 decision skips its nudge after a connection. No email
		// renderer, transport or credential can be reached by this fixture.
		server.Raise(model.AddOnboardingEvent(ctx, &model.OnboardingEvent{NetworkId: networkId, Name: model.EventConnectFirst, At: now}))
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		target := task.NewTaskTargetWithPost(OnboardingCampaignStep, OnboardingCampaignStepPost)
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(target)
		id := task.ScheduleTask(OnboardingCampaignStep, &OnboardingCampaignStepArgs{NetworkId: networkId, Step: onboarding.StepE1}, owner,
			task.RunAt(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)))
		if _, _, err := target.RunSpecific(ctx, task.GetTasks(ctx, id)[id]); err != nil {
			t.Fatal("first campaign body failed", err)
		}
		row := model.GetNetworkOnboarding(ctx, networkId)
		if row.NextStep != onboarding.StepE2 || row.LastStep != onboarding.StepE1 || row.NextSendAt == nil {
			t.Fatal("body did not advance", row)
		}
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || finished[0] != id || len(retried)+len(posts) != 0 {
			t.Fatal("campaign replay did not finish", finished, retried, posts, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var successor, completed bool
			var emails int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(args_json::jsonb->>'step'=$2 AND run_at=$3)
				FROM pending_task WHERE function_name=$1`, target.TargetFunctionName(), onboarding.StepE2, *row.NextSendAt).Scan(&successor))
			server.Raise(conn.QueryRow(ctx, `SELECT post_completed FROM finished_task WHERE task_id=$1`, id).Scan(&completed))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_onboarding_email WHERE network_id=$1`, networkId).Scan(&emails))
			if !successor || !completed || emails != 0 {
				t.Fatal("committed campaign advance lost or repeated its successor", successor, completed, emails)
			}
		})
		// A stopped or removed campaign must still drain stale task rows.
		model.ExitNetworkOnboarding(ctx, networkId, "synthetic-stop", now)
		for _, id := range []server.Id{networkId, server.NewId()} {
			result, err := OnboardingCampaignStep(&OnboardingCampaignStepArgs{NetworkId: id, Step: onboarding.StepE1}, owner)
			if err != nil || result.NextStep != "" || result.NextSendAt != nil {
				t.Fatal("stopped campaign revived a successor", result, err)
			}
		}
	})
}
