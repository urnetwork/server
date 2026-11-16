package controller

// Database tests of the 256 GiB pack (data_256gib, $0.75; data_256gib_test.go):
// Gamma sells it, as a purchase and as the automatic top-up, and x402's grant
// credits exactly 256 GiB. They read the environment's pro.yml, the one the
// config release ships.
//
// Before this change Gamma sold only data_1tib and data_10tib, and the x402
// catalog had no pack at $1 or less.

import (
	"context"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Gamma's terms list the pack at 0.75 gamma, an intent for it is charged 0.75
// gamma from the deposit, and its credit is exactly 256 GiB of data on the
// account, not Pro.
func TestGammaSellsThe256GibPack(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gamma256gib")
		connect.AssertEqual(t, f.run().Readiness, gammaReady)

		terms, err := GammaTerms(clientSession)
		if err != nil {
			t.Fatal(err)
		}
		var item *GammaTermsItem
		for _, termsItem := range terms.Items {
			if termsItem.ItemId == data256GibTestSkuId {
				item = termsItem
			}
		}
		if item == nil {
			t.Fatalf("Gamma's terms do not list %s: %+v", data256GibTestSkuId, terms.Items)
		}
		connect.AssertEqual(t, item.PriceGamma, "0.75")
		connect.AssertEqual(t, item.PriceUsd, 0.75)
		connect.AssertEqual(t, item.MinDailyCapGamma, "0.75")
		connect.AssertEqual(t, item.Renews, false)

		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: data256GibTestSkuId})
		if intent.Error != nil {
			t.Fatalf("the intent was refused: %+v", intent.Error)
		}
		connect.AssertEqual(t, intent.ItemId, data256GibTestSkuId)
		connect.AssertEqual(t, intent.Plan, "")
		connect.AssertEqual(t, intent.AmountGamma, "0.75")
		connect.AssertEqual(t, intent.PriceUsd, 0.75)
		connect.AssertEqual(t, intent.MinDailyCapGamma, "0.75")

		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		coldkey := gammaTestAccount(t, gammaTestColdkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, spender: f.settings.Spender, balance: 1_000_000_000})
		connect.AssertEqual(t, f.run().Charged, 1)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbStatus(t, clientSession, intent).Status, gammaStatusPaid)
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), 256*model.Gib)
		connect.AssertEqual(t, model.IsProNetworkFresh(ctx, networkId), false)
		// 0.75 gamma was spent, the rest stays in the deposit
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(250_000_000))
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM account_message_outbox WHERE template_name = 'gamma_receipt'`), 1)
	})
}

// The pack can be the account's automatic top-up item: when the data runs out,
// the biller buys it, once, and credits exactly 256 GiB.
func TestGammaTopsUpWithThe256GibPack(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newGammaDbFixture(t)
		ctx := f.ctx
		networkId, _, clientSession := cryptoDbTestNetwork(t, ctx, "gamma256topup")
		f.run()
		coldkey := gammaTestAccount(t, gammaTestColdkey)

		// a pack bought as a code: the account pays with gamma, and its own
		// data runs out
		intent := gammaDbIntent(t, clientSession, &CreateGammaPaymentIntentArgs{ItemId: data256GibTestSkuId, Delivery: model.CryptoDeliveryCode})
		if intent.Error != nil {
			t.Fatalf("the intent was refused: %+v", intent.Error)
		}
		hotkey := gammaTestAccount(t, intent.DepositHotkey)
		f.sim.setDeposit(hotkey, &gammaSimDeposit{coldkey: coldkey, spender: f.settings.Spender, balance: 2_000_000_000})
		f.run()
		f.sim.mine()
		f.run()
		status := gammaDbStatus(t, clientSession, intent)
		connect.AssertEqual(t, status.Status, gammaStatusPaid)
		connect.AssertEqual(t, status.BalanceCode != "", true)
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), model.ByteCount(0))

		topupItem := data256GibTestSkuId
		settings, err := SetGammaSettings(&GammaSettingsArgs{TopupItemId: &topupItem}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, settings.Error == nil, true)
		connect.AssertEqual(t, settings.TopupItemId, data256GibTestSkuId)
		connect.AssertEqual(t, settings.AutoTopup, true)

		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'topup' AND item = $2`, networkId, data256GibTestSkuId), 1)
		f.sim.mine()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'topup' AND credited_at IS NOT NULL`, networkId), 1)
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), 256*model.Gib)
		connect.AssertEqual(t, f.sim.deposit(hotkey).balance, uint64(500_000_000))
		// not again within the hour, and not while the data lasts
		f.autoNow()
		f.run()
		connect.AssertEqual(t, gammaDbCount(t, ctx, `SELECT COUNT(*) FROM gamma_payment_intent WHERE network_id = $1 AND purpose = 'topup'`, networkId), 1)
	})
}

// The x402 grant of the pack is exactly 256 GiB of data, for a data code's
// year, and never Pro.
func TestX402GrantsExactly256Gib(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "x402256gib", server.NewId())

		// an x402.yml that turns on the Pro month only: the pack is pro.yml's
		c := &X402Config{
			Enabled:  true,
			Networks: []string{"base"},
			Asset:    "usdc",
			Skus:     map[string]x402SkuConfig{X402SkuProMonth: {}},
		}
		sku := x402SkuByIdForConfig(c, data256GibTestSkuId)
		if sku == nil {
			t.Fatalf("the x402 catalog has no %s: %v", data256GibTestSkuId, x402SkusForConfig(c))
		}
		err := x402GrantData(ctx, networkId, sku, model.UsdToNanoCents(sku.PriceUsd), &X402SettleResponse{
			Success:     true,
			Transaction: "0x256gib",
			Network:     "base",
		}, nil)
		connect.AssertEqual(t, err, nil)

		balances := model.GetActiveTransferBalances(ctx, networkId)
		connect.AssertEqual(t, len(balances), 1)
		connect.AssertEqual(t, balances[0].StartBalanceByteCount, 256*model.Gib)
		connect.AssertEqual(t, balances[0].BalanceByteCount, 256*model.Gib)
		connect.AssertEqual(t, balances[0].Pro, false)
		connect.AssertEqual(t, balances[0].EndTime.Sub(balances[0].StartTime), model.Pro().DataCodeDuration)
		connect.AssertEqual(t, model.GetActiveTransferBalanceByteCount(ctx, networkId), 256*model.Gib)
		connect.AssertEqual(t, model.IsProNetwork(ctx, networkId), false)
	})
}
