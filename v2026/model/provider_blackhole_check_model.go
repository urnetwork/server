package model

import (
	"context"
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

// ProviderBlackholeCheckDueAge is how old a check must be before the provider
// is offered up again. Half the max age, for the same reason
// providerEgressDueAge is half ProviderEgressLocationMaxAge: the sweep gets a
// full window to refresh a check before it expires.
const ProviderBlackholeCheckDueAge = ProviderBlackholeCheckMaxAge / 2

// MaxProviderBlackholeFailureLen mirrors the failure column width. A submission
// longer than this is rejected rather than truncated, so a caller learns its
// class name does not fit instead of having it silently mangled.
const MaxProviderBlackholeFailureLen = 64

// ProviderBlackholeCheck is the answer to one question, asked of one provider:
// did ANY traffic get through.
//
// It is not a measurement of quality and must not be treated as one. Egress
// health samples ~131 destinations and can say "this provider resolves names
// and carries bytes but is refused by content providers"; this says only
// "something got through" or "nothing did". The two are kept in separate tables
// because they answer different questions on different cadences, and folding
// this into provider_egress_health would overwrite a rich per-class measurement
// with a two-destination sample every hour.
type ProviderBlackholeCheck struct {
	ClientId  server.Id
	CheckedAt time.Time
	OK        bool
	// Failure is "" when OK, otherwise a short class such as tunnel_failed or
	// all_destinations_failed.
	Failure    string
	UpdateTime time.Time
}

// SetProviderBlackholeCheck upserts the latest check for a provider.
//
// The upsert is monotonic in checked_at, like SetProviderEgressLocation and
// SetProviderEgressProbeAttempt: a replayed or out-of-order report older than
// what is stored is dropped rather than moving the provider's last-checked time
// backwards, which would hand it straight back to the sweep.
func SetProviderBlackholeCheck(ctx context.Context, c *ProviderBlackholeCheck) {
	failure := c.Failure
	if c.OK {
		// a successful check has nothing to explain, and storing a failure class
		// beside ok=true would leave two contradictory answers in one row
		failure = ""
	}

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO provider_blackhole_check (
				client_id,
				checked_at,
				ok,
				failure,
				update_time
			)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (client_id) DO UPDATE
			SET
				checked_at = $2,
				ok = $3,
				failure = $4,
				update_time = $5
			WHERE provider_blackhole_check.checked_at < EXCLUDED.checked_at
			`,
			c.ClientId,
			c.CheckedAt.UTC(),
			c.OK,
			failure,
			server.NowUtc(),
		))
	})
}

// GetProviderBlackholeCheck returns the latest check for a provider, or nil
// when it has never been checked. Never checked is not the same as checked-bad,
// so it is a nil result rather than a zero-valued one.
func GetProviderBlackholeCheck(ctx context.Context, clientId server.Id) *ProviderBlackholeCheck {
	var c *ProviderBlackholeCheck
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT client_id, checked_at, ok, failure, update_time
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
					&c.UpdateTime,
				))
			}
		})
	})
	return c
}

// GetAllProviderBlackholedClientIds returns the providers whose most recent
// check says nothing got through, bounded to checks still considered current.
//
// It returns the FAILING set rather than a map of every provider, because that
// is what the gate needs and the failing set is the small one: in a healthy
// fleet almost every check passes, so loading only the failures keeps this
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

	minCheckedAt := server.NowUtc().Add(-ProviderBlackholeCheckMaxAge)

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT client_id
			FROM provider_blackhole_check
			WHERE
				ok = false AND
				checked_at >= $1
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

// GetProviderBlackholeCheckDue returns the providers to check next: those never
// checked, then those checked least recently, oldest first.
//
// The candidate set is the same "can this provider serve a stranger" predicate
// the egress-location queue uses -- connected, valid, and holding a Public
// provide key. A provider that cannot accept a contract cannot be checked, and
// offering it would burn a slot on a tunnel that will be refused.
//
// Unlike the egress-location queue this has no attempt backoff. The check is
// cheap and the whole design is that it runs hourly over everything, so a
// provider that failed last hour must be re-checked this hour -- that is how it
// gets back into the list once it recovers. Rate limiting belongs in the
// sweep's own cadence, not in a per-provider deferral here.
func GetProviderBlackholeCheckDue(
	ctx context.Context,
	minCheckedAt time.Time,
	limit int,
	shardIndex int,
	shardCount int,
) []server.Id {
	clientIds := []server.Id{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_client_location_reliability.client_id
			FROM network_client_location_reliability

			LEFT JOIN provider_blackhole_check ON
				provider_blackhole_check.client_id = network_client_location_reliability.client_id

			WHERE
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
					provider_blackhole_check.checked_at < $2
				) AND
				-- the same shard partition the egress-location queue uses.
				-- hashtext returns a SIGNED int32 and postgres '%' keeps the sign
				-- of the dividend, so a bare hashtext(...) % n = i never matches
				-- the negative half of the hash space and roughly half the fleet
				-- would never be checked. The extra (+ n) % n normalises into
				-- [0, n). $4 <= 1 short-circuits to the unsharded behaviour.
				(
					$4 <= 1 OR
					((hashtext(network_client_location_reliability.client_id::text) % $4) + $4) % $4 = $5
				)

			-- never checked first (NULL sorts first here), then least recently
			-- checked; client_id breaks the tie so batch composition is
			-- deterministic rather than plan-dependent
			ORDER BY
				provider_blackhole_check.checked_at ASC NULLS FIRST,
				network_client_location_reliability.client_id ASC
			LIMIT $3
			`,
			ProvideModePublic,
			minCheckedAt.UTC(),
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
