package controller

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/session"
)

// Every campaign step that asks whether a network connected (support inbox
// 8146). The connection path writes one connect.day event per network and UTC
// day, and RemoveDisconnectedNetworkClients prunes a closed connection row 8 h
// after the disconnect; apps that predate connect.first never send it. A
// network whose connections show only in connect.day must get E1's skip, E2's
// activated email and the engaged feedback email on both paths, never the
// "never connected" texts a network without any connection gets.
func TestOnboardingCampaignCountsConnectDayAsConnected(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		// enrolled 6 days ago, so every step up to the feedback email is due
		createdAt := now.Add(-6 * 24 * time.Hour)

		enroll := func(path string) server.Id {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-campaign-%s", networkId), server.NewId())
			// a Play phone on a build with the widget: E2's activated
			// email goes only there (onboarding.WidgetSettings)
			if !model.CreateNetworkOnboarding(ctx, &model.NetworkOnboarding{NetworkId: networkId, CreatedAt: createdAt,
				Path: path, Email: true, TimeZone: "UTC", Platform: "android", Distribution: session.ClientDistributionPlay,
				NextStep: onboarding.StepE1, NextSendAt: &now}) {
				t.Fatal("campaign fixture missing")
			}
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic phone", "17.1 Pixel 8 Pro")
			model.SetDeviceClientVersion(ctx, clientId, "2026.10.15-1072765730")
			return networkId
		}
		// what is left of connections on three days once their rows are pruned
		addConnectDays := func(networkId server.Id) {
			for _, day := range []int{0, 3, 4} {
				server.Raise(model.AddOnboardingEvent(ctx, &model.OnboardingEvent{
					NetworkId: networkId,
					Name:      model.EventConnectDay,
					At:        createdAt.Add(time.Duration(day)*24*time.Hour + time.Hour),
				}))
			}
		}

		connectedB := enroll(onboarding.PathB)
		addConnectDays(connectedB)
		connectedA := enroll(onboarding.PathA)
		addConnectDays(connectedA)
		neverB := enroll(onboarding.PathB)

		for _, c := range []struct {
			name      string
			networkId server.Id
			step      string
			action    string
			template  string
			variant   string
		}{
			{"E1 connected", connectedB, onboarding.StepE1, onboarding.ActionSkip, "", ""},
			{"E2 connected", connectedB, onboarding.StepE2, onboarding.ActionSend, onboarding.TemplateE2Widget, onboarding.VariantActivated},
			{"E3b feedback connected", connectedB, onboarding.StepE3, onboarding.ActionSend, onboarding.TemplateE4Feedback, onboarding.VariantEngaged},
			{"E4a feedback connected", connectedA, onboarding.StepE4, onboarding.ActionSend, onboarding.TemplateE4Feedback, onboarding.VariantEngaged},
			{"E1 never connected", neverB, onboarding.StepE1, onboarding.ActionSend, onboarding.TemplateE1Connect, onboarding.VariantDefault},
			{"E2 never connected", neverB, onboarding.StepE2, onboarding.ActionSend, onboarding.TemplateE2Widget, onboarding.VariantNotActivated},
			{"E3b feedback never connected", neverB, onboarding.StepE3, onboarding.ActionSend, onboarding.TemplateE4Feedback, onboarding.VariantNotActivated},
		} {
			preview, err := OnboardingCampaignPreview(ctx, c.networkId, c.step)
			if err != nil {
				t.Fatalf("%s: %s", c.name, err)
			}
			d := preview.Decision
			if d.Action != c.action || d.Template != c.template || d.Variant != c.variant {
				t.Errorf("%s: %s %s/%s, want %s %s/%s", c.name, d.Action, d.Template, d.Variant, c.action, c.template, c.variant)
			}
		}

		// The step task itself skips E1's connect nudge and moves on to E2.
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		result, err := OnboardingCampaignStep(&OnboardingCampaignStepArgs{NetworkId: connectedB, Step: onboarding.StepE1}, owner)
		if err != nil {
			t.Fatalf("E1 step: %s", err)
		}
		if result.Action != onboarding.ActionSkip || result.NextStep != onboarding.StepE2 {
			t.Fatalf("E1 step: %+v, want a skip to E2", result)
		}
		if emails := model.ListNetworkOnboardingEmails(ctx, connectedB); len(emails) != 0 {
			t.Fatalf("E1 step sent %d emails", len(emails))
		}
	})
}
