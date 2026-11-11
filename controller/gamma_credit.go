package controller

// Pay with Gamma: the credit, the receipt and the skip notice
// (mmm/BTCPAY.md §18.4).
//
// An intent is credited only from its spent charge (our finalized
// GammaSpent with the intent's reference), in one ReadCommitted transaction:
//
//  1. the network lock (LockPaymentNetworkInTx)
//  2. the intent's one-shot (credited_at IS NULL AND voided_at IS NULL)
//  3. a plan: the renewal (market gamma), ending at the term plus the grace
//     period, and the Pro balance with the price as its subsidy revenue; the
//     price tier on the renewal and the welcome offer's redemption. A
//     renewal's term starts where the paid term ended, so it does not shift.
//  4. a data pack to the network: a pro = false balance and the applied note
//  5. a data pack as a code: the balance code, emailed when the intent has an
//     email
//  6. the account is recorded as paying with gamma: automatic renewal and
//     top-up on by default, from the hotkey that paid
//  7. the receipt email to the network's admin, and the sales notice
//
// Nothing here changes how the network provides: an automatic upgrade never
// touches the provide mode.

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// gammaCreditIntent credits an intent whose charge was spent. credited =
// false when another credit got there first.
func gammaCreditIntent(clientSession *session.ClientSession, intent *model.GammaPaymentIntent, charge *model.GammaCharge) (credited bool, returnErr error) {
	ctx := clientSession.Ctx
	if charge.State != model.GammaChargeSpent {
		return false, fmt.Errorf("intent %s's charge is %s, not spent", intent.IntentId, charge.State)
	}
	plan := slices.Contains(gammaKnownPlans, intent.Item)
	var byteCount model.ByteCount
	duration := model.Pro().DataCodeDuration
	if !plan {
		var ok bool
		// what the charged pack grants, whatever its rails say now
		byteCount, ok = dataPackByteCount(intent.Item)
		if !ok || byteCount <= 0 {
			return false, fmt.Errorf("intent %s names an unknown data pack", intent.IntentId)
		}
		if duration <= 0 {
			return false, fmt.Errorf("data code duration is not configured (pro.yml)")
		}
	}
	now := gammaNow()
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			credited = gammaCreditIntentInTx(tx, clientSession, intent, charge, plan, byteCount, duration, now)
		}, server.TxReadCommitted)
	}, func(err error) {
		credited = false
		returnErr = err
	})
	if returnErr != nil || !credited {
		return false, returnErr
	}
	if plan {
		model.UpdateProNetworkInConn(nil, ctx, intent.NetworkId)
	}
	gammaCredits.WithLabelValues(intent.Purpose, intent.Delivery).Inc()
	glog.Infof("[gamma]credited intent %s (%s %s, %s)\n", intent.IntentId, intent.Purpose, intent.Item, intent.Delivery)
	return true, nil
}

func gammaCreditIntentInTx(
	tx server.PgTx,
	clientSession *session.ClientSession,
	intent *model.GammaPaymentIntent,
	charge *model.GammaCharge,
	plan bool,
	byteCount model.ByteCount,
	duration time.Duration,
	now time.Time,
) bool {
	ctx := clientSession.Ctx
	networkId := intent.NetworkId
	server.Raise(model.LockPaymentNetworkInTx(tx, ctx, networkId))
	if !model.MarkGammaPaymentIntentCreditedInTx(tx, ctx, intent.IntentId, now) {
		return false
	}
	amount := intent.PriceUsdNanoCents
	var balanceId, balanceCodeId *server.Id
	renewPlan := ""
	switch {
	case plan:
		start := now
		if intent.TermStartTime != nil && intent.TermStartTime.Before(now.Add(gammaRenewLead)) {
			// a renewal continues the paid term
			start = *intent.TermStartTime
			// where Pro paid elsewhere ran past it (and has ended: the biller
			// charges nothing while it runs), the new term starts after it,
			// so no day is paid for twice
			if coverEnd := model.GetGammaProCoverEndInTx(tx, ctx, networkId, start, &start); coverEnd.After(start) && !coverEnd.After(now) {
				start = coverEnd
			}
		}
		endTime := start.Add(solanaPlanDuration(intent.Item) + manualPaymentGracePeriod)
		server.Raise(model.AddSubscriptionRenewalInTx(tx, ctx, &model.SubscriptionRenewal{
			NetworkId:          networkId,
			SubscriptionType:   model.SubscriptionTypeSupporter,
			StartTime:          start,
			EndTime:            endTime,
			NetRevenue:         amount,
			SubscriptionMarket: model.SubscriptionMarketGamma,
			TransactionId:      intent.IntentId.String(),
		}))
		transferBalance := &model.TransferBalance{
			NetworkId:             networkId,
			StartTime:             start,
			EndTime:               endTime,
			StartBalanceByteCount: RefreshSupporterTransferBalance,
			SubsidyNetRevenue:     amount,
			BalanceByteCount:      RefreshSupporterTransferBalance,
			Pro:                   true,
		}
		model.AddTransferBalanceInTx(ctx, tx, transferBalance)
		balanceId = &transferBalance.BalanceId
		model.SetSubscriptionRenewalPriceTierByTransactionInTx(tx, ctx, networkId, model.SubscriptionMarketGamma, intent.IntentId.String(), intent.Tier)
		if intent.Item == model.SolanaPlanYearlyOnboarding {
			if model.RedeemOnboardingOfferInTx(tx, ctx, networkId, model.OnboardingStoreGamma, now) {
				glog.Infof("[onboarding]offer redeemed on gamma by intent %s\n", intent.IntentId)
			}
		}
		renewPlan = intent.Item
		if renewPlan == model.SolanaPlanYearlyOnboarding {
			// the offer is once; a renewal is at the regular price
			renewPlan = model.SolanaPlanYearly
		}

	case intent.Delivery == model.CryptoDeliveryCode:
		balanceCode, created, err := model.CreateBalanceCodeInTx(
			tx,
			ctx,
			byteCount,
			duration,
			amount,
			model.GammaPaymentPurchaseEventId(intent.IntentId),
			model.MarshalCryptoRateSources(map[string]any{"asset": "gamma", "item": intent.Item}),
			intent.Email,
		)
		server.Raise(err)
		balanceCodeId = &balanceCode.BalanceCodeId
		if created && intent.Email != "" {
			normalEmail, userAuthType := model.NormalUserAuth(intent.Email)
			if userAuthType == model.UserAuthTypeEmail {
				addAccountMessageInTx(ctx, tx, &accountMessage{
					key:      fmt.Sprintf("gamma_code/%s/%s", intent.IntentId, balanceCode.BalanceCodeId),
					userAuth: normalEmail,
					template: &SubscriptionTransferBalanceCodeTemplate{
						Secret:           balanceCode.Secret,
						BalanceByteCount: balanceCode.BalanceByteCount,
					},
				})
			}
		}

	default:
		transferBalance := &model.TransferBalance{
			NetworkId:             networkId,
			StartTime:             now,
			EndTime:               now.Add(duration),
			StartBalanceByteCount: byteCount,
			BalanceByteCount:      byteCount,
			NetRevenue:            amount,
			Pro:                   false,
		}
		model.AddTransferBalanceInTx(ctx, tx, transferBalance)
		balanceId = &transferBalance.BalanceId
	}

	model.SetGammaPaymentIntentGrantInTx(tx, ctx, intent.IntentId, balanceId, balanceCodeId)
	model.RecordGammaAccountPaymentInTx(tx, ctx, networkId, intent.Hotkey, renewPlan, intent.Tier, now)
	gammaAddReceiptInTx(tx, clientSession, intent, charge, plan, byteCount)
	addSalesPaymentNoticeInTx(tx, func() *model.SalesPaymentNotice {
		return gammaSalesNotice(intent, plan, byteCount)
	})
	return true
}

// gammaAddReceiptInTx adds the receipt for the credit to the account message
// outbox, to the network's admin when their login is an email: one receipt
// per intent.
func gammaAddReceiptInTx(tx server.PgTx, clientSession *session.ClientSession, intent *model.GammaPaymentIntent, charge *model.GammaCharge, plan bool, byteCount model.ByteCount) {
	ctx := clientSession.Ctx
	_, userAuth, ok := model.GetNetworkAdminUserAuthInTx(ctx, tx, intent.NetworkId)
	if !ok || userAuth == "" {
		return
	}
	normalUserAuth, userAuthType := model.NormalUserAuth(userAuth)
	if userAuthType != model.UserAuthTypeEmail {
		return
	}
	block := ""
	if charge.BlockNumber != nil {
		block = fmt.Sprintf("%d", *charge.BlockNumber)
	}
	networkId := intent.NetworkId
	template := &GammaReceiptTemplate{
		Description: gammaItemDescription(intent.Item),
		AmountGamma: gammaFormatUnits(intent.AmountGamma()),
		Purpose:     intent.Purpose,
		Reference:   gammaHex(intent.Reference),
		Block:       block,
		Pro:         plan,
	}
	if !plan {
		template.BalanceByteCount = byteCount
	}
	addAccountMessageInTx(ctx, tx, &accountMessage{
		key:       "gamma_receipt/" + intent.IntentId.String(),
		networkId: &networkId,
		userAuth:  normalUserAuth,
		template:  template,
	})
}

// gammaSendSkipNotice sends, once, the notice that an automatic charge could
// not be spent, and what to change; nothing retries until the deposit does.
func gammaSendSkipNotice(ctx context.Context, intent *model.GammaPaymentIntent, charge *model.GammaCharge, settings *gammaSettings, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		if !model.MarkGammaChargeNoticeSentInTx(tx, ctx, charge.Reference, now) {
			return
		}
		_, userAuth, ok := model.GetNetworkAdminUserAuthInTx(ctx, tx, intent.NetworkId)
		if !ok || userAuth == "" {
			return
		}
		normalUserAuth, userAuthType := model.NormalUserAuth(userAuth)
		if userAuthType != model.UserAuthTypeEmail {
			return
		}
		networkId := intent.NetworkId
		addAccountMessageInTx(ctx, tx, &accountMessage{
			key:       "gamma_skipped/" + intent.IntentId.String(),
			networkId: &networkId,
			userAuth:  normalUserAuth,
			template: &GammaChargeSkippedTemplate{
				Description:   gammaItemDescription(intent.Item),
				AmountGamma:   gammaFormatUnits(intent.AmountGamma()),
				Purpose:       intent.Purpose,
				Reason:        charge.SkipReason,
				DepositHotkey: intent.Hotkey,
				Netuid:        settings.Netuid,
				ServiceHotkey: settings.ServiceAddress,
			},
		})
	})
}

// gammaSalesNotice is the sales notice of a credited intent.
func gammaSalesNotice(intent *model.GammaPaymentIntent, plan bool, byteCount model.ByteCount) *model.SalesPaymentNotice {
	networkId := intent.NetworkId
	notice := &model.SalesPaymentNotice{
		RecordKey: "gamma_intent/" + intent.IntentId.String(),
		NetworkId: &networkId,
		Kind:      model.SalesPaymentKindOneTime,
		Method:    "SN25 Gamma",
	}
	switch intent.Purpose {
	case model.GammaPurposeRenew:
		notice.Kind = "renewal"
	case model.GammaPurposeTopup:
		notice.Kind = "automatic top-up"
	case model.GammaPurposeSeat:
		notice.Kind = "automatic upgrade at the client limit"
	}
	switch {
	case plan && intent.Item == model.SolanaPlanYearlyOnboarding:
		notice.Item = model.SalesProItem(model.PlanYearly)
		notice.Kind = "one-time, welcome offer"
	case plan:
		notice.Item = model.SalesProItem(planForProductId(intent.Item))
	default:
		notice.Item = model.SalesDataItem(byteCount)
	}
	notice.Amount = fmt.Sprintf("%s gamma (burned on SN25, credited as %s)", gammaFormatUnits(intent.AmountGamma()), salesNanoCentsUsd(intent.PriceUsdNanoCents))
	return notice
}

// gammaItemDescription names what an intent buys, for receipts.
func gammaItemDescription(item string) string {
	switch item {
	case model.SolanaPlanMonthly:
		return "URnetwork Pro monthly"
	}
	return cryptoItemDescription(item)
}

// gammaFormatUnits writes gamma units as an exact decimal string of gamma:
// "40", "0.5", "3.686319800" without trailing zeros.
func gammaFormatUnits(units int64) string {
	negative := units < 0
	if negative {
		units = -units
	}
	whole := units / gammaUnitsPerGamma
	fraction := units % gammaUnitsPerGamma
	text := fmt.Sprintf("%d", whole)
	if fraction != 0 {
		decimals := fmt.Sprintf("%09d", fraction)
		for decimals[len(decimals)-1] == '0' {
			decimals = decimals[:len(decimals)-1]
		}
		text += "." + decimals
	}
	if negative {
		text = "-" + text
	}
	return text
}
