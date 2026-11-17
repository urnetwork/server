package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A paid gift order's codes, its giver's network as their referrer.
func networkCreateGiftTestCodes(t testing.TB, ctx context.Context, quantity int, referrerNetworkId *server.Id) []*model.GiftCode {
	t.Helper()
	order := &model.GiftOrder{
		Quantity:          quantity,
		UnitPriceCents:    model.Gift().UnitPriceCents(quantity),
		TotalCents:        int64(quantity) * model.Gift().UnitPriceCents(quantity),
		Rail:              model.GiftRailStripe,
		Delivery:          model.GiftDeliveryMe,
		BuyerEmail:        "giver@example.invalid",
		Language:          "en",
		ReferrerNetworkId: referrerNetworkId,
	}
	if err := model.CreateGiftOrder(ctx, order); err != nil {
		t.Fatal(err)
	}
	sessionId := "cs_test_gift_" + server.NewId().String()
	if err := model.SetGiftOrderStripeSessionId(ctx, order.GiftOrderId, sessionId); err != nil {
		t.Fatal(err)
	}
	var codes []*model.GiftCode
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		codes, _, err = model.FulfillGiftOrderInTx(tx, ctx, order.GiftOrderId, sessionId, 0, server.NowUtc())
	}, server.TxReadCommitted)
	if err != nil || len(codes) != quantity {
		t.Fatalf("fulfil: %d codes, %v", len(codes), err)
	}
	return codes
}

func networkCreateGiftTestReferrer(ctx context.Context, networkId server.Id) *server.Id {
	referralNetwork := model.GetReferralNetworkByChildNetworkId(ctx, networkId)
	if referralNetwork == nil {
		return nil
	}
	return &referralNetwork.Id
}

// A network created with a giver's gift code starts Pro with its first
// cycle's data, and becomes the giver's referral after the create commits. A
// referral code the sign-up names itself is linked first and is never
// replaced by the gift's giver. A data code at sign-up is not Pro.
func TestNetworkCreateWithGiftCodeLinksTheGiver(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetGiftConfig(model.Testing_GiftConfig())()
		messages := newOutboxTestSender()
		previousSender := GetAWSMessageSender()
		SetMessageSender(messages)
		defer SetMessageSender(previousSender)

		giverNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, giverNetworkId, fmt.Sprintf("synthetic-giver-%s", giverNetworkId.String()[:8]), server.NewId())
		model.CreateNetworkReferralCode(ctx, giverNetworkId)
		codes := networkCreateGiftTestCodes(t, ctx, 2, &giverNetworkId)

		result, err := NetworkCreate(model.NetworkCreateArgs{Terms: true, BalanceCode: &codes[0].Secret}, session.Testing_CreateClientSession(ctx, nil))
		if err != nil || result.Error != nil || result.Network == nil {
			t.Fatalf("create: %+v %v", result, err)
		}
		networkId := result.Network.NetworkId
		if !result.Network.IsPro || !model.IsPro(ctx, &networkId) {
			t.Fatal("the network created with a gift is not Pro")
		}
		if referrer := networkCreateGiftTestReferrer(ctx, networkId); referrer == nil || *referrer != giverNetworkId {
			t.Fatalf("the new network's referrer is %v, want the giver %s", referrer, giverNetworkId)
		}
		if 0 < model.Pro().DataAmount(true) {
			proData := model.ByteCount(0)
			for _, balance := range model.GetActiveTransferBalances(ctx, networkId) {
				if balance.Pro {
					proData += balance.BalanceByteCount
				}
			}
			if proData != model.Pro().DataAmount(true) {
				t.Fatalf("the new network holds %d bytes of Pro data, want the cycle's %d", proData, model.Pro().DataAmount(true))
			}
		}

		// the sign-up's own referral code wins
		ownReferrerNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, ownReferrerNetworkId, fmt.Sprintf("synthetic-referrer-%s", ownReferrerNetworkId.String()[:8]), server.NewId())
		ownCode := model.CreateNetworkReferralCode(ctx, ownReferrerNetworkId)
		result, err = NetworkCreate(model.NetworkCreateArgs{Terms: true, BalanceCode: &codes[1].Secret, ReferralCode: &ownCode.ReferralCode}, session.Testing_CreateClientSession(ctx, nil))
		if err != nil || result.Error != nil || result.Network == nil {
			t.Fatalf("create with a referral code: %+v %v", result, err)
		}
		if referrer := networkCreateGiftTestReferrer(ctx, result.Network.NetworkId); referrer == nil || *referrer != ownReferrerNetworkId {
			t.Fatalf("the sign-up's referrer is %v, want its own code's %s", referrer, ownReferrerNetworkId)
		}
		if !result.Network.IsPro {
			t.Fatal("the second gift network is not Pro")
		}

		// a data code is data only
		dataCode, err := model.CreateBalanceCode(ctx, 1024*1024, 0, model.UsdToNanoCents(1), "synthetic-signup-data-"+server.NewId().String(), "receipt", "")
		if err != nil {
			t.Fatal(err)
		}
		result, err = NetworkCreate(model.NetworkCreateArgs{Terms: true, BalanceCode: &dataCode.Secret}, session.Testing_CreateClientSession(ctx, nil))
		if err != nil || result.Error != nil || result.Network == nil {
			t.Fatalf("create with a data code: %+v %v", result, err)
		}
		if result.Network.IsPro || model.IsPro(ctx, &result.Network.NetworkId) || networkCreateGiftTestReferrer(ctx, result.Network.NetworkId) != nil {
			t.Fatal("a data code at sign-up made the network Pro or a referral")
		}
	})
}

// The signed-in redeem route grants a gift's year, says so, and links the
// giver's referral; the year leaves a new purchase open.
func TestRedeemBalanceCodeGrantsAGiftYear(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer model.Testing_SetGiftConfig(model.Testing_GiftConfig())()

		giverNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, giverNetworkId, fmt.Sprintf("synthetic-giver-%s", giverNetworkId.String()[:8]), server.NewId())
		model.CreateNetworkReferralCode(ctx, giverNetworkId)
		codes := networkCreateGiftTestCodes(t, ctx, 1, &giverNetworkId)

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("synthetic-friend-%s", networkId.String()[:8]), userId)
		clientSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "friend", false, false))

		result, err := RedeemBalanceCode(RedeemBalanceCodeArgs{Secret: codes[0].Secret}, clientSession)
		if err != nil || result.Error != nil || result.Pro == nil || result.Pro.Queued || result.TransferBalance == nil || result.TransferBalance.BalanceByteCount != 0 {
			t.Fatalf("redeem: %+v %v", result, err)
		}
		if !model.IsPro(ctx, &networkId) {
			t.Fatal("the friend is not Pro")
		}
		if referrer := networkCreateGiftTestReferrer(ctx, networkId); referrer == nil || *referrer != giverNetworkId {
			t.Fatalf("the friend's referrer is %v, want the giver %s", referrer, giverNetworkId)
		}

		// a gift year never renews, so it blocks neither a card checkout nor a
		// Gamma plan (the owner, 2026-11-13)
		markets := model.GetActiveSubscriptionRenewalMarkets(ctx, networkId, model.SubscriptionTypeSupporter)
		if len(markets) != 1 || markets[0] != model.SubscriptionMarketGift {
			t.Fatalf("the friend's active markets are %v", markets)
		}
		for _, gammaRenews := range []bool{false, true} {
			if subscriptionMarketsBlockNewPurchase(markets, gammaRenews) {
				t.Fatalf("a gift year blocks a new purchase (gamma renews %t)", gammaRenews)
			}
		}
	})
}
