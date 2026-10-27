package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server"
)

// Account-based (not IP-based) rate limits on sensitive actions. Each action
// has its own independent counter over its own trailing window, keyed by the
// id its caller passes: the user for an account action, the calling client for
// a per-client budget such as an extender activation.

const (
	AccountActionAddAuth              = "add_auth"
	AccountActionRemoveAuth           = "remove_auth"
	AccountActionClaimNetworkName     = "claim_network_name"
	AccountActionChangeNetworkName    = "change_network_name"
	AccountActionGenerateSeedphrase   = "generate_seedphrase"
	AccountActionRegenerateSeedphrase = "regenerate_seedphrase"
	AccountActionVerifyStorePurchase  = "verify_store_purchase"
	// the extender endpoints' budgets (controller/extender_*.go), named here
	// so a refusal says which budget was spent
	AccountActionExtenderActivate        = "extender_activate"
	AccountActionExtenderActivateAccount = "extender_activate_account"
	AccountActionExtenderPingReport      = "ping_report"
	AccountActionExtenderLatencyReport   = "extender_latency_report"
	AccountActionExtenderBlockReport     = "extender_block_report"
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

// The purchase-verify endpoints are verify-with-store calls an abuser could
// spin, so they carry an hourly (not daily) budget: a well-behaved client
// polling a pending purchase at the retry-policy cap (5 minutes) uses 12
// attempts per hour, comfortably under the limit, while a tight abuse loop is
// cut off within the hour.
const (
	AccountActionVerifyStorePurchaseWindow      = time.Hour
	AccountActionVerifyStorePurchaseWindowLimit = 30
)

// actionDisplayNames gives each action a human-readable name for the
// rate-limit error message, so a client can tell which of the several
// limits it hit instead of seeing one generic message for all of them.
var actionDisplayNames = map[string]string{
	AccountActionAddAuth:                 "adding a sign-in method",
	AccountActionRemoveAuth:              "removing a sign-in method",
	AccountActionClaimNetworkName:        "setting your network name",
	AccountActionChangeNetworkName:       "changing your network name",
	AccountActionGenerateSeedphrase:      "generating a seedphrase",
	AccountActionRegenerateSeedphrase:    "regenerating a seedphrase",
	AccountActionVerifyStorePurchase:     "verifying a store purchase",
	AccountActionExtenderActivate:        "activating an extender",
	AccountActionExtenderActivateAccount: "activating your account's extenders",
	AccountActionExtenderPingReport:      "reporting extender pings",
	AccountActionExtenderLatencyReport:   "reporting extender latency",
	AccountActionExtenderBlockReport:     "reporting a blocked extender",
}

// The refusal of an action whose budget is spent, naming the action and the
// trailing window the budget counts. It used to say "today" whatever the
// window, and an action without a display name read "this action", so an
// extender activation (six an hour) was refused "for this action today".
func maxAccountActionAttemptsError(action string, window time.Duration) error {
	name, ok := actionDisplayNames[action]
	if !ok {
		name = "this action"
	}
	return fmt.Errorf(
		"You have reached the maximum number of attempts for %s in the last %s. Please try again later.",
		name,
		accountActionWindowText(window),
	)
}

// The trailing window of a budget as words: "hour", "24 hours", "30 minutes".
func accountActionWindowText(window time.Duration) string {
	switch {
	case window == time.Hour:
		return "hour"
	case window == time.Minute:
		return "minute"
	case 0 < window && window%time.Hour == 0:
		return fmt.Sprintf("%d hours", window/time.Hour)
	case 0 < window && window%time.Minute == 0:
		return fmt.Sprintf("%d minutes", window/time.Minute)
	default:
		return window.String()
	}
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
		return maxAccountActionAttemptsError(action, window)
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
		return maxAccountActionAttemptsError(action, window)
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
