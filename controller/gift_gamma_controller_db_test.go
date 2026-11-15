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

		// each order has its own intent, from the same deposit
		second := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(1, model.GiftDeliveryMe))
		connect.AssertEqual(t, second.Error == nil, true)
		connect.AssertEqual(t, second.IntentId != intent.IntentId, true)
		connect.AssertEqual(t, second.OrderId != intent.OrderId, true)
		connect.AssertEqual(t, second.DepositHotkey, intent.DepositHotkey)
		connect.AssertEqual(t, second.AmountGamma, "39.99")

		status := giftGammaTestOrderStatus(t, clientSession, intent)
		connect.AssertEqual(t, status.Status, model.GiftOrderStatusPending)
		connect.AssertEqual(t, status.FriendsSendAt == nil, true)

		// the deposit pays both: the biller charges the gifts like any purchase
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		coldkey := gammaTestAccount(t, gammaTestColdkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, spender: f.settings.Spender, balance: 200_000_000_000})
		result := f.run()
		connect.AssertEqual(t, result.Charged, 2)
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
		connect.AssertEqual(t, giftNotices["gamma_intent/"+second.IntentId].Item, "1 Pro gift code (one year)")

		// the second order is paid too, every card to its buyer
		secondOrder := giftGammaTestOrderStatus(t, clientSession, second)
		connect.AssertEqual(t, secondOrder.Status, model.GiftOrderStatusPaid)
		connect.AssertEqual(t, secondOrder.FriendsSendAt == nil, true)

		// nothing is charged twice
		f.run()
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(200_000_000_000-89_970_000_000-39_990_000_000))
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

		// the network's open purchases' limit, Gamma's own
		for i := range gammaMaxOpenIntentsPerNetwork {
			open := giftGammaTestIntent(t, clientSession, giftGammaTestArgs(1, model.GiftDeliveryMe))
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
		failed := start.issue(clientSession, func(tx server.PgTx, intent *model.GammaPaymentIntent) error {
			return model.SetGiftOrderGammaIntentIdInTx(tx, ctx, server.NewId(), intent.IntentId)
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
