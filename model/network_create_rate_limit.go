package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const NetworkCreateDailyLimit = 5
const NetworkCreateDailyWindow = 24 * time.Hour

func maxNetworkCreateAttemptsError() error {
	return fmt.Errorf("429 You have reached the maximum number of account creations for today. Please try again later.")
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

	server.Tx(ctx, func(tx server.PgTx) {
		// Count how many network creates this IP has done in the last 24 hours
		result, err := tx.Query(
			ctx,
			`
				SELECT COUNT(*)
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
				server.Raise(result.Scan(&count))
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
		return maxNetworkCreateAttemptsError()
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
