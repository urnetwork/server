// Bounded independent queue heads retain failed-provider recovery and admit
// first checks without ranking or materializing the entire eligible fleet.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Each head repeats the exact active/top-level/connected/valid/Public/shard
// predicate. The retry head includes NotMeasured-only and legacy rows, and
// preserves next_due_at or checked_at+90m. The first head uses a primary-key
// anti-join, not a new absence/history schema. Only <=limit rows per head
// leave PostgreSQL; the stable work-conserving merge returns <=limit total.
// Separate read-committed statements may see category movement, so dedup the
// bounded heads instead of assuming they are disjoint across snapshots.
func getProviderBlackholeCheckDueWithQuery(
	ctx context.Context,
	query server.PgCanQuery,
	now time.Time,
	limit, shardIndex, shardCount int,
) []server.Id {
	read := func(sql string, args ...any) []server.Id {
		server.Raise(ctx.Err())
		ids := []server.Id{}
		result, err := query.Query(ctx, sql, args...)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var id server.Id
				server.Raise(result.Scan(&id))
				ids = append(ids, id)
			}
		})
		return ids
	}
	checked := read(fmt.Sprintf(`
		SELECT pbc.client_id
		FROM provider_blackhole_check pbc
		INNER JOIN network_client_location_reliability nclr ON nclr.client_id = pbc.client_id
		INNER JOIN network_client nc ON nc.client_id = pbc.client_id
		WHERE nc.active AND nc.source_client_id IS NULL AND nclr.connected AND nclr.valid
		  AND EXISTS (SELECT 1 FROM provide_key pk WHERE pk.client_id = pbc.client_id AND pk.provide_mode = $1)
		  AND COALESCE(pbc.next_due_at, pbc.checked_at + interval '%d seconds') <= $2
		  AND ($4 <= 1 OR ((hashtext(pbc.client_id::text) %% $4) + $4) %% $4 = $5)
		ORDER BY COALESCE(pbc.next_due_at, pbc.checked_at + interval '%d seconds'), pbc.client_id
		LIMIT $3
	`, int64(ProviderBlackholeCheckDueAge/time.Second), int64(ProviderBlackholeCheckDueAge/time.Second)),
		ProvideModePublic, now.UTC(), limit, shardCount, shardIndex)
	// Preserve retry priority for a one-slot caller and avoid an unused read.
	if limit == 1 && len(checked) == 1 {
		return checked
	}
	first := read(`
		SELECT nclr.client_id
		FROM network_client_location_reliability nclr
		INNER JOIN network_client nc ON nc.client_id = nclr.client_id
		WHERE nc.active AND nc.source_client_id IS NULL AND nclr.connected AND nclr.valid
		  AND EXISTS (SELECT 1 FROM provide_key pk WHERE pk.client_id = nclr.client_id AND pk.provide_mode = $1)
		  AND NOT EXISTS (SELECT 1 FROM provider_blackhole_check pbc WHERE pbc.client_id = nclr.client_id)
		  AND ($3 <= 1 OR ((hashtext(nclr.client_id::text) % $3) + $3) % $3 = $4)
		ORDER BY nclr.client_id
		LIMIT $2
	`, ProvideModePublic, limit, shardCount, shardIndex)
	server.Raise(ctx.Err())
	clientIds := []server.Id{}
	seen := map[server.Id]bool{}
	heads := [2][]server.Id{checked, first}
	for len(clientIds) < limit && (len(heads[0]) != 0 || len(heads[1]) != 0) {
		for lane := range heads {
			for len(heads[lane]) != 0 && len(clientIds) < limit {
				id := heads[lane][0]
				heads[lane] = heads[lane][1:]
				if !seen[id] {
					seen[id] = true
					clientIds = append(clientIds, id)
					break
				}
			}
		}
	}
	return clientIds
}
