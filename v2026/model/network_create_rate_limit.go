package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

const NetworkCreateDailyLimit = 5
const NetworkCreateDailyWindow = 24 * time.Hour

// maxNetworkCreateAttemptsError reports the account-creation limit.
//
// The message used to read "You have reached the maximum number of account
// creations for today", which is false for almost everyone who sees it: the
// budget is keyed on the caller's network address (bucketed by subnet, see
// server.ClientIpHashForAddr), not on the person, so the usual recipient has
// created no accounts at all and is being refused for what others sharing the
// address did. Saying so plainly is what lets support tell a wrongly-refused
// user on a shared connection apart from actual abuse, instead of both being
// handed the same accusation.
//
// retryAfterSeconds is the real remaining time on the window: the oldest
// attempt still counted expires then, freeing exactly one slot.
func maxNetworkCreateAttemptsError(retryAfterSeconds int) error {
	return &rateLimitError{
		message: "429 Too many accounts have been created recently from your network " +
			"address. This limit is scoped to the address you are connecting from " +
			"and is shared with everyone else on it, so this may not be your own " +
			"activity. Please try again later or from a different connection.",
		retryAfterSeconds: retryAfterSeconds,
	}
}

// CheckNetworkCreateRateLimit checks if the IP has exceeded the daily account
// creation limit. It records the attempt and returns an error if over the limit.
// Must be called BEFORE the network is actually created (attempt recorded atomically).
func CheckNetworkCreateRateLimit(
	ctx context.Context,
	session *session.ClientSession,
) error {
	rateLimitClient, err := server.NewRateLimitClient(session.ClientAddress)
	if err != nil {
		// Deferred sessions can carry only the persisted privacy-preserving
		// address hash. They cannot be newly classified, but retain their
		// original budget ownership.
		clientAddressHash, _, err := session.ClientAddressHashPort()
		if err != nil {
			// Can't determine client address — allow.
			return nil
		}
		rateLimitClient = server.NewStoredRateLimitClient(clientAddressHash)
	} else if testSeedphraseRateLimitBypassForAddr(rateLimitClient.Addr()) {
		return nil
	}
	result := server.CheckNetworkCreateIpRateLimit(
		ctx,
		rateLimitClient,
		NetworkCreateDailyLimit,
		NetworkCreateDailyWindow,
	)

	if result.Count >= NetworkCreateDailyLimit {
		// clamp: a clock skew or a just-expired row must never produce a
		// nonsensical hint, and the wait can never exceed the window itself
		if result.RetryAfterSeconds < 1 {
			result.RetryAfterSeconds = 1
		}
		if maxSeconds := int(NetworkCreateDailyWindow / time.Second); maxSeconds < result.RetryAfterSeconds {
			result.RetryAfterSeconds = maxSeconds
		}
		return maxNetworkCreateAttemptsError(result.RetryAfterSeconds)
	}

	return nil
}

// RemoveExpiredNetworkCreateAttempts cleans up attempts older than the window.
func RemoveExpiredNetworkCreateAttempts(ctx context.Context, minTime time.Time) {
	server.RemoveExpiredNetworkCreateIpRateLimitAttempts(ctx, minTime)
}
