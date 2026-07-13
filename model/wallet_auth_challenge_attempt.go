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

func WalletAuthChallengeAttempt(
	session *session.ClientSession,
) (walletAuthChallengeAttemptId server.Id, allow bool) {
	clientAddressHash, clientPort, err := session.ClientAddressHashPort()
	if err != nil {
		return
	}

	server.Tx(session.Ctx, func(tx server.PgTx) {
		walletAuthChallengeAttemptId = server.NewId()

		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				INSERT INTO wallet_auth_challenge_attempt
				(wallet_auth_challenge_attempt_id, client_address_hash, client_address_port, success)
				VALUES ($1, $2, $3, $4)
			`,
			walletAuthChallengeAttemptId,
			clientAddressHash[:],
			clientPort,
			false,
		))

		type WalletAuthChallengeAttemptResult struct {
			attemptTime time.Time
			success     bool
		}

		parseAttempts := func(result server.PgResult) []WalletAuthChallengeAttemptResult {
			attempts := []WalletAuthChallengeAttemptResult{}
			for result.Next() {
				var attempt WalletAuthChallengeAttemptResult
				server.Raise(result.Scan(
					&attempt.attemptTime,
					&attempt.success,
				))
				attempts = append(attempts, attempt)
			}
			return attempts
		}

		passesThreshold := func(attempts []WalletAuthChallengeAttemptResult) bool {
			failedCount := 0
			for i := 0; i < len(attempts); i += 1 {
				if !attempts[i].success {
					failedCount += 1
				}
			}
			return failedCount < WalletAuthChallengeAttemptFailedCountThreshold
		}

		var attempts []WalletAuthChallengeAttemptResult
		result, err := tx.Query(
			session.Ctx,
			`
				SELECT
					attempt_time,
					success
				FROM wallet_auth_challenge_attempt
				WHERE
					client_address_hash = $1 AND
					client_address_port = $2 AND
					now() - INTERVAL '1 seconds' * $3 <= attempt_time AND success = false
				ORDER BY attempt_time DESC
				LIMIT $4
			`,
			clientAddressHash[:],
			clientPort,
			WalletAuthChallengeAttemptLookback/time.Second,
			WalletAuthChallengeAttemptFailedCountThreshold,
		)
		server.WithPgResult(result, err, func() {
			attempts = parseAttempts(result)
		})
		if !passesThreshold(attempts) {
			return
		}

		allow = true
	})
	return
}

func SetWalletAuthChallengeAttemptSuccess(
	ctx context.Context,
	walletAuthChallengeAttemptId server.Id,
	success bool,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE wallet_auth_challenge_attempt
				SET success = $1
				WHERE wallet_auth_challenge_attempt_id = $2
			`,
			success,
			walletAuthChallengeAttemptId,
		))
	})
}
