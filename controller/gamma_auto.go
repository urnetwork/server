package controller

// Pay with Gamma: automatic renewals, data top-ups and seat upgrades
// (mmm/BTCPAY.md §18.5). The owner, 2026-11-11: "if an account runs out of
// balance or seats, automatically add more with gamma."
//
// Every account that pays with gamma has both switches on until it turns them
// off (POST /pay/gamma/settings): auto_renew renews a plan paid with gamma at
// the end of its term, at the plan's current price; auto_topup buys the
// account's top-up item (data_1tib unless it picked another) when its data
// balance runs out or will within the next check, and upgrades a free account
// at its client limit to Pro. An optional monthly limit bounds what the
// automatic charges take in a calendar month (UTC).
//
// Each automatic charge is one intent per (account, purpose, period): a
// renewal per paid term, a top-up per hour (so at most one an hour), a seat
// upgrade per day. Its charge, receipt and skip notice are the same as a
// purchase's; a skipped charge sends one notice and is not retried until the
// deposit changes. Nothing here changes how the network provides.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// gammaTopupItems are the data packs an account may top up with: every pack
// pro.yml sells with Gamma, the 256 GiB pack included.
func gammaTopupItems() []string {
	return gammaKnownItems()
}

// gammaTopupItemMessage is the refusal of a top-up item Gamma does not sell:
// "The top-up item is data_1tib, data_10tib or data_256gib."
func gammaTopupItemMessage() string {
	items := gammaTopupItems()
	switch len(items) {
	case 0:
		return "No data pack is sold with Gamma now."
	case 1:
		return fmt.Sprintf("The top-up item is %s.", items[0])
	}
	return fmt.Sprintf("The top-up item is %s or %s.", strings.Join(items[:len(items)-1], ", "), items[len(items)-1])
}

func (self *gammaBiller) autoDemand() {
	if self.readiness != gammaReady {
		return
	}
	gammaAutoLock.Lock()
	due := gammaAutoLastRun.IsZero() || gammaDefaultAutoInterval <= self.now.Sub(gammaAutoLastRun)
	if due {
		gammaAutoLastRun = self.now
	}
	gammaAutoLock.Unlock()
	if !due {
		return
	}
	var after *server.Id
	for {
		accounts := model.ListGammaAccounts(self.ctx, after, 500)
		for _, account := range accounts {
			self.autoRenew(account)
			self.autoTopup(account)
			self.autoSeat(account)
		}
		if len(accounts) < 500 {
			return
		}
		last := accounts[len(accounts)-1].NetworkId
		after = &last
	}
}

// gammaAutoPrice is the current price of an automatic plan purchase at the
// account's tier.
func gammaAutoPlanPrice(account *model.GammaAccount, plan string) (priceUsd float64, tier *model.ProPriceTier, ok bool) {
	pro := model.Pro()
	tier = pro.PriceTierByName(account.PriceTier)
	if tier == nil {
		tier = pro.DefaultPriceTier()
	}
	priceUsd, ok = solanaPlanPriceUsdForTier(plan, tier)
	return priceUsd, tier, ok && 0 < priceUsd
}

// withinMonthlyLimit reports whether an automatic charge of amount fits the
// account's monthly limit.
func (self *gammaBiller) withinMonthlyLimit(account *model.GammaAccount, amount int64) bool {
	if account.MonthlyLimit == nil {
		return true
	}
	monthStart := time.Date(self.now.Year(), self.now.Month(), 1, 0, 0, 0, 0, time.UTC)
	spent := model.SumGammaAutomaticSpend(self.ctx, account.NetworkId, monthStart)
	if *account.MonthlyLimit < spent+amount {
		gammaReports.report("limit/"+account.NetworkId.String(), func() {
			glog.Infof("[gamma]network %s is at its monthly limit (%d of %d units)\n", account.NetworkId, spent, *account.MonthlyLimit)
		})
		return false
	}
	return true
}

// newAutoIntent makes an automatic intent, once per (account, purpose,
// period). created = false when the period already has one.
func (self *gammaBiller) newAutoIntent(account *model.GammaAccount, purpose string, periodKey string, item string, priceUsd float64, tier string, termStart *time.Time, expiresAt time.Time) bool {
	_, tokenHash := cryptoNewStatusToken()
	key := periodKey
	intent := &model.GammaPaymentIntent{
		IntentId:                 server.NewId(),
		StatusTokenHash:          tokenHash,
		NetworkId:                account.NetworkId,
		Hotkey:                   account.Hotkey,
		Purpose:                  purpose,
		PeriodKey:                &key,
		Reference:                gammaNewReference(),
		Delivery:                 model.CryptoDeliveryAccount,
		Item:                     item,
		Tier:                     tier,
		PriceUsdNanoCents:        model.UsdToNanoCents(priceUsd),
		RegularPriceUsdNanoCents: model.UsdToNanoCents(priceUsd),
		TermStartTime:            termStart,
		CreateTime:               self.now,
		ExpiresAt:                expiresAt,
	}
	if !self.withinMonthlyLimit(account, intent.AmountGamma()) {
		return false
	}
	if !model.CreateGammaPaymentIntent(self.ctx, intent) {
		return false
	}
	gammaIntentsCreated.WithLabelValues(purpose).Inc()
	glog.Infof("[gamma]%s intent %s for network %s: %s at %s gamma\n", purpose, intent.IntentId, account.NetworkId, item, gammaFormatUnits(intent.AmountGamma()))
	return true
}

// gammaAccountRenews reports whether the biller renews the account's plan paid
// with gamma at the end of its term: automatic renewal on, with a plan to
// renew. Card checkout is refused beside a plan paid with gamma only while it
// renews (stripeNewSubscriptionRefusal). nil: the network never paid with
// gamma.
func gammaAccountRenews(account *model.GammaAccount) bool {
	return account != nil && account.AutoRenew && account.RenewPlan != ""
}

// autoRenew renews a plan paid with gamma when its paid term ends within the
// lead time, unless another Pro subscription covers or still bills the next
// term (gammaRenewCoverEnd): never two charges for the same days. When that
// other Pro ended after this term did, within the term's grace, the renewal
// starts where it ended.
func (self *gammaBiller) autoRenew(account *model.GammaAccount) {
	if !gammaAccountRenews(account) {
		return
	}
	_, _, graceEnd, ok := model.GetLatestOneTimeSubscriptionRenewal(self.ctx, account.NetworkId, []string{model.SubscriptionMarketGamma}, self.now)
	if !ok {
		return
	}
	termEnd := graceEnd.Add(-manualPaymentGracePeriod)
	if self.now.Before(termEnd.Add(-gammaRenewLead)) {
		return
	}
	termStart := termEnd
	if coverEnd := gammaRenewCoverEnd(self.ctx, account.NetworkId, termEnd); coverEnd.After(termEnd) {
		if coverEnd.After(self.now) {
			// another subscription pays for (or still bills) these days
			return
		}
		termStart = coverEnd
	}
	priceUsd, tier, ok := gammaAutoPlanPrice(account, account.RenewPlan)
	if !ok {
		return
	}
	periodKey := "renew/" + termEnd.UTC().Format(time.RFC3339)
	self.newAutoIntent(account, model.GammaPurposeRenew, periodKey, account.RenewPlan, priceUsd, tier.Name, &termStart, graceEnd)
}

// gammaRenewCoverEnd is when the Pro that covers a renewal's next term from
// elsewhere ends, for a term that would start at termStart: another
// market's window, or a gamma plan bought after that term began; zero when
// none covers any of it.
func gammaRenewCoverEnd(ctx context.Context, networkId server.Id, termStart time.Time) time.Time {
	return model.GetGammaProCoverEnd(ctx, networkId, termStart, &termStart)
}

// gammaPlanCoverEnd is when the Pro that another subscription pays for, or
// still bills, ends, for the term a gamma plan intent would buy: from its
// term start for a renewal, from now for a purchase or a seat upgrade. A
// renewal does not count the gamma term it continues, a purchase counts no
// gamma window (it may upgrade one), and a seat upgrade counts every one: it
// is only for an account without Pro. Zero when nothing else covers it.
func gammaPlanCoverEnd(ctx context.Context, intent *model.GammaPaymentIntent, now time.Time) time.Time {
	switch intent.Purpose {
	case model.GammaPurposeRenew:
		termStart := now
		if intent.TermStartTime != nil {
			termStart = *intent.TermStartTime
		}
		return gammaRenewCoverEnd(ctx, intent.NetworkId, termStart)
	case model.GammaPurposeSeat:
		return model.GetGammaProCoverEnd(ctx, intent.NetworkId, now, &time.Time{})
	default:
		return model.GetGammaProCoverEnd(ctx, intent.NetworkId, now, nil)
	}
}

// gammaPlanHold is why a gamma plan intent must not be charged now, as the
// message its status shows; "" when it may be. A data pack is never held.
//
//   - Another Pro subscription pays for, or still bills, part of the term it
//     would buy (gammaPlanCoverEnd). It waits; once that subscription ends,
//     while the intent is still open, it is charged, and a renewal then
//     starts where the other Pro ended (gammaCreditIntentInTx).
//   - Another Gamma plan was paid at or after it was made: that one paid for
//     these days, so it is never charged. A purchase ignores the network's own
//     Gamma windows (it may upgrade a plan), so this is what keeps a purchase
//     made before a renewal (or another purchase) was paid from being charged
//     on top of it.
//   - Another Gamma plan charge of the network is in flight. It waits, and is
//     charged only if that one is not paid. One plan charge at a time: the
//     biller would otherwise put both in one batch.
//   - A renewal or a seat upgrade, while a plan purchase of the network is
//     open: the purchase replaces it. It waits, is voided once the purchase is
//     paid (gammaCreditIntentInTx), and goes ahead if the purchase expires
//     unpaid, so a quote never paid does not cost the customer the renewal.
func gammaPlanHold(ctx context.Context, intent *model.GammaPaymentIntent, now time.Time) string {
	if !slices.Contains(gammaKnownPlans, intent.Item) {
		return ""
	}
	if gammaPlanCoverEnd(ctx, intent, now).After(now) {
		return gammaPlanCoveredMessage
	}
	createTime := intent.CreateTime
	charging, paidSince := model.GetGammaPlanChargeState(ctx, intent.NetworkId, gammaKnownPlans, &intent.IntentId, &createTime)
	switch {
	case paidSince:
		return gammaPlanPaidSinceMessage
	case charging:
		return gammaPlanChargingMessage
	}
	if intent.Automatic() && model.HasOpenGammaPlanPurchase(ctx, intent.NetworkId, gammaKnownPlans, now) {
		return gammaPlanReplacedMessage
	}
	return ""
}

// gammaPlanCovered reports whether a gamma plan intent must not be charged
// now (gammaPlanHold).
func gammaPlanCovered(ctx context.Context, intent *model.GammaPaymentIntent, now time.Time) bool {
	return gammaPlanHold(ctx, intent, now) != ""
}

// gammaManageUrl explains the automatic charges and their switches.
const gammaManageUrl = "https://ur.io/gamma"

// gammaLookupSubscriptionState answers subscription details for a plan paid
// with gamma: it renews at the end of the term unless the account turned
// automatic renewal off (POST /pay/gamma/settings).
func gammaLookupSubscriptionState(ctx context.Context, intentId string) (*subscriptionStoreState, error) {
	id, err := server.ParseId(intentId)
	if err != nil {
		return nil, err
	}
	intent := model.GetGammaPaymentIntent(ctx, id)
	if intent == nil {
		return nil, fmt.Errorf("no gamma intent %s", intentId)
	}
	autoRenew := false
	if account := model.GetGammaAccount(ctx, intent.NetworkId); account != nil {
		autoRenew = account.AutoRenew
	}
	return &subscriptionStoreState{AutoRenew: &autoRenew, CancelAtPeriodEnd: !autoRenew}, nil
}

// gammaTopupDue reports whether a data balance runs out, or will within the
// next check at the use seen since the last one.
func gammaTopupDue(balance int64, lastBalance *int64, lastAt *time.Time, now time.Time, interval time.Duration) bool {
	if balance <= int64(gammaTopupFloorBytes) {
		return true
	}
	if lastBalance == nil || lastAt == nil || !lastAt.Before(now) || *lastBalance <= balance {
		return false
	}
	used := float64(*lastBalance - balance)
	elapsed := now.Sub(*lastAt).Seconds()
	// twice the next interval's projected use, for a check that runs late
	projected := used / elapsed * interval.Seconds() * 2
	return float64(balance) <= projected
}

// autoTopup buys the account's top-up item when its data balance runs out.
func (self *gammaBiller) autoTopup(account *model.GammaAccount) {
	if !account.AutoTopup {
		return
	}
	balance := int64(model.GetActiveTransferBalanceByteCount(self.ctx, account.NetworkId))
	due := gammaTopupDue(balance, account.LastBalanceBytes, account.LastBalanceAt, self.now, gammaDefaultAutoInterval)
	model.SetGammaAccountBalanceSample(self.ctx, account.NetworkId, balance, self.now)
	if !due {
		return
	}
	item := account.TopupItem
	if item == "" || !slices.Contains(gammaTopupItems(), item) {
		item = StripeItemData1Tib
	}
	priceUsd, ok := dataPackPriceUsd(item, model.DataRailGamma)
	if !ok {
		return
	}
	hour := self.now.UTC().Truncate(time.Hour)
	self.newAutoIntent(account, model.GammaPurposeTopup, "topup/"+hour.Format(time.RFC3339), item, priceUsd, "", nil, self.now.Add(gammaTopupTtl))
}

// autoSeat upgrades a free account that hit its client limit to Pro.
func (self *gammaBiller) autoSeat(account *model.GammaAccount) {
	if account.SeatDemandAt == nil {
		return
	}
	if !account.AutoTopup || model.IsProNetwork(self.ctx, account.NetworkId) ||
		model.GetGammaProCoverEnd(self.ctx, account.NetworkId, self.now, &time.Time{}).After(self.now) {
		// Pro already, a Pro subscription that pays for (or still bills) now
		// without the entitlement showing yet, or the customer turned it off:
		// the error is final, and no second Pro is bought for the same days
		model.ClearGammaSeatDemand(self.ctx, account.NetworkId)
		return
	}
	plan := account.RenewPlan
	if plan == "" {
		plan = model.SolanaPlanMonthly
	}
	priceUsd, tier, ok := gammaAutoPlanPrice(account, plan)
	if !ok {
		return
	}
	day := self.now.UTC().Format("2006-01-02")
	if self.newAutoIntent(account, model.GammaPurposeSeat, "seat/"+day, plan, priceUsd, tier.Name, nil, self.now.Add(gammaSeatTtl)) {
		// the renewal after it renews this plan
		server.Tx(self.ctx, func(tx server.PgTx) {
			model.RecordGammaAccountPaymentInTx(tx, self.ctx, account.NetworkId, account.Hotkey, plan, tier.Name, self.now)
		})
	}
	model.ClearGammaSeatDemand(self.ctx, account.NetworkId)
}

// The seat demand a free account at its client limit raises, at most once a
// minute per network per process.
var gammaSeatDemandLimiter = newCryptoPayKeyedReport(time.Minute)

// gammaNoteSeatDemand is called with a refused client creation that an
// upgrade would lift. It never fails the refusal.
func gammaNoteSeatDemand(networkId server.Id) {
	gammaSeatDemandLimiter.report(networkId.String(), func() {
		server.HandleError(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if model.NoteGammaSeatDemand(ctx, networkId, gammaNow()) {
				glog.Infof("[gamma]network %s is at its client limit: a Pro upgrade is queued\n", networkId)
			}
		})
	})
}
