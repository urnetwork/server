package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The window of the extender supply counts.
const ExtenderSupplyWindow = 24 * time.Hour

// ExtenderSupplyCount is how the extender directory grew and shrank over the
// trailing ExtenderSupplyWindow, for the supply signal (monitor/SIGNALS.md
// §28.5).
//
// A row of network_extender is one identity key. A re-activation of a key
// reuses its row and clears revoke_time, so Revoked counts the extenders that
// lost their last active address in the window and have not activated again
// since: the supply the directory lost, not every revocation. Created counts
// the keys first activated in the window.
//
// From 2026-11-02 to 11-11 the counts were about 11 created and 20 revoked a
// day (factury/research/extender-health-2026-11-11.md §1.1), and about 136
// extenders were revoked in the half day after the uptime probe's false
// failures of 2026-11-01.
type ExtenderSupplyCount struct {
	Created int
	Revoked int
}

// CountExtenderSupply counts the extenders created and revoked in the trailing
// window ending at now. network_extender holds one row per key ever activated,
// a few hundred, so this is one small scan on the replica.
func CountExtenderSupply(ctx context.Context, now time.Time) ExtenderSupplyCount {
	count := ExtenderSupplyCount{}
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				COUNT(*) FILTER (WHERE $1 <= create_time AND create_time <= $2),
				COUNT(*) FILTER (WHERE $1 <= revoke_time AND revoke_time <= $2)
			FROM network_extender
			`,
			now.UTC().Add(-ExtenderSupplyWindow),
			now.UTC(),
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count.Created, &count.Revoked))
			}
		})
	})
	return count
}

// ExtenderConcentration is how much of the active extender pool one network
// holds (sn WHITEPAPER §8.6.3): the networks holding an active extender, and
// what the network holding the most holds, by extenders and by pool units.
//
// A pool unit is the scoring unit of the §8.6 extender pool, which pays per
// unit rather than per extender (§8.6.2): an extender's active ipv4 address's
// /29 when it has one, else its active ipv6 address's /48, so extenders that
// share a block share its unit. On 2026-11-16 two networks on one fork build
// activated about 3,200 extenders in a day and held 3,033 of the 3,370 units,
// the network holding the most 1,803 of them: the §8.6.3 condition for
// coarser units or a weighting by use, one operator's share of units growing.
type ExtenderConcentration struct {
	// networks with an active extender that has an active address
	NetworkCount int64
	// the active extenders with an active address of the network that holds
	// the most
	TopNetworkExtenderCount int64
	// the pool units holding an active extender with an active address
	UnitCount int64
	// the pool units of the network whose extenders hold the most
	TopNetworkUnitCount int64
}

// CountExtenderConcentration reads the concentration of the active extender
// pool. network_extender holds one row per key ever activated, a few
// thousand, and each extender's unit is read by the address primary key, so
// this is one small scan on the replica.
func CountExtenderConcentration(ctx context.Context) ExtenderConcentration {
	concentration := ExtenderConcentration{}
	server.ReplicaDb(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			WITH extender_unit AS (
				SELECT
					network_extender.network_id,
					unit_address.unit
				FROM network_extender
				INNER JOIN LATERAL (
					SELECT
						CASE
							WHEN network_extender_address.ip_version = 4 THEN set_masklen(network_extender_address.ip::cidr, 29)
							ELSE set_masklen(network_extender_address.ip::cidr, 48)
						END AS unit
					FROM network_extender_address
					WHERE
						network_extender_address.extender_id = network_extender.extender_id AND
						network_extender_address.active
					ORDER BY network_extender_address.ip_version
					LIMIT 1
				) AS unit_address ON true
				WHERE network_extender.active
			),
			network_holding AS (
				SELECT
					network_id,
					COUNT(*) AS extender_count,
					COUNT(DISTINCT unit) AS unit_count
				FROM extender_unit
				GROUP BY network_id
			)
			SELECT
				(SELECT COUNT(*) FROM network_holding),
				COALESCE((SELECT MAX(extender_count) FROM network_holding), 0),
				(SELECT COUNT(DISTINCT unit) FROM extender_unit),
				COALESCE((SELECT MAX(unit_count) FROM network_holding), 0)
			`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&concentration.NetworkCount,
					&concentration.TopNetworkExtenderCount,
					&concentration.UnitCount,
					&concentration.TopNetworkUnitCount,
				))
			}
		})
	})
	return concentration
}
