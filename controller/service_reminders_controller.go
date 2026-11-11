// SPDX-License-Identifier: MPL-2.0

package controller

// Service reminders (mmm/reengagement/PLAN.md "SERVICE REMINDERS", stream P):
// account messages about the network's own subscription, sent through the
// account message outbox whatever its marketing preference.
//
//   - Pro paid in crypto (USDC, Monero, Zcash) is a one-time payment that never
//     renews: without a reminder every crypto subscriber lapses by default. A
//     notice 14 days and 2 days before Pro stops, inside the 30-day grace.
//   - A free trial ends in a charge: the offer screen and ur.io promise "a
//     reminder before the charge" (store key offer_timeline_reminder;
//     mmm/onboarding/REVIEW.md finding 6). A notice 2 days before an App Store
//     or Play trial (purchase.completed{trial}) ends, unless its outcome is
//     already recorded. A Stripe trial (the web's card checkout and the
//     desktop apps') gets its notice when Stripe posts
//     customer.subscription.trial_will_end, 3 days before the trial ends:
//     the web sends no purchase.completed for a card purchase, so the daily
//     task never saw those trials, and nobody could confirm Stripe's own
//     dashboard reminder (mmm/ONBOARDING-RETENTION.md P4). Each store gets
//     the notice when service_reminders.trial_ending_stores names it.
//
// A daily task finds the crypto years and the store trials and adds one
// outbox message per window and notice; the outbox key makes a replayed run
// (or a replayed Stripe event) add nothing. Both kinds are off until
// reengagement.yml `service_reminders` turns them on; off, the task counts.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// ----- templates -----

// ProEndingTemplate is the notice that Pro paid in crypto is about to stop.
// A crypto year never renews by itself; when it ends Pro keeps working through
// a 30-day grace, and the apps show their renewal notice in that grace
// (CryptoTerm). A renewal starts a new year on the day it is paid, so the
// notices come inside the grace, where renewing loses no paid day.
type ProEndingTemplate struct {
	// when the paid year ended (CryptoTerm.TermEndTime)
	TermEndTime time.Time
	// when Pro stops (CryptoTerm.GraceEndTime, the window's end_time)
	EndTime time.Time
	// solana | monero | zcash (the subscription_renewal market; solana = USDC)
	Asset string
	BaseTemplate
}

func (self *ProEndingTemplate) Name() string {
	return "pro_ending"
}

func (self *ProEndingTemplate) EndDate() string {
	return self.EndTime.UTC().Format("January 2, 2006")
}

func (self *ProEndingTemplate) EndDateUpper() string {
	return strings.ToUpper(self.EndDate())
}

func (self *ProEndingTemplate) TermEndDate() string {
	return self.TermEndTime.UTC().Format("January 2, 2006")
}

func (self *ProEndingTemplate) AssetName() string {
	switch self.Asset {
	case model.SubscriptionMarketMonero:
		return "Monero"
	case model.SubscriptionMarketZcash:
		return "Zcash"
	case model.SubscriptionMarketBittensor:
		return "TAO"
	case model.SubscriptionMarketBitcoin:
		return "Bitcoin"
	case model.SubscriptionMarketEthereum:
		return "ETH"
	case cryptoReminderAssetSol:
		return "SOL"
	default:
		return "USDC"
	}
}

// The reminder's asset for a term paid in native SOL: its market is solana,
// like USDC's, so the reminder names the coin from the invoice.
const cryptoReminderAssetSol = "solana_sol"

func (self *ProEndingTemplate) RenewUrl() string {
	return "https://ur.io/app/subscription"
}

// TrialEndingTemplate is the reminder before a free trial's first charge.
type TrialEndingTemplate struct {
	EndTime time.Time
	// the purchase.completed store: apple | play | stripe
	Store string
	BaseTemplate
}

func (self *TrialEndingTemplate) Name() string {
	return "trial_ending"
}

func (self *TrialEndingTemplate) EndDate() string {
	return self.EndTime.UTC().Format("January 2, 2006")
}

func (self *TrialEndingTemplate) EndDateUpper() string {
	return strings.ToUpper(self.EndDate())
}

func (self *TrialEndingTemplate) Stripe() bool {
	return self.Store != model.OnboardingStoreApple && self.Store != model.OnboardingStorePlay
}

func (self *TrialEndingTemplate) ManageUrl() string {
	return "https://ur.io/app/subscription"
}

// ChargeLine says who charges what, without quoting a price (the store shows
// the reader's own price and currency).
func (self *TrialEndingTemplate) ChargeLine() string {
	switch self.Store {
	case model.OnboardingStoreApple:
		return "If you keep it, your subscription starts that day and Apple charges the price you saw when you started the trial."
	case model.OnboardingStorePlay:
		return "If you keep it, your subscription starts that day and Google Play charges the price you saw when you started the trial."
	default:
		return "If you keep it, your subscription starts that day and we charge your card the price you saw when you started the trial."
	}
}

func (self *TrialEndingTemplate) CancelLine() string {
	switch self.Store {
	case model.OnboardingStoreApple:
		// "apple" covers iOS and macOS trials alike
		return "To cancel before then, open Subscriptions in your Apple Account settings (on an iPhone: Settings, your name, Subscriptions)."
	case model.OnboardingStorePlay:
		return "To cancel before then, open the Play Store, tap your profile, then Payments & subscriptions."
	default:
		return "To cancel before then, sign in at ur.io and open Manage Subscription: " + self.ManageUrl()
	}
}

func (self *TrialEndingTemplate) CancelShort() string {
	switch self.Store {
	case model.OnboardingStoreApple:
		return "Cancel in your App Store subscriptions."
	case model.OnboardingStorePlay:
		return "Cancel in your Play Store subscriptions."
	default:
		return "Cancel at " + self.ManageUrl()
	}
}

// NoticeWhen is how long before the trial's end the notice goes, for the
// footer's "why" line: the daily task's 2 days for a store trial, Stripe's
// trial_will_end 3 days for a Stripe trial.
func (self *TrialEndingTemplate) NoticeWhen() string {
	if self.Stripe() {
		return "three days"
	}
	return "two days"
}

// ----- the Stripe trial notice -----

// stripeTrialWillEndSubscription is the customer.subscription.trial_will_end
// payload's fields the notice needs.
type stripeTrialWillEndSubscription struct {
	Id                   string            `json:"id"`
	Status               string            `json:"status"`
	TrialEnd             int64             `json:"trial_end"`
	CancelAtPeriodEnd    bool              `json:"cancel_at_period_end"`
	Metadata             map[string]string `json:"metadata"`
	DefaultPaymentMethod json.RawMessage   `json:"default_payment_method"`
}

// stripeTrialEndingMinLead is how far ahead a trial's end must be for its
// notice: Stripe also posts trial_will_end when a trial is ended at once
// (trial_end=now), and "ends on" a date already reached would be no notice.
const stripeTrialEndingMinLead = 24 * time.Hour

// stripeTrialEndingNotice is the trial-ending notice a trial_will_end payload
// calls for: the network and the trial's end, or ok false when no charge is
// coming. A trial the customer already cancelled (cancel_at_period_end), a
// subscription that is no longer trialing, a payment sheet the customer
// abandoned before adding a card (canceled and replaced, never charged), a
// trial that ends within stripeTrialEndingMinLead and a subscription with no
// network get none.
func stripeTrialEndingNotice(sub *stripeTrialWillEndSubscription, now time.Time) (networkId server.Id, endTime time.Time, ok bool) {
	if sub == nil || sub.Id == "" || sub.Status != "trialing" || sub.CancelAtPeriodEnd || sub.TrialEnd <= 0 {
		return
	}
	if sub.Metadata[stripeMetadataPaymentSheet] == stripePaymentSheetPending && stripeIdFromRaw(sub.DefaultPaymentMethod) == "" {
		return
	}
	endTime = time.Unix(sub.TrialEnd, 0).UTC()
	if endTime.Before(now.Add(stripeTrialEndingMinLead)) {
		return
	}
	networkId, err := server.ParseId(sub.Metadata[stripeMetadataNetworkId])
	if err != nil {
		return
	}
	return networkId, endTime, true
}

// stripeTrialEndingKey is the outbox key of a Stripe trial's notice: one per
// subscription, so a replayed event adds nothing.
func stripeTrialEndingKey(networkId server.Id, subscriptionId string) string {
	return fmt.Sprintf("trial_ending/%s/stripe/%s", networkId, subscriptionId)
}

// trialEndingStoreEnabled is whether service_reminders sends the trial-ending
// notice for a store's trials.
func trialEndingStoreEnabled(cfg model.ReengagementServiceRemindersConfig, store string) bool {
	return cfg.TrialEnding && slices.Contains(cfg.EffectiveTrialEndingStores(), store)
}

// clientRecordedTrialStores are the configured stores whose trials the daily
// task finds through the app's purchase.completed{trial}. Stripe's are not
// among them: its trial_will_end decides those (an app's Stripe trial would
// otherwise get a second notice).
func clientRecordedTrialStores(stores []string) []string {
	out := []string{}
	for _, store := range stores {
		if store != model.OnboardingStoreStripe {
			out = append(out, store)
		}
	}
	return out
}

// stripeHandleSubscriptionTrialWillEnd queues the trial-ending notice when
// Stripe says a trial ends in three days. Always 200: the webhook endpoint
// takes every event type, and a refusal would make Stripe retry and in the
// end disable it.
func stripeHandleSubscriptionTrialWillEnd(object json.RawMessage, clientSession *session.ClientSession) (*StripeWebhookResult, error) {
	var sub stripeTrialWillEndSubscription
	if err := json.Unmarshal(object, &sub); err != nil {
		glog.Warningf("[service_reminders]could not parse a trial_will_end subscription: %s\n", err)
		return &StripeWebhookResult{}, nil
	}
	cfg := model.Reengagement().ServiceReminders
	if !trialEndingStoreEnabled(cfg, model.OnboardingStoreStripe) {
		return &StripeWebhookResult{}, nil
	}
	networkId, endTime, ok := stripeTrialEndingNotice(&sub, server.NowUtc())
	if !ok {
		return &StripeWebhookResult{}, nil
	}
	template := &TrialEndingTemplate{EndTime: endTime, Store: model.OnboardingStoreStripe}
	if addServiceReminder(clientSession.Ctx, networkId, stripeTrialEndingKey(networkId, sub.Id), template) {
		glog.Infof("[service_reminders]trial ending on stripe for network %s (subscription %s, ends %s)\n", networkId, sub.Id, endTime.Format(time.RFC3339))
	}
	return &StripeWebhookResult{}, nil
}

// ----- the daily task -----

// The markets whose Pro is a one-time crypto payment.
var serviceReminderCryptoMarkets = model.SubscriptionOneTimeCryptoMarkets

// The notices before a crypto Pro stops, in days: both inside the 30-day grace.
var proEndingNoticeDays = []int{14, 2}

// The notice before a store trial's first charge, in days.
const trialEndingNoticeDays = 2

// How many missed daily runs a run makes up for. A notice that was already
// queued is not queued again (the outbox key), so the overlap is harmless; a
// late notice still names the real date, and none names a date already past.
const serviceRemindersCatchUpDays = 2

type ServiceRemindersArgs struct {
	// count what would be queued and queue nothing
	DryRun bool `json:"dry_run,omitempty"`
}

type ServiceRemindersResult struct {
	ProEnding   int  `json:"pro_ending"`
	TrialEnding int  `json:"trial_ending"`
	DryRun      bool `json:"dry_run"`
}

// NextServiceRemindersAt is the next 03:00 UTC after `now`.
func NextServiceRemindersAt(now time.Time) time.Time {
	at := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, time.UTC)
	if !at.After(now) {
		at = at.Add(24 * time.Hour)
	}
	return at
}

func ScheduleServiceReminders(clientSession *session.ClientSession, tx server.PgTx, at time.Time) {
	task.ScheduleTaskInTx(
		tx,
		ServiceReminders,
		&ServiceRemindersArgs{},
		clientSession,
		task.RunOnce("service_reminders"),
		task.RunAt(at),
	)
}

// ServiceReminders queues the day's notices in the account message outbox.
// Each kind is off until reengagement.yml turns it on (service_reminders);
// while off, the run only counts.
func ServiceReminders(args *ServiceRemindersArgs, clientSession *session.ClientSession) (*ServiceRemindersResult, error) {
	ctx := clientSession.Ctx
	cfg := model.Reengagement()
	now := server.NowUtc()
	today := dayStart(now)
	result := &ServiceRemindersResult{DryRun: args.DryRun}

	proSend := cfg.ServiceReminders.ProEnding && !args.DryRun
	catchUp := time.Duration(serviceRemindersCatchUpDays) * day
	for _, days := range proEndingNoticeDays {
		endTo := today.Add(time.Duration(days+1) * day)
		endFrom := endTo.Add(-day - catchUp)
		if endFrom.Before(now) {
			endFrom = now
		}
		for _, w := range model.ListLastSupporterWindowsEnding(ctx, endFrom, endTo, serviceReminderCryptoMarkets, 5000) {
			template := &ProEndingTemplate{
				TermEndTime: w.EndTime.Add(-manualPaymentGracePeriod),
				EndTime:     w.EndTime,
				Asset:       w.Market,
			}
			if w.CryptoAsset == model.CryptoAssetSol {
				template.Asset = cryptoReminderAssetSol
			}
			key := fmt.Sprintf("pro_ending/%s/%d/%d", w.NetworkId, w.EndTime.Unix(), days)
			if !proSend {
				result.ProEnding++
			} else if addServiceReminder(ctx, w.NetworkId, key, template) {
				result.ProEnding++
			}
		}
	}

	trialSend := cfg.ServiceReminders.TrialEnding && !args.DryRun
	startedTo := today.Add(time.Duration(trialEndingNoticeDays+1)*day - onboarding.TrialLength)
	startedFrom := startedTo.Add(-day - catchUp)
	if startedFrom.Before(now.Add(-onboarding.TrialLength)) {
		// a trial that already ended gets no "ends on" notice
		startedFrom = now.Add(-onboarding.TrialLength)
	}
	// Stripe's trials get their notice from trial_will_end
	// (stripeHandleSubscriptionTrialWillEnd), not from here
	stores := clientRecordedTrialStores(cfg.ServiceReminders.EffectiveTrialEndingStores())
	for _, trial := range model.ListOpenStoreTrialsStarted(ctx, startedFrom, startedTo, stores, 5000) {
		endTime := trial.StartedAt.Add(onboarding.TrialLength)
		template := &TrialEndingTemplate{EndTime: endTime, Store: trial.Store}
		key := fmt.Sprintf("trial_ending/%s/%d", trial.NetworkId, trial.StartedAt.Unix())
		if !trialSend {
			result.TrialEnding++
		} else if addServiceReminder(ctx, trial.NetworkId, key, template) {
			result.TrialEnding++
		}
	}
	glog.Infof("[service_reminders]pro ending %d, trial ending %d, dry run %t (pro on %t, trial on %t)\n",
		result.ProEnding, result.TrialEnding, args.DryRun, cfg.ServiceReminders.ProEnding, cfg.ServiceReminders.TrialEnding)
	return result, nil
}

func ServiceRemindersPost(args *ServiceRemindersArgs, result *ServiceRemindersResult, clientSession *session.ClientSession, tx server.PgTx) error {
	if args.DryRun {
		// a CLI dry run is a one-off, not the daily chain
		return nil
	}
	ScheduleServiceReminders(clientSession, tx, NextServiceRemindersAt(server.NowUtc()))
	return nil
}

// addServiceReminder queues one notice to the network's admin login. False
// when the network has no login to write to or the notice was already queued.
func addServiceReminder(ctx context.Context, networkId server.Id, key string, template Template) (added bool) {
	_, userAuth, ok := model.GetNetworkAdminUserAuth(ctx, networkId)
	if !ok || userAuth == "" {
		return false
	}
	server.Tx(ctx, func(tx server.PgTx) {
		added = addAccountMessageInTx(ctx, tx, &accountMessage{
			key:       key,
			networkId: &networkId,
			userAuth:  userAuth,
			template:  template,
		})
	})
	return
}
