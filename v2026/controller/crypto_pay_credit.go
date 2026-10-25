package controller

// The one credit of the crypto payment rail (mmm/ZCASH-CONTRACT.md
// §3.7, mmm/ZCASH.md §4.8): every coin, every delivery and every caller (the
// scanner, the reconciler, the Helius webhook for a linked USDC intent, an
// operator) credits an invoice through cryptoCreditIntentInConn, in ONE
// ReadCommitted transaction:
//
//  1. the network lock (LockPaymentNetworkInTx) when the delivery has one
//  2. the invoice one-shot (credited_at IS NULL AND voided_at IS NULL, rows
//     affected checked), and for USDC the Solana intent's own one-shot in the
//     same transaction
//  3. a plan: the renewal (market monero / zcash / solana), the Pro transfer
//     balance with the USD value as subsidy revenue, then the price tier on
//     the renewal and the welcome offer's redemption -- inside the
//     transaction, on every path (the Solana reconciler used to skip both)
//  4. a data pack to a network: a pro = false balance and the applied note
//  5. a data pack as a code: the balance code, emailed through the account
//     message outbox when the invoice has an email
//  6. the receipts that made up the payment move to credited
//  7. after commit, the Pro cache is refreshed
//
// A scanner run racing a reconcile credit of the same invoice credits once:
// the one-shot is the gate, exactly as for the Solana intents.

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// cryptoCredit is what one credit of an invoice is worth and what paid it.
type cryptoCredit struct {
	// the USD value: the quoted price for an on-time payment, the receipts'
	// first-seen value capped at the price for a late one
	usdNanoCents model.NanoCents
	// the receipts that made up the payment, with each one's share of the value
	receipts   []model.CryptoPaymentReceiptKey
	receiptUsd map[model.CryptoPaymentReceiptKey]model.NanoCents
	// usdc_solana: the transaction signature the Solana intent is consumed by
	solanaSignature string
	late            bool
}

// The invoice one-shot won and the linked Solana intent's lost: the two can
// only disagree if something credited the Solana intent around the invoice.
// The credit's transaction is rolled back rather than commit half of it.
var errCryptoSolanaOneShotDiverged = errors.New("the USDC invoice is open but its Solana intent is already consumed")

// cryptoCreditIntent credits an invoice on its own connection.
func cryptoCreditIntent(
	clientSession *session.ClientSession,
	intent *model.CryptoPaymentIntent,
	credit *cryptoCredit,
) (credited bool, returnErr error) {
	return cryptoCreditIntentInConn(nil, clientSession, intent, credit)
}

// Reuse the caller's PostgreSQL session; nil selects the outer acquisition boundary.
func cryptoCreditIntentInConn(
	connOwner server.PgConn,
	clientSession *session.ClientSession,
	intent *model.CryptoPaymentIntent,
	credit *cryptoCredit,
) (credited bool, returnErr error) {
	ctx := clientSession.Ctx
	plan := slices.Contains(cryptoPayKnownPlans, intent.Item)
	market := cryptoAssetMarket(intent.Asset)
	if market == "" {
		return false, fmt.Errorf("invoice %s has an unknown asset", intent.IntentId)
	}
	if intent.Delivery != model.CryptoDeliveryCode && intent.NetworkId == nil {
		return false, fmt.Errorf("invoice %s has no network to deliver to", intent.IntentId)
	}
	if intent.Asset == model.CryptoAssetUsdcSolana && (intent.SolanaReference == nil || credit.solanaSignature == "") {
		return false, fmt.Errorf("USDC invoice %s needs its Solana reference and the paying signature", intent.IntentId)
	}
	var byteCount model.ByteCount
	duration := model.Pro().DataCodeDuration
	if !plan {
		var ok bool
		byteCount, ok = stripeDataPackByteCount(intent.Item)
		if !ok || byteCount <= 0 {
			return false, fmt.Errorf("invoice %s names an unknown data pack", intent.IntentId)
		}
		// a PAID path: data that expires the instant it lands is refused, so
		// the failure is visible and the invoice stays open for a retry
		if duration <= 0 {
			glog.Errorf("[cryptopay]refusing to credit data pack invoice %s with a zero duration. Is pro.yml present?\n", intent.IntentId)
			return false, fmt.Errorf("data code duration is not configured (pro.yml)")
		}
	}

	now := server.NowUtc()
	server.HandleError(func() {
		server.TxInConn(ctx, connOwner, func(tx server.PgTx) {
			credited = false
			returnErr = nil
			if intent.NetworkId != nil {
				if err := model.LockPaymentNetworkInTx(tx, ctx, *intent.NetworkId); err != nil {
					returnErr = err
					return
				}
			}
			// the invoice one-shot first: when it loses, nothing was written
			if !model.MarkCryptoPaymentIntentCreditedInTx(tx, ctx, intent.IntentId, now, credit.usdNanoCents) {
				return
			}
			if intent.Asset == model.CryptoAssetUsdcSolana {
				completed, err := model.MarkPaymentIntentCompletedInTx(tx, *intent.SolanaReference, credit.solanaSignature, clientSession)
				server.Raise(err)
				if !completed {
					server.Raise(errCryptoSolanaOneShotDiverged)
				}
			}

			var balanceId *server.Id
			var balanceCodeId *server.Id
			switch {
			case plan:
				networkId := *intent.NetworkId
				_, transactionId := model.CryptoPaymentRenewalHandle(intent)
				endTime := now.Add(solanaPlanDuration(intent.Item) + manualPaymentGracePeriod)
				// a failure past the one-shot raises: committing a credited
				// invoice without its grant would lose the payment
				server.Raise(model.AddSubscriptionRenewalInTx(tx, ctx, &model.SubscriptionRenewal{
					NetworkId:          networkId,
					SubscriptionType:   model.SubscriptionTypeSupporter,
					StartTime:          now,
					EndTime:            endTime,
					NetRevenue:         credit.usdNanoCents,
					SubscriptionMarket: market,
					TransactionId:      transactionId,
				}))
				// a supporter subscription carries the Pro entitlement
				transferBalance := &model.TransferBalance{
					NetworkId:             networkId,
					StartTime:             now,
					EndTime:               endTime,
					StartBalanceByteCount: RefreshSupporterTransferBalance,
					SubsidyNetRevenue:     credit.usdNanoCents,
					BalanceByteCount:      RefreshSupporterTransferBalance,
					Pro:                   true,
				}
				model.AddTransferBalanceInTx(ctx, tx, transferBalance)
				balanceId = &transferBalance.BalanceId
				// the regional tier and the welcome offer, in the credit's
				// transaction on every path
				model.SetSubscriptionRenewalPriceTierByTransactionInTx(tx, ctx, networkId, market, transactionId, intent.Tier)
				if intent.Item == model.SolanaPlanYearlyOnboarding {
					if model.RedeemOnboardingOfferInTx(tx, ctx, networkId, cryptoAssetOnboardingStore(intent.Asset), now) {
						glog.Infof("[onboarding]offer redeemed on %s by invoice %s\n", cryptoAssetOnboardingStore(intent.Asset), intent.IntentId)
					}
				}

			case intent.Delivery == model.CryptoDeliveryCode:
				balanceCode, created, err := model.CreateBalanceCodeInTx(
					tx,
					ctx,
					byteCount,
					duration,
					credit.usdNanoCents,
					model.CryptoPaymentPurchaseEventId(intent.IntentId),
					model.MarshalCryptoRateSources(map[string]any{"asset": intent.Asset, "item": intent.Item}),
					intent.Email,
				)
				server.Raise(err)
				balanceCodeId = &balanceCode.BalanceCodeId
				if created && intent.Email != "" {
					// owed exactly when the credit commits; the delivery task
					// sends it after
					normalEmail, userAuthType := model.NormalUserAuth(intent.Email)
					if userAuthType == model.UserAuthTypeEmail {
						// keyed by the code too: a code granted again after its
						// payment left the chain and came back is a new email
						addAccountMessageInTx(ctx, tx, &accountMessage{
							key:      fmt.Sprintf("crypto_code/%s/%s", intent.IntentId, balanceCode.BalanceCodeId),
							userAuth: normalEmail,
							template: &SubscriptionTransferBalanceCodeTemplate{
								Secret:           balanceCode.Secret,
								BalanceByteCount: balanceCode.BalanceByteCount,
							},
						})
					}
				}

			default:
				networkId := *intent.NetworkId
				// pro = false: a data pack is DATA ONLY, exactly like a redeemed code
				transferBalance := &model.TransferBalance{
					NetworkId:             networkId,
					StartTime:             now,
					EndTime:               now.Add(duration),
					StartBalanceByteCount: byteCount,
					BalanceByteCount:      byteCount,
					NetRevenue:            credit.usdNanoCents,
					Pro:                   false,
				}
				model.AddTransferBalanceInTx(ctx, tx, transferBalance)
				balanceId = &transferBalance.BalanceId
				addDataAppliedNoteInTx(ctx, tx, networkId, byteCount, fmt.Sprintf("crypto_data_pack/%s", intent.IntentId))
			}

			model.SetCryptoPaymentIntentGrantInTx(tx, ctx, intent.IntentId, balanceId, balanceCodeId)
			if 0 < len(credit.receipts) {
				intentId := intent.IntentId
				model.SetCryptoPaymentReceiptsStateInTx(tx, ctx, &intentId, credit.receipts, model.CryptoReceiptStateCredited, "")
				for _, key := range credit.receipts {
					if value, ok := credit.receiptUsd[key]; ok {
						model.SetCryptoPaymentReceiptUsdValueInTx(tx, ctx, key, value)
					}
				}
			}
			credited = true
		}, server.TxReadCommitted)
	}, func(err error) {
		credited = false
		returnErr = err
	})
	if returnErr != nil {
		return false, returnErr
	}
	if !credited {
		return false, nil
	}

	if plan {
		// the pro balance is committed: refresh the entitlement so the upgrade
		// is visible at once, not after ProCacheTtl
		model.UpdateProNetworkInConn(connOwner, ctx, *intent.NetworkId)
	}
	cryptoPayCredits.WithLabelValues(intent.Asset, intent.Delivery, cryptoCreditTiming(credit.late)).Inc()
	glog.Infof("[cryptopay]credited invoice %s (%s %s, delivery %s)\n", intent.IntentId, intent.Asset, intent.Item, intent.Delivery)
	return true, nil
}

func cryptoCreditTiming(late bool) string {
	if late {
		return "late"
	}
	return "on_time"
}

// addDataAppliedNoteInTx adds the note that the data is on the network for
// the network's admin, when their login is an email, in the credit's
// transaction. One note per key: a credit that runs again adds none.
func addDataAppliedNoteInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	byteCount model.ByteCount,
	key string,
) {
	networkName, userAuth, ok := model.GetNetworkAdminUserAuthInTx(ctx, tx, networkId)
	if !ok || userAuth == "" {
		return
	}
	normalUserAuth, userAuthType := model.NormalUserAuth(userAuth)
	if userAuthType != model.UserAuthTypeEmail {
		return
	}
	addAccountMessageInTx(ctx, tx, &accountMessage{
		key:       key,
		networkId: &networkId,
		userAuth:  normalUserAuth,
		template: &SubscriptionDataAppliedTemplate{
			BalanceByteCount: byteCount,
			NetworkName:      networkName,
		},
	})
}

// ----- the Helius webhook's hand-off -----

// cryptoCreditLinkedSolanaIntentInConn credits the invoice a Solana intent is
// linked from, when there is one: the USDC payment of a generic invoice is
// credited by the generic credit, which consumes both one-shots. linked =
// false for a Solana intent of the old endpoints, which keeps its own path.
// A payment to a voided invoice buys nothing and is recorded as unfulfilled
// for an operator (mmm/ZCASH-CONTRACT.md §3.13.12), also when the void lands between
// this lookup and the credit's one-shot.
func cryptoCreditLinkedSolanaIntentInConn(
	connOwner server.PgConn,
	clientSession *session.ClientSession,
	paymentSearchResult *model.PaymentIntentSearchResult,
	signature string,
	tokenAmountReceivedUsd float64,
) (linked bool, credited bool, returnErr error) {
	intent := cryptoLinkedSolanaIntentLookup(connOwner, clientSession, paymentSearchResult.PaymentReference)
	if intent == nil {
		return false, false, nil
	}
	if intent.CreditedAt != nil {
		// a redelivery of the payment that already credited it
		return true, false, nil
	}
	// the price on time (the webhook already checked the payment against the
	// quote); a late one is honored at what arrived, capped at the price
	usdNanoCents := intent.PriceUsdNanoCents
	late := intent.ExpiresAt.Before(server.NowUtc())
	if late {
		usdNanoCents = min(model.UsdToNanoCents(tokenAmountReceivedUsd), intent.PriceUsdNanoCents)
	}
	if intent.VoidedAt == nil {
		credited, err := cryptoCreditIntentInConn(connOwner, clientSession, intent, &cryptoCredit{
			usdNanoCents:    usdNanoCents,
			solanaSignature: signature,
			late:            late,
		})
		if err != nil || credited {
			return true, credited, err
		}
		// not credited: another credit got there first, or a void did
		intent = cryptoLinkedSolanaIntentLookup(connOwner, clientSession, paymentSearchResult.PaymentReference)
		if intent == nil || intent.VoidedAt == nil {
			return true, false, nil
		}
	}
	expectedAmountUsd := paymentSearchResult.ExpectedAmountUsd
	reference := paymentSearchResult.PaymentReference
	note := fmt.Sprintf("paid to crypto invoice %s, which an operator voided", intent.IntentId)
	glog.Infof("[cryptopay]a USDC payment reached voided invoice %s; recorded unfulfilled\n", intent.IntentId)
	err := model.RecordUnfulfilledSolanaPaymentInConn(connOwner, clientSession.Ctx, &model.UnfulfilledSolanaPayment{
		TxSignature:       signature,
		Reason:            model.SolanaUnfulfilledReasonNoIntent,
		TokenAmountUsd:    tokenAmountReceivedUsd,
		ExpectedAmountUsd: &expectedAmountUsd,
		PaymentReference:  &reference,
		NetworkId:         intent.NetworkId,
		MatchNote:         &note,
	})
	return true, false, err
}

// The invoice a Solana reference is linked from. Replaceable only by tests,
// which run the webhook's decision without Postgres.
var cryptoLinkedSolanaIntentLookup = func(connOwner server.PgConn, clientSession *session.ClientSession, reference string) *model.CryptoPaymentIntent {
	return model.GetCryptoPaymentIntentBySolanaReferenceInConn(connOwner, clientSession.Ctx, reference)
}
