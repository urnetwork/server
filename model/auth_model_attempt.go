package model

import (
	"context"
	"errors"
	"time"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func maxUserAuthAttemptsError() error {
	return errors.New("503 User auth attempts exceeded limits.")
}

const AttemptLookback = 5 * time.Minute
const AttemptFailedCountThreshold = 10

func UserAuthAttempt(
	userAuth *string,
	session *session.ClientSession,
) (userAuthAttemptId server.Id, allow bool) {
	// insert attempt with success false
	// select attempts by userAuth in past 5 minutes
	// select attempts by clientIp in past 5 minutes
	// if more than 10 failed in any, return false

	clientAddressHash, clientPort, err := session.ClientAddressHashPort()
	if err != nil {
		return
	}

	server.Tx(session.Ctx, func(tx server.PgTx) {
		userAuthAttemptId = server.NewId()

		server.RaisePgResult(tx.Exec(
			session.Ctx,
			`
				INSERT INTO user_auth_attempt
				(user_auth_attempt_id, user_auth, client_address_hash, client_address_port, success)
				VALUES ($1, $2, $3, $4, $5)
			`,
			userAuthAttemptId,
			userAuth,
			clientAddressHash[:],
			clientPort,
			false,
		))

		type UserAuthAttemptResult struct {
			attemptTime time.Time
			success     bool
		}

		parseAttempts := func(result server.PgResult) []UserAuthAttemptResult {
			attempts := []UserAuthAttemptResult{}
			for result.Next() {
				var attempt UserAuthAttemptResult
				server.Raise(result.Scan(
					&attempt.attemptTime,
					&attempt.success,
				))
				attempts = append(attempts, attempt)
			}
			return attempts
		}

		passesThreshold := func(attempts []UserAuthAttemptResult) bool {
			failedCount := 0
			for i := 0; i < len(attempts); i += 1 {
				if !attempts[i].success {
					failedCount += 1
				}
			}
			return failedCount < AttemptFailedCountThreshold
		}

		if userAuth != nil {
			// lookback by user auth
			var attempts []UserAuthAttemptResult
			result, err := tx.Query(
				session.Ctx,
				`
					SELECT 
						attempt_time,
						success
					FROM user_auth_attempt
					WHERE 
						user_auth = $1 AND
						now() - INTERVAL '1 seconds' * $2 <= attempt_time AND
						success = false
					ORDER BY attempt_time DESC
					LIMIT $3
				`,
				userAuth,
				AttemptLookback/time.Second,
				AttemptFailedCountThreshold,
			)
			server.WithPgResult(result, err, func() {
				attempts = parseAttempts(result)
			})
			if !passesThreshold(attempts) {
				return
			}
		}

		var attempts []UserAuthAttemptResult
		result, err := tx.Query(
			session.Ctx,
			`
				SELECT 
					attempt_time,
					success
				FROM user_auth_attempt
				WHERE 
					client_address_hash = $1 AND
					now() - INTERVAL '1 seconds' * $2 <= attempt_time AND
					success = false
				ORDER BY attempt_time DESC
				LIMIT $3
			`,
			clientAddressHash[:],
			AttemptLookback/time.Second,
			AttemptFailedCountThreshold,
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

func SetUserAuthAttemptSuccess(
	ctx context.Context,
	userAuthAttemptId server.Id,
	success bool,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE user_auth_attempt
				SET success = $1
				WHERE user_auth_attempt_id = $2
			`,
			success,
			userAuthAttemptId,
		))
	})
}

func RemoveExpiredAuthAttempts(ctx context.Context, minTime time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM user_auth_attempt
				WHERE attempt_time < $1
			`,
			minTime.UTC(),
		))
	})
}
