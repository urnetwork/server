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
	// so a refusal says which budget was spent. An activation is budgeted per
	// address family, for the calling client and for its account, so one
	// family's attempts never spend the other's (support inbox 8083).
	AccountActionExtenderActivateV4        = "extender_activate_v4"
	AccountActionExtenderActivateV6        = "extender_activate_v6"
	AccountActionExtenderActivateAccountV4 = "extender_activate_account_v4"
	AccountActionExtenderActivateAccountV6 = "extender_activate_account_v6"
	AccountActionExtenderPingReport        = "ping_report"
	AccountActionExtenderLatencyReport     = "extender_latency_report"
	AccountActionExtenderBlockReport       = "extender_block_report"
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
	AccountActionAddAuth:                   "adding a sign-in method",
	AccountActionRemoveAuth:                "removing a sign-in method",
	AccountActionClaimNetworkName:          "setting your network name",
	AccountActionChangeNetworkName:         "changing your network name",
	AccountActionGenerateSeedphrase:        "generating a seedphrase",
	AccountActionRegenerateSeedphrase:      "regenerating a seedphrase",
	AccountActionVerifyStorePurchase:       "verifying a store purchase",
	AccountActionExtenderActivateV4:        "activating an extender over IPv4",
	AccountActionExtenderActivateV6:        "activating an extender over IPv6",
	AccountActionExtenderActivateAccountV4: "activating your account's extenders over IPv4",
	AccountActionExtenderActivateAccountV6: "activating your account's extenders over IPv6",
	AccountActionExtenderPingReport:        "reporting extender pings",
	AccountActionExtenderLatencyReport:     "reporting extender latency",
	AccountActionExtenderBlockReport:       "reporting a blocked extender",
}

// The display name of an action, "this action" for one that has none.
func accountActionDisplayName(action string) string {
	if name, ok := actionDisplayNames[action]; ok {
		return name
	}
	return "this action"
}

// The refusal of an action whose budget is spent, naming the action and the
// trailing window the budget counts. It used to say "today" whatever the
// window, and an action without a display name read "this action", so an
// extender activation (six an hour) was refused "for this action today".
func maxAccountActionAttemptsError(action string, window time.Duration) error {
	return fmt.Errorf(
		"You have reached the maximum number of attempts for %s in the last %s. Please try again later.",
		accountActionDisplayName(action),
		accountActionWindowText(window),
	)
}

// maxAccountActionAttemptsError for a budget that knows when it has room
// again: the refusal names that wait rather than "later".
func maxAccountActionAttemptsRetryError(action string, window time.Duration, retryAfter time.Duration) error {
	return fmt.Errorf(
		"You have reached the maximum number of attempts for %s in the last %s. Please try again in %s.",
		accountActionDisplayName(action),
		accountActionWindowText(window),
		accountActionRetryText(retryAfter),
	)
}

// A wait as words, rounded up so that trying again after it is never early:
// "1 minute", "23 minutes", and whole hours from two hours on.
func accountActionRetryText(retryAfter time.Duration) string {
	minutes := int((retryAfter + time.Minute - 1) / time.Minute)
	switch {
	case minutes <= 1:
		return "1 minute"
	case minutes < 120:
		return fmt.Sprintf("%d minutes", minutes)
	default:
		return fmt.Sprintf("%d hours", (minutes+59)/60)
	}
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

// checkAndRecordAccountActionRateLimitInTx is CheckAndRecordAccountActionRateLimit
// in the caller's transaction, for a budget that must be spent together with
// what it guards. A refusal records nothing and returns how long until the
// oldest attempt it counted leaves the window, when the budget has room again.
func checkAndRecordAccountActionRateLimitInTx(
	ctx context.Context,
	tx server.PgTx,
	userId server.Id,
	action string,
	limit int,
	window time.Duration,
) (allow bool, retryAfter time.Duration) {
	now := server.NowUtc()
	allow, retryAfter = readAccountActionBudgetInTx(ctx, tx, userId, action, limit, window, now)
	if !allow {
		return false, retryAfter
	}
	insertAccountActionAttemptInTx(ctx, tx, userId, action, now)
	return true, 0
}

// Reads the budget of `action` by userId at `now`, in the caller's
// transaction: whether it has room for one more attempt, and when it has none,
// how long until the oldest attempt it counted leaves the window.
func readAccountActionBudgetInTx(
	ctx context.Context,
	tx server.PgTx,
	userId server.Id,
	action string,
	limit int,
	window time.Duration,
	now time.Time,
) (allow bool, retryAfter time.Duration) {
	// a utc cutoff computed here, as in countRecentAccountActionAttempts
	var count int
	var oldestTime *time.Time
	result, err := tx.Query(
		ctx,
		`
		SELECT COUNT(*), MIN(create_time) FROM network_user_action_attempt
		WHERE user_id = $1 AND action = $2
		  AND $3 <= create_time
		`,
		userId, action, now.Add(-window),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&count, &oldestTime))
		}
	})
	if limit <= count {
		retryAfter = window
		if oldestTime != nil {
			retryAfter = oldestTime.Add(window).Sub(now)
		}
		return false, min(max(retryAfter, time.Second), window)
	}
	return true, 0
}

// Records one attempt of `action` by userId at `now`, in the caller's
// transaction.
func insertAccountActionAttemptInTx(
	ctx context.Context,
	tx server.PgTx,
	userId server.Id,
	action string,
	now time.Time,
) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO network_user_action_attempt (network_user_action_attempt_id, user_id, action, create_time)
		VALUES ($1, $2, $3, $4)
		`,
		server.NewId(), userId, action, now,
	))
}

// One attempt budget: at most Limit attempts of Action by Id in the trailing
// Window. Id is the user for an account's budget, the calling client for a
// client's own.
type AccountActionBudget struct {
	Id     server.Id
	Action string
	Limit  int
	Window time.Duration
}

// CheckAndRecordAccountActionBudgets spends one attempt from every budget, in
// one transaction, and only when every one of them has room. A refusal spends
// nothing from any budget, so an action refused by one budget leaves the
// others as they were: a client refused by its account's budget keeps its
// own. The refusal names the first budget, in the order given, that has no
// room, its window, and how long until it has room again.
//
// Like CheckAndRecordAccountActionRateLimit, call it only once every check
// that can refuse without doing the work has passed, and immediately before
// the work the budgets pay for.
func CheckAndRecordAccountActionBudgets(ctx context.Context, budgets ...AccountActionBudget) error {
	var refusal error
	server.Tx(ctx, func(tx server.PgTx) {
		// a transaction that is run again decides again from what it reads
		refusal = nil
		now := server.NowUtc()
		for _, budget := range budgets {
			allow, retryAfter := readAccountActionBudgetInTx(
				ctx,
				tx,
				budget.Id,
				budget.Action,
				budget.Limit,
				budget.Window,
				now,
			)
			if !allow {
				refusal = maxAccountActionAttemptsRetryError(budget.Action, budget.Window, retryAfter)
				return
			}
		}
		for _, budget := range budgets {
			insertAccountActionAttemptInTx(ctx, tx, budget.Id, budget.Action, now)
		}
	})
	return refusal
}
