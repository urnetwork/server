package model

import (
	"context"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func TestWalletAuthChallengeAttemptSuccessfulDoNotCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.Testing_CreateClientSession(ctx, nil)

		threshold := WalletAuthChallengeAttemptFailedCountThreshold
		for i := 0; i < threshold; i += 1 {
			attemptId, allow := WalletAuthChallengeAttempt(clientSession)
			connect.AssertEqual(t, allow, true)
			SetWalletAuthChallengeAttemptSuccess(
				clientSession.Ctx,
				attemptId,
				true,
			)
		}

		// all prior attempts succeeded, so another attempt is allowed
		_, allow := WalletAuthChallengeAttempt(clientSession)
		connect.AssertEqual(t, allow, true)
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
			attemptId, allow := WalletAuthChallengeAttempt(clientSession)
			connect.AssertEqual(t, allow, true)
			SetWalletAuthChallengeAttemptSuccess(
				clientSession.Ctx,
				attemptId,
				false,
			)
		}

		_, allow := WalletAuthChallengeAttempt(clientSession)
		connect.AssertEqual(t, allow, false)
	})
}

func TestWalletAuthChallengeAttemptsErrorFormat(t *testing.T) {
	err := MaxWalletAuthChallengeAttemptsError()
	connect.AssertEqual(t, strings.HasPrefix(err.Error(), "429 "), true)
}
