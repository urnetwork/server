package model

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Referral codes are checked before an account exists (the sign-up screens on
// every app ask "is this bonus code valid?" while the user is still filling in
// the form), so the check cannot require a session. Without a session the only
// budget owner is the caller's address, and the only thing a caller can learn
// is whether a code exists and whether it is used up, so the budget is set to
// stop enumeration, not to stop a person retyping a code a few times.
const (
	referralCodeValidateRedisKeyPrefix = "referral_code_validate."
	// one address may check this many codes per lookback
	ReferralCodeValidateAddressLimit    = 60
	ReferralCodeValidateAddressLookback = 10 * time.Minute
	// a shared ceiling across every address, so a botnet cannot spread the
	// enumeration thin enough to stay under the per-address budget
	ReferralCodeValidateGlobalLimit    = 6000
	ReferralCodeValidateGlobalLookback = 1 * time.Minute
)

func maxReferralCodeValidateAttemptsError() error {
	return &rateLimitError{
		message: "429 Too many referral code checks from your network address. " +
			"Please try again in a few minutes.",
		retryAfterSeconds: int(ReferralCodeValidateAddressLookback / time.Second),
	}
}

// CheckReferralCodeValidateRateLimit records one referral code check for the
// session's address and refuses it once the address, or everyone together, has
// spent the budget. A session with no resolvable address is allowed: refusing
// it would deny the check to callers behind an unparseable proxy chain while
// teaching an attacker nothing.
func CheckReferralCodeValidateRateLimit(clientSession *session.ClientSession) error {
	rateLimitClient, err := server.NewRateLimitClient(clientSession.ClientAddress)
	if err != nil {
		// Deferred sessions can carry only a persisted hash. They cannot be
		// newly classified, but retain their original address budget.
		clientAddressHash, _, err := clientSession.ClientAddressHashPort()
		if err != nil {
			return nil
		}
		rateLimitClient = server.NewStoredRateLimitClient(clientAddressHash)
	}
	_, allowed, err := server.CheckIpRateLimitAttempt(
		clientSession.Ctx,
		rateLimitClient,
		"referral_code_validate",
		server.NowUtc(),
		server.IpRateLimitAttemptSettings{
			KeyPrefix:       referralCodeValidateRedisKeyPrefix,
			AddressLookback: ReferralCodeValidateAddressLookback,
			// The shared history includes the current check and rejects at its
			// threshold; this route's constants describe allowed checks.
			AddressLimit:   ReferralCodeValidateAddressLimit + 1,
			GlobalLookback: ReferralCodeValidateGlobalLookback,
			GlobalLimit:    ReferralCodeValidateGlobalLimit + 1,
		},
	)
	server.Raise(err)
	if !allowed {
		return maxReferralCodeValidateAttemptsError()
	}
	return nil
}
