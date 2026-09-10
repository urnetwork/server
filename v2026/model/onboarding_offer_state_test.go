package model

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// TestOnboardingOfferState pins the offer state machine: none -> active ->
// redeemed | expired, with redeemed winning over expired.
func TestOnboardingOfferState(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	connect.AssertEqual(t, OnboardingOfferStateNone, OnboardingOfferState(nil, now))

	offer := &OnboardingOffer{
		NetworkId:  server.NewId(),
		IssuedAt:   now.Add(-24 * time.Hour),
		ExpiresAt:  now.Add(4 * 24 * time.Hour),
		PercentOff: 25,
		MonthsFree: 3,
	}
	connect.AssertEqual(t, OnboardingOfferStateActive, OnboardingOfferState(offer, now))
	connect.AssertEqual(t, true, offer.Eligible(now))

	// the expiry instant itself is expired
	connect.AssertEqual(t, OnboardingOfferStateExpired, OnboardingOfferState(offer, offer.ExpiresAt))
	connect.AssertEqual(t, OnboardingOfferStateExpired, OnboardingOfferState(offer, now.Add(30*24*time.Hour)))
	connect.AssertEqual(t, false, offer.Eligible(offer.ExpiresAt))

	redeemedAt := now.Add(time.Hour)
	offer.RedeemedAt = &redeemedAt
	connect.AssertEqual(t, OnboardingOfferStateRedeemed, OnboardingOfferState(offer, now.Add(2*time.Hour)))
	connect.AssertEqual(t, false, offer.Eligible(now.Add(2*time.Hour)))
	// redeemed stays redeemed after the expiry
	connect.AssertEqual(t, OnboardingOfferStateRedeemed, OnboardingOfferState(offer, now.Add(30*24*time.Hour)))
}

func TestOnboardingPathForNetwork(t *testing.T) {
	connect.AssertEqual(t, "", OnboardingPathForNetwork(nil))
	connect.AssertEqual(t, "A", OnboardingPathForNetwork(&OnboardingOffer{IssuedBy: OnboardingOfferIssuedByInApp}))
	connect.AssertEqual(t, "B", OnboardingPathForNetwork(&OnboardingOffer{IssuedBy: OnboardingOfferIssuedByEmail}))
	connect.AssertEqual(t, "", OnboardingPathForNetwork(&OnboardingOffer{IssuedBy: "other"}))
}

func TestParseAppleOfferCodeCsv(t *testing.T) {
	codes, err := ParseAppleOfferCodeCsv("code,expires\nABCD1234,2026-12-31\n\n# comment\nEFGH5678, 2027-01-15T00:00:00Z\n")
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, 2, len(codes))
	connect.AssertEqual(t, "ABCD1234", codes[0].Code)
	connect.AssertEqual(t, time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), codes[0].ExpiresAt)
	connect.AssertEqual(t, "EFGH5678", codes[1].Code)

	_, err = ParseAppleOfferCodeCsv("ABCD1234,not-a-date\n")
	connect.AssertEqual(t, true, err != nil)
	_, err = ParseAppleOfferCodeCsv("ABCD1234\n")
	connect.AssertEqual(t, true, err != nil)
}
