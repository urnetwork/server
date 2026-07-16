package model

import (
	"context"
	"errors"
	"time"

	// "github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

func maxUserAuthAttemptsError() error {
	return errors.New("503 User auth attempts exceeded limits.")
}

const AttemptLookback = 5 * time.Minute
const AttemptFailedCountThreshold = 5
const AttemptLookback2 = 30 * time.Minute
const AttemptFailedCountThreshold2 = 300

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
		passesThreshold2 := func(attempts []UserAuthAttemptResult) bool {
			failedCount := 0
			for i := 0; i < len(attempts); i += 1 {
				if !attempts[i].success {
					failedCount += 1
				}
			}
			return failedCount < AttemptFailedCountThreshold2
		}

		if userAuth != nil {
			// lookback by user auth and client ip hash
			// then if that passes, lookback by user auth

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
						client_address_hash = $2 AND
						now() - INTERVAL '1 seconds' * $3 <= attempt_time AND
						success = false
					ORDER BY attempt_time DESC
					LIMIT $4
				`,
				userAuth,
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

			var attempts2 []UserAuthAttemptResult
			result, err = tx.Query(
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
				AttemptLookback2/time.Second,
				AttemptFailedCountThreshold2,
			)
			server.WithPgResult(result, err, func() {
				attempts2 = parseAttempts(result)
			})
			if !passesThreshold2(attempts2) {
				return
			}
		} else {
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

// removeExpiredAuthAttemptsBatchSize bounds each delete pass. user_auth_attempt
// is high-write, so an unbounded `DELETE ... WHERE attempt_time < $1` deletes the
// whole older-than-window backlog in one long-locking statement; instead drain it
// as a series of bounded, short-locking transactions. A var (not const) so tests
// can drive the multi-batch drain loop with a small batch.
var removeExpiredAuthAttemptsBatchSize = 50000

func RemoveExpiredAuthAttempts(ctx context.Context, minTime time.Time) {
	// LIMIT-batched drain: each pass deletes at most one batch in its own tx,
	// driven by the user_auth_attempt_attempt_time_user_auth_attempt_id index,
	// until a pass removes fewer than the batch size (backlog drained).
	for {
		batchCount := int64(0)
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`
					DELETE FROM user_auth_attempt
					USING (
						SELECT user_auth_attempt_id
						FROM user_auth_attempt
						WHERE attempt_time < $1
						ORDER BY attempt_time
						LIMIT $2
					) t
					WHERE user_auth_attempt.user_auth_attempt_id = t.user_auth_attempt_id
				`,
				minTime.UTC(),
				removeExpiredAuthAttemptsBatchSize,
			))
			batchCount = tag.RowsAffected()
		})
		if batchCount < int64(removeExpiredAuthAttemptsBatchSize) {
			break
		}
	}
	// wallet_auth_challenge_attempt cleanup is handled by RemoveExpiredWalletAuthChallenges
}
