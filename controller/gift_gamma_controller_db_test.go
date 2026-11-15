package controller

// Database tests of a gift order paid with Gamma (gift_gamma_controller.go)
// against the simulated chain (gamma_sim_chain_test.go): the intent is the
// gift's own purchase from the caller's deposit hotkey, refused like the other
// gift rails and like POST /pay/gamma/intent; the biller charges it like any
// purchase and credits it only on our finalized spend, in one transaction with
// the order's codes, their revenue and the delivery; nothing enrolls the
// account in Gamma's automatic charges or sends a Gamma receipt; the friends'
// emails are not held; the order is final sale; GET /pay/gift/config says when
// Gamma takes gifts; and the gift retention unlinks the order 30 days on.
// Needs the test database and pro.yml.

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A valid order of quantity gifts paid with Gamma; delivery "them" sends each
// friend their card right after payment.
func giftGammaTestArgs(quantity int, delivery string) *GiftGammaIntentArgs {
	args := &GiftGammaIntentArgs{
		GiftPurchaseArgs: GiftPurchaseArgs{
			Quantity:  quantity,
			Delivery:  delivery,
			Email:     "buyer@synthetic.example",
			Message:   "For the team.",
			Signature: "Sam",
			Language:  "de",
		},
		BittensorFooter: true,
	}
	if delivery == model.GiftDeliveryThem {
		for i := range quantity {
			args.Recipients = append(args.Recipients, fmt.Sprintf("friend%d@example.invalid", i))
		}
	}
	return args
}

func giftGammaTestIntent(t testing.TB, clientSession *session.ClientSession, args *GiftGammaIntentArgs) *GiftGammaIntentResult {
	t.Helper()
	giftPurchaseTestResetLimiters()
	result, err := GiftGammaIntent(args, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// The order's status as the gift dialog reads it, with its token.
func giftGammaTestOrderStatus(t testing.TB, clientSession *session.ClientSession, result *GiftGammaIntentResult) *GiftOrderStatusResult {
	t.Helper()
	giftPurchaseTestResetLimiters()
	status, err := GiftOrderStatus(&GiftOrderStatusArgs{OrderId: result.OrderId, Token: result.OrderToken}, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return status
}

// A gift order is a Gamma purchase of its own: the caller's deposit hotkey,
// the order's total in gamma, item gift_pro_year, delivery code, no automatic
// notice, and the order beside it. The biller charges it like any purchase and
// credits it on its finalized spend: the codes (gamma/<intent>/gift/<n>, the
// price as their revenue), the delivery scheduled, the order paid with its
// friends' emails due at once, the sales notice; no Gamma account, renewal,
// top-up or receipt; the intent's status shows no code. Nothing is charged
// twice, and the order is final sale.
func TestGiftGammaOrderLifecycle(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		giftPurchaseTestConfig(t)
		notices, restoreNotices := model.Testing_CaptureSalesPaymentNotices()
		t.Cleanup(restoreNotices)
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "giftgamma")
		connect.AssertEqual(t, f.run().Readiness, gammaReady)

		giftPurchaseTestResetLimiters()
		config, err := GiftConfig(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, config.GammaAvailable, true)

		args := giftGammaTestArgs(3, model.GiftDeliveryThem)
		args.ReferralCredit = true
		intent := giftGammaTestIntent(t, clientSession, args)
		if intent.Error != nil {
			t.Fatalf("intent refused: %+v", intent.Error)
		}
		_, firstHotkey, _ := gammaDepositHotkeyAt(f.settings.DepositHotkeyMaster, 0)
		connect.AssertEqual(t, intent.DepositHotkey, firstHotkey)
		connect.AssertEqual(t, intent.Netuid, uint16(25))
		connect.AssertEqual(t, intent.Plan, "")
		connect.AssertEqual(t, intent.ItemId, "gift_pro_year")
		connect.AssertEqual(t, intent.Delivery, model.CryptoDeliveryCode)
		connect.AssertEqual(t, intent.Tier, "")
		connect.AssertEqual(t, intent.OfferApplied, false)
		connect.AssertEqual(t, intent.PriceUsd, 89.97)
		connect.AssertEqual(t, intent.RegularPriceUsd, 89.97)
		connect.AssertEqual(t, intent.AmountGamma, "89.97")
		connect.AssertEqual(t, intent.MinDailyCapGamma, "89.97")
		connect.AssertEqual(t, intent.AutoMinDailyCapGamma, "89.97")
		connect.AssertEqual(t, intent.AutomaticNotice, "")
		connect.AssertEqual(t, intent.Disclaimer, GammaDisclaimer)
		connect.AssertEqual(t, intent.StatusToken != "", true)
		connect.AssertEqual(t, len(intent.Reference), 66)
		connect.AssertEqual(t, strings.Contains(intent.MintCommand, firstHotkey), true)
		connect.AssertEqual(t, intent.Status.Status, gammaStatusPending)
		connect.AssertEqual(t, intent.Status.ItemId, "gift_pro_year")
		connect.AssertEqual(t, intent.OrderId != "", true)
		connect.AssertEqual(t, intent.OrderToken != "", true)
		connect.AssertEqual(t, intent.BittensorFooter, true)

		intentId := server.RequireParseId(intent.IntentId)
		giftOrderId := server.RequireParseId(intent.OrderId)
		order := model.GetGiftOrder(ctx, giftOrderId)
		connect.AssertEqual(t, order.Rail, model.GiftRailGamma)
		connect.AssertEqual(t, *order.GammaIntentId, intentId)
		connect.AssertEqual(t, order.Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, order.TotalCents, int64(8997))
		connect.AssertEqual(t, order.BittensorFooter, true)
		connect.AssertEqual(t, *order.ReferrerNetworkId, networkId)
		connect.AssertEqual(t, *model.GetGiftOrderIdForGammaIntent(ctx, intentId), giftOrderId)
		stored := model.GetGammaPaymentIntent(ctx, intentId)
		connect.AssertEqual(t, stored.Item, "gift_pro_year")
		connect.AssertEqual(t, stored.Delivery, model.CryptoDeliveryCode)
		connect.AssertEqual(t, stored.Purpose, model.GammaPurposePurchase)
		connect.AssertEqual(t, stored.NetworkId, networkId)
		connect.AssertEqual(t, stored.Tier, "")
		connect.AssertEqual(t, stored.Email, "")
		connect.AssertEqual(t, stored.PriceUsdNanoCents, model.UsdToNanoCents(89.97))

		status := giftGammaTestOrderStatus(t, clientSession, intent)
		connect.AssertEqual(t, status.Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, status.FriendsSendAt == nil, true)

		// the deposit pays: the biller charges the gift like any purchase
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		coldkey := gammaTestAccount(t, gammaTestColdkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, spender: f.settings.Spender, balance: 200_000_000_000})
		result := f.run()
		connect.AssertEqual(t, result.Charged, 1)
		connect.AssertEqual(t, len(f.sim.submitted), 1)
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, &intent.GammaPaymentIntentResult).Status, gammaStatusConfirming)
		// nothing is credited before our spend is finalized
		connect.AssertEqual(t, model.GetGiftOrder(ctx, giftOrderId).Status, model.GiftOrderStatusPending)

		f.now = f.now.Add(time.Minute)
		block := f.sim.mine()
		connect.AssertEqual(t, len(block.extrinsics), 1)
		f.run()

		paidStatus := gammaDbStatus(t, clientSession, &intent.GammaPaymentIntentResult)
		connect.AssertEqual(t, paidStatus.Status, gammaStatusPaid)
		connect.AssertEqual(t, *paidStatus.SpentBlock, int64(block.number))
		connect.AssertEqual(t, paidStatus.BalanceCode, "")
		connect.AssertEqual(t, paidStatus.ItemId, "gift_pro_year")
		connect.AssertEqual(t, paidStatus.Delivery, model.CryptoDeliveryCode)
		credited := model.GetGammaPaymentIntent(ctx, intentId)
		connect.AssertEqual(t, credited.CreditedAt != nil, true)
		connect.AssertEqual(t, credited.BalanceId == nil, true)
		connect.AssertEqual(t, credited.BalanceCodeId == nil, true)

		order = model.GetGiftOrder(ctx, giftOrderId)
		connect.AssertEqual(t, order.Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, order.PaidTime.Equal(*credited.CreditedAt), true)
		codes := model.GetGiftOrderCodes(ctx, giftOrderId)
		connect.AssertEqual(t, len(codes), 3)
		revenue := model.NanoCents(0)
		for n, code := range codes {
			balanceCode, err := model.GetBalanceCode(ctx, code.BalanceCodeId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, balanceCode.PurchaseEventId, fmt.Sprintf("gamma/%s/gift/%d", intentId, n))
			connect.AssertEqual(t, *code.RecipientIndex, n)
			revenue += balanceCode.NetRevenue
		}
		// the price is the codes' revenue
		connect.AssertEqual(t, revenue, model.UsdToNanoCents(89.97))

		// the delivery is scheduled with the codes
		renderName := task.NewTaskTarget(GiftRenderCards).TargetFunctionName()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM pending_task WHERE function_name = $1 AND run_once_key = $2`, renderName, giftRenderCardsRunOnce(giftOrderId).String()), 1)

		// paid with Gamma, the friends' emails are not held: due at the payment
		status = giftGammaTestOrderStatus(t, clientSession, intent)
		connect.AssertEqual(t, status.Status, model.GiftOrderStatusPaid)
		if status.FriendsSendAt == nil || !status.FriendsSendAt.Equal(*order.PaidTime) {
			t.Fatalf("friends_send_at %v, want the payment %s", status.FriendsSendAt, order.PaidTime)
		}
		beforePost := server.NowUtc()
		giftDeliveryTestPost(t, ctx, giftOrderId)
		for i := range 3 {
			friend := model.GetAccountMessageByKey(ctx, giftCardFriendTemplateName, giftFriendEmailKey(giftOrderId, i))
			if friend == nil || friend.UserAuth != fmt.Sprintf("friend%d@example.invalid", i) || friend.DeliverTime.After(beforePost.Add(time.Minute)) {
				t.Fatalf("friend %d email %+v, want it due now", i, friend)
			}
		}

		// a gift enrolls the account in no automatic charge, and sends no
		// Gamma receipt: the gift's own buyer email is the receipt
		connect.AssertEqual(t, model.GetGammaAccount(ctx, networkId) == nil, true)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE template_name = 'gamma_receipt'`), 0)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM subscription_renewal WHERE network_id = $1`, networkId), 0)

		// the sales notice, as the other gift rails post one, for no network
		giftNotices := map[string]*model.SalesPaymentNotice{}
		for _, notice := range notices() {
			giftNotices[notice.RecordKey] = notice
		}
		notice := giftNotices["gamma_intent/"+intent.IntentId]
		if notice == nil {
			t.Fatalf("no sales notice for the gift intent: %+v", notices())
		}
		connect.AssertEqual(t, notice.Item, "3 Pro gift codes (one year each)")
		connect.AssertEqual(t, notice.Method, "SN25 Gamma")
		connect.AssertEqual(t, notice.Kind, model.SalesPaymentKindOneTime)
		connect.AssertEqual(t, notice.NetworkId == nil, true)
		connect.AssertEqual(t, strings.HasPrefix(notice.Amount, "89.97 gamma"), true)

		// nothing is charged twice
		f.run()
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(200_000_000_000-89_970_000_000))

		// the next gift from the same deposit is an order of its own
		second := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(1, model.GiftDeliveryMe))
		connect.AssertEqual(t, second.Error == nil, true)
		connect.AssertEqual(t, second.IntentId != intent.IntentId, true)
		connect.AssertEqual(t, second.OrderId != intent.OrderId, true)
		connect.AssertEqual(t, second.DepositHotkey, intent.DepositHotkey)
		connect.AssertEqual(t, second.AmountGamma, "39.99")
		// a paid gift is never voided by the next
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, intentId).VoidedAt == nil, true)
		connect.AssertEqual(t, len(model.GetGiftOrderCodes(ctx, giftOrderId)), 3)

		// final sale: a refund that named it would void nothing, for an
		// operator to settle
		server.Tx(ctx, func(tx server.PgTx) {
			details := map[string]any{}
			action := stripeGiftRefundInTx(tx, ctx, giftOrderId, "re_gamma_"+server.NewId().String(), model.PaymentReconcileActionRefunded, 8997, details, server.NowUtc())
			connect.AssertEqual(t, action, model.PaymentReconcileActionRefundUnmatched)
			connect.AssertEqual(t, details["reason"], giftFinalSaleReason)
			details = map[string]any{}
			action = stripeGiftRefundInTx(tx, ctx, giftOrderId, "dp_gamma_"+server.NewId().String(), model.PaymentReconcileActionDisputed, 8997, details, server.NowUtc())
			connect.AssertEqual(t, action, model.PaymentReconcileActionRefundUnmatched)
			connect.AssertEqual(t, details["reason"], giftFinalSaleReason)
		}, server.TxReadCommitted)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, giftOrderId).Status, model.GiftOrderStatusPaid)
		for _, code := range model.GetGiftOrderCodes(ctx, giftOrderId) {
			connect.AssertEqual(t, code.Voided, false)
		}
	})
}

// A guest network may buy gifts with Gamma, since the codes are the delivery,
// though it may not buy a Gamma plan.
func TestGiftGammaGuestNetworkMayBuy(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		giftPurchaseTestConfig(t)
		f.run()
		guest := newPurchaseGuestDbNetwork(t, ctx)

		plan := gammaDbIntent(t, guest.clientSession, &CreateGammaPaymentIntentArgs{Plan: model.SolanaPlanYearly})
		connect.AssertEqual(t, plan.Error.Code, PurchaseErrorCodeGuestSignInRequired)

		gift := giftGammaTestIntent(t, guest.clientSession, giftGammaTestArgs(2, model.GiftDeliveryMe))
		if gift.Error != nil {
			t.Fatalf("a guest's gift order was refused: %+v", gift.Error)
		}
		connect.AssertEqual(t, gift.OrderId != "", true)
		connect.AssertEqual(t, gift.AmountGamma, "59.98")
		stored := model.GetGammaPaymentIntent(ctx, server.RequireParseId(gift.IntentId))
		connect.AssertEqual(t, stored.NetworkId, guest.networkId)
	})
}

// The refusals: Gamma unavailable, the gift cap, minting off with an empty
// deposit, the daily friend emails, the open purchases' limit and the
// per-address limit refuse with their codes and store no order and no
// intent; a link that cannot be recorded records no intent either.
func TestGiftGammaIntentRefusals(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		config := giftPurchaseTestConfig(t)
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "giftgammarefusals")
		counts := func() (orders int, intents int) {
			return gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gift_order`), gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent`)
		}
		refused := func(name string, args *GiftGammaIntentArgs, code string) *GiftGammaIntentResult {
			t.Helper()
			ordersBefore, intentsBefore := counts()
			result := giftGammaTestIntent(t, clientSession, args)
			if result.Error == nil || result.Error.Code != code {
				t.Fatalf("%s: %+v, want %s", name, result.Error, code)
			}
			ordersAfter, intentsAfter := counts()
			if result.OrderId != "" || result.IntentId != "" || ordersAfter != ordersBefore || intentsAfter != intentsBefore {
				t.Fatalf("%s stored an order or an intent: %+v (orders %d to %d, intents %d to %d)", name, result, ordersBefore, ordersAfter, intentsBefore, intentsAfter)
			}
			return result
		}

		// before the biller has judged the chain, Gamma is unavailable
		refused("unavailable", giftGammaTestArgs(1, model.GiftDeliveryMe), PurchaseErrorCodeAssetUnavailable)
		giftPurchaseTestResetLimiters()
		available, err := GiftConfig(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, available.GammaAvailable, false)
		connect.AssertEqual(t, f.run().Readiness, gammaReady)

		// the cap of a gift paid in a coin or with Gamma
		config.CryptoMaxInvoiceUsd = 50
		refused("over the cap", giftGammaTestArgs(2, model.GiftDeliveryMe), PurchaseErrorCodeItemUnavailable)
		config.CryptoMaxInvoiceUsd = 0
		refused("no cap", giftGammaTestArgs(1, model.GiftDeliveryMe), PurchaseErrorCodeItemUnavailable)
		giftPurchaseTestResetLimiters()
		available, err = GiftConfig(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, available.GammaAvailable, false)
		config.CryptoMaxInvoiceUsd = 500

		// the per-address limit of the gift purchases
		giftPurchaseTestResetLimiters()
		for range payDataCheckoutIpLimitPerMinute {
			giftPurchaseLimiter.allow(clientSession)
		}
		ordersBefore, intentsBefore := counts()
		result, err := GiftGammaIntent(giftGammaTestArgs(1, model.GiftDeliveryMe), clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error.Code, PurchaseErrorCodeRateLimited)
		ordersAfter, intentsAfter := counts()
		connect.AssertEqual(t, ordersAfter, ordersBefore)
		connect.AssertEqual(t, intentsAfter, intentsBefore)

		// the buyer's daily friend emails, as on the other rails
		giftRailsTestOrder(t, ctx, "buyer@synthetic.example", model.GiftDeliveryThem, 20)
		over := refused("over the daily friend emails", giftGammaTestArgs(1, model.GiftDeliveryThem), GiftErrorCodeDailyLimit)
		connect.AssertEqual(t, strings.Contains(over.Error.Message, `("me")`), true)

		// the network's open purchases' limit, Gamma's own: a gift voids the
		// account's other open gift, so data codes fill it
		for i := range gammaMaxOpenIntentsPerNetwork {
			open := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib, Delivery: model.CryptoDeliveryCode})
			if open.Error != nil {
				t.Fatalf("open purchase %d refused: %+v", i, open.Error)
			}
		}
		refused("too many open purchases", giftGammaTestArgs(1, model.GiftDeliveryMe), PurchaseErrorCodeRateLimited)
		cryptoDbExec(ctx, `UPDATE gamma_payment_intent SET voided_at = $1 WHERE voided_at IS NULL`, f.now)

		// minting off, and the deposit cannot already pay
		f.sim.priceAge = 301
		f.run()
		mintOff := refused("minting off", giftGammaTestArgs(1, model.GiftDeliveryMe), PurchaseErrorCodeAssetUnavailable)
		connect.AssertEqual(t, mintOff.MintUnavailableReason, gammaMintPriceStale)
		f.sim.priceAge = 10
		f.run()

		// an intent whose link cannot be recorded is never recorded: the
		// biller can never charge an intent without its order
		view, refusal := gammaPurchaseView(clientSession)
		connect.AssertEqual(t, refusal == nil, true)
		start, refusal := gammaStartPurchase(clientSession, view, &gammaPurchase{
			item:       GiftGammaItem,
			delivery:   model.CryptoDeliveryCode,
			priceUsd:   39.99,
			regularUsd: 39.99,
		})
		connect.AssertEqual(t, refusal == nil, true)
		_, intentsBefore = counts()
		failed := start.issue(clientSession, func(tx server.PgTx, intent *model.GammaPaymentIntent) (*GammaPaymentIntentResult, error) {
			return nil, model.SetGiftOrderGammaIntentIdInTx(tx, ctx, server.NewId(), intent.IntentId)
		})
		connect.AssertEqual(t, failed.Error.Code, PurchaseErrorCodeStartFailed)
		_, intentsAfter = counts()
		connect.AssertEqual(t, intentsAfter, intentsBefore)

		// and a valid order is still sold
		sold := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(1, model.GiftDeliveryMe))
		connect.AssertEqual(t, sold.Error == nil, true)
	})
}

// GET /pay/gift/config's gamma_available follows Gamma's readiness, as GET
// /pay/gamma/terms reports it: true while the biller is ready, false once its
// state is stale, and false while gift sales are closed.
func TestGiftConfigGammaAvailableFollowsGamma(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		giftPurchaseTestConfig(t)
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "giftgammaconfig")
		giftConfig := func() *GiftConfigResult {
			t.Helper()
			giftPurchaseTestResetLimiters()
			result, err := GiftConfig(clientSession)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}

		connect.AssertEqual(t, giftConfig().GammaAvailable, false)
		f.run()
		connect.AssertEqual(t, giftConfig().GammaAvailable, true)
		terms, err := GammaTerms(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, terms.Available, true)

		// the biller stopped writing its state
		f.now = f.now.Add(gammaStateFreshness + time.Minute)
		connect.AssertEqual(t, giftConfig().GammaAvailable, false)
		terms, err = GammaTerms(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, terms.Available, false)
		f.run()
		connect.AssertEqual(t, giftConfig().GammaAvailable, true)

		closed := model.Testing_GiftConfig()
		closed.Enabled = false
		t.Cleanup(model.Testing_SetGiftConfig(closed))
		connect.AssertEqual(t, giftConfig().GammaAvailable, false)
	})
}

// The daily gift retention unlinks a Gamma order 30 days on, as the crypto
// rail's unlink does a coin's: its codes still redeem, and its buyer's
// address goes once no friend's email is owed.
func TestGiftRetentionUnlinksGammaOrders(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		giftPurchaseTestConfig(t)
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "giftgammaunlink")
		f.run()

		intent := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(2, model.GiftDeliveryMe))
		connect.AssertEqual(t, intent.Error == nil, true)
		f.sim.setDeposit(gammaTestAccount(t, intent.DepositHotkey), &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), spender: f.settings.Spender, balance: 100_000_000_000})
		f.run()
		f.now = f.now.Add(time.Minute)
		f.sim.mine()
		f.run()
		giftOrderId := server.RequireParseId(intent.OrderId)
		intentId := server.RequireParseId(intent.IntentId)
		connect.AssertEqual(t, model.GetGiftOrder(ctx, giftOrderId).Status, model.GiftOrderStatusPaid)
		codes := model.GetGiftOrderCodes(ctx, giftOrderId)
		connect.AssertEqual(t, len(codes), 2)

		// not yet: credited today
		result, err := runGiftRetention(ctx, server.NowUtc())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.GammaUnlinkedCount, 0)
		connect.AssertEqual(t, *model.GetGiftOrder(ctx, giftOrderId).GammaIntentId, intentId)

		// 31 days on
		result, err = runGiftRetention(ctx, server.NowUtc().Add(model.GiftGammaUnlinkAfter+24*time.Hour))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.GammaUnlinkedCount, 1)
		order := model.GetGiftOrder(ctx, giftOrderId)
		connect.AssertEqual(t, order.GammaIntentId == nil, true)
		connect.AssertEqual(t, order.BuyerEmail, "")
		connect.AssertEqual(t, model.GetGiftOrderIdForGammaIntent(ctx, intentId) == nil, true)
		unlinked := model.GetGiftOrderCodes(ctx, giftOrderId)
		connect.AssertEqual(t, len(unlinked), 2)
		for n, code := range unlinked {
			connect.AssertEqual(t, code.Secret, codes[n].Secret)
			balanceCode, err := model.GetBalanceCode(ctx, code.BalanceCodeId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, strings.HasPrefix(balanceCode.PurchaseEventId, "gamma/unlinked/"), true)
		}
		// its codes still redeem
		giftCryptoTestRedeem(t, ctx, unlinked[0].Secret)
		// the intent still reads as paid with its token
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, &intent.GammaPaymentIntentResult).Status, gammaStatusPaid)
	})
}

// Cancels the order's Gamma purchase with its token, as "pay another way"
// does.
func giftGammaTestCancel(t testing.TB, clientSession *session.ClientSession, orderId string, token string) *GiftGammaCancelResult {
	t.Helper()
	giftGammaCancelLimiter.stateLock.Lock()
	giftGammaCancelLimiter.window = time.Time{}
	giftGammaCancelLimiter.stateLock.Unlock()
	result, err := GiftGammaCancel(&GiftOrderStatusArgs{OrderId: orderId, Token: token}, clientSession)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// A new gift purchase voids the account's open gift purchase in the
// transaction that records it, and never a data purchase. The order left
// behind stays pending, and its friends no longer count toward the daily cap,
// so changing a large gift is not refused. The deposit could pay both: the
// biller charges only the new one, and never the voided one.
func TestGiftGammaNewPurchaseVoidsTheOpenOne(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		giftPurchaseTestConfig(t)
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "giftgammareplace")
		f.run()

		data := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: StripeItemData1Tib, Delivery: model.CryptoDeliveryCode})
		connect.AssertEqual(t, data.Error == nil, true)
		// 15 friends, then changed to 12: 27 would pass the daily cap of 20
		// if the first still counted
		first := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(15, model.GiftDeliveryThem))
		connect.AssertEqual(t, first.Error == nil, true)
		second := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(12, model.GiftDeliveryThem))
		if second.Error != nil {
			t.Fatalf("the changed gift was refused: %+v", second.Error)
		}
		firstIntentId := server.RequireParseId(first.IntentId)
		secondIntentId := server.RequireParseId(second.IntentId)
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, firstIntentId).VoidedAt != nil, true)
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, secondIntentId).VoidedAt == nil, true)
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, server.RequireParseId(data.IntentId)).VoidedAt == nil, true)
		connect.AssertEqual(t, model.GetGiftFriendEmailCount(ctx, "buyer@synthetic.example", server.NowUtc().Add(-model.GiftFriendEmailWindow)), 12)

		// the voided purchase reads voided, its order pending
		voided := gammaDbStatus(t, clientSession, &first.GammaPaymentIntentResult)
		connect.AssertEqual(t, voided.Status, gammaStatusReview)
		connect.AssertEqual(t, voided.ReviewReason, "voided")
		firstOrder := giftGammaTestOrderStatus(t, clientSession, first)
		connect.AssertEqual(t, firstOrder.Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, firstOrder.FriendsSendAt == nil, true)
		// cancelling it again changes nothing
		again := giftGammaTestCancel(t, clientSession, first.OrderId, first.OrderToken)
		connect.AssertEqual(t, again.Cancelled, true)
		connect.AssertEqual(t, again.Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, again.Error == nil, true)

		// the deposit could pay everything: the voided gift is never charged
		hotkey := gammaTestAccount(t, second.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), spender: f.settings.Spender, balance: 1_000_000_000_000})
		connect.AssertEqual(t, f.run().Charged, 2)
		f.now = f.now.Add(time.Minute)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, &second.GammaPaymentIntentResult).Status, gammaStatusPaid)
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, data).Status, gammaStatusPaid)
		connect.AssertEqual(t, model.GetGammaChargeForIntent(ctx, firstIntentId) == nil, true)
		connect.AssertEqual(t, giftGammaTestOrderStatus(t, clientSession, second).Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, giftGammaTestOrderStatus(t, clientSession, first).Status, model.GiftOrderStatusPending)
		dataUnits, ok := gammaParseUnits(data.AmountGamma)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(1_000_000_000_000-287_880_000_000-dataUnits))
		f.run()
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, model.GetGammaChargeForIntent(ctx, firstIntentId) == nil, true)
	})
}

// While an earlier gift's spend is in flight, a new gift purchase is refused
// with payment_in_progress, storing nothing, and so is the earlier order's
// cancel. Once it is credited, the cancel answers paid, and the next gift is
// sold. A spend that landed and is not yet credited is in flight too.
func TestGiftGammaPaymentInProgress(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		giftPurchaseTestConfig(t)
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "giftgammainflight")
		f.run()
		counts := func() (orders int, intents int) {
			return gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gift_order`), gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent`)
		}

		first := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(1, model.GiftDeliveryMe))
		connect.AssertEqual(t, first.Error == nil, true)
		hotkey := gammaTestAccount(t, first.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: gammaTestAccount(t, gammaTestColdkey), spender: f.settings.Spender, balance: 100_000_000_000})
		connect.AssertEqual(t, f.run().Charged, 1)
		connect.AssertEqual(t, len(f.sim.submitted), 1)

		// its spend is in a submitted batch
		ordersBefore, intentsBefore := counts()
		refused := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(2, model.GiftDeliveryMe))
		connect.AssertEqual(t, refused.Error.Code, GiftErrorCodePaymentInProgress)
		connect.AssertEqual(t, refused.Error.Message, "A Gamma payment for an earlier gift is still being processed. Try again in a few minutes.")
		connect.AssertEqual(t, refused.OrderId, "")
		ordersAfter, intentsAfter := counts()
		connect.AssertEqual(t, ordersAfter, ordersBefore)
		connect.AssertEqual(t, intentsAfter, intentsBefore)
		cancel := giftGammaTestCancel(t, clientSession, first.OrderId, first.OrderToken)
		connect.AssertEqual(t, cancel.Cancelled, false)
		connect.AssertEqual(t, cancel.Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, cancel.Error.Code, GiftErrorCodePaymentInProgress)
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, server.RequireParseId(first.IntentId)).VoidedAt == nil, true)

		// landed and credited: paid
		f.now = f.now.Add(time.Minute)
		f.sim.mine()
		f.run()
		cancel = giftGammaTestCancel(t, clientSession, first.OrderId, first.OrderToken)
		connect.AssertEqual(t, cancel.Cancelled, false)
		connect.AssertEqual(t, cancel.Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, cancel.Error == nil, true)
		next := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(2, model.GiftDeliveryMe))
		connect.AssertEqual(t, next.Error == nil, true)

		// a spend that landed and is not yet credited is in flight too
		nextIntent := model.GetGammaPaymentIntent(ctx, server.RequireParseId(next.IntentId))
		connect.AssertEqual(t, model.CreateGammaCharge(ctx, nextIntent, f.now), true)
		cryptoDbExec(ctx, `UPDATE gamma_charge SET state = 'spent', block_number = 1, resolved_at = $2 WHERE intent_id = $1`, nextIntent.IntentId, f.now)
		connect.AssertEqual(t, giftGammaTestIntent(t, clientSession, giftGammaTestArgs(1, model.GiftDeliveryMe)).Error.Code, GiftErrorCodePaymentInProgress)
		connect.AssertEqual(t, giftGammaTestCancel(t, clientSession, next.OrderId, next.OrderToken).Error.Code, GiftErrorCodePaymentInProgress)
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, nextIntent.IntentId).VoidedAt == nil, true)
	})
}

// POST /pay/gift/gamma-cancel in each state: a token that opens no order and a
// card order are invalid_request; an open purchase is voided, with a pending
// charge not yet in a batch or a skipped one, and a repeated cancel answers
// the same; the biller never charges a voided purchase, nor re-arms its
// skipped charge when the deposit changes.
func TestGiftGammaCancel(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		giftPurchaseTestConfig(t)
		_, _, clientSession := cryptoDbTestNetwork(t, ctx, "giftgammacancel")
		signedOut := giftPurchaseTestSession()
		f.run()

		// a token that opens no order
		unknown := giftGammaTestCancel(t, signedOut, server.NewId().String(), "ORDERTOKEN")
		connect.AssertEqual(t, unknown.Cancelled, false)
		connect.AssertEqual(t, unknown.Status, "")
		connect.AssertEqual(t, unknown.Error.Code, PurchaseErrorCodeInvalidRequest)
		// a card order
		card := &model.GiftOrder{Quantity: 1, UnitPriceCents: 3999, TotalCents: 3999, Rail: model.GiftRailStripe, Delivery: model.GiftDeliveryMe, BuyerEmail: "buyer@synthetic.example", Language: "en"}
		connect.AssertEqual(t, model.CreateGiftOrder(ctx, card), nil)
		notGamma := giftGammaTestCancel(t, signedOut, card.GiftOrderId.String(), card.OrderToken)
		connect.AssertEqual(t, notGamma.Cancelled, false)
		connect.AssertEqual(t, notGamma.Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, notGamma.Error.Code, PurchaseErrorCodeInvalidRequest)

		// open, nothing charged yet; a wrong token opens nothing
		open := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(1, model.GiftDeliveryMe))
		connect.AssertEqual(t, open.Error == nil, true)
		wrong := giftGammaTestCancel(t, signedOut, open.OrderId, "WRONGTOKEN")
		connect.AssertEqual(t, wrong.Error.Code, PurchaseErrorCodeInvalidRequest)
		connect.AssertEqual(t, wrong.Status, "")
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, server.RequireParseId(open.IntentId)).VoidedAt == nil, true)
		cancelled := giftGammaTestCancel(t, signedOut, open.OrderId, open.OrderToken)
		connect.AssertEqual(t, cancelled.Cancelled, true)
		connect.AssertEqual(t, cancelled.Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, cancelled.Error == nil, true)
		connect.AssertEqual(t, model.GetGammaPaymentIntent(ctx, server.RequireParseId(open.IntentId)).VoidedAt != nil, true)
		connect.AssertEqual(t, giftGammaTestCancel(t, signedOut, open.OrderId, open.OrderToken).Cancelled, true)

		// a pending charge not yet in a batch is voided with it
		pending := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(1, model.GiftDeliveryMe))
		pendingIntent := model.GetGammaPaymentIntent(ctx, server.RequireParseId(pending.IntentId))
		connect.AssertEqual(t, model.CreateGammaCharge(ctx, pendingIntent, f.now), true)
		connect.AssertEqual(t, giftGammaTestCancel(t, signedOut, pending.OrderId, pending.OrderToken).Cancelled, true)
		connect.AssertEqual(t, model.GetGammaChargeForIntent(ctx, pendingIntent.IntentId).State, model.GammaChargeVoided)

		// the biller never charges a voided purchase, however much the
		// deposit holds
		hotkey := gammaTestAccount(t, pending.DepositHotkey)
		coldkey := gammaTestAccount(t, gammaTestColdkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, spender: f.settings.Spender, balance: 100_000_000_000})
		connect.AssertEqual(t, f.run().Charged, 0)
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_spend_batch`), 0)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(100_000_000_000))

		// a skipped charge: the deposit was emptied as the spend landed
		skipped := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(1, model.GiftDeliveryMe))
		connect.AssertEqual(t, skipped.Error == nil, true)
		connect.AssertEqual(t, f.run().Charged, 1)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, spender: f.settings.Spender, balance: 1_000_000_000})
		f.now = f.now.Add(time.Minute)
		f.sim.mine()
		f.run()
		skippedIntentId := server.RequireParseId(skipped.IntentId)
		connect.AssertEqual(t, model.GetGammaChargeForIntent(ctx, skippedIntentId).State, model.GammaChargeSkipped)
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, &skipped.GammaPaymentIntentResult).Status, gammaStatusSeen)
		cancelled = giftGammaTestCancel(t, signedOut, skipped.OrderId, skipped.OrderToken)
		connect.AssertEqual(t, cancelled.Cancelled, true)
		connect.AssertEqual(t, model.GetGammaChargeForIntent(ctx, skippedIntentId).State, model.GammaChargeVoided)
		// the deposit changes and could pay: nothing is re-armed or sent
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, spender: f.settings.Spender, balance: 100_000_000_000})
		f.run()
		f.run()
		connect.AssertEqual(t, len(f.sim.submitted), 0)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(100_000_000_000))
		connect.AssertEqual(t, giftGammaTestOrderStatus(t, clientSession, skipped).Status, model.GiftOrderStatusPending)
	})
}

// Two gift purchases of one account at once: the account's gift purchases are
// serialized, so the later one voids the earlier and one stays open.
func TestGiftGammaConcurrentPurchasesLeaveOneOpen(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		giftPurchaseTestConfig(t)
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "giftgammaconcurrent")
		f.run()
		// the account's hotkey exists before the race, as after its first
		// purchase
		connect.AssertEqual(t, giftGammaTestIntent(t, clientSession, giftGammaTestArgs(1, model.GiftDeliveryMe)).Error == nil, true)

		for round := range 3 {
			giftPurchaseTestResetLimiters()
			start := make(chan struct{})
			var wait sync.WaitGroup
			results := make([]*GiftGammaIntentResult, 2)
			errs := make([]error, 2)
			for i := range 2 {
				wait.Add(1)
				go func() {
					defer wait.Done()
					<-start
					results[i], errs[i] = GiftGammaIntent(giftGammaTestArgs(i+1, model.GiftDeliveryMe), clientSession)
				}()
			}
			close(start)
			wait.Wait()
			for i := range 2 {
				if errs[i] != nil || results[i].Error != nil {
					t.Fatalf("round %d purchase %d: %+v %v", round, i, results[i], errs[i])
				}
			}
			open := gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND item = 'gift_pro_year' AND voided_at IS NULL AND credited_at IS NULL`, networkId)
			if open != 1 {
				t.Fatalf("round %d: %d open gift purchases, want 1", round, open)
			}
		}
	})
}
