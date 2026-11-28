package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/reengagement"
)

// The plan response over the wire: winback_offer is absent until the network
// is issued one, then exactly the shape the apps code against, beside
// onboarding_offer, and absent again once the offer is redeemed.
//
// INTEGRATION: needs the local postgres/redis stack (WARP_ENV=local).
func TestSubscriptionBalanceCarriesWinbackOffer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, userId, networkName := handlerTestAccount(t, ctx, "winbackplan")

		balance := func() map[string]json.RawMessage {
			t.Helper()
			w := httptest.NewRecorder()
			SubscriptionBalance(w, authedRequestRaw(t, http.MethodGet, "/subscription/balance", networkId, userId, networkName, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("balance answered %d: %s", w.Code, w.Body.String())
			}
			fields := map[string]json.RawMessage{}
			if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
				t.Fatalf("balance body: %s", err)
			}
			return fields
		}

		fields := balance()
		if _, ok := fields["winback_offer"]; ok {
			t.Fatalf("a network without an offer answered winback_offer: %s", fields["winback_offer"])
		}
		if _, ok := fields["onboarding_offer"]; !ok {
			t.Fatal("the plan response lost onboarding_offer")
		}

		issuedAt := server.NowUtc().Truncate(time.Second)
		offer, created, err := model.IssueWinbackOffer(ctx, &model.IssueWinbackOfferArgs{
			NetworkId:    networkId,
			Store:        reengagement.WinbackStorePlay,
			Plan:         model.PlanMonthly,
			PercentOff:   50,
			IssuedAt:     issuedAt,
			ExpiresAt:    issuedAt.Add(14 * 24 * time.Hour),
			PlayOfferTag: "winback-monthly-50",
		})
		if err != nil || !created {
			t.Fatalf("issue: %v %t", err, created)
		}

		fields = balance()
		var winbackOffer map[string]any
		if err := json.Unmarshal(fields["winback_offer"], &winbackOffer); err != nil {
			t.Fatalf("winback_offer %s: %s", fields["winback_offer"], err)
		}
		want := map[string]any{
			"expires_at":     offer.ExpiresAt.UTC().Format(time.RFC3339),
			"percent_off":    float64(50),
			"plan":           "monthly",
			"store":          "play",
			"play_offer_tag": "winback-monthly-50",
		}
		if len(winbackOffer) != len(want) {
			t.Fatalf("winback_offer is %v, want %v", winbackOffer, want)
		}
		for key, value := range want {
			if winbackOffer[key] != value {
				t.Fatalf("winback_offer.%s is %v, want %v (%v)", key, winbackOffer[key], value, winbackOffer)
			}
		}
		if _, err := time.Parse(time.RFC3339, winbackOffer["expires_at"].(string)); err != nil {
			t.Fatalf("expires_at is not RFC 3339: %s", err)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			if model.RedeemWinbackOfferInTx(tx, ctx, networkId, reengagement.WinbackStorePlay, nil, issuedAt.Add(time.Minute)) == nil {
				t.Fatal("the offer was not redeemed")
			}
		})
		if _, ok := balance()["winback_offer"]; ok {
			t.Fatal("a redeemed offer is still on the plan response")
		}
	})
}
