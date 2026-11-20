// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/onboarding"
	"github.com/urnetwork/server/v2026/session"
)

// An offer email's click by an iPhone network on path B answers with Apple's
// redeem link and the network's one-time code (owner, 2026-11-15): the
// emailed offer has no code yet, so the click draws one from the pool, and a
// second click opens the same code. Every other reader keeps the page's own
// route: a network the App Store does not bill, an offer seen in the app
// (path A), an offer already redeemed, and any step but the offer.
func TestOnboardingClickOfferOpensAppleRedeemLink(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		now := server.NowUtc()
		poolCodes := map[string]bool{}
		codes := []*model.AppleOfferCode{}
		for i := 0; i < 3; i++ {
			code := strings.ToUpper(strings.ReplaceAll(server.NewId().String(), "-", ""))[:18]
			poolCodes[code] = true
			codes = append(codes, &model.AppleOfferCode{Code: code, ExpiresAt: now.Add(7 * 24 * time.Hour)})
		}
		if _, err := model.LoadAppleOfferCodePool(ctx, codes); err != nil {
			tb.Fatal(err)
		}

		network := func(name string, platform string, issuedBy string) server.Id {
			networkId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "synthetic-redeem-"+name+"-"+networkId.String(), server.NewId())
			sendAt := now.Add(time.Hour)
			row := &model.NetworkOnboarding{NetworkId: networkId, CreatedAt: now.Add(-7 * 24 * time.Hour), Email: true, TimeZone: "UTC",
				Platform: platform, Path: onboarding.PathB, NextStep: onboarding.StepE5, NextSendAt: &sendAt}
			if !model.CreateNetworkOnboarding(ctx, row) {
				tb.Fatal("fixture row not created", name)
			}
			surface := model.OnboardingOfferSurfaceEmailLink
			if issuedBy == model.OnboardingOfferIssuedByInApp {
				surface = model.OnboardingOfferSurfaceFinalScreen
			}
			if _, _, err := model.IssueOnboardingOffer(ctx, &model.IssueOnboardingOfferArgs{
				NetworkId: networkId, IssuedBy: issuedBy, Surface: surface, Tier: "standard",
				PercentOff: 25, MonthsFree: 3, Validity: 5 * 24 * time.Hour,
			}); err != nil {
				tb.Fatal(err)
			}
			return networkId
		}
		click := func(networkId server.Id, template string, flowStep string) *OnboardingClickResult {
			token, err := onboarding.NewToken(&onboarding.TokenClaims{
				NetworkId: networkId,
				Step:      template,
				FlowStep:  flowStep,
				ExpiresAt: now.Add(30 * 24 * time.Hour).Unix(),
			})
			connect.AssertEqual(tb, err, nil)
			result, err := OnboardingClick(&OnboardingClickArgs{Token: token}, session.Testing_CreateClientSession(ctx, nil))
			connect.AssertEqual(tb, err, nil)
			connect.AssertEqual(tb, result.Ok, true)
			return result
		}

		iphone := network("iphone", "ios", model.OnboardingOfferIssuedByEmail)
		if model.OnboardingOfferHasAppleCode(model.GetOnboardingOffer(ctx, iphone)) {
			tb.Fatal("an emailed offer has a code before any read")
		}
		first := click(iphone, onboarding.TemplateE4bOffer, onboarding.StepE4)
		connect.AssertEqual(tb, first.Step, onboarding.SlugOffer)
		offer := model.GetOnboardingOffer(ctx, iphone)
		if !model.OnboardingOfferHasAppleCode(offer) || !poolCodes[*offer.AppleOfferCode] {
			tb.Fatal("the click drew no pool code", offer.AppleOfferCode)
		}
		connect.AssertEqual(tb, first.OpenUrl, "https://apps.apple.com/redeem?ctx=offercodes&id=6741000606&code="+*offer.AppleOfferCode)
		again := click(iphone, onboarding.TemplateE3LastChance, onboarding.StepE5)
		connect.AssertEqual(tb, again.OpenUrl, first.OpenUrl)
		// another step of the same network keeps the page's route
		connect.AssertEqual(tb, click(iphone, onboarding.TemplateE1Connect, onboarding.StepE1).OpenUrl, "")

		web := network("web", onboarding.PlatformWeb, model.OnboardingOfferIssuedByEmail)
		connect.AssertEqual(tb, click(web, onboarding.TemplateE4bOffer, onboarding.StepE4).OpenUrl, "")
		connect.AssertEqual(tb, model.OnboardingOfferHasAppleCode(model.GetOnboardingOffer(ctx, web)), false)

		pathA := network("path-a", "ios", model.OnboardingOfferIssuedByInApp)
		connect.AssertEqual(tb, click(pathA, onboarding.TemplateE3LastChance, onboarding.StepE3).OpenUrl, "")

		redeemed := network("redeemed", "ios", model.OnboardingOfferIssuedByEmail)
		server.Tx(ctx, func(tx server.PgTx) {
			if !model.RedeemOnboardingOfferInTx(tx, ctx, redeemed, model.OnboardingStoreStripe, now) {
				tb.Fatal("fixture offer not redeemed")
			}
		})
		connect.AssertEqual(tb, click(redeemed, onboarding.TemplateE4bOffer, onboarding.StepE4).OpenUrl, "")
	})
}
