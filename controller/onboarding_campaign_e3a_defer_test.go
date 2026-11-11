package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// mmm/ONBOARDING-RETENTION.md P7: an in-app offer seen after sign-up still has
// days left when E3a runs on day 4, and the step used to be skipped, so the
// network never got its last-chance email. The step now waits: it stays the
// row's next step, sends nothing, and its task runs again on the offer's last
// day, where the decision sends.
func TestOnboardingCampaignE3aWaitsForALateOffersLastDay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-campaign-late-offer", server.NewId())
		now := server.NowUtc()
		createdAt := now.Add(-4 * 24 * time.Hour)
		if !model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{NetworkId: networkId, CreatedAt: createdAt,
			Email: true, TimeZone: "UTC", Path: onboarding.PathA, NextStep: onboarding.StepE3, NextSendAt: &now}) {
			t.Fatal("campaign fixture missing")
		}
		// the offer screen seen two days after sign-up: three days left on day 4
		offer, created, err := model.IssueOnboardingOffer(ctx, &model.IssueOnboardingOfferArgs{
			NetworkId: networkId, IssuedBy: model.OnboardingOfferIssuedByInApp, Surface: "final_screen",
			Tier: "standard", PercentOff: 25, MonthsFree: 3, Validity: 3 * 24 * time.Hour,
		})
		if err != nil || !created {
			t.Fatal("offer fixture", err)
		}
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()

		result, err := OnboardingCampaignStep(&OnboardingCampaignStepArgs{NetworkId: networkId, Step: onboarding.StepE3}, owner)
		if err != nil {
			t.Fatal(err)
		}
		want := onboarding.LastChanceAt(offer.ExpiresAt, time.UTC, onboarding.DefaultSendWindow)
		if result.Action != onboarding.ActionDefer || result.NextStep != onboarding.StepE3 || result.NextSendAt == nil || !result.NextSendAt.Equal(want) {
			t.Fatalf("e3a with the offer's last day ahead: %+v, want a deferral to %s", result, want)
		}
		if !onboarding.OfferEndsSoon(offer.ExpiresAt, want) {
			t.Fatalf("the deferred e3a at %s would not find the offer ending soon (expires %s)", want, offer.ExpiresAt)
		}
		row := model.GetNetworkOnboarding(ctx, networkId)
		if row.Exited() || row.NextStep != onboarding.StepE3 || row.NextSendAt == nil || !row.NextSendAt.Equal(want) || row.E3SentAt != nil {
			t.Fatalf("the row after the deferral: %+v", row)
		}
		if emails := model.ListNetworkOnboardingEmails(ctx, networkId); len(emails) != 0 {
			t.Fatalf("a deferral sent %d emails", len(emails))
		}

		// the post schedules the same step at its moment
		target := task.NewTaskTargetWithPost(OnboardingCampaignStep, OnboardingCampaignStepPost)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(OnboardingCampaignStepPost(&OnboardingCampaignStepArgs{NetworkId: networkId, Step: onboarding.StepE3}, result, owner, tx))
		})
		server.Db(ctx, func(conn server.PgConn) {
			var scheduled bool
			server.Raise(conn.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(run_at=$3)
				FROM pending_task WHERE function_name=$1 AND args_json::jsonb->>'step'=$2 AND args_json::jsonb->>'network_id'=$4`,
				target.TargetFunctionName(), onboarding.StepE3, want, networkId.String()).Scan(&scheduled))
			if !scheduled {
				t.Fatal("the deferred e3a is not scheduled at its last-chance moment")
			}
		})
	})
}
