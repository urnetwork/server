package controller

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// TestGuestPurchaseRefusedUntilSignInAdded runs the purchase guard against a
// real legacy guest network (no row in any auth table) holding a refreshed jwt
// without the GuestMode claim. It reads as a guest on the balance and every
// checkout and intent refuses it. Adding an email and password in place
// (AddAuth) clears both, on the same network. Needs the test database.
func TestGuestPurchaseRefusedUntilSignInAdded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateLegacyGuestNetwork(ctx, networkId, userId)

		clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId:   networkId,
			UserId:      userId,
			NetworkName: "g" + networkId.String(),
			GuestMode:   false,
		})

		balance, err := SubscriptionBalance(clientSession)
		if err != nil || balance == nil || !balance.Guest {
			t.Fatalf("legacy guest balance=%+v err=%v, want guest", balance, err)
		}

		guestRefusal := func(entry string, code string, message string) {
			if code != PurchaseErrorCodeGuestSignInRequired || message != purchaseGuestSignInRequiredMessage {
				t.Fatalf("%s: error code=%q message=%q, want the guest refusal", entry, code, message)
			}
		}

		checkout, err := StripeCreateCheckoutSession(&StripeCreateCheckoutSessionArgs{
			ItemId: StripeItemProYearly,
		}, clientSession)
		if err != nil || checkout == nil || checkout.Error == nil {
			t.Fatalf("checkout session: result=%+v err=%v, want a refusal", checkout, err)
		}
		guestRefusal("checkout session", checkout.Error.Code, checkout.Error.Message)

		sheet, err := StripePaymentSheet(&StripePaymentSheetArgs{
			Plan: model.PlanYearly,
		}, clientSession)
		if err != nil || sheet == nil || sheet.Error == nil {
			t.Fatalf("payment sheet: result=%+v err=%v, want a refusal", sheet, err)
		}
		guestRefusal("payment sheet", sheet.Error.Code, sheet.Error.Message)

		intent, err := StripeCreatePaymentIntent(&StripeCreatePaymentIntentArgs{}, clientSession)
		if err != nil || intent == nil || intent.Error == nil {
			t.Fatalf("payment intent: result=%+v err=%v, want a refusal", intent, err)
		}
		guestRefusal("payment intent", intent.Error.Code, intent.Error.Message)

		solanaReference := fmt.Sprintf("synthetic-guest-%s", networkId)
		solana, err := CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
			Reference: solanaReference,
			Plan:      model.SolanaPlanMonthly,
		}, clientSession)
		if err != nil || solana == nil || solana.Error == nil {
			t.Fatalf("solana payment intent: result=%+v err=%v, want a refusal", solana, err)
		}
		guestRefusal("solana payment intent", solana.Error.Code, solana.Error.Message)
		if recorded := model.GetSolanaPaymentIntent(ctx, solanaReference); recorded != nil {
			t.Fatalf("refused solana payment intent was recorded: %+v", recorded)
		}

		// convert in place, as the apps do
		userAuth := fmt.Sprintf("guest-purchase-%s@example.com", networkId)
		password := "SomeValidPassword123!"
		addResult, err := model.AddAuth(model.AddAuthMethod{
			UserAuth: &userAuth,
			Password: &password,
		}, clientSession)
		if err != nil || addResult == nil || addResult.Error != nil {
			t.Fatalf("AddAuth result=%+v err=%v", addResult, err)
		}

		balance, err = SubscriptionBalance(clientSession)
		if err != nil || balance == nil || balance.Guest {
			t.Fatalf("converted balance=%+v err=%v, want not guest", balance, err)
		}

		// past the guard, each entry answers with its own validation
		checkout, err = StripeCreateCheckoutSession(&StripeCreateCheckoutSessionArgs{
			ItemId: StripeItemProYearly,
			UiMode: "synthetic-unknown",
		}, clientSession)
		if err != nil || checkout == nil || checkout.Error == nil || checkout.Error.Code != "" || checkout.Error.Message != "Unknown ui mode." {
			t.Fatalf("converted checkout session: result=%+v err=%v, want the ui mode error", checkout, err)
		}
		sheet, err = StripePaymentSheet(&StripePaymentSheetArgs{
			Plan: "synthetic-unknown",
		}, clientSession)
		if err != nil || sheet == nil || sheet.Error == nil || sheet.Error.Code != "" || sheet.Error.Message != "Unknown plan." {
			t.Fatalf("converted payment sheet: result=%+v err=%v, want the plan error", sheet, err)
		}
		solana, err = CreateSolanaPaymentIntent(&SolanaPaymentIntentArgs{
			Reference: solanaReference,
			Plan:      "synthetic-unknown",
		}, clientSession)
		if err != nil || solana == nil || solana.Error == nil || solana.Error.Code != "" || solana.Error.Message != "Unknown plan." {
			t.Fatalf("converted solana payment intent: result=%+v err=%v, want the plan error", solana, err)
		}

		// the plan stays with the same network
		network := model.GetNetwork(clientSession)
		if network == nil || network.NetworkId == nil || *network.NetworkId != networkId {
			t.Fatalf("converted network=%+v, want %s", network, networkId)
		}
	})
}
