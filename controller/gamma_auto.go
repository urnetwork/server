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
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The data items an account may top up with.
var gammaTopupItems = []string{StripeItemData1Tib, StripeItemData10Tib}

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

// autoRenew renews a plan paid with gamma when its paid term ends within the
// lead time, unless another market's Pro runs past it.
func (self *gammaBiller) autoRenew(account *model.GammaAccount) {
	if !account.AutoRenew || account.RenewPlan == "" {
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
	if model.HasSupporterRenewalEndingAfter(self.ctx, account.NetworkId, graceEnd, model.SubscriptionMarketGamma) {
		// paid elsewhere past this term: nothing to renew here
		return
	}
	priceUsd, tier, ok := gammaAutoPlanPrice(account, account.RenewPlan)
	if !ok {
		return
	}
	periodKey := "renew/" + termEnd.UTC().Format(time.RFC3339)
	self.newAutoIntent(account, model.GammaPurposeRenew, periodKey, account.RenewPlan, priceUsd, tier.Name, &termEnd, graceEnd)
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
	if item == "" || !slices.Contains(gammaTopupItems, item) {
		item = StripeItemData1Tib
	}
	priceUsd, ok := dataPackPriceUsd(item)
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
	if !account.AutoTopup || model.IsProNetwork(self.ctx, account.NetworkId) {
		// Pro already, or the customer turned it off: the error is final
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
