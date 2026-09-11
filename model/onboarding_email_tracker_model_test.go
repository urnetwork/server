package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/onboarding"
)

// TestOnboardingEmailTrackerAttribution is the synthetic root-cause test for
// the E3/E5 collision: both can use e3_last_chance, so only an explicit
// flow_step may attribute their engagement. Webhook retries count one network,
// duplicate accepted sends count one exposure, and product activity belongs to
// the most recent send window.
func TestOnboardingEmailTrackerAttribution(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		networkA := server.NewId()
		networkB := server.NewId()
		networkC := server.NewId()
		networkD := server.NewId()

		emails := []*NetworkOnboardingEmail{
			{MessageId: "synthetic-a-e3", NetworkId: networkA, Step: onboarding.StepE3, Template: onboarding.TemplateE3LastChance, Variant: onboarding.VariantEngaged, SentAt: day.Add(8 * time.Hour)},
			{MessageId: "synthetic-a-e5", NetworkId: networkA, Step: onboarding.StepE5, Template: onboarding.TemplateE3LastChance, Variant: onboarding.VariantEngaged, SentAt: day.Add(4*24*time.Hour + 8*time.Hour)},
			{MessageId: "synthetic-b-e1", NetworkId: networkB, Step: onboarding.StepE1, Template: onboarding.TemplateE1Connect, Variant: onboarding.VariantDefault, SentAt: day.Add(24*time.Hour + 8*time.Hour)},
			// A provider retry can yield another accepted message id. The
			// tracker still counts this network/flow-step once.
			{MessageId: "synthetic-b-e1-retry", NetworkId: networkB, Step: onboarding.StepE1, Template: onboarding.TemplateE1Connect, Variant: onboarding.VariantDefault, SentAt: day.Add(24*time.Hour + 9*time.Hour)},
			{MessageId: "synthetic-c-e3", NetworkId: networkC, Step: onboarding.StepE3, Template: onboarding.TemplateE3LastChance, Variant: onboarding.VariantNotActivated, SentAt: day.Add(2*24*time.Hour + 8*time.Hour)},
			{MessageId: "synthetic-c-e5", NetworkId: networkC, Step: onboarding.StepE5, Template: onboarding.TemplateE3LastChance, Variant: onboarding.VariantNotActivated, SentAt: day.Add(6*24*time.Hour + 8*time.Hour)},
			{MessageId: "synthetic-d-e2", NetworkId: networkD, Step: onboarding.StepE2, Template: onboarding.TemplateE2Widget, Variant: onboarding.VariantDefault, SentAt: day.Add(24*time.Hour + 8*time.Hour)},
		}
		server.Tx(ctx, func(tx server.PgTx) {
			for _, email := range emails {
				AddNetworkOnboardingEmailInTx(tx, ctx, email)
			}
		})

		event := func(networkId server.Id, name string, at time.Time, props map[string]any) *OnboardingEvent {
			return &OnboardingEvent{NetworkId: networkId, Name: name, At: at, ReceivedAt: at, Props: props}
		}
		err := AddOnboardingEvents(ctx, []*OnboardingEvent{
			// Exact flow attribution remains correct even after the next send.
			event(networkA, EventEmailOpened, day.Add(5*24*time.Hour), map[string]any{"step": onboarding.TemplateE3LastChance, "flow_step": onboarding.StepE3}),
			event(networkA, EventEmailClicked, day.Add(5*24*time.Hour), map[string]any{"step": onboarding.TemplateE3LastChance, "flow_step": onboarding.StepE5}),
			event(networkA, EventConnectDay, day.Add(2*24*time.Hour), nil),
			event(networkA, EventWidgetAdded, day.Add(5*24*time.Hour), map[string]any{"kind": "synthetic_widget"}),

			// A unique legacy template remains attributable, but webhook
			// duplicates still count one network.
			event(networkB, EventEmailOpened, day.Add(2*24*time.Hour), map[string]any{"step": onboarding.TemplateE1Connect}),
			event(networkB, EventEmailOpened, day.Add(2*24*time.Hour+time.Minute), map[string]any{"step": onboarding.TemplateE1Connect}),
			event(networkB, EventLandingClicked, day.Add(2*24*time.Hour), map[string]any{"step": onboarding.TemplateE1Connect, "flow_step": onboarding.StepE1}),
			event(networkB, EventPurchaseCompleted, day.Add(2*24*time.Hour), map[string]any{"store": OnboardingStorePlay}),
			// Provider delivery corrections remain attributable for 30 days,
			// while product engagement after the 14-day ceiling does not.
			event(networkB, EventEmailDelivered, day.Add(20*24*time.Hour), map[string]any{"step": onboarding.TemplateE1Connect, "flow_step": onboarding.StepE1}),
			event(networkB, EventConnectDay, day.Add(20*24*time.Hour), nil),

			// This legacy event could mean E3 or E5. It must be surfaced as
			// ambiguous and must not inflate either opened count.
			event(networkC, EventEmailOpened, day.Add(4*24*time.Hour), map[string]any{"step": onboarding.TemplateE3LastChance}),

			// App-open attribution must preserve the landing token's exact
			// flow step rather than falling back to the shared template.
			event(networkD, EventLandingClicked, day.Add(2*24*time.Hour), map[string]any{"step": onboarding.TemplateE2Widget, "flow_step": onboarding.StepE2}),
		})
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, true, AttributeAppOpen(ctx, networkD, day.Add(2*24*time.Hour+time.Minute)))
		connect.AssertEqual(t, false, AttributeAppOpen(ctx, networkD, day.Add(2*24*time.Hour+2*time.Minute)))

		computedAt := day.Add(30 * 24 * time.Hour)
		rows, err := RebuildOnboardingEmailTracker(ctx, day, day.Add(9*24*time.Hour), computedAt)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, int64(6), rows)

		totals := SumOnboardingEmailTracker(ctx, day, day.Add(9*24*time.Hour))
		e1 := totals[onboarding.StepE1]
		connect.AssertEqual(t, int64(1), e1.Sent)
		connect.AssertEqual(t, int64(1), e1.Delivered)
		connect.AssertEqual(t, int64(1), e1.Opened)
		connect.AssertEqual(t, int64(1), e1.LandingClicked)
		connect.AssertEqual(t, int64(1), e1.ProStarted)
		connect.AssertEqual(t, int64(1), e1.Engaged)
		connect.AssertEqual(t, int64(0), e1.Connected)
		e2 := totals[onboarding.StepE2]
		connect.AssertEqual(t, int64(1), e2.Sent)
		connect.AssertEqual(t, int64(1), e2.AppOpened)
		connect.AssertEqual(t, int64(1), e2.Engaged)

		e3 := totals[onboarding.StepE3]
		connect.AssertEqual(t, int64(2), e3.Sent)
		connect.AssertEqual(t, int64(1), e3.Opened)
		connect.AssertEqual(t, int64(1), e3.Connected)
		connect.AssertEqual(t, int64(1), e3.Engaged)
		connect.AssertEqual(t, int64(1), e3.AttributionAmbiguous)

		e5 := totals[onboarding.StepE5]
		connect.AssertEqual(t, int64(2), e5.Sent)
		connect.AssertEqual(t, int64(1), e5.Clicked)
		connect.AssertEqual(t, int64(1), e5.WidgetAdded)
		connect.AssertEqual(t, int64(1), e5.Engaged)
		connect.AssertEqual(t, int64(0), e5.AttributionAmbiguous)

		// Rebuilding is replacement, not accumulation.
		_, err = RebuildOnboardingEmailTracker(ctx, day, day.Add(9*24*time.Hour), computedAt.Add(time.Minute))
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, int64(1), SumOnboardingEmailTracker(ctx, day, day.Add(9*24*time.Hour))[onboarding.StepE1].Sent)

		page, next := ListOnboardingEmailTracker(ctx, OnboardingEmailTrackerFilter{From: day, To: day.Add(9 * 24 * time.Hour), Limit: 1})
		connect.AssertEqual(t, 1, len(page))
		connect.AssertEqual(t, true, next != nil)
		page2, _ := ListOnboardingEmailTracker(ctx, OnboardingEmailTrackerFilter{From: day, To: day.Add(9 * 24 * time.Hour), After: next, Limit: 10})
		connect.AssertEqual(t, 5, len(page2))
	})
}
