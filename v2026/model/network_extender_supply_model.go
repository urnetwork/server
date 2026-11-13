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
