package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// TestSubscriptionBalanceMultipleMarkets is the whole point of the Subscriptions
// field: a network billed by two stores at once must see BOTH, because the two
// are unrelated payment systems and cancelling one leaves the other charging.
// The panel can only offer a cancel path for a subscription the api names.
func TestSubscriptionBalanceMultipleMarkets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "multimarket", userId)

		clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		now := server.NowUtc()
		day := 24 * time.Hour

		// no subscription at all: nothing to cancel, and nothing claimed
		result, err := SubscriptionBalance(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.Subscriptions), 0)
		connect.AssertEqual(t, result.CurrentSubscription, nil)

		// subscribed in the App Store, then again on the web through Stripe.
		// The windows differ because the purchases happened on different days --
		// they have to: subscription_renewal is keyed by
		// (network_id, subscription_type, end_time, start_time), so two markets
		// sharing a window to the microsecond would upsert onto one row.
		renewals := []struct {
			market           model.SubscriptionMarket
			startDay, endDay int
		}{
			{model.SubscriptionMarketApple, -10, 20},
			{model.SubscriptionMarketStripe, -5, 25},
		}
		for _, renewal := range renewals {
			err = model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   model.SubscriptionTypeSupporter,
				StartTime:          now.Add(time.Duration(renewal.startDay) * day),
				EndTime:            now.Add(time.Duration(renewal.endDay) * day),
				NetRevenue:         model.NanoCents(0),
				SubscriptionMarket: renewal.market,
			})
			connect.AssertEqual(t, err, nil)
		}
		// the pro grant that follows a renewal -- what makes the network Pro
		model.AddTransferBalance(ctx, &model.TransferBalance{
			NetworkId:             networkId,
			StartTime:             now.Add(-1 * day),
			EndTime:               now.Add(29 * day),
			StartBalanceByteCount: 10 * model.Gib,
			BalanceByteCount:      10 * model.Gib,
			SubsidyNetRevenue:     model.UsdToNanoCents(10),
			PurchaseToken:         "pro_multimarket_token",
			Pro:                   true,
		})

		result, err = SubscriptionBalance(clientSession)
		connect.AssertEqual(t, err, nil)

		stores := []string{}
		for _, subscription := range result.Subscriptions {
			stores = append(stores, subscription.Store)
			// every entry is a real plan, so every row can be rendered and acted on
			connect.AssertEqual(t, subscription.Plan, model.SubscriptionTypeSupporter)
		}
		connect.AssertEqual(t, stores, []string{
			model.SubscriptionMarketApple,
			model.SubscriptionMarketStripe,
		})

		// CurrentSubscription keeps its exact single-value meaning -- shipped
		// apple/android/windows/linux clients read it through the sdk
		connect.AssertNotEqual(t, result.CurrentSubscription, nil)
		connect.AssertEqual(t, result.CurrentSubscription.Plan, model.SubscriptionTypeSupporter)
		connect.AssertEqual(t, slices.Contains(stores, result.CurrentSubscription.Store), true)
	})
}

/*
func TestGooglePlayPubSub(t *testing.T) { (&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
	// https://cloud.google.com/pubsub/docs/authenticate-push-subscriptions?hl=en#protocol
	jwt := "eyJhbGciOiJSUzI1NiIsImtpZCI6ImJkYzRlMTA5ODE1ZjQ2OTQ2MGU2M2QzNGNkNjg0MjE1MTQ4ZDdiNTkiLCJ0eXAiOiJKV1QifQ.eyJhdWQiOiJodHRwczovL2FwaS5icmluZ3lvdXIuY29tL3BheS9wbGF5IiwiYXpwIjoiMTA4NTQyODI4MTk0ODEzMjUyNjI4IiwiZW1haWwiOiIzMzg2Mzg4NjUzOTAtY29tcHV0ZUBkZXZlbG9wZXIuZ3NlcnZpY2VhY2NvdW50LmNvbSIsImVtYWlsX3ZlcmlmaWVkIjp0cnVlLCJleHAiOjE3MDc3Njc3NzksImlhdCI6MTcwNzc2NDE3OSwiaXNzIjoiaHR0cHM6Ly9hY2NvdW50cy5nb29nbGUuY29tIiwic3ViIjoiMTA4NTQyODI4MTk0ODEzMjUyNjI4In0.qyBEr-SiWZ4OiY1ettW3CC-17cd0pRVGLRl3QCvFoEF9JtY-ivrPQuTdWKMbCe668-2Sejq1jhfF_rXnigIv42SuIN7QwS9cYstRIHbwP4Qekvk5ArSuAQnOnLcEgiAqLS8MC4a0JHuUkJPzOrFKfH_IBM6e7J3BJMoEQ-WuTDsaONqeMB4RENwuwV48R_pUs9P3OY1LM-5S2qtXEnYnbWFuBWETj6ewtw0X2jiq8Feh8ZMeGdSjLaG8CXfFhOMgOAy6Kg-3CxDThCN6ozLVXsP5ICB99KsxErsivpNYfe02TuDRtMPdVnRrnvGwKQs0ak1oKtHAPyFBp-X5LJ5cAA"
	url := fmt.Sprintf("https://oauth2.googleapis.com/tokeninfo?id_token=%s", jwt)

	bodyBytes, err := server.HttpGetRawRequireStatusOk(url, server.NoCustomHeaders)
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, bodyBytes, nil)

	// parse the body as a claim map
	var claims map[string]any
	err = json.Unmarshal(bodyBytes, &claims)
	connect.AssertEqual(t, err, nil)

	connect.AssertEqual(t, claims["email"], "338638865390-compute@developer.gserviceaccount.com")
})}
*/
