// Wallet-challenge rate-limit policy and domain error mapping.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const (
	WalletAuthChallengeAttemptLookback             = 5 * time.Minute
	WalletAuthChallengeAttemptFailedCountThreshold = 5
)

func maxWalletAuthChallengeAttemptsError() error {
	return errors.New("429 Wallet auth challenge attempts exceeded limits.")
}

func MaxWalletAuthChallengeAttemptsError() error {
	return maxWalletAuthChallengeAttemptsError()
}

// Applies the model policy through the canonical ip limiter.
func WalletAuthChallengeAttempt(
	clientSession *session.ClientSession,
) (walletAuthChallengeAttemptId server.Id, allow bool) {
	rateLimitClient, rateLimitErr := server.NewRateLimitClient(clientSession.ClientAddress)
	clientAddressHash, clientPort, err := clientSession.ClientAddressHashPort()
	if err != nil {
		return
	}
	if rateLimitErr != nil {
		rateLimitClient = server.NewStoredRateLimitClient(clientAddressHash)
	}
	return server.CheckWalletChallengeIpRateLimit(
		clientSession.Ctx,
		rateLimitClient,
		clientPort,
		WalletAuthChallengeAttemptLookback,
		WalletAuthChallengeAttemptFailedCountThreshold,
	)
}

// Records the domain outcome through the canonical ip limiter.
func SetWalletAuthChallengeAttemptSuccess(
	ctx context.Context,
	walletAuthChallengeAttemptId server.Id,
	success bool,
) {
	server.SetWalletChallengeIpRateLimitSuccess(
		ctx,
		walletAuthChallengeAttemptId,
		success,
	)
}
