package model

// An offer issued without an App Store code takes one when a reader on an App
// Store build reads it (AssignOnboardingOfferAppleCode): once, only while the
// offer is active, from a pool code that outlives the offer, else the custom
// code, and a code once set is never replaced.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Issues an offer with no App Store code, as the email offer and an offer to
// a reader on another platform are issued.
func issueOfferWithoutAppleCode(t testing.TB, ctx context.Context, validity time.Duration) server.Id {
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "offerapplecode"+networkId.String(), server.NewId())
	offer, created, err := IssueOnboardingOffer(ctx, &IssueOnboardingOfferArgs{
		NetworkId:  networkId,
		IssuedBy:   OnboardingOfferIssuedByEmail,
		Surface:    OnboardingOfferSurfaceEmailLink,
		Tier:       "standard",
		PercentOff: 25,
		MonthsFree: 3,
		Validity:   validity,
	})
	if err != nil || !created || OnboardingOfferHasAppleCode(offer) {
		t.Fatalf("could not issue an offer without a code: offer %+v, created %t, err %v", offer, created, err)
	}
	return networkId
}

func loadOfferAppleCodes(t testing.TB, ctx context.Context, expiresAt time.Time, codes ...string) {
	batch := []*AppleOfferCode{}
	for _, code := range codes {
		batch = append(batch, &AppleOfferCode{Code: code, ExpiresAt: expiresAt})
	}
	if added, err := LoadAppleOfferCodePool(ctx, batch); err != nil || added != len(codes) {
		t.Fatalf("could not load the pool: added %d of %d, err %v", added, len(codes), err)
	}
}

// The first read draws one code and stamps it on the offer; every later read
// returns it and draws no other.
func TestAssignOnboardingOfferAppleCodeDrawsOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		loadOfferAppleCodes(t, ctx, server.NowUtc().Add(30*24*time.Hour), "SYNTHETIC-ONCE-1", "SYNTHETIC-ONCE-2")
		networkId := issueOfferWithoutAppleCode(t, ctx, 5*24*time.Hour)

		offer, stamped := AssignOnboardingOfferAppleCode(ctx, networkId, "")
		if !stamped || offer == nil || offer.AppleOfferCode == nil {
			t.Fatalf("the first read drew no code: offer %+v, stamped %t", offer, stamped)
		}
		code := *offer.AppleOfferCode
		if stored := GetOnboardingOffer(ctx, networkId); stored == nil || stored.AppleOfferCode == nil || *stored.AppleOfferCode != code {
			t.Fatalf("the offer row does not hold the drawn code: %+v", stored)
		}

		again, stamped := AssignOnboardingOfferAppleCode(ctx, networkId, "")
		if stamped || again == nil || again.AppleOfferCode == nil || *again.AppleOfferCode != code {
			t.Fatalf("a second read changed the code: offer %+v, stamped %t", again, stamped)
		}
		if available := CountAvailableAppleOfferCodes(ctx, server.NowUtc()); available != 1 {
			t.Errorf("%d codes left after two reads, want 1", available)
		}

		if offer, stamped := AssignOnboardingOfferAppleCode(ctx, server.NewId(), "SYNTHETIC-CUSTOM"); offer != nil || stamped {
			t.Errorf("a network without an offer took %+v", offer)
		}
	})
}

// A pool code that expires before the offer cannot serve it; with none that
// outlives the offer, the custom code is the fallback, and it stays the
// offer's code after the pool refills: the reader may already hold it.
func TestAssignOnboardingOfferAppleCodeFallsBackToTheCustomCode(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := issueOfferWithoutAppleCode(t, ctx, 5*24*time.Hour)
		// expires before the offer
		loadOfferAppleCodes(t, ctx, server.NowUtc().Add(2*24*time.Hour), "SYNTHETIC-SHORT-1")

		offer, stamped := AssignOnboardingOfferAppleCode(ctx, networkId, "")
		if stamped || OnboardingOfferHasAppleCode(offer) {
			t.Fatalf("a code that expires before the offer was given: %+v", offer)
		}

		offer, stamped = AssignOnboardingOfferAppleCode(ctx, networkId, " SYNTHETIC-CUSTOM ")
		if !stamped || offer == nil || offer.AppleOfferCode == nil || *offer.AppleOfferCode != "SYNTHETIC-CUSTOM" {
			t.Fatalf("the custom code was not the fallback: offer %+v, stamped %t", offer, stamped)
		}

		loadOfferAppleCodes(t, ctx, server.NowUtc().Add(30*24*time.Hour), "SYNTHETIC-LATER-1")
		offer, stamped = AssignOnboardingOfferAppleCode(ctx, networkId, "SYNTHETIC-CUSTOM")
		if stamped || offer == nil || offer.AppleOfferCode == nil || *offer.AppleOfferCode != "SYNTHETIC-CUSTOM" {
			t.Errorf("the custom code was replaced after the refill: offer %+v, stamped %t", offer, stamped)
		}
		// the short code never outlived the offer; the later one is untouched
		if available := CountAvailableAppleOfferCodes(ctx, server.NowUtc().Add(5*24*time.Hour)); available != 1 {
			t.Errorf("%d codes that outlive an offer left, want the 1 loaded after the fallback", available)
		}
	})
}

// An expired or redeemed offer takes no code.
func TestAssignOnboardingOfferAppleCodeOnlyForAnActiveOffer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		loadOfferAppleCodes(t, ctx, server.NowUtc().Add(30*24*time.Hour), "SYNTHETIC-ACTIVE-1")

		expiredNetworkId := issueOfferWithoutAppleCode(t, ctx, -time.Hour)
		if offer, stamped := AssignOnboardingOfferAppleCode(ctx, expiredNetworkId, "SYNTHETIC-CUSTOM"); stamped || OnboardingOfferHasAppleCode(offer) {
			t.Errorf("an expired offer took a code: %+v", offer)
		}

		redeemedNetworkId := issueOfferWithoutAppleCode(t, ctx, 5*24*time.Hour)
		if !RedeemOnboardingOffer(ctx, redeemedNetworkId, OnboardingStorePlay, server.NowUtc()) {
			t.Fatal("could not redeem the offer")
		}
		if offer, stamped := AssignOnboardingOfferAppleCode(ctx, redeemedNetworkId, "SYNTHETIC-CUSTOM"); stamped || OnboardingOfferHasAppleCode(offer) {
			t.Errorf("a redeemed offer took a code: %+v", offer)
		}

		if available := CountAvailableAppleOfferCodes(ctx, server.NowUtc()); available != 1 {
			t.Errorf("inactive offers drew %d of the pool's 1", 1-available)
		}
	})
}
