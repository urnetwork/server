package model

import (
	"context"
	"strings"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func TestWalletAuthChallengeAttemptSuccessfulDoNotCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)

		threshold := WalletAuthChallengeAttemptFailedCountThreshold
		for i := 0; i < threshold; i += 1 {
			attemptId, allow := WalletAuthChallengeAttempt(clientSession)
			assert.Equal(t, allow, true)
			SetWalletAuthChallengeAttemptSuccess(
				clientSession.Ctx,
				attemptId,
				true,
			)
		}

		// all prior attempts succeeded, so another attempt is allowed
		_, allow := WalletAuthChallengeAttempt(clientSession)
		assert.Equal(t, allow, true)
	})
}

func TestWalletAuthChallengeAttemptFailedRateLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)

		// threshold of 5 means the 5th failed attempt in the window is blocked,
		// matching the semantics of UserAuthAttempt
		threshold := WalletAuthChallengeAttemptFailedCountThreshold
		for i := 0; i < threshold-1; i += 1 {
			_, allow := WalletAuthChallengeAttempt(clientSession)
			assert.Equal(t, allow, true)
			SetWalletAuthChallengeAttemptSuccess(
				clientSession.Ctx,
				server.NewId(),
				false,
			)
		}

		_, allow := WalletAuthChallengeAttempt(clientSession)
		assert.Equal(t, allow, false)
	})
}

func TestWalletAuthChallengeAttemptsErrorFormat(t *testing.T) {
	err := MaxWalletAuthChallengeAttemptsError()
	assert.Equal(t, strings.HasPrefix(err.Error(), "429 "), true)
}
