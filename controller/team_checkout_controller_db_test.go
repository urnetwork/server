//go:build sidecar_pending

package controller

// POST /network/team-checkout against a fake Stripe (QUOTA-M6-02), on Postgres with MG6: the
// session it creates and each refusal. The role gate is the route's, in
// api/route_team_checkout_db_test.go.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Sets the clock Team checkout reads until the returned restore function runs.
func testSetTeamCheckoutNow(now time.Time) func() {
	previous := teamCheckoutNow
	teamCheckoutNow = func() time.Time { return now }
	return func() { teamCheckoutNow = previous }
}

// A checkout creates one subscription session: the base once, the seats and transfer overage by
// their meters, no storage item while storage is unpriced, expiring in an hour and anchored at
// the first 00:00:00 UTC after that, across a day end and a month end, with the network and the
// team mark on the subscription.
func TestTeamCheckoutCreatesAnAnchoredSession(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTeamStripeTestEnv(t)
		defer model.Testing_SetEscrowDrawOrder(true)()
		networkId, clientSession := newTeamStripeTestNetwork(t, ctx)

		for i, c := range []struct {
			now    time.Time
			anchor time.Time
		}{
			{time.Date(2026, 11, 10, 14, 30, 0, 0, time.UTC), time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)},
			// the expiry crosses a day end
			{time.Date(2026, 11, 10, 23, 30, 0, 0, time.UTC), time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)},
			// and a month end
			{time.Date(2026, 11, 30, 23, 30, 0, 0, time.UTC), time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)},
		} {
			restore := testSetTeamCheckoutNow(c.now)
			result, err := TeamCheckout(&TeamCheckoutArgs{}, clientSession)
			restore()
			connect.AssertEqual(t, err, nil)
			if result.Error != nil {
				t.Fatalf("checkout refused: %+v", result.Error)
			}
			connect.AssertEqual(t, result.UiMode, StripeUiModeHosted)
			connect.AssertEqual(t, result.CheckoutUrl != "", true)
			connect.AssertEqual(t, result.SessionId != "", true)

			forms := env.postedCheckoutForms()
			connect.AssertEqual(t, len(forms), i+1)
			form := forms[i]
			connect.AssertEqual(t, form.Get("mode"), "subscription")
			connect.AssertEqual(t, form.Get("client_reference_id"), networkId.String())
			connect.AssertEqual(t, form.Get("expires_at"), strconv.FormatInt(c.now.Add(time.Hour).Unix(), 10))
			connect.AssertEqual(t, form.Get("subscription_data[billing_cycle_anchor]"), strconv.FormatInt(c.anchor.Unix(), 10))
			connect.AssertEqual(t, form.Get("subscription_data[metadata][network_id]"), networkId.String())
			connect.AssertEqual(t, form.Get("subscription_data[metadata][team]"), "1")
			// the base once; a metered item takes no quantity
			connect.AssertEqual(t, form.Get("line_items[0][price]"), testTeamPriceBase)
			connect.AssertEqual(t, form.Get("line_items[0][quantity]"), "1")
			connect.AssertEqual(t, form.Get("line_items[1][price]"), testTeamPriceSeats)
			connect.AssertEqual(t, form.Has("line_items[1][quantity]"), false)
			connect.AssertEqual(t, form.Get("line_items[2][price]"), testTeamPriceTransfer)
			connect.AssertEqual(t, form.Has("line_items[2][quantity]"), false)
			// storage overage is unpriced: no item
			connect.AssertEqual(t, form.Has("line_items[3][price]"), false)
		}

		// embedded: the client secret and the publishable key
		result, err := TeamCheckout(&TeamCheckoutArgs{UiMode: StripeUiModeEmbedded}, clientSession)
		connect.AssertEqual(t, err, nil)
		if result.Error != nil {
			t.Fatalf("embedded checkout refused: %+v", result.Error)
		}
		connect.AssertEqual(t, result.ClientSecret != "", true)
		connect.AssertEqual(t, result.PublishableKey, "pk_test_team")
		connect.AssertEqual(t, result.CheckoutUrl, "")

		// storage priced: its item joins the subscription
		priced := testTeamPrices()
		priced.StorageGibMonthUsd = 0.02
		restore := model.Testing_SetTeamPrices(priced)
		result, err = TeamCheckout(&TeamCheckoutArgs{}, clientSession)
		restore()
		connect.AssertEqual(t, err, nil)
		if result.Error != nil {
			t.Fatalf("checkout with storage priced refused: %+v", result.Error)
		}
		forms := env.postedCheckoutForms()
		connect.AssertEqual(t, forms[len(forms)-1].Get("line_items[3][price]"), testTeamPriceStorage)
	})
}

// Checkout refuses, and creates no session: before the escrow_draw_order gate is open, for a
// network that is not a team, with no prices configured, with a Stripe price whose amount
// differs from pro.yml's, for a network already paying for Pro or still billed for Team, for a
// client credential and for an unknown ui mode.
func TestTeamCheckoutRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		env := newTeamStripeTestEnv(t)
		networkId, clientSession := newTeamStripeTestNetwork(t, ctx)

		refused := func(code string, clientSession *session.ClientSession, args *TeamCheckoutArgs) {
			t.Helper()
			result, err := TeamCheckout(args, clientSession)
			connect.AssertEqual(t, err, nil)
			if result.Error == nil || result.Error.Code != code {
				t.Fatalf("want %s, got %+v", code, result.Error)
			}
			connect.AssertEqual(t, result.CheckoutUrl, "")
			connect.AssertEqual(t, result.SessionId, "")
			connect.AssertEqual(t, len(env.postedCheckoutForms()), 0)
		}

		// the gate reads off, as it ships: refused
		restoreGate := model.Testing_SetEscrowDrawOrder(false)
		refused(PurchaseErrorCodeCheckoutUnavailable, clientSession, &TeamCheckoutArgs{})
		restoreGate()
		defer model.Testing_SetEscrowDrawOrder(true)()

		refused(PurchaseErrorCodeInvalidRequest, clientSession, &TeamCheckoutArgs{UiMode: "inline"})
		refused(PurchaseErrorCodeInvalidRequest, clientSession, &TeamCheckoutArgs{RedirectOnCompletion: "never"})

		// a personal network
		personalNetworkId := server.NewId()
		personalUserId := server.NewId()
		model.Testing_CreateNetwork(ctx, personalNetworkId, "personal-"+personalNetworkId.String(), personalUserId)
		personalSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: personalNetworkId, UserId: personalUserId})
		refused(TeamCheckoutErrorCodeNotTeam, personalSession, &TeamCheckoutArgs{})

		// a client credential
		clientId := server.NewId()
		clientCredentialSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: clientSession.ByJwt.UserId, ClientId: &clientId})
		if _, err := TeamCheckout(&TeamCheckoutArgs{}, clientCredentialSession); err == nil {
			t.Fatal("a client credential bought a team plan")
		}

		// no team_prices in this env
		previousTeamPrices := stripeTeamPrices
		stripeTeamPrices = func() StripeTeamPrices { return StripeTeamPrices{} }
		refused(PurchaseErrorCodePlanUnavailable, clientSession, &TeamCheckoutArgs{})
		stripeTeamPrices = previousTeamPrices

		// pro.yml names no price
		restore := model.Testing_SetTeamPrices(model.Testing_TeamPrices{})
		refused(PurchaseErrorCodePlanUnavailable, clientSession, &TeamCheckoutArgs{})
		restore()

		// each Stripe price at an amount pro.yml does not name
		for _, differ := range []func() func(){
			func() func() {
				previous := env.prices[testTeamPriceBase]
				env.setPrice(testTeamPriceBase, map[string]any{
					"billing_scheme":      "per_unit",
					"unit_amount":         9900,
					"unit_amount_decimal": "9900",
					"recurring":           map[string]any{"interval": "month", "interval_count": 1, "usage_type": "licensed"},
				})
				return func() { env.prices[testTeamPriceBase] = previous }
			},
			func() func() {
				previous := env.prices[testTeamPriceSeats]
				env.setPrice(testTeamPriceSeats, map[string]any{
					"billing_scheme": "tiered",
					"tiers_mode":     "graduated",
					"tiers": []any{
						map[string]any{"up_to": 5, "unit_amount": 0, "unit_amount_decimal": "0"},
						map[string]any{"up_to": nil, "unit_amount": 1500, "unit_amount_decimal": "1500"},
					},
					"recurring": map[string]any{"interval": "month", "interval_count": 1, "usage_type": "metered", "meter": "mtr_synthetic_seats"},
				})
				return func() { env.prices[testTeamPriceSeats] = previous }
			},
			func() func() {
				previous := env.prices[testTeamPriceTransfer]
				env.setPrice(testTeamPriceTransfer, map[string]any{
					"billing_scheme":      "per_unit",
					"unit_amount_decimal": "0.5",
					"recurring":           map[string]any{"interval": "month", "interval_count": 1, "usage_type": "metered", "meter": "mtr_synthetic_transfer"},
				})
				return func() { env.prices[testTeamPriceTransfer] = previous }
			},
		} {
			restore := differ()
			refused(PurchaseErrorCodePlanUnavailable, clientSession, &TeamCheckoutArgs{})
			restore()
		}

		// a price Stripe does not hold: the read fails, and nothing is sold
		previousBase := env.prices[testTeamPriceBase]
		delete(env.prices, testTeamPriceBase)
		refused(PurchaseErrorCodeStartFailed, clientSession, &TeamCheckoutArgs{})
		env.prices[testTeamPriceBase] = previousBase

		// a network already paying for Pro keeps Pro's refusal
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(model.AddSubscriptionRenewalInTx(tx, ctx, &model.SubscriptionRenewal{
				NetworkId:          networkId,
				SubscriptionType:   model.SubscriptionTypeSupporter,
				StartTime:          now.Add(-time.Hour),
				EndTime:            now.Add(24 * time.Hour),
				NetRevenue:         model.UsdToNanoCents(5),
				SubscriptionMarket: model.SubscriptionMarketApple,
			}))
		})
		refused(PurchaseErrorCodeAlreadySubscribed, clientSession, &TeamCheckoutArgs{})

		// a team Stripe still bills is never sold a second subscription
		billedNetworkId, billedSession := newTeamStripeTestNetwork(t, ctx)
		env.setSubscription("sub_synthetic_billed", billedNetworkId, true, "active")
		env.setInvoice("in_synthetic_billed", "sub_synthetic_billed", "paid", "subscription_cycle", 10000)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(model.AddSubscriptionRenewalInTx(tx, ctx, &model.SubscriptionRenewal{
				NetworkId:          billedNetworkId,
				SubscriptionType:   model.SubscriptionTypeTeam,
				StartTime:          now.Add(-40 * 24 * time.Hour),
				EndTime:            now.Add(-10 * 24 * time.Hour),
				NetRevenue:         model.UsdToNanoCents(70),
				SubscriptionMarket: model.SubscriptionMarketStripe,
				TransactionId:      "in_synthetic_billed",
			}))
		})
		refused(PurchaseErrorCodeAlreadySubscribed, billedSession, &TeamCheckoutArgs{})

		// once Stripe reports that subscription ended, the team may buy again
		env.setSubscriptionStatus("sub_synthetic_billed", "canceled")
		result, err := TeamCheckout(&TeamCheckoutArgs{}, billedSession)
		connect.AssertEqual(t, err, nil)
		if result.Error != nil {
			t.Fatalf("checkout after the subscription ended refused: %+v", result.Error)
		}
		connect.AssertEqual(t, len(env.postedCheckoutForms()), 1)
	})
}
