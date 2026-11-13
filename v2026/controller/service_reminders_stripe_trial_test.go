package controller

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// mmm/ONBOARDING-RETENTION.md P4: ur.io promises a reminder before a free
// trial's first charge, and the trial-ending notice reached only App Store and
// Play trials, found through the app's purchase.completed{trial}. The web
// sends no purchase.completed for a card purchase, so a Stripe trial got the
// notice only if Stripe's own dashboard reminder was on, which nobody could
// confirm. Stripe's customer.subscription.trial_will_end, three days before
// the trial ends, now queues it.
func TestStripeTrialEndingNotice(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	networkId := server.NewId()
	trialEnd := now.Add(3 * 24 * time.Hour)
	sub := func(mutate func(s *stripeTrialWillEndSubscription)) *stripeTrialWillEndSubscription {
		s := &stripeTrialWillEndSubscription{
			Id: "sub_trial", Status: "trialing", TrialEnd: trialEnd.Unix(),
			Metadata:             map[string]string{stripeMetadataNetworkId: networkId.String(), stripeMetadataPaymentSheet: stripePaymentSheetAttached},
			DefaultPaymentMethod: json.RawMessage(`"pm_card"`),
		}
		if mutate != nil {
			mutate(s)
		}
		return s
	}
	gotId, gotEnd, ok := stripeTrialEndingNotice(sub(nil), now)
	if !ok || gotId != networkId || !gotEnd.Equal(trialEnd) {
		t.Fatalf("a trial ending in 3 days: %s %s %t", gotId, gotEnd, ok)
	}
	// a Checkout subscription carries no payment-sheet metadata
	if _, _, ok := stripeTrialEndingNotice(sub(func(s *stripeTrialWillEndSubscription) {
		delete(s.Metadata, stripeMetadataPaymentSheet)
		s.DefaultPaymentMethod = json.RawMessage(`null`)
	}), now); !ok {
		t.Error("a Checkout trial gets no notice")
	}
	for name, mutate := range map[string]func(s *stripeTrialWillEndSubscription){
		"cancelled during the trial": func(s *stripeTrialWillEndSubscription) { s.CancelAtPeriodEnd = true },
		"no longer trialing":         func(s *stripeTrialWillEndSubscription) { s.Status = "active" },
		"canceled":                   func(s *stripeTrialWillEndSubscription) { s.Status = "canceled" },
		"an abandoned payment sheet": func(s *stripeTrialWillEndSubscription) {
			s.Metadata[stripeMetadataPaymentSheet] = stripePaymentSheetPending
			s.DefaultPaymentMethod = json.RawMessage(`null`)
		},
		"a trial ended at once": func(s *stripeTrialWillEndSubscription) { s.TrialEnd = now.Add(time.Minute).Unix() },
		"no trial end":          func(s *stripeTrialWillEndSubscription) { s.TrialEnd = 0 },
		"no network":            func(s *stripeTrialWillEndSubscription) { delete(s.Metadata, stripeMetadataNetworkId) },
		"no subscription id":    func(s *stripeTrialWillEndSubscription) { s.Id = "" },
	} {
		if _, _, ok := stripeTrialEndingNotice(sub(mutate), now); ok {
			t.Errorf("%s: a notice", name)
		}
	}
	// a payment sheet whose card arrived before its webhook is a real trial
	if _, _, ok := stripeTrialEndingNotice(sub(func(s *stripeTrialWillEndSubscription) {
		s.Metadata[stripeMetadataPaymentSheet] = stripePaymentSheetPending
	}), now); !ok {
		t.Error("a payment sheet with a card gets no notice")
	}
	if stripeTrialEndingKey(networkId, "sub_trial") != fmt.Sprintf("trial_ending/%s/stripe/sub_trial", networkId) {
		t.Error("the outbox key is not one per subscription")
	}
}

// Which stores get the notice, and where each store's trials come from.
func TestTrialEndingStores(t *testing.T) {
	defaults := model.ReengagementServiceRemindersConfig{TrialEnding: true}
	if trialEndingStoreEnabled(defaults, model.OnboardingStoreStripe) || !trialEndingStoreEnabled(defaults, model.OnboardingStoreApple) {
		t.Error("the default stores are apple and play")
	}
	withStripe := model.ReengagementServiceRemindersConfig{TrialEnding: true, TrialEndingStores: []string{model.OnboardingStoreApple, model.OnboardingStorePlay, model.OnboardingStoreStripe}}
	if !trialEndingStoreEnabled(withStripe, model.OnboardingStoreStripe) {
		t.Error("stripe in trial_ending_stores is not enabled")
	}
	withStripe.TrialEnding = false
	if trialEndingStoreEnabled(withStripe, model.OnboardingStoreStripe) {
		t.Error("trial_ending off still sends")
	}
	// the daily task never reads Stripe's trials: trial_will_end has them,
	// and an app's Stripe trial would get a second notice
	if got := clientRecordedTrialStores(withStripe.TrialEndingStores); !slices.Equal(got, []string{model.OnboardingStoreApple, model.OnboardingStorePlay}) {
		t.Errorf("the daily task's stores: %v", got)
	}
}

// The footer says when the notice goes: two days before for the daily task's
// store trials, three for Stripe's trial_will_end. The Stripe notice links
// ur.io's Manage subscription.
func TestTrialEndingTemplateNamesItsLead(t *testing.T) {
	end := time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)
	for store, want := range map[string]string{
		model.OnboardingStoreApple:  "two days before",
		model.OnboardingStorePlay:   "two days before",
		model.OnboardingStoreStripe: "three days before",
	} {
		_, html, text, err := RenderEmailTemplate(&TrialEndingTemplate{EndTime: end, Store: store})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.ReplaceAll(text, "\n", " "), "account "+want+" a UR Pro free trial ends") || !strings.Contains(html, "account "+want+" a UR Pro free trial ends") {
			t.Errorf("%s: the footer does not say %q:\n%s", store, want, text)
		}
	}
	_, html, text, err := RenderEmailTemplate(&TrialEndingTemplate{EndTime: end, Store: model.OnboardingStoreStripe})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "November 23, 2026") || !strings.Contains(text, "https://ur.io/app/subscription") || !strings.Contains(html, "Manage subscription</a>") {
		t.Errorf("the Stripe notice lacks the date or the manage link:\n%s", text)
	}
}

// The Stripe webhook queues one notice per trial: a replayed event adds
// nothing, and without stripe in trial_ending_stores it queues none.
func TestStripeTrialWillEndQueuesTheNotice(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		event := func(networkId server.Id, subscriptionId string, trialEnd time.Time) *StripeWebhookArgs {
			object, err := json.Marshal(map[string]any{
				"id": subscriptionId, "object": "subscription", "status": "trialing",
				"trial_end": trialEnd.Unix(), "cancel_at_period_end": false,
				"default_payment_method": "pm_synthetic",
				"metadata":               map[string]string{stripeMetadataNetworkId: networkId.String()},
			})
			if err != nil {
				t.Fatal(err)
			}
			return &StripeWebhookArgs{Id: "evt_" + subscriptionId, Type: "customer.subscription.trial_will_end", Data: &StripeEventData{Object: object}}
		}
		trialEnd := server.NowUtc().Add(3 * 24 * time.Hour).Truncate(time.Second)

		// stripe not in trial_ending_stores: nothing
		restore := model.Testing_SetReengagementConfig(&model.ReengagementConfig{ServiceReminders: model.ReengagementServiceRemindersConfig{TrialEnding: true}})
		offId := server.NewId()
		model.Testing_CreateNetwork(ctx, offId, "synthetic-trial-off", server.NewId())
		if _, err := StripeWebhook(event(offId, "sub_synthetic_off", trialEnd), owner); err != nil {
			t.Fatal(err)
		}
		if message := model.GetAccountMessageByKey(ctx, (&TrialEndingTemplate{}).Name(), stripeTrialEndingKey(offId, "sub_synthetic_off")); message != nil {
			t.Fatal("a notice without stripe in trial_ending_stores")
		}
		restore()

		restore = model.Testing_SetReengagementConfig(&model.ReengagementConfig{ServiceReminders: model.ReengagementServiceRemindersConfig{
			TrialEnding: true, TrialEndingStores: []string{model.OnboardingStoreApple, model.OnboardingStorePlay, model.OnboardingStoreStripe},
		}})
		defer restore()
		networkId := server.NewId()
		userAuth := model.Testing_CreateNetwork(ctx, networkId, "synthetic-trial-on", server.NewId())
		for range 2 {
			if _, err := StripeWebhook(event(networkId, "sub_synthetic_on", trialEnd), owner); err != nil {
				t.Fatal(err)
			}
		}
		message := model.GetAccountMessageByKey(ctx, (&TrialEndingTemplate{}).Name(), stripeTrialEndingKey(networkId, "sub_synthetic_on"))
		if message == nil || message.NetworkId == nil || *message.NetworkId != networkId || message.UserAuth != userAuth {
			t.Fatalf("the notice: %+v", message)
		}
		var template TrialEndingTemplate
		if err := json.Unmarshal([]byte(message.TemplateJson), &template); err != nil {
			t.Fatal(err)
		}
		if template.Store != model.OnboardingStoreStripe || !template.EndTime.Equal(trialEnd) {
			t.Errorf("the notice's template: %+v", template)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM account_message_outbox WHERE network_id = $1`, networkId).Scan(&count))
			if count != 1 {
				t.Errorf("a replayed trial_will_end queued %d notices", count)
			}
		})
	})
}
