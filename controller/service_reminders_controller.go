// SPDX-License-Identifier: MPL-2.0

package controller

// Service reminders (mmm/reengagement/PLAN.md "SERVICE REMINDERS", stream P):
// account messages about the network's own subscription, sent through the
// account message outbox whatever its marketing preference.
//
//   - Pro paid in crypto (USDC, Monero, Zcash) is a one-time payment that never
//     renews: without a reminder every crypto subscriber lapses by default. A
//     notice 14 days and 2 days before Pro stops, inside the 30-day grace.
//   - A free trial ends in a charge: the offer screen promises "a reminder
//     before the charge" (store key offer_timeline_reminder;
//     mmm/onboarding/REVIEW.md finding 6). A notice 2 days before an App Store
//     or Play trial (purchase.completed{trial}) ends, unless its outcome is
//     already recorded. Stripe sends its own when its dashboard setting is on;
//     service_reminders.trial_ending_stores can add it.
//
// A daily task finds them and adds one outbox message per window and notice;
// the outbox key makes a replayed run add nothing. Both kinds are off until
// reengagement.yml `service_reminders` turns them on; off, the task counts.

import (
	"context"
	"fmt"
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
	case "monero":
		return "Monero"
	case "zcash":
		return "Zcash"
	default:
		return "USDC"
	}
}

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

// ----- the daily task -----

// The markets whose Pro is a one-time crypto payment.
var serviceReminderCryptoMarkets = []string{
	model.SubscriptionMarketSolana,
	model.SubscriptionMarketMonero,
	model.SubscriptionMarketZcash,
}

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
	stores := cfg.ServiceReminders.EffectiveTrialEndingStores()
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
