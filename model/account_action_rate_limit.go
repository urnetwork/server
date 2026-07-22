package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// Account-based (not IP-based) daily rate limits on sensitive account-mutation
// actions. Each action has its own independent daily counter, keyed by userId.

const (
	AccountActionAddAuth              = "add_auth"
	AccountActionRemoveAuth           = "remove_auth"
	AccountActionClaimNetworkName     = "claim_network_name"
	AccountActionChangeNetworkName    = "change_network_name"
	AccountActionGenerateSeedphrase   = "generate_seedphrase"
	AccountActionRegenerateSeedphrase = "regenerate_seedphrase"
)

const (
	AccountActionAddAuthDailyLimit              = 5
	AccountActionRemoveAuthDailyLimit           = 5
	AccountActionClaimNetworkNameDailyLimit     = 2
	AccountActionChangeNetworkNameDailyLimit    = 2
	AccountActionGenerateSeedphraseDailyLimit   = 5
	AccountActionRegenerateSeedphraseDailyLimit = 5
)

const AccountActionDailyWindow = 24 * time.Hour

// actionDisplayNames gives each action a human-readable name for the
// rate-limit error message, so a client can tell which of the several
// daily limits it hit instead of seeing one generic message for all of them.
var actionDisplayNames = map[string]string{
	AccountActionAddAuth:              "adding a sign-in method",
	AccountActionRemoveAuth:           "removing a sign-in method",
	AccountActionClaimNetworkName:     "setting your network name",
	AccountActionChangeNetworkName:    "changing your network name",
	AccountActionGenerateSeedphrase:   "generating a seedphrase",
	AccountActionRegenerateSeedphrase: "regenerating a seedphrase",
}

func maxAccountActionAttemptsError(action string) error {
	name, ok := actionDisplayNames[action]
	if !ok {
		name = "this action"
	}
	return fmt.Errorf("You have reached the maximum number of attempts for %s today. Please try again later.", name)
}

func countRecentAccountActionAttempts(
	ctx context.Context,
	tx server.PgTx,
	userId server.Id,
	action string,
	window time.Duration,
) int {
	var count int
	// the cutoff is computed here in Go, not via `now() - INTERVAL ...` in
	// SQL: create_time is a naive `timestamp` column holding UTC wall-clock
	// values (via server.NowUtc()), and `now()` returns `timestamptz`.
	// Comparing the two forces Postgres to cast one side using the
	// session's TimeZone setting -- on a non-UTC session that silently
	// shifts the effective window by the zone offset. Passing an explicit
	// UTC cutoff avoids any zone-dependent cast.
	cutoff := server.NowUtc().Add(-window)
	result, err := tx.Query(
		ctx,
		`
		SELECT COUNT(*) FROM network_user_action_attempt
		WHERE user_id = $1 AND action = $2
		  AND $3 <= create_time
		`,
		userId, action, cutoff,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&count))
		}
	})
	return count
}

// CheckAccountActionRateLimit reports whether userId is still under `limit`
// attempts of `action` in the trailing `window`. It does not record
// anything — pair with RecordAccountActionAttempt after the action actually
// succeeds, so failed/rejected attempts don't burn the caller's budget.
func CheckAccountActionRateLimit(
	ctx context.Context,
	userId server.Id,
	action string,
	limit int,
	window time.Duration,
) error {
	var count int
	cutoff := server.NowUtc().Add(-window)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT COUNT(*) FROM network_user_action_attempt
			WHERE user_id = $1 AND action = $2
			  AND $3 <= create_time
			`,
			userId, action, cutoff,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	if limit <= count {
		return maxAccountActionAttemptsError(action)
	}
	return nil
}

// RecordAccountActionAttempt records one attempt of `action` by userId.
// Call this only once the action it is guarding has actually succeeded.
func RecordAccountActionAttempt(ctx context.Context, userId server.Id, action string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_user_action_attempt (network_user_action_attempt_id, user_id, action, create_time)
			VALUES ($1, $2, $3, $4)
			`,
			server.NewId(), userId, action, server.NowUtc(),
		))
	})
}

// CheckAndRecordAccountActionRateLimit atomically checks and records an
// attempt in one transaction. Use this for strict, attempt-counted actions
// (e.g. a name change) where every real attempt -- not just successes --
// should count against the budget. Call it only after cheap validation
// (format, availability) has already passed, so malformed requests don't
// burn budget.
func CheckAndRecordAccountActionRateLimit(
	ctx context.Context,
	userId server.Id,
	action string,
	limit int,
	window time.Duration,
) error {
	var count int
	server.Tx(ctx, func(tx server.PgTx) {
		count = countRecentAccountActionAttempts(ctx, tx, userId, action, window)
		if limit <= count {
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO network_user_action_attempt (network_user_action_attempt_id, user_id, action, create_time)
			VALUES ($1, $2, $3, $4)
			`,
			server.NewId(), userId, action, server.NowUtc(),
		))
	})
	if limit <= count {
		return maxAccountActionAttemptsError(action)
	}
	return nil
}

// RemoveExpiredAccountActionAttempts cleans up attempts older than minTime.
func RemoveExpiredAccountActionAttempts(ctx context.Context, minTime time.Time) {
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM network_user_action_attempt WHERE create_time < $1`,
			minTime.UTC(),
		))
	})
}
