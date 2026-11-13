package model

import (
	"context"
	"net/netip"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
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

// CheckNetworkCreateRateLimit checks a seed-phrase sign-up against the budgets
// of its source, records it in each, and returns a 429 error over any. Must be
// called BEFORE the network is actually created.
//
// The budgets, in order (network_create_source_limit.go):
//   - the address budget, NetworkCreateDailyLimit per address bucket per
//     rolling NetworkCreateDailyWindow, in PostgreSQL;
//   - the source network budget, DefaultNetworkCreateSourceLimitSettings, in
//     Redis, for a sign-up the address budget admitted.
//
// A sign-up refused by either is recorded in neither: the source network budget
// counts only admitted sign-ups, and its refusal removes the attempt the
// address budget recorded. So a person retrying a refusal does not extend it,
// and repeated attempts from one address bucket the address budget refuses never
// spend its source network's budget.
func CheckNetworkCreateRateLimit(
	ctx context.Context,
	session *session.ClientSession,
) error {
	return checkNetworkCreateRateLimit(ctx, session, server.NowUtc(), DefaultNetworkCreateSourceLimitSettings())
}

// CheckNetworkCreateRateLimit with the source network budget's clock and
// settings.
func checkNetworkCreateRateLimit(
	ctx context.Context,
	session *session.ClientSession,
	now time.Time,
	sourceSettings *NetworkCreateSourceLimitSettings,
) error {
	rateLimitClient, err := server.NewRateLimitClient(session.ClientAddress)
	if err != nil {
		// Deferred sessions can carry only the persisted privacy-preserving
		// address hash. They cannot be newly classified, but retain their
		// original budget ownership.
		clientAddressHash, _, err := session.ClientAddressHashPort()
		if err != nil {
			// Can't determine client address — allow.
			noteSeedphraseSignupAdmission(seedphraseSignupUnscoped, seedphraseSignupFamily(netip.Addr{}))
			return nil
		}
		rateLimitClient = server.NewStoredRateLimitClient(clientAddressHash)
	} else if testSeedphraseRateLimitBypassForAddr(rateLimitClient.Addr()) {
		noteSeedphraseSignupAdmission(seedphraseSignupUnscoped, seedphraseSignupFamily(rateLimitClient.Addr()))
		return nil
	}
	addr := rateLimitClient.Addr()
	family := seedphraseSignupFamily(addr)
	if rateLimitClient.Excluded() {
		noteSeedphraseSignupAdmission(seedphraseSignupUnscoped, family)
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
		noteSeedphraseSignupAdmission(seedphraseSignupLimitedAddress, family)
		return maxNetworkCreateAttemptsError(result.RetryAfterSeconds)
	}

	if !addr.IsValid() {
		// a stored address hash names no network to count in
		observeSeedphraseSignupSource(seedphraseSignupScopeAddress, family, int64(result.Count)+1)
		noteSeedphraseSignupAdmission(seedphraseSignupAddressOnly, family)
		return nil
	}

	decision, err := checkNetworkCreateSourceLimit(ctx, addr, now, sourceSettings)
	if err != nil {
		glog.Warningf("[signup]seed-phrase source network budget not counted; the sign-up goes ahead: %s\n", err)
		observeSeedphraseSignupSource(seedphraseSignupScopeAddress, family, int64(result.Count)+1)
		noteSeedphraseSignupAdmission(seedphraseSignupUnavailable, family)
		return nil
	}
	if decision.limited != "" {
		// the address budget recorded this attempt; a refused attempt must not
		// spend it. When the removal fails, the attempt stays counted in the
		// address budget and the refusal still stands.
		server.HandleError(func() {
			server.RemoveNetworkCreateIpRateLimitAttempt(ctx, result.AttemptId)
		}, func(err error) {
			glog.Warningf("[signup]a seed-phrase sign-up refused by its source network stays counted in its address budget: %s\n", err)
		})
		if decision.limited == networkCreateSourceBudgetShort {
			noteSeedphraseSignupAdmission(seedphraseSignupLimitedNetworkShort, family)
		} else {
			noteSeedphraseSignupAdmission(seedphraseSignupLimitedNetworkLong, family)
		}
		return maxNetworkCreateSourceAttemptsError(decision.retryAfterSeconds)
	}

	observeSeedphraseSignupSource(seedphraseSignupScopeAddress, family, int64(result.Count)+1)
	observeSeedphraseSignupSource(seedphraseSignupScopeNetworkShort, family, decision.shortCount)
	observeSeedphraseSignupSource(seedphraseSignupScopeNetworkLong, family, decision.longCount)
	if 0 < sourceSettings.AsnWindow {
		if asn := server.GetIpAsn(addr); asn != nil {
			asnCount, err := countNetworkCreateSourceAsn(ctx, asn.Number, now, sourceSettings)
			if err == nil {
				observeSeedphraseSignupSource(seedphraseSignupScopeAsn, family, asnCount)
			} else if glog.V(1) {
				glog.Infof("[signup]seed-phrase sign-up not counted for its autonomous system: %s\n", err)
			}
		}
	}
	noteSeedphraseSignupAdmission(seedphraseSignupAllowed, family)
	return nil
}

// Attempts are kept twice the window that counts them (NetworkCreateDailyWindow).
const NetworkCreateAttemptRetention = 2 * NetworkCreateDailyWindow

var networkCreateAttemptRetention = newTimeOrderedRetention(
	RetentionTableNetworkCreateAttempt,
	"network_create_attempt_id",
	"create_time",
)

// RemoveExpiredNetworkCreateAttempts removes the attempts older than minTime,
// oldest first, in bounded batches for at most retentionRunBudget.
func RemoveExpiredNetworkCreateAttempts(ctx context.Context, minTime time.Time) (removedCount int64) {
	removedCount, _ = networkCreateAttemptRetention.RemoveBefore(ctx, minTime, retentionBatchSize, time.Now().Add(retentionRunBudget))
	return
}
