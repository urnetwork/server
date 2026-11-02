package model

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	mathrand "math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// Account-based (not IP-based) rate limits on sensitive actions. Each action
// has its own independent counter over its own trailing window, keyed by the
// id its caller passes: the user for an account action, the calling client for
// a per-client budget such as an extender activation, or a keyed hash of the
// caller's address for a per-address one.

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
	// address family, for the calling client, for the address it is probed
	// at -- or, for a key its network already proved there, for that key at
	// that address -- and for its account, so one family's attempts never
	// spend the other's (support inbox 8083, connect#230).
	AccountActionExtenderActivateV4              = "extender_activate_v4"
	AccountActionExtenderActivateV6              = "extender_activate_v6"
	AccountActionExtenderActivateAddressV4       = "extender_activate_address_v4"
	AccountActionExtenderActivateAddressV6       = "extender_activate_address_v6"
	AccountActionExtenderActivateProvenAddressV4 = "extender_activate_proven_address_v4"
	AccountActionExtenderActivateProvenAddressV6 = "extender_activate_proven_address_v6"
	AccountActionExtenderActivateAccountV4       = "extender_activate_account_v4"
	AccountActionExtenderActivateAccountV6       = "extender_activate_account_v6"
	AccountActionExtenderPingReport              = "ping_report"
	AccountActionExtenderLatencyReport           = "extender_latency_report"
	AccountActionExtenderBlockReport             = "extender_block_report"
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
	AccountActionAddAuth:                         "adding a sign-in method",
	AccountActionRemoveAuth:                      "removing a sign-in method",
	AccountActionClaimNetworkName:                "setting your network name",
	AccountActionChangeNetworkName:               "changing your network name",
	AccountActionGenerateSeedphrase:              "generating a seedphrase",
	AccountActionRegenerateSeedphrase:            "regenerating a seedphrase",
	AccountActionVerifyStorePurchase:             "verifying a store purchase",
	AccountActionExtenderActivateV4:              "activating an extender over IPv4",
	AccountActionExtenderActivateV6:              "activating an extender over IPv6",
	AccountActionExtenderActivateAddressV4:       "activating an extender from this address over IPv4",
	AccountActionExtenderActivateAddressV6:       "activating an extender from this address over IPv6",
	AccountActionExtenderActivateProvenAddressV4: "re-activating an extender from its own address over IPv4",
	AccountActionExtenderActivateProvenAddressV6: "re-activating an extender from its own address over IPv6",
	AccountActionExtenderActivateAccountV4:       "activating your account's extenders over IPv4",
	AccountActionExtenderActivateAccountV6:       "activating your account's extenders over IPv6",
	AccountActionExtenderPingReport:              "reporting extender pings",
	AccountActionExtenderLatencyReport:           "reporting extender latency",
	AccountActionExtenderBlockReport:             "reporting a blocked extender",
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

// A wait as words, the way a refusal by a budget names it, for a refusal of
// another kind that also knows its wait (an operator too busy to probe an
// extender activation, connect#230).
func AccountActionRetryText(retryAfter time.Duration) string {
	return accountActionRetryText(retryAfter)
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

// Attempts are kept twice the longest window that counts them
// (AccountActionDailyWindow and UserAuthCodeSendWindow, 24 hours), so no
// rate limit ever reads a deleted attempt.
const AccountActionAttemptRetention = 48 * time.Hour

var accountActionAttemptRetention = newTimeOrderedRetention(
	RetentionTableNetworkUserActionAttempt,
	"network_user_action_attempt_id",
	"create_time",
)

// RemoveExpiredAccountActionAttempts removes the attempts older than minTime,
// oldest first, in bounded batches for at most retentionRunBudget; the next
// run continues a backlog.
func RemoveExpiredAccountActionAttempts(ctx context.Context, minTime time.Time) (removedCount int64) {
	removedCount, _ = accountActionAttemptRetention.RemoveBefore(ctx, minTime, retentionBatchSize, time.Now().Add(retentionRunBudget))
	return
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
// how long until it has (AccountActionBudgetRetryAfter).
func readAccountActionBudgetInTx(
	ctx context.Context,
	tx server.PgTx,
	userId server.Id,
	action string,
	limit int,
	window time.Duration,
	now time.Time,
) (allow bool, retryAfter time.Duration) {
	return readAccountActionBudgetLimitInTx(
		ctx,
		tx,
		AccountActionBudget{
			Id:     userId,
			Action: action,
			Limit:  limit,
			Window: window,
		},
		now,
	)
}

// Reads one budget at `now`, in the caller's transaction: whether it has room
// for one more attempt, and when it has none, how long until it has. A budget
// whose count has reached Limit asks its ExtendLimitInTx, when it has one, for
// the limit it has grown to.
func readAccountActionBudgetLimitInTx(
	ctx context.Context,
	tx server.PgTx,
	budget AccountActionBudget,
	now time.Time,
) (allow bool, retryAfter time.Duration) {
	// a utc cutoff computed here, as in countRecentAccountActionAttempts
	cutoff := now.Add(-budget.Window)
	var count int
	var oldestTime *time.Time
	result, err := tx.Query(
		ctx,
		`
		SELECT COUNT(*), MIN(create_time) FROM network_user_action_attempt
		WHERE user_id = $1 AND action = $2
		  AND $3 <= create_time
		`,
		budget.Id, budget.Action, cutoff,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&count, &oldestTime))
		}
	})
	limit := budget.Limit
	if limit <= count && budget.ExtendLimitInTx != nil {
		limit = max(limit, budget.ExtendLimitInTx(ctx, tx))
	}
	if count < limit {
		return true, 0
	}
	// the attempt whose leaving makes room: the oldest while the count is at
	// the limit, and a later one when the limit is below the count, which a
	// limit that shrank leaves (connect#230)
	boundaryTime := oldestTime
	if limit < count {
		boundaryTime = nil
		result, err := tx.Query(
			ctx,
			`
			SELECT create_time FROM network_user_action_attempt
			WHERE user_id = $1 AND action = $2
			  AND $3 <= create_time
			ORDER BY create_time ASC
			OFFSET $4 LIMIT 1
			`,
			budget.Id, budget.Action, cutoff, count-max(limit, 0),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&boundaryTime))
			}
		})
	}
	return false, AccountActionBudgetRetryAfter(boundaryTime, budget.Window, now)
}

// How long until a budget with no room has room again: until the attempt whose
// leaving makes room, `boundaryTime`, leaves the trailing window. At least a
// second and at most the window; the window when no attempt can make room.
func AccountActionBudgetRetryAfter(boundaryTime *time.Time, window time.Duration, now time.Time) time.Duration {
	retryAfter := window
	if boundaryTime != nil {
		retryAfter = boundaryTime.Add(window).Sub(now)
	}
	return min(max(retryAfter, time.Second), window)
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
// client's own, and a keyed hash of the caller's address for an address's.
type AccountActionBudget struct {
	Id     server.Id
	Action string
	Limit  int
	Window time.Duration
	// when set, the limit once the count has reached Limit, read in the
	// spending transaction: a limit that grows with what its owner has proven
	// (an account's proven extender hosts, connect#230) is read only by an
	// owner that has spent its floor. A value below Limit leaves Limit.
	ExtendLimitInTx func(ctx context.Context, tx server.PgTx) int
	// when positive, the attempt is recorded at a time drawn uniformly over
	// this much before the spend, each budget's on its own, so the attempts
	// one spend records share no time that joins them (connect#230): an
	// address's hashed budget row is not tied to its client's and its
	// account's rows by an identical create_time. Zero records the spend's
	// time.
	AttemptTimeSpread time.Duration
}

// The refusal of a spend by one of its budgets
// (CheckAndRecordAccountActionBudgets): the budget that had no room, and how
// long until it has.
type AccountActionBudgetRefusal struct {
	Action     string
	Window     time.Duration
	RetryAfter time.Duration
}

func (self *AccountActionBudgetRefusal) Error() string {
	return maxAccountActionAttemptsRetryError(self.Action, self.Window, self.RetryAfter).Error()
}

// Testing_AccountActionBudgetsBeforeSpend, when set, runs inside the
// transaction of CheckAndRecordAccountActionBudgets once every budget has been
// read with room, before any is spent. It exists so a test can hold one spend
// between its reads and its writes while another spend of the same budgets
// runs. Test only, and never set in production.
var Testing_AccountActionBudgetsBeforeSpend func()

// The refusal of a spend that waited past its lock bound for budgets another
// spend held (accountActionBudgetsLockTimeout, connect#230): nothing was
// spent, and the caller may try again shortly. The wait's 55P03 used to raise
// out of the spend, and the route answered it as an unexpected error.
var ErrAccountActionBudgetsBusy = errors.New("the budgets are held by another spend")

// How long a spend waits for the database lock of each of its budgets: the
// main pool's bound (server.PgLockTimeout), past which waiters each holding a
// pooler server would queue on one hot budget. A test that holds a spend's
// budgets on purpose lengthens it, and one that needs the bound to end a
// wait shortens it.
var accountActionBudgetsLockTimeout = server.PgLockTimeout

// Testing_AccountActionBudgetsLockWait, when set, runs when a spend finds a
// budget it spends held by another spend in this process, before it waits for
// that spend to end. Test only, and never set in production.
var Testing_AccountActionBudgetsLockWait func()

// The process locks of the budgets, by a hash of the budget. A spend holds the
// locks of its budgets for its whole transaction, so the spends of one budget
// in one process wait here, where waiting holds no database connection, and
// the database lock each spend also takes (lockAccountActionBudgetsInTx)
// queues at most one spend of a budget per process. A queue of waiters there
// would each hold a pooler server (server.PgLockTimeout), and an account's
// fleet restarting at once is exactly a queue of spends of one budget.
var accountActionBudgetProcessLocks [256]sync.Mutex

// The lock keys of a spend's budgets, in the one order every spend takes them:
// sorted and distinct, so two spends that share budgets never each hold one the
// other waits for.
func accountActionBudgetLockKeys(budgets []AccountActionBudget) []string {
	lockKeys := []string{}
	for _, budget := range budgets {
		lockKeys = append(lockKeys, fmt.Sprintf("account-action-budget/v1:%s:%s", budget.Action, budget.Id))
	}
	slices.Sort(lockKeys)
	return slices.Compact(lockKeys)
}

// Takes the process locks of a spend's budgets, in the order of their indexes,
// and returns their release.
func lockAccountActionBudgetsInProcess(lockKeys []string) (unlock func()) {
	lockIndexes := []int{}
	for _, lockKey := range lockKeys {
		lockHash := fnv.New32a()
		lockHash.Write([]byte(lockKey))
		lockIndexes = append(lockIndexes, int(lockHash.Sum32()%uint32(len(accountActionBudgetProcessLocks))))
	}
	slices.Sort(lockIndexes)
	lockIndexes = slices.Compact(lockIndexes)
	for _, lockIndex := range lockIndexes {
		processLock := &accountActionBudgetProcessLocks[lockIndex]
		if !processLock.TryLock() {
			if Testing_AccountActionBudgetsLockWait != nil {
				Testing_AccountActionBudgetsLockWait()
			}
			processLock.Lock()
		}
	}
	return func() {
		for i := len(lockIndexes) - 1; 0 <= i; i -= 1 {
			accountActionBudgetProcessLocks[lockIndexes[i]].Unlock()
		}
	}
}

// Takes the database lock of each of a spend's budgets, in the caller's
// transaction, so the spends of a budget in every process run one after
// another and each reads what the one before it spent. That takes read
// committed isolation: a repeatable read transaction keeps the snapshot of
// its first statement, which precedes the wait, and would miss what the spend
// it waited for wrote.
func lockAccountActionBudgetsInTx(ctx context.Context, tx server.PgTx, lockKeys []string) {
	for _, lockKey := range lockKeys {
		var isolation string
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT current_setting('transaction_isolation') FROM pg_advisory_xact_lock(hashtextextended($1, 0))`,
			lockKey,
		).Scan(&isolation))
		if isolation != "read committed" {
			panic(fmt.Errorf("the account action budget lock requires read committed isolation, not %s", isolation))
		}
	}
}

// Reads a spend's budgets in the caller's transaction: the refusal of the
// budget with no room that has the longest wait, nil when every one has room
// (see CheckAndRecordAccountActionBudgets).
func readAccountActionBudgetsRefusalInTx(
	ctx context.Context,
	tx server.PgTx,
	budgets []AccountActionBudget,
	now time.Time,
) *AccountActionBudgetRefusal {
	budgetRefusals := []*AccountActionBudgetRefusal{}
	retryAfter := time.Duration(0)
	waitSpread := time.Duration(0)
	for _, budget := range budgets {
		allow, budgetRetryAfter := readAccountActionBudgetLimitInTx(ctx, tx, budget, now)
		if !allow {
			budgetRefusals = append(budgetRefusals, &AccountActionBudgetRefusal{
				Action:     budget.Action,
				Window:     budget.Window,
				RetryAfter: budgetRetryAfter,
			})
			retryAfter = max(retryAfter, budgetRetryAfter)
		}
		waitSpread = max(waitSpread, budget.AttemptTimeSpread)
	}
	for _, budgetRefusal := range budgetRefusals {
		if retryAfter-budgetRefusal.RetryAfter <= waitSpread {
			budgetRefusal.RetryAfter = retryAfter
			return budgetRefusal
		}
	}
	return nil
}

// CheckAndRecordAccountActionBudgets spends one attempt from every budget, in
// one transaction, and only when every one of them has room. A refusal spends
// nothing from any budget, so an action refused by one budget leaves the
// others as they were: a client refused by its account's budget keeps its
// own. The refusal, an *AccountActionBudgetRefusal, names the budget with no
// room that has the longest wait, and its window, and asks for that wait
// (connect#230): a caller that waits it out finds every budget with room,
// where the first budget's wait could end before another's. Waits apart by
// no more than the budgets' AttemptTimeSpread are one wait, and the first of
// their budgets in the order given is named, with the longest of them: two
// budgets spent by the same spends wait apart by less than the spread, and
// would otherwise be named at random.
//
// A spend that waits past its bound for a budget's database lock, which
// another spend holds, returns ErrAccountActionBudgetsBusy and spends nothing.
//
// The spends of a budget run one at a time (connect#230): each takes the locks
// of its budgets, in this process and in the database, before it reads them.
// Two spends that both read a budget one short of its limit would otherwise
// both spend it, and the budget would be passed by as many spends as ran at
// once.
//
// A budget that is already spent refuses first, on a read without the locks: a
// wave of a fleet's posts that its account's budget refuses all share that one
// budget, and each would otherwise queue on its locks, holding a pooler server
// in the database, to be told what the read says. The read only ever refuses.
// It can be behind by a spend not yet committed, so what it finds room for is
// decided again under the locks.
//
// Like CheckAndRecordAccountActionRateLimit, call it only once every check
// that can refuse without doing the work has passed, and immediately before
// the work the budgets pay for.
func CheckAndRecordAccountActionBudgets(ctx context.Context, budgets ...AccountActionBudget) error {
	var spentRefusal *AccountActionBudgetRefusal
	server.Tx(ctx, func(tx server.PgTx) {
		spentRefusal = readAccountActionBudgetsRefusalInTx(ctx, tx, budgets, server.NowUtc())
	}, server.TxReadCommitted)
	if spentRefusal != nil {
		return spentRefusal
	}

	lockKeys := accountActionBudgetLockKeys(budgets)
	unlockInProcess := lockAccountActionBudgetsInProcess(lockKeys)
	defer unlockInProcess()

	var refusal error
	// a wait for a budget's database lock that ran past its bound ends the
	// spend with nothing spent, which is a busy answer for the caller rather
	// than an error of the request (connect#230); every other raise is the
	// caller's as it was
	busy := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				if !server.IsLockTimeoutRefusal(r) {
					panic(r)
				}
				busy = true
			}
		}()
		server.Tx(ctx, func(tx server.PgTx) {
			// a transaction that is run again decides again from what it reads
			refusal = nil
			lockAccountActionBudgetsInTx(ctx, tx, lockKeys)
			now := server.NowUtc()
			if budgetRefusal := readAccountActionBudgetsRefusalInTx(ctx, tx, budgets, now); budgetRefusal != nil {
				refusal = budgetRefusal
				return
			}
			if Testing_AccountActionBudgetsBeforeSpend != nil {
				Testing_AccountActionBudgetsBeforeSpend()
			}
			// each attempt at a time of its own, so no two rows of a spend
			// share a create_time
			attemptTimes := map[time.Time]bool{}
			for _, budget := range budgets {
				attemptTime := now
				if 0 < budget.AttemptTimeSpread {
					attemptTime = now.Add(-mathrand.N(budget.AttemptTimeSpread)).Truncate(time.Microsecond)
					for attemptTimes[attemptTime] {
						attemptTime = attemptTime.Add(-time.Microsecond)
					}
					attemptTimes[attemptTime] = true
				}
				insertAccountActionAttemptInTx(ctx, tx, budget.Id, budget.Action, attemptTime)
			}
		}, server.TxReadCommitted, server.TxLockTimeout(accountActionBudgetsLockTimeout))
	}()
	if busy {
		return ErrAccountActionBudgetsBusy
	}
	return refusal
}
