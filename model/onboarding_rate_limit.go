package model

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Per-address budgets for the onboarding endpoints. The client event endpoint is
// authenticated but cheap to spam (a batch is stored as written), and the landing
// click and feedback token endpoints have no session at all, so the address is the
// budget owner in every case. The budgets stop a flood, not a person: a client
// flushes every 30 s (120 calls in 10 minutes is sixty times that), and a landing
// page is clicked a handful of times.
const (
	// POST /client/events: calls, not events (a call carries up to 200)
	ClientEventsAddressLimit    = 120
	ClientEventsAddressLookback = 10 * time.Minute
	ClientEventsGlobalLimit     = 60000
	ClientEventsGlobalLookback  = 1 * time.Minute

	// POST /onboarding/click and GET /onboarding/feedback/{token}
	OnboardingTokenAddressLimit    = 60
	OnboardingTokenAddressLookback = 10 * time.Minute
	OnboardingTokenGlobalLimit     = 6000
	OnboardingTokenGlobalLookback  = 1 * time.Minute

	// POST /onboarding/offer/issue and the Stripe payment sheet
	OnboardingOfferAddressLimit    = 30
	OnboardingOfferAddressLookback = 10 * time.Minute
	OnboardingOfferGlobalLimit     = 6000
	OnboardingOfferGlobalLookback  = 1 * time.Minute
)

type OnboardingRateLimit struct {
	Action          string
	AddressLimit    int
	AddressLookback time.Duration
	GlobalLimit     int
	GlobalLookback  time.Duration
}

var ClientEventsRateLimit = &OnboardingRateLimit{
	Action:          "client_events",
	AddressLimit:    ClientEventsAddressLimit,
	AddressLookback: ClientEventsAddressLookback,
	GlobalLimit:     ClientEventsGlobalLimit,
	GlobalLookback:  ClientEventsGlobalLookback,
}

var OnboardingTokenRateLimit = &OnboardingRateLimit{
	Action:          "onboarding_token",
	AddressLimit:    OnboardingTokenAddressLimit,
	AddressLookback: OnboardingTokenAddressLookback,
	GlobalLimit:     OnboardingTokenGlobalLimit,
	GlobalLookback:  OnboardingTokenGlobalLookback,
}

var OnboardingOfferRateLimit = &OnboardingRateLimit{
	Action:          "onboarding_offer",
	AddressLimit:    OnboardingOfferAddressLimit,
	AddressLookback: OnboardingOfferAddressLookback,
	GlobalLimit:     OnboardingOfferGlobalLimit,
	GlobalLookback:  OnboardingOfferGlobalLookback,
}

// CheckOnboardingRateLimit records one attempt for the session's address and
// refuses it once the address, or everyone together, has spent the budget. A
// session with no resolvable address is allowed, as the referral limiter does:
// refusing it would deny callers behind an unparseable proxy chain while teaching
// an attacker nothing.
func CheckOnboardingRateLimit(clientSession *session.ClientSession, limit *OnboardingRateLimit) error {
	rateLimitClient, err := server.NewRateLimitClient(clientSession.ClientAddress)
	if err != nil {
		clientAddressHash, _, err := clientSession.ClientAddressHashPort()
		if err != nil {
			return nil
		}
		rateLimitClient = server.NewStoredRateLimitClient(clientAddressHash)
	}
	_, allowed, err := server.CheckIpRateLimitAttempt(
		clientSession.Ctx,
		rateLimitClient,
		limit.Action,
		server.NowUtc(),
		server.IpRateLimitAttemptSettings{
			KeyPrefix:       "onboarding." + limit.Action + ".",
			AddressLookback: limit.AddressLookback,
			// The shared history includes the current request and rejects at its
			// threshold; onboarding settings describe allowed requests.
			AddressLimit:   limit.AddressLimit + 1,
			GlobalLookback: limit.GlobalLookback,
			GlobalLimit:    limit.GlobalLimit + 1,
		},
	)
	server.Raise(err)
	if !allowed {
		return &rateLimitError{
			message: "429 Too many requests from your network address. " +
				"Please try again in a few minutes.",
			retryAfterSeconds: int(limit.AddressLookback / time.Second),
		}
	}
	return nil
}
