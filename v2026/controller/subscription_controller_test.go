package controller

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
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
		// The windows differ because the purchases happened on different days.
		// (They no longer HAVE to: market is part of the renewal key now, so
		// identical windows keep separate rows too -- see
		// TestSubscriptionBalanceIdenticalWindowsTwoMarkets.)
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

// TestSubscriptionBalanceIdenticalWindowsTwoMarkets pins the S8 fix: the renewal
// key now includes market, so two markets landing the IDENTICAL window -- to the
// microsecond -- keep two rows, each with its own revenue and transaction id. The
// old key (network_id, subscription_type, end_time, start_time) upserted the
// second market onto the first market's row: revenue OVERWRITTEN (not summed),
// market and transaction id kept from the first -- so the second store was
// invisible in the UI, with no cancel path, while it kept charging.
func TestSubscriptionBalanceIdenticalWindowsTwoMarkets(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "samewindow", userId)

		clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
			UserId:    userId,
		})

		now := server.NowUtc()
		day := 24 * time.Hour

		// the same window to the microsecond, from two different stores
		startTime := now.Add(-1 * day)
		endTime := now.Add(29 * day)

		renewals := []struct {
			market        model.SubscriptionMarket
			transactionId string
			netRevenue    model.NanoCents
		}{
			{model.SubscriptionMarketStripe, "in_samewindow_1", model.UsdToNanoCents(10)},
			{model.SubscriptionMarketX402, "0xsamewindow1", model.UsdToNanoCents(12)},
		}
		for _, renewal := range renewals {
			err := model.AddSubscriptionRenewal(ctx, &model.SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   model.SubscriptionTypeSupporter,
				StartTime:          startTime,
				EndTime:            endTime,
				NetRevenue:         renewal.netRevenue,
				SubscriptionMarket: renewal.market,
				TransactionId:      renewal.transactionId,
			})
			connect.AssertEqual(t, err, nil)
		}

		// both markets surface, so both have a cancel path in the UI
		markets := model.GetActiveSubscriptionRenewalMarkets(
			ctx,
			networkId,
			model.SubscriptionTypeSupporter,
		)
		connect.AssertEqual(t, markets, []string{
			model.SubscriptionMarketStripe,
			model.SubscriptionMarketX402,
		})

		result, err := SubscriptionBalance(clientSession)
		connect.AssertEqual(t, err, nil)
		stores := []string{}
		for _, subscription := range result.Subscriptions {
			stores = append(stores, subscription.Store)
		}
		connect.AssertEqual(t, stores, []string{
			model.SubscriptionMarketStripe,
			model.SubscriptionMarketX402,
		})

		// each row keeps ITS OWN revenue and transaction id -- nothing was
		// overwritten
		type renewalRow struct {
			market        string
			transactionId string
			netRevenue    model.NanoCents
		}
		rows := map[string]renewalRow{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT market, transaction_id, net_revenue_nano_cents
				FROM subscription_renewal
				WHERE network_id = $1
				`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var row renewalRow
					server.Raise(result.Scan(&row.market, &row.transactionId, &row.netRevenue))
					rows[row.market] = row
				}
			})
		})
		connect.AssertEqual(t, len(rows), 2)
		for _, renewal := range renewals {
			row := rows[renewal.market]
			connect.AssertEqual(t, row.transactionId, renewal.transactionId)
			connect.AssertEqual(t, row.netRevenue, renewal.netRevenue)
		}
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
