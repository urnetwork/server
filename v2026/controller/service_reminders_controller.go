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
//   - A redeemed gift year never renews (GIFTOFPRIVACY.md §9.2): one notice
//     14 days before it ends, when nothing else will carry Pro on, with a
//     link to keep Pro at the regular price. That is an offer, so unlike the
//     others it goes only where the mail policy lets an offer go
//     (giftEndingRefusal), in the reader's language. It is no campaign: it
//     writes no re-engagement event, opens no episode and never goes during
//     one, so no re-engagement or win-back result counts it.
//
// A daily task finds the crypto years, the store trials and the gift years,
// and adds one outbox message per window and notice; the outbox key makes a
// replayed run (or a replayed Stripe event) add nothing. Each kind is off
// until reengagement.yml `service_reminders` turns it on; off, the task
// counts.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	texttemplate "text/template"
	"time"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/onboarding"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
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

// GiftEndingTemplate is the notice that a redeemed gift year of Pro ends in 14
// days with nothing else to carry Pro on (GIFTOFPRIVACY.md §9.2), in one of
// the six gift languages, with a link to keep Pro at the regular price. It
// names no price and no discount: ur.io's subscription page shows the plans.
type GiftEndingTemplate struct {
	// when the gift year ends: its renewal's end_time
	EndTime time.Time `json:"end_time"`
	// the reader's language, one of giftCardLanguages
	Lang string `json:"language"`
	// the reader's IANA zone the date is written in; "" is UTC
	TimeZone string `json:"time_zone,omitempty"`
	BaseTemplate
}

func (self *GiftEndingTemplate) Name() string {
	return giftEndingTemplateName
}

const giftEndingTemplateName = "gift_ending"

// The reader's language (LanguageTemplate).
func (self *GiftEndingTemplate) Language() string {
	return giftCardLanguage(self.Lang)
}

// `T` (giftCardText in the reader's language) and `Upper` (the text part's
// headline).
func (self *GiftEndingTemplate) Funcs(funcs texttemplate.FuncMap) {
	self.BaseTemplate.Funcs(funcs)
	language := self.Language()
	funcs["T"] = func(key string, args ...string) string {
		return giftCardText(language, key, args...)
	}
	funcs["Upper"] = strings.ToUpper
}

// The day the year ends, in the reader's zone.
func (self *GiftEndingTemplate) EndDate() string {
	return giftCardDate(self.Language(), self.EndTime.In(model.GiftOrderLocation(self.TimeZone)))
}

func (self *GiftEndingTemplate) Title() string {
	return giftCardText(self.Language(), "ending.title", "date", self.EndDate())
}

func (self *GiftEndingTemplate) Body() string {
	return giftCardText(self.Language(), "ending.body", "date", self.EndDate())
}

// Where Pro is kept, as the other reminders link it: ur.io's subscription
// page, at the regular prices.
func (self *GiftEndingTemplate) KeepUrl() string {
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

// The notice before a gift year ends, in days.
const giftEndingNoticeDays = 14

// How many missed daily runs a run makes up for. A notice that was already
// queued is not queued again (the outbox key), so the overlap is harmless; a
// late notice still names the real date, and none names a date already past.
const serviceRemindersCatchUpDays = 2

type ServiceRemindersArgs struct {
	// count what would be queued and queue nothing
	DryRun bool `json:"dry_run,omitempty"`
}

type ServiceRemindersResult struct {
	ProEnding   int `json:"pro_ending"`
	TrialEnding int `json:"trial_ending"`
	GiftEnding  int `json:"gift_ending"`
	// gift years ending whose notice the mail policy kept back, by reason
	// (giftEndingRefusal)
	GiftEndingRefused map[string]int `json:"gift_ending_refused,omitempty"`
	DryRun            bool           `json:"dry_run"`
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

	giftSend := cfg.ServiceReminders.GiftEnding && !args.DryRun
	runGiftEndingReminders(ctx, now, giftSend, result)

	glog.Infof("[service_reminders]pro ending %d, trial ending %d, gift ending %d (kept back %v), dry run %t (pro on %t, trial on %t, gift on %t)\n",
		result.ProEnding, result.TrialEnding, result.GiftEnding, result.GiftEndingRefused, args.DryRun,
		cfg.ServiceReminders.ProEnding, cfg.ServiceReminders.TrialEnding, cfg.ServiceReminders.GiftEnding)
	return result, nil
}

// runGiftEndingReminders queues the notice of each gift year that ends in
// giftEndingNoticeDays with nothing after it (model.ListGiftYearsEnding),
// making up for missed runs as the others do, when the mail policy lets it
// reach its reader (giftEndingNotice). One per gift year: the outbox key is
// the year's code. With send false it only counts.
func runGiftEndingReminders(ctx context.Context, now time.Time, send bool, result *ServiceRemindersResult) {
	today := dayStart(now)
	endTo := today.Add(time.Duration(giftEndingNoticeDays+1) * day)
	endFrom := endTo.Add(-day - time.Duration(serviceRemindersCatchUpDays)*day)
	if endFrom.Before(now) {
		endFrom = now
	}
	for _, year := range model.ListGiftYearsEnding(ctx, endFrom, endTo, now, 5000) {
		template, userAuth, refusal := giftEndingNotice(ctx, year, now)
		if refusal != "" {
			if result.GiftEndingRefused == nil {
				result.GiftEndingRefused = map[string]int{}
			}
			result.GiftEndingRefused[refusal]++
			continue
		}
		if !send {
			result.GiftEnding++
			continue
		}
		networkId := year.NetworkId
		added := false
		server.Tx(ctx, func(tx server.PgTx) {
			added = addAccountMessageInTx(ctx, tx, &accountMessage{
				key:       giftEndingKey(year),
				networkId: &networkId,
				userAuth:  userAuth,
				template:  template,
			})
		})
		if added {
			result.GiftEnding++
		}
	}
}

// The notice's outbox key, one per gift year: its renewal's code, else its end.
func giftEndingKey(year *model.GiftYearEnding) string {
	if year.TransactionId != "" {
		return fmt.Sprintf("%s/%s/%s", giftEndingTemplateName, year.NetworkId, year.TransactionId)
	}
	return fmt.Sprintf("%s/%s/%d", giftEndingTemplateName, year.NetworkId, year.EndTime.Unix())
}

// Why a gift-ending notice was kept back, besides the mail policy's own exits
// (onboarding.ExitCountryNoMarketing, ExitCanadaConsent,
// ExitConsentEvidence).
const (
	// the admin login is a phone number, or the network has none
	giftEndingRefusalNoEmail = "no_email"
	// the reader turned product updates off
	giftEndingRefusalProductUpdates = "product_updates_off"
	// an unsubscribe, a complaint or a bounce suppressed marketing mail
	giftEndingRefusalSuppressed = "suppressed"
	// a re-engagement or win-back episode is open
	giftEndingRefusalEpisode = "reengagement_open"
	// the country may not get an offer by email, or is unknown
	giftEndingRefusalNoOffer = "country_no_offer"
)

// giftEndingNotice is the notice of the gift year for its network's admin
// login, or the reason it does not go. It offers to keep Pro, so it goes only
// where the mail policy lets an offer go, read as the campaigns read it: an
// email login (no SMS), product updates on and no marketing suppression, no
// open re-engagement or win-back episode (so a purchase after this notice is
// never counted as theirs), the policy's country and consent rules for its
// purpose (onboarding.MailPolicy.Exit with onboarding.PurposeGiftEnding: the
// countries that get no marketing mail, Canada's consent, the evidence
// countries' consent evidence), and a country that may get an offer
// (OfferAllowed; an unknown one may not). It is written in the reader's
// language as the campaigns choose theirs (the first of the reader's languages
// a gift language serves, else their country's, else English), with the date
// in the reader's zone (else UTC).
func giftEndingNotice(ctx context.Context, year *model.GiftYearEnding, now time.Time) (template *GiftEndingTemplate, userAuth string, refusal string) {
	networkId := year.NetworkId
	_, userAuth, ok := model.GetNetworkAdminUserAuth(ctx, networkId)
	if !ok || !strings.Contains(userAuth, "@") {
		return nil, "", giftEndingRefusalNoEmail
	}
	mail := model.GetNetworkMailState(ctx, networkId)
	if !mail.ProductUpdates {
		return nil, "", giftEndingRefusalProductUpdates
	}
	if mail.SuppressedAt != nil {
		return nil, "", giftEndingRefusalSuppressed
	}
	if episode := model.GetNetworkReengagement(ctx, networkId); episode != nil && !episode.Exited() {
		return nil, "", giftEndingRefusalEpisode
	}
	mailContext, reader := reengagementContextReader(ctx, networkId, model.NetworkLastConnectDay(ctx, networkId))
	_, createTime, _ := model.NetworkEnrollmentFacts(ctx, networkId)
	policy := model.Onboarding().MailPolicy()
	if exit := policy.Exit(onboarding.PurposeGiftEnding, onboarding.MailFacts{
		Country:       mailContext.Country,
		ConsentSource: mail.Source,
		CreatedAt:     createTime,
		LastPaidEnd:   model.NetworkLastPaidEnd(ctx, networkId),
		Now:           now,
	}); exit != "" {
		return nil, "", exit
	}
	if !policy.OfferAllowed(mailContext.Country) {
		return nil, "", giftEndingRefusalNoOffer
	}
	language, _ := onboarding.ChooseTemplateLocale(reader.Languages, giftCardLanguages, model.Onboarding().EffectiveMailLanguageSettings())
	return &GiftEndingTemplate{
		EndTime:  year.EndTime,
		Lang:     giftCardLanguage(language),
		TimeZone: mailContext.TimeZone,
	}, userAuth, ""
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
