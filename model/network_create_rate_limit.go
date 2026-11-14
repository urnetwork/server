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
//   - the autonomous system budget, DefaultNetworkCreateSourceLimitSettings,
//     in Redis, for a sign-up the address budget admitted;
//   - the source network budget, DefaultNetworkCreateSourceLimitSettings, in
//     Redis, for a sign-up both admitted.
//
// A sign-up refused by any is recorded in none: the source budgets count only
// admitted sign-ups, a refusal removes the attempt the address budget
// recorded, and a source network's refusal takes the sign-up back out of the
// autonomous system's counters. So a person retrying a refusal does not extend
// it, and repeated attempts that one budget refuses never spend the budgets
// after it. The autonomous system goes before the source network because in a
// wave it is the one that refuses, and its refusal leaves nothing in Redis to
// undo.
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

	// the address budget recorded this attempt; a refused attempt must not
	// spend it. When the removal fails, the attempt stays counted in the
	// address budget and the refusal still stands.
	refuse := func(outcome seedphraseSignupOutcome, refusal error) error {
		server.HandleError(func() {
			server.RemoveNetworkCreateIpRateLimitAttempt(ctx, result.AttemptId)
		}, func(err error) {
			glog.Warningf("[signup]a seed-phrase sign-up refused by its source budgets stays counted in its address budget: %s\n", err)
		})
		noteSeedphraseSignupAdmission(outcome, family)
		return refusal
	}

	// An address with no known autonomous system (none in the database, or no
	// database deployed) has no autonomous system budget.
	asn := server.GetIpAsn(addr)
	var asnDecision networkCreateSourceDecision
	asnCounted := false
	unavailable := false
	if asn != nil {
		var asnErr error
		asnDecision, asnErr = checkNetworkCreateSourceAsnLimit(ctx, asn.Number, now, sourceSettings)
		switch {
		case asnErr != nil:
			glog.Warningf("[signup]seed-phrase autonomous system budget not counted; the sign-up goes on to its source network budget: %s\n", asnErr)
			unavailable = true
		case asnDecision.limited == networkCreateSourceBudgetShort:
			return refuse(seedphraseSignupLimitedAsnShort, maxNetworkCreateSourceAsnAttemptsError(asnDecision.retryAfterSeconds))
		case asnDecision.limited == networkCreateSourceBudgetLong:
			return refuse(seedphraseSignupLimitedAsnLong, maxNetworkCreateSourceAsnAttemptsError(asnDecision.retryAfterSeconds))
		default:
			asnCounted = true
		}
	}

	decision, networkErr := checkNetworkCreateSourceLimit(ctx, addr, now, sourceSettings)
	if networkErr != nil {
		glog.Warningf("[signup]seed-phrase source network budget not counted; the sign-up goes ahead: %s\n", networkErr)
		unavailable = true
	} else if decision.limited != "" {
		if asnCounted {
			// the autonomous system admitted and counted it
			if err := uncountNetworkCreateSourceAsn(ctx, asn.Number, now, sourceSettings); err != nil {
				glog.Warningf("[signup]a seed-phrase sign-up refused by its source network stays counted in its autonomous system budget: %s\n", err)
			}
		}
		if decision.limited == networkCreateSourceBudgetShort {
			return refuse(seedphraseSignupLimitedNetworkShort, maxNetworkCreateSourceAttemptsError(decision.retryAfterSeconds))
		}
		return refuse(seedphraseSignupLimitedNetworkLong, maxNetworkCreateSourceAttemptsError(decision.retryAfterSeconds))
	}

	observeSeedphraseSignupSource(seedphraseSignupScopeAddress, family, int64(result.Count)+1)
	if networkErr == nil {
		observeSeedphraseSignupSource(seedphraseSignupScopeNetworkShort, family, decision.shortCount)
		observeSeedphraseSignupSource(seedphraseSignupScopeNetworkLong, family, decision.longCount)
	}
	if asnCounted {
		observeSeedphraseSignupSource(seedphraseSignupScopeAsn, family, asnDecision.shortCount)
		observeSeedphraseSignupSource(seedphraseSignupScopeAsnLong, family, asnDecision.longCount)
		// the sign-up that spends a window's budget names the autonomous
		// system, once per window: a public number, so the systems a farm
		// holds, and whose real users the budget refuses with it, can be read
		// back from the log
		if 0 < sourceSettings.AsnShortLimit && asnDecision.shortCount == int64(sourceSettings.AsnShortLimit) {
			glog.Infof("[signup]autonomous system AS%d (%s) spent its seed-phrase sign-up budget of %d per %s\n", asn.Number, asn.Organization, sourceSettings.AsnShortLimit, sourceSettings.AsnShortWindow)
		}
		if 0 < sourceSettings.AsnLongLimit && asnDecision.longCount == int64(sourceSettings.AsnLongLimit) {
			glog.Infof("[signup]autonomous system AS%d (%s) spent its seed-phrase sign-up budget of %d per %s\n", asn.Number, asn.Organization, sourceSettings.AsnLongLimit, sourceSettings.AsnLongWindow)
		}
	}
	if unavailable {
		noteSeedphraseSignupAdmission(seedphraseSignupUnavailable, family)
	} else {
		noteSeedphraseSignupAdmission(seedphraseSignupAllowed, family)
	}
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
