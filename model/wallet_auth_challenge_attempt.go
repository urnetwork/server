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

		// This threshold keys on client_address_port as well as the address
		// hash, which has two consequences an operator should know about, and
		// neither is repaired here.
		//
		// The port is CLIENT-INFLUENCED wherever a trusted proxy passes
		// X-Forwarded-Source-Port or X-UR-Forwarded-For through instead of
		// overwriting them, so a caller behind such a proxy can vary one header
		// and get a fresh bucket per attempt. The remedy is the proxy's, and it
		// is what every report in session/client_session.go now states: a
		// trusted proxy must overwrite or strip all three forwarding headers.
		//
		// The port also MOVES with the deployment's proxy. Where the proxy
		// sends a bare X-Forwarded-For and no port companion, the resolved port
		// is 0 for every client, so this key becomes per-/29 instead of
		// per-connection: the threshold binds for the first time (a wedged
		// deployment's ephemeral peer port made it per-connection, i.e. never),
		// and at the same time five failed challenges start being shared across
		// everyone in a /29.
		//
		// Dropping the port from the key would close the first at the cost of
		// making the second universal -- a shared budget for every deployment,
		// which is the wrongful-refusal failure this branch exists to remove,
		// in a limiter this suite does not cover. So it is left alone
		// deliberately rather than by omission.
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
