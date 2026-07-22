package model

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
)

func TestCheckAccountActionRateLimitAllowsUpToLimitThenBlocks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()

		const limit = 5
		for i := 0; i < limit; i++ {
			err := CheckAccountActionRateLimit(ctx, userId, "test_action", limit, AccountActionDailyWindow)
			assert.Equal(t, err, nil)
			RecordAccountActionAttempt(ctx, userId, "test_action")
		}

		// the 6th check, after 5 recorded successes, must be blocked
		err := CheckAccountActionRateLimit(ctx, userId, "test_action", limit, AccountActionDailyWindow)
		assert.NotEqual(t, err, nil)
	})
}

func TestCheckAccountActionRateLimitIsPerActionAndPerUser(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()
		otherUserId := server.NewId()

		const limit = 1
		err := CheckAccountActionRateLimit(ctx, userId, "action_a", limit, AccountActionDailyWindow)
		assert.Equal(t, err, nil)
		RecordAccountActionAttempt(ctx, userId, "action_a")

		// same user, different action: independent counter
		err = CheckAccountActionRateLimit(ctx, userId, "action_b", limit, AccountActionDailyWindow)
		assert.Equal(t, err, nil)

		// different user, same action: independent counter
		err = CheckAccountActionRateLimit(ctx, otherUserId, "action_a", limit, AccountActionDailyWindow)
		assert.Equal(t, err, nil)

		// same user, same action, already at limit: blocked
		err = CheckAccountActionRateLimit(ctx, userId, "action_a", limit, AccountActionDailyWindow)
		assert.NotEqual(t, err, nil)
	})
}

func TestCheckAccountActionRateLimitDoesNotCountUnrecordedChecks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()

		const limit = 1

		// repeatedly checking (without recording) never burns budget -- this
		// is the success-only counting semantics: a caller that checks, then
		// fails validation/the underlying action, and never calls
		// RecordAccountActionAttempt, must not have spent the user's budget.
		for i := 0; i < 10; i++ {
			err := CheckAccountActionRateLimit(ctx, userId, "test_action", limit, AccountActionDailyWindow)
			assert.Equal(t, err, nil)
		}
	})
}

func TestCheckAndRecordAccountActionRateLimitAllowsUpToLimitThenBlocks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()

		const limit = 2
		for i := 0; i < limit; i++ {
			err := CheckAndRecordAccountActionRateLimit(ctx, userId, AccountActionChangeNetworkName, limit, AccountActionDailyWindow)
			assert.Equal(t, err, nil)
		}

		// the 3rd attempt, already at limit, must be blocked -- and blocked
		// attempts must not themselves record (else the block would never
		// clear even after the window passes)
		err := CheckAndRecordAccountActionRateLimit(ctx, userId, AccountActionChangeNetworkName, limit, AccountActionDailyWindow)
		assert.NotEqual(t, err, nil)
	})
}

func TestAccountActionRateLimitWindowExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()

		const limit = 1
		err := CheckAccountActionRateLimit(ctx, userId, "test_action", limit, AccountActionDailyWindow)
		assert.Equal(t, err, nil)
		RecordAccountActionAttempt(ctx, userId, "test_action")

		err = CheckAccountActionRateLimit(ctx, userId, "test_action", limit, AccountActionDailyWindow)
		assert.NotEqual(t, err, nil)

		// with a window shorter than the time that has already elapsed since
		// recording (any positive duration, since the insert above already
		// took non-zero time), the prior attempt falls outside the window and
		// no longer counts
		err = CheckAccountActionRateLimit(ctx, userId, "test_action", limit, time.Nanosecond)
		assert.Equal(t, err, nil)
	})
}

func TestAccountActionConstantsAreDistinctAndHaveDisplayNames(t *testing.T) {
	actions := []string{
		AccountActionAddAuth,
		AccountActionRemoveAuth,
		AccountActionClaimNetworkName,
		AccountActionChangeNetworkName,
		AccountActionGenerateSeedphrase,
		AccountActionRegenerateSeedphrase,
	}

	seen := map[string]bool{}
	for _, action := range actions {
		// every real action constant must have a distinct string value --
		// two constants colliding would silently merge their rate limits
		assert.Equal(t, seen[action], false)
		seen[action] = true

		// every real action constant must produce a specific, non-generic
		// error message -- the fallback "this action" wording should only
		// ever be reached for a caller-supplied action string that isn't
		// one of the real constants, never for a real one
		err := maxAccountActionAttemptsError(action)
		assert.NotEqual(t, err.Error(), maxAccountActionAttemptsError("not_a_real_action").Error())
	}
}

func TestClaimAndChangeNetworkNameHaveIndependentCounters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()

		// exhaust the claim budget
		for i := 0; i < AccountActionClaimNetworkNameDailyLimit; i++ {
			err := CheckAndRecordAccountActionRateLimit(ctx, userId, AccountActionClaimNetworkName, AccountActionClaimNetworkNameDailyLimit, AccountActionDailyWindow)
			assert.Equal(t, err, nil)
		}
		err := CheckAndRecordAccountActionRateLimit(ctx, userId, AccountActionClaimNetworkName, AccountActionClaimNetworkNameDailyLimit, AccountActionDailyWindow)
		assert.NotEqual(t, err, nil)

		// a subsequent rename (a real, separate action) must still be
		// allowed -- claiming a name during onboarding must not consume the
		// same-day rename budget
		err = CheckAndRecordAccountActionRateLimit(ctx, userId, AccountActionChangeNetworkName, AccountActionChangeNetworkNameDailyLimit, AccountActionDailyWindow)
		assert.Equal(t, err, nil)
	})
}

func TestRemoveExpiredAccountActionAttempts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		userId := server.NewId()

		RecordAccountActionAttempt(ctx, userId, "test_action")

		// cutoff in the future: the just-recorded attempt is older than it,
		// so it gets removed, freeing up budget immediately
		RemoveExpiredAccountActionAttempts(ctx, server.NowUtc().Add(time.Second))

		err := CheckAccountActionRateLimit(ctx, userId, "test_action", 1, AccountActionDailyWindow)
		assert.Equal(t, err, nil)
	})
}
