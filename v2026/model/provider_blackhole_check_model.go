package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
)

// ProviderBlackholeCheckMaxAge is how long a blackhole check is treated as
// current. Past it the provider is indistinguishable from one never checked.
//
// This is deliberately short. The whole point of the check is that it is cheap
// enough to run hourly across the entire fleet, so evidence should never BE
// this old in a healthy deployment -- if it is, the sweep is not keeping up and
// the provider should fall back to being judged on egress health alone rather
// than on a stale liveness answer.
//
// 3h, not 1h, so a single missed or slow sweep does not evict the fleet: the
// checker gets three attempts at a provider before its evidence lapses.
const ProviderBlackholeCheckMaxAge = 3 * time.Hour

// ProviderBlackholeCheckDueAge is how long after a passing check the provider
// is offered up again: the next_due_at a pass sets, and the due time of a row
// written before next_due_at existed. Half the max age, for the same reason
// providerEgressDueAge is half ProviderEgressLocationMaxAge: the sweep gets a
// full window to refresh a check before it expires.
const ProviderBlackholeCheckDueAge = ProviderBlackholeCheckMaxAge / 2

// MaxProviderBlackholeFailureLen mirrors the failure column width. A submission
// longer than this is rejected rather than truncated, so a caller learns its
// class name does not fit instead of having it silently mangled.
const MaxProviderBlackholeFailureLen = 64

// ProviderBlackholeTlsAuthenticationFailure is the prober's failure class for
// a check whose peer could not authenticate the requested host
// (egresshealth.FailureTLSAuthentication). It is the one failure that is a
// verdict on its own: a forged identity is not a slow cold start, and a
// provider that presents one is unsafe from the first check.
const ProviderBlackholeTlsAuthenticationFailure = "tls_authentication_failed"

// ProviderBlackholeNotMeasuredFailure is the failure class a check that
// measured nothing is stored under when the provider has no earlier check
// (egresshealth.FailureNotMeasured). It counts nothing against the provider.
const ProviderBlackholeNotMeasuredFailure = "not_measured"

// ProviderBlackholeCheck is one provider's current answer to one question: did
// ANY traffic get through -- and, since GEOMAP step 7, how many checks in a row
// have said no.
//
// It is not a measurement of quality and must not be treated as one. Egress
// health samples ~50 loads across four classes and can say "this provider
// resolves names and carries bytes but is refused by content providers"; this
// says only "something got through" or "nothing did". The two are kept in
// separate tables because they answer different questions on different
// cadences.
//
// A failed check is a failure, not a verdict (connect/GEOMAP.md §11.3): a
// provider is dark only after ProviderEgressRules.DarkConsecutiveFailures
// failed checks in a row spanning ProviderEgressRules.DarkMinimumSpan (see
// IsDark). The count is the provider's, whatever its connections did between
// checks: the prober's retries and re-created tunnels already carry a check
// across a reconnect.
type ProviderBlackholeCheck struct {
	ClientId server.Id
	// CheckedAt is when the latest measured check started. A check that
	// measured nothing never moves it, so a provider the prober cannot reach
	// ages out of the dark set rather than being held in it.
	CheckedAt time.Time
	OK        bool
	// Failure is "" when OK, otherwise a short class such as tunnel_failed or
	// all_destinations_failed, or not_measured for a row whose only check
	// measured nothing.
	Failure string
	// ConsecutiveFailures is how many measured checks in a row have failed; a
	// passing check resets it to 0.
	ConsecutiveFailures int
	// FirstFailedAt is when the first check of that run started; nil when the
	// count is 0.
	FirstFailedAt *time.Time
	// NextDueAt is when the provider is next offered to the sweep: a backoff
	// step after a failure or an unmeasured check, the ordinary due age after
	// a pass. Nil on a row written before the column existed, which is due
	// ProviderBlackholeCheckDueAge after CheckedAt.
	NextDueAt  *time.Time
	UpdateTime time.Time
}

// The current-dark rule of connect/GEOMAP.md §11.3 and §10.3 for one
// row at now: a check within ProviderBlackholeCheckMaxAge, and either a TLS
// authentication failure -- immediate hard evidence -- or a run of at least
// DarkConsecutiveFailures failed checks whose first failure began at least
// DarkMinimumSpan before the latest. ProviderBlackholeDarkSql is the same rule
// in SQL; the two are held together by TestProviderBlackholeDarkSqlMatchesIsDark.
func (self *ProviderBlackholeCheck) IsDark(now time.Time, rules ProviderEgressRules) bool {
	if self == nil || self.CheckedAt.Before(now.Add(-ProviderBlackholeCheckMaxAge)) {
		return false
	}
	if !self.OK && self.Failure == ProviderBlackholeTlsAuthenticationFailure {
		return true
	}
	// A legacy passing write can leave the prior streak columns intact.
	return !self.OK && rules.DarkConsecutiveFailures <= self.ConsecutiveFailures &&
		self.FirstFailedAt != nil &&
		!self.CheckedAt.Add(-rules.DarkMinimumSpan()).Before(*self.FirstFailedAt)
}

// IsDark over the provider_blackhole_check row named alias, for a query that
// gives the max-age cutoff as minCheckedAtSql (a parameter such as "$7", or an
// expression). The rule's integers are inlined: they are validated settings
// (see ProviderEgressRules.Validate), not caller input, and inlining them lets
// every query that needs the rule -- the dark set, the full due heads, the
// §2.19 monitor's population -- share one fragment whatever its other
// parameters.
func ProviderBlackholeDarkSql(alias string, minCheckedAtSql string, rules ProviderEgressRules) string {
	return fmt.Sprintf(`(
		%[2]s <= %[1]s.checked_at AND (
			(%[1]s.ok = false AND %[1]s.failure = '%[3]s') OR
			(
				%[1]s.ok = false AND
				%[4]d <= %[1]s.consecutive_failures AND
				%[1]s.first_failed_at IS NOT NULL AND
				%[1]s.first_failed_at <= %[1]s.checked_at - interval '%[5]d seconds'
			)
		)
	)`,
		alias,
		minCheckedAtSql,
		ProviderBlackholeTlsAuthenticationFailure,
		rules.DarkConsecutiveFailures,
		rules.DarkMinimumSpanSeconds,
	)
}

// One check as the prober submitted it.
type ProviderBlackholeCheckReport struct {
	ClientId  server.Id
	CheckedAt time.Time
	Ok        bool
	Failure   string
	// NotMeasured is a check none of whose loads could be measured: its tunnel
	// was gone and could not be re-created. It is not a verdict either way.
	NotMeasured bool
}

// The consecutive-failure state machine of connect/GEOMAP.md §11.3: the row a
// report leaves behind, given the row before it (nil when the provider was
// never checked), and whether anything changed.
//
//   - a report no newer than the stored check changes nothing: a replayed or
//     out-of-order report must not move the count or the schedule;
//   - a check that measured nothing only reschedules, a backoff step on from
//     where the provider stands: it neither counts a failure nor clears one,
//     and a provider without a row gets one that says not_measured;
//   - a passing check clears the count and is next due after the ordinary due
//     age;
//   - a failing check extends the run (starting it on the first failure) and
//     is next due after the backoff step of its length.
//
// Schedules count from now, the ingest time: a check spans up to its retries'
// length, and a backoff counted from its start could already have passed by
// the time it is reported.
func NextProviderBlackholeCheck(
	previous *ProviderBlackholeCheck,
	report ProviderBlackholeCheckReport,
	now time.Time,
	rules ProviderEgressRules,
) (*ProviderBlackholeCheck, bool) {
	if previous != nil && !previous.CheckedAt.Before(report.CheckedAt) {
		return previous, false
	}

	if report.NotMeasured {
		var next ProviderBlackholeCheck
		if previous == nil {
			next = ProviderBlackholeCheck{
				ClientId:  report.ClientId,
				CheckedAt: report.CheckedAt,
				OK:        false,
				Failure:   ProviderBlackholeNotMeasuredFailure,
			}
		} else {
			next = *previous
			if next.OK {
				// Preserve the measured pass, not an older writer's stale run.
				next.ConsecutiveFailures = 0
				next.FirstFailedAt = nil
			}
		}
		nextDueAt := now.Add(rules.DarkBackoff(next.ConsecutiveFailures))
		next.NextDueAt = &nextDueAt
		return &next, true
	}

	next := &ProviderBlackholeCheck{
		ClientId:  report.ClientId,
		CheckedAt: report.CheckedAt,
		OK:        report.Ok,
	}
	if report.Ok {
		nextDueAt := now.Add(ProviderBlackholeCheckDueAge)
		next.NextDueAt = &nextDueAt
		return next, true
	}

	next.Failure = report.Failure
	if previous != nil && !previous.OK && 0 < previous.ConsecutiveFailures && previous.FirstFailedAt != nil {
		next.ConsecutiveFailures = previous.ConsecutiveFailures + 1
		firstFailedAt := *previous.FirstFailedAt
		next.FirstFailedAt = &firstFailedAt
	} else {
		next.ConsecutiveFailures = 1
		firstFailedAt := report.CheckedAt
		next.FirstFailedAt = &firstFailedAt
	}
	nextDueAt := now.Add(rules.DarkBackoff(next.ConsecutiveFailures))
	next.NextDueAt = &nextDueAt
	return next, true
}

// The ingest path: every report advances its provider's row through
// NextProviderBlackholeCheck, in one transaction that holds the rows it reads,
// so two ingests of the same provider cannot both build on the same previous
// row.
func RecordProviderBlackholeChecks(
	ctx context.Context,
	reports []ProviderBlackholeCheckReport,
	rules ProviderEgressRules,
) {
	if len(reports) == 0 {
		return
	}
	clientIds := make([]server.Id, 0, len(reports))
	for _, report := range reports {
		clientIds = append(clientIds, report.ClientId)
	}

	server.Tx(ctx, func(tx server.PgTx) {
		now := server.NowUtc()
		current := map[server.Id]*ProviderBlackholeCheck{}
		result, err := tx.Query(
			ctx,
			`
			SELECT
				client_id,
				checked_at,
				ok,
				failure,
				consecutive_failures,
				first_failed_at,
				next_due_at,
				update_time
			FROM provider_blackhole_check
			WHERE client_id = ANY($1)
			ORDER BY client_id
			FOR UPDATE
			`,
			clientIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				c := &ProviderBlackholeCheck{}
				server.Raise(result.Scan(
					&c.ClientId,
					&c.CheckedAt,
					&c.OK,
					&c.Failure,
					&c.ConsecutiveFailures,
					&c.FirstFailedAt,
					&c.NextDueAt,
					&c.UpdateTime,
				))
				current[c.ClientId] = c
			}
		})

		// reports are applied in order, each on the row the one before it
		// left, so a batch that names a provider twice is the same as two
		// batches
		changed := map[server.Id]*ProviderBlackholeCheck{}
		for _, report := range reports {
			next, ok := NextProviderBlackholeCheck(current[report.ClientId], report, now, rules)
			if !ok {
				continue
			}
			current[report.ClientId] = next
			changed[report.ClientId] = next
		}
		for _, c := range changed {
			writeProviderBlackholeCheck(ctx, tx, c, now)
		}
	})
}

// Upserts one row as given. The guard lets a check that measured nothing --
// which keeps checked_at -- reschedule the row, while still refusing to let an
// older check overwrite a newer one that raced it in.
func writeProviderBlackholeCheck(ctx context.Context, tx server.PgTx, c *ProviderBlackholeCheck, now time.Time) {
	failure := c.Failure
	consecutiveFailures := c.ConsecutiveFailures
	if c.OK {
		// A pass ends the run even when a backfill supplies retained columns.
		// Normalize the stored copy without mutating the caller's evidence.
		failure = ""
		consecutiveFailures = 0
	}
	var firstFailedAt *time.Time
	if c.FirstFailedAt != nil && 0 < consecutiveFailures {
		utc := c.FirstFailedAt.UTC()
		firstFailedAt = &utc
	}
	var nextDueAt *time.Time
	if c.NextDueAt != nil {
		utc := c.NextDueAt.UTC()
		nextDueAt = &utc
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO provider_blackhole_check (
			client_id,
			checked_at,
			ok,
			failure,
			consecutive_failures,
			first_failed_at,
			next_due_at,
			update_time
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (client_id) DO UPDATE
		SET
			checked_at = $2,
			ok = $3,
			failure = $4,
			consecutive_failures = $5,
			first_failed_at = $6,
			next_due_at = $7,
			update_time = $8
		WHERE provider_blackhole_check.checked_at <= EXCLUDED.checked_at
		`,
		c.ClientId,
		c.CheckedAt.UTC(),
		c.OK,
		failure,
		consecutiveFailures,
		firstFailedAt,
		nextDueAt,
		now,
	))
}

// SetProviderBlackholeCheck writes one provider's row as given -- the check,
// its run of failures and its schedule -- for callers that already hold the
// state they want (fixtures, backfills). Ingest never calls it: a report goes
// through RecordProviderBlackholeChecks, which is where the count is kept.
//
// The upsert is monotonic in checked_at, like SetProviderEgressLocation and
// SetProviderEgressProbeAttempt: a replayed or out-of-order row older than what
// is stored is dropped rather than moving the provider's last-checked time
// backwards.
func SetProviderBlackholeCheck(ctx context.Context, c *ProviderBlackholeCheck) {
	server.Tx(ctx, func(tx server.PgTx) {
		existing := GetProviderBlackholeCheckInTx(ctx, tx, c.ClientId)
		if existing != nil && !existing.CheckedAt.Before(c.CheckedAt) {
			return
		}
		writeProviderBlackholeCheck(ctx, tx, c, server.NowUtc())
	})
}

// Writes a current dark verdict for a provider as of checkedAt under the
// deployment's rules: the run of failures the rule needs, spanning exactly its
// minimum. Fixtures that need a dark provider use this rather than a single
// failed check, which is not dark (connect/GEOMAP.md §11.3).
func Testing_SetProviderBlackholed(ctx context.Context, clientId server.Id, checkedAt time.Time) {
	rules := GetProviderEgressRules()
	firstFailedAt := checkedAt.Add(-rules.DarkMinimumSpan())
	nextDueAt := checkedAt.Add(rules.DarkBackoff(rules.DarkConsecutiveFailures))
	SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
		ClientId:            clientId,
		CheckedAt:           checkedAt,
		OK:                  false,
		Failure:             "all_destinations_failed",
		ConsecutiveFailures: rules.DarkConsecutiveFailures,
		FirstFailedAt:       &firstFailedAt,
		NextDueAt:           &nextDueAt,
	})
}

// GetProviderBlackholeCheck returns the latest check for a provider, or nil
// when it has never been checked. Never checked is not the same as checked-bad,
// so it is a nil result rather than a zero-valued one.
func GetProviderBlackholeCheck(ctx context.Context, clientId server.Id) *ProviderBlackholeCheck {
	var c *ProviderBlackholeCheck
	server.Db(ctx, func(conn server.PgConn) {
		c = getProviderBlackholeCheck(ctx, conn, clientId)
	})
	return c
}

// The same read as GetProviderBlackholeCheck, inside a caller's transaction.
func GetProviderBlackholeCheckInTx(ctx context.Context, tx server.PgTx, clientId server.Id) *ProviderBlackholeCheck {
	return getProviderBlackholeCheck(ctx, tx, clientId)
}

// Reads one provider's row through conn, a connection or a transaction; nil
// when the provider was never checked.
func getProviderBlackholeCheck(
	ctx context.Context,
	conn interface {
		Query(ctx context.Context, sql string, args ...any) (server.PgResult, error)
	},
	clientId server.Id,
) *ProviderBlackholeCheck {
	var c *ProviderBlackholeCheck
	result, err := conn.Query(
		ctx,
		`
		SELECT
			client_id,
			checked_at,
			ok,
			failure,
			consecutive_failures,
			first_failed_at,
			next_due_at,
			update_time
		FROM provider_blackhole_check
		WHERE client_id = $1
		`,
		clientId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			c = &ProviderBlackholeCheck{}
			server.Raise(result.Scan(
				&c.ClientId,
				&c.CheckedAt,
				&c.OK,
				&c.Failure,
				&c.ConsecutiveFailures,
				&c.FirstFailedAt,
				&c.NextDueAt,
				&c.UpdateTime,
			))
		}
	})
	return c
}

// GetAllProviderBlackholedClientIds returns the current-dark set of
// connect/GEOMAP.md §10.3: the providers IsDark holds for now, under the
// deployment's rules.
//
// It returns the dark set rather than a map of every provider, because that is
// what the gate needs and the dark set is the small one: in a healthy fleet
// almost every check passes, so loading only the verdicts keeps this
// proportional to the problem rather than to the population.
//
// A stale check is omitted, which means the provider is NOT treated as
// blackholed. That is the deliberate direction to fail: this signal can only
// ever remove providers from the list, so when its evidence lapses the provider
// falls back to being judged on egress health alone. The alternative -- treating
// "we have not checked recently" as "blackholed" -- would empty the list the
// moment the sweep stalled, which is exactly the failure the count gate's
// fleet-wide floor exists to prevent.
func GetAllProviderBlackholedClientIds(ctx context.Context) map[server.Id]bool {
	blackholed := map[server.Id]bool{}

	rules := GetProviderEgressRules()
	minCheckedAt := server.NowUtc().Add(-ProviderBlackholeCheckMaxAge)

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT client_id
			FROM provider_blackhole_check
			WHERE `+ProviderBlackholeDarkSql("provider_blackhole_check", "$1", rules)+`
			`,
			minCheckedAt.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				blackholed[clientId] = true
			}
		})
	})

	return blackholed
}

// How many providers have a measured check within
// ProviderBlackholeCheckMaxAge: the denominator of the fleet's dark share. A
// row whose only check measured nothing is not among them.
func CountCurrentProviderBlackholeChecks(ctx context.Context) int {
	count := 0
	minCheckedAt := server.NowUtc().Add(-ProviderBlackholeCheckMaxAge)
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT count(*)
			FROM provider_blackhole_check
			WHERE
				$1 <= checked_at AND
				NOT (ok = false AND failure = $2 AND consecutive_failures = 0)
			`,
			minCheckedAt.UTC(),
			ProviderBlackholeNotMeasuredFailure,
		).Scan(&count))
	})
	return count
}

// GetProviderBlackholeCheckDue returns the providers to check next, as of now.
//
// The candidate set is the same "can this provider serve a stranger" predicate
// the egress-location queue uses -- active, top-level, connected, valid, and
// holding a Public provide key. A provider that cannot accept a contract cannot
// be checked, and offering it would burn a slot on a tunnel that will be refused.
//
// Every row whose next_due_at has passed comes before every never-checked
// provider, oldest due first (connect/GEOMAP.md §11.3). A failing provider's
// retry is due a backoff step after its failure, so a failing provider is
// confirmed or cleared on schedule while the prober runs at all: first checks
// of newly connected providers cannot push a retry out of the batch, and a
// retry queue cannot starve. A row written before next_due_at existed is due
// ProviderBlackholeCheckDueAge after its check, as it always was.
//
// There is no attempt backoff here beyond next_due_at. The check is cheap and
// the whole design is that it runs over everything, so a provider that failed
// is re-checked within minutes -- that is how it gets back into the list once
// it recovers. Rate limiting belongs in the sweep's own cadence.
func GetProviderBlackholeCheckDue(
	ctx context.Context,
	now time.Time,
	limit int,
	shardIndex int,
	shardCount int,
) []server.Id {
	clientIds := []server.Id{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			fmt.Sprintf(
				`
				SELECT
					network_client_location_reliability.client_id
				FROM network_client_location_reliability

				INNER JOIN network_client ON
					network_client.client_id = network_client_location_reliability.client_id

				LEFT JOIN provider_blackhole_check ON
					provider_blackhole_check.client_id = network_client_location_reliability.client_id

				WHERE
					network_client.active = true AND
					network_client.source_client_id IS NULL AND
					network_client_location_reliability.connected = true AND
					network_client_location_reliability.valid = true AND
					EXISTS (
						SELECT 1 FROM provide_key
						WHERE
							provide_key.client_id = network_client_location_reliability.client_id AND
							provide_key.provide_mode = $1
					) AND
					(
						provider_blackhole_check.client_id IS NULL OR
						COALESCE(
							provider_blackhole_check.next_due_at,
							provider_blackhole_check.checked_at + interval '%[1]d seconds'
						) <= $2
					) AND
					-- the same shard partition the egress-location queue uses.
					-- hashtext returns a SIGNED int32 and postgres '%%' keeps the sign
					-- of the dividend, so a bare hashtext(...) %% n = i never matches
					-- the negative half of the hash space and roughly half the fleet
					-- would never be checked. The extra (+ n) %% n normalises into
					-- [0, n). $4 <= 1 short-circuits to the unsharded behaviour.
					(
						$4 <= 1 OR
						((hashtext(network_client_location_reliability.client_id::text) %% $4) + $4) %% $4 = $5
					)

				-- every due row before every first check, oldest due first;
				-- client_id breaks the tie so batch composition is deterministic
				-- rather than plan-dependent
				ORDER BY
					provider_blackhole_check.client_id IS NULL ASC,
					COALESCE(
						provider_blackhole_check.next_due_at,
						provider_blackhole_check.checked_at + interval '%[1]d seconds'
					) ASC,
					network_client_location_reliability.client_id ASC
				LIMIT $3
				`,
				int64(ProviderBlackholeCheckDueAge/time.Second),
			),
			ProvideModePublic,
			now.UTC(),
			limit,
			shardCount,
			shardIndex,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})

	return clientIds
}

// RemoveExpiredProviderBlackholeChecks drops checks older than minCheckedAt, so
// the table tracks the live population rather than growing without bound as
// providers come and go.
func RemoveExpiredProviderBlackholeChecks(ctx context.Context, minCheckedAt time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM provider_blackhole_check
			WHERE checked_at < $1
			`,
			minCheckedAt.UTC(),
		))
	})
}
