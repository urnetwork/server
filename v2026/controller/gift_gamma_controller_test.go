package controller

// Tests of POST /pay/gift/gamma-intent and GET /pay/gift/config's
// gamma_available that need no database (gift_gamma_controller.go): the
// refusals that come before Gamma's state is read, the answer's shape the
// gift dialog reads, and the config while Gamma is not configured.

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// A signed-in session of a network that need not exist: nothing here reads
// the database.
func giftGammaTestSession() *session.ClientSession {
	return session.Testing_CreateClientSession(context.Background(), &session.ByJwt{
		NetworkId: server.NewId(),
		UserId:    server.NewId(),
	})
}

// Gift sales closed, the order's own fields and a coldkey that is not SS58 are
// refused before Gamma is asked, with the gift routes' codes; a valid order
// while Gamma is not configured is asset_unavailable. None stores an order.
func TestGiftGammaIntentRefusesBeforeGamma(t *testing.T) {
	t.Cleanup(setGammaSettingsForTest(nil, errors.New("no gamma.yml")))
	clientSession := giftGammaTestSession()
	giftPurchaseTestResetLimiters()

	closed := model.Testing_GiftConfig()
	closed.Enabled = false
	restoreClosed := model.Testing_SetGiftConfig(closed)
	args := &GiftGammaIntentArgs{GiftPurchaseArgs: giftPurchaseTestArgs()}
	result, err := GiftGammaIntent(args, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Code, GiftErrorCodeSalesClosed)
	connect.AssertEqual(t, result.OrderId, "")
	restoreClosed()

	giftPurchaseTestConfig(t)
	for _, c := range []struct {
		name   string
		mutate func(args *GiftGammaIntentArgs)
		code   string
	}{
		{"no gifts", func(args *GiftGammaIntentArgs) { args.Quantity = 0 }, GiftErrorCodeInvalidQuantity},
		{"21 gifts", func(args *GiftGammaIntentArgs) { args.Quantity = 21 }, GiftErrorCodeInvalidQuantity},
		{"no email", func(args *GiftGammaIntentArgs) { args.Email = "nobody" }, PurchaseErrorCodeInvalidEmail},
		{"them without recipients", func(args *GiftGammaIntentArgs) { args.Delivery = model.GiftDeliveryThem }, GiftErrorCodeInvalidRecipients},
		{"a long note", func(args *GiftGammaIntentArgs) { args.Message = strings.Repeat("x", 241) }, GiftErrorCodeMessageTooLong},
		{"a link in the note", func(args *GiftGammaIntentArgs) { args.Message = "see example.com" }, GiftErrorCodeNoteHasLink},
		{"an unknown language", func(args *GiftGammaIntentArgs) { args.Language = "xx" }, PurchaseErrorCodeInvalidRequest},
		{"a coldkey that is not ss58", func(args *GiftGammaIntentArgs) { args.Coldkey = "not-a-coldkey" }, PurchaseErrorCodeInvalidRequest},
		{"gamma not configured", func(args *GiftGammaIntentArgs) {}, PurchaseErrorCodeAssetUnavailable},
	} {
		args := &GiftGammaIntentArgs{GiftPurchaseArgs: giftPurchaseTestArgs(), BittensorFooter: true}
		c.mutate(args)
		result, err := GiftGammaIntent(args, clientSession)
		connect.AssertEqual(t, err, nil)
		if result.Error == nil || result.Error.Code != c.code {
			t.Fatalf("%s: %+v, want %s", c.name, result.Error, c.code)
		}
		if result.OrderId != "" || result.OrderToken != "" || result.IntentId != "" {
			t.Fatalf("%s: a refusal carries an order or an intent: %+v", c.name, result)
		}
	}
}

// The answer is POST /pay/gamma/intent's, every field at the top level, with
// the order's fields beside them; a refusal carries error.code.
func TestGiftGammaIntentResultShape(t *testing.T) {
	result := &GiftGammaIntentResult{
		GammaPaymentIntentResult: GammaPaymentIntentResult{
			IntentId:    server.NewId().String(),
			StatusToken: "token",
			ItemId:      GiftGammaItem,
			Delivery:    model.CryptoDeliveryCode,
			AmountGamma: "89.97",
		},
		OrderId:         server.NewId().String(),
		OrderToken:      "ORDERTOKEN",
		BittensorFooter: true,
	}
	encoded, err := json.Marshal(result)
	connect.AssertEqual(t, err, nil)
	fields := map[string]any{}
	connect.AssertEqual(t, json.Unmarshal(encoded, &fields), nil)
	for _, key := range []string{
		"intent_id", "status_token", "plan", "item_id", "delivery", "tier", "offer_applied",
		"price_usd", "regular_price_usd", "amount_gamma", "reference", "netuid", "deposit_hotkey",
		"service_hotkey", "spender", "min_daily_cap_gamma", "auto_min_daily_cap_gamma",
		"min_mint_gamma", "indicative_tao", "mint_available", "mint_dry_run_command", "mint_command",
		"disclaimer", "automatic_notice", "expires_at", "order_id", "order_token", "bittensor_footer",
	} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("the answer has no %q: %s", key, encoded)
		}
	}
	connect.AssertEqual(t, fields["item_id"], "gift_pro_year")
	connect.AssertEqual(t, fields["delivery"], "code")
	connect.AssertEqual(t, fields["bittensor_footer"], true)
	if _, ok := fields["error"]; ok {
		t.Fatalf("an answer without a refusal carries error: %s", encoded)
	}

	encoded, err = json.Marshal(giftGammaIntentError(GiftErrorCodeDailyLimit, "over"))
	connect.AssertEqual(t, err, nil)
	refusal := struct {
		OrderId string `json:"order_id"`
		Error   *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{}
	connect.AssertEqual(t, json.Unmarshal(encoded, &refusal), nil)
	connect.AssertEqual(t, refusal.Error.Code, GiftErrorCodeDailyLimit)
	connect.AssertEqual(t, refusal.OrderId, "")
}

// gamma_available is false, and the config still answers, while gift sales
// are closed, the gift cap is 0, or Gamma is not configured.
func TestGiftConfigGammaUnavailable(t *testing.T) {
	t.Cleanup(setGammaSettingsForTest(nil, errors.New("no gamma.yml")))
	clientSession := giftPurchaseTestSession()
	cryptoTestEnable(t, cryptoTestConfig(), map[string]*fakeCryptoWallet{})
	cryptoTestScanCursors(t, map[string]*model.CryptoScanCursor{})

	config := giftPurchaseTestConfig(t)
	giftPurchaseTestResetLimiters()
	result, err := GiftConfig(clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Enabled, true)
	connect.AssertEqual(t, result.GammaAvailable, false)

	// no cap: Gamma is not asked
	config.CryptoMaxInvoiceUsd = 0
	connect.AssertEqual(t, giftGammaAvailable(clientSession, config), false)

	closed := model.Testing_GiftConfig()
	closed.Enabled = false
	t.Cleanup(model.Testing_SetGiftConfig(closed))
	giftPurchaseTestResetLimiters()
	result, err = GiftConfig(clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.GammaAvailable, false)

	encoded, err := json.Marshal(result)
	connect.AssertEqual(t, err, nil)
	fields := map[string]any{}
	connect.AssertEqual(t, json.Unmarshal(encoded, &fields), nil)
	if value, ok := fields["gamma_available"]; !ok || value != false {
		t.Fatalf("the config answers gamma_available %v: %s", value, encoded)
	}
}

// The gift's intent item is the coin rails' gift item, which the Gamma credit
// tells apart from every plan and data pack it sells.
func TestGiftGammaItemIsNoPlanOrPack(t *testing.T) {
	connect.AssertEqual(t, GiftGammaItem, "gift_pro_year")
	// the coin rails' gift item: one item on every gift invoice and intent
	connect.AssertEqual(t, GiftGammaItem, giftCryptoItem)
	connect.AssertEqual(t, slices.Contains(gammaKnownPlans, GiftGammaItem), false)
	connect.AssertEqual(t, slices.Contains(gammaKnownItems(), GiftGammaItem), false)
	connect.AssertEqual(t, gammaGiftIntent(&model.GammaPaymentIntent{Item: GiftGammaItem}), true)
	connect.AssertEqual(t, gammaGiftIntent(&model.GammaPaymentIntent{Item: model.SolanaPlanYearly}), false)
	connect.AssertEqual(t, gammaItemDescription(GiftGammaItem), "URnetwork Pro gift codes")
}
