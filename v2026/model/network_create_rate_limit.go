package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
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

// CheckNetworkCreateRateLimit checks if the IP has exceeded the daily account
// creation limit. It records the attempt and returns an error if over the limit.
// Must be called BEFORE the network is actually created (attempt recorded atomically).
func CheckNetworkCreateRateLimit(
	ctx context.Context,
	session *session.ClientSession,
) error {
	clientAddressHash, _, err := session.ClientAddressHashPort()
	if err != nil {
		// can't determine client address — allow
		return nil
	}

	var count int
	// seconds until the oldest attempt still inside the window expires, which
	// is when one slot frees up. Computed in the same statement, against the
	// same clock, so the Retry-After we hand the client is the real wait and
	// not a flat restatement of the window length.
	var retryAfterSeconds int

	server.Tx(ctx, func(tx server.PgTx) {
		// Count how many network creates this IP has done in the last 24 hours
		result, err := tx.Query(
			ctx,
			`
				SELECT
					COUNT(*),
					COALESCE(
						CEIL(EXTRACT(EPOCH FROM (
							MIN(create_time) + INTERVAL '1 seconds' * $2 - now()
						)))::bigint,
						0
					)
				FROM network_create_attempt
				WHERE
					client_address_hash = $1 AND
					now() - INTERVAL '1 seconds' * $2 <= create_time
			`,
			clientAddressHash[:],
			int(NetworkCreateDailyWindow/time.Second),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count, &retryAfterSeconds))
			}
		})

		if count >= NetworkCreateDailyLimit {
			return
		}

		// Record this attempt
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_create_attempt
				(network_create_attempt_id, client_address_hash, create_time)
				VALUES ($1, $2, $3)
			`,
			server.NewId(),
			clientAddressHash[:],
			server.NowUtc(),
		))
	})

	if count >= NetworkCreateDailyLimit {
		// clamp: a clock skew or a just-expired row must never produce a
		// nonsensical hint, and the wait can never exceed the window itself
		if retryAfterSeconds < 1 {
			retryAfterSeconds = 1
		}
		if maxSeconds := int(NetworkCreateDailyWindow / time.Second); maxSeconds < retryAfterSeconds {
			retryAfterSeconds = maxSeconds
		}
		return maxNetworkCreateAttemptsError(retryAfterSeconds)
	}

	return nil
}

// RemoveExpiredNetworkCreateAttempts cleans up attempts older than the window.
func RemoveExpiredNetworkCreateAttempts(ctx context.Context, minTime time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				DELETE FROM network_create_attempt
				WHERE create_time < $1
			`,
			minTime.UTC(),
		))
	})
}
