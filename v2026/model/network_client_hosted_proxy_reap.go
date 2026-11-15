// Removed and expired hosted proxies are hard deleted two days after their
// deactivation, instead of waiting out the 30-day inactive reap
// (NetworkClientReapAfterDeactivate) with every other client.
//
// A hosted proxy is a client with a proxy_device_config. Nothing reactivates a
// deactivated client, so once a proxy is removed (by its owner, a trim or the
// descendant cascade) or marked idle (TopLevelClientIdleExpiration), its token
// is dead for good. Its proxy_client row, though, lives until the client row
// is hard deleted (removeProxyClientData), and every proxy instance restores
// all proxy_client rows of its (host, block) as WireGuard clients at startup
// and reads them all again on every reconcile (the proxy's ReconcileTimeout).
// On Main on 2026-11-05, 92% of the 3.79M proxy_device_config rows belonged to
// inactive clients (about 120k a day deactivated, each held 30 days), and
// about 93% of the 823k WireGuard clients configured on the proxy hosts were
// theirs.
//
// The idle window is unchanged: an unused proxy is still marked inactive only
// after TopLevelClientIdleExpiration, so a proxy used every few weeks keeps
// working. Only the time a dead proxy's rows linger after that changes.
//
// The reap walks proxy_device_config in proxy_id order with a cursor, so it
// examines hosted proxies only, each at most once a pass, and a pass that
// reaches the end waits PassInterval before the next one starts.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

type HostedProxyReapSettings struct {
	// How long a deactivated hosted proxy keeps its rows. It must exceed the
	// retired window reap (RetiredHostedNetworkClientReapSettings
	// ReapAfterDeactivate): that reap recognizes a window as hosted by its
	// parent's proxy_device_config, so the proxy's windows must go first.
	ReapAfterDeactivate time.Duration
	// proxy_device_config rows examined in one transaction
	PageCount int
	// at most this many proxies deleted in one run, which paces a backlog
	MaxRemoveCount int
	// a run examines pages for this long, then returns with more to do
	RunBudget time.Duration
	// a run that left work behind continues this much later
	ContinueInterval time.Duration
	// after a pass reaches the end of proxy_device_config, the next pass
	// starts this much later
	PassInterval time.Duration
}

func DefaultHostedProxyReapSettings() *HostedProxyReapSettings {
	return &HostedProxyReapSettings{
		ReapAfterDeactivate: 48 * time.Hour,
		PageCount:           5000,
		MaxRemoveCount:      5000,
		RunBudget:           2 * time.Minute,
		ContinueInterval:    1 * time.Minute,
		PassInterval:        1 * time.Hour,
	}
}

// Where the reap resumes: the last proxy_device_config proxy_id it examined.
// The zero cursor starts a pass at the beginning.
type HostedProxyReapCursor struct {
	ProxyId server.Id `json:"proxy_id"`
}

// One page of hosted proxies past the cursor, each with its client and whether
// the reap may delete that client now. A config whose client row is gone is
// examined and skipped; the orphan sweep owns it.
const hostedProxyReapPageSql = `
	SELECT
		proxy_device_config.proxy_id,
		network_client.client_id,
		COALESCE(
			NOT network_client.active AND
			COALESCE(network_client.deactivate_time, network_client.create_time) < $2 AND
			NOT EXISTS (
				SELECT 1 FROM network_client_connection
				WHERE
					network_client_connection.client_id = network_client.client_id AND
					network_client_connection.connected
			),
			false
		)
	FROM proxy_device_config
	LEFT JOIN network_client ON network_client.client_id = proxy_device_config.client_id
	WHERE proxy_device_config.proxy_id > $1
	ORDER BY proxy_device_config.proxy_id
	LIMIT $3
`

// Deletes the page's reapable clients that are still inactive and deactivated
// before the horizon. A row another transaction holds is skipped rather than
// waited for, and stays for the next pass.
const removeHostedProxyClientsSql = `
	DELETE FROM network_client
	WHERE network_client.client_id IN (
		SELECT locked.client_id
		FROM network_client AS locked
		WHERE
			locked.client_id = ANY($1) AND
			NOT locked.active AND
			COALESCE(locked.deactivate_time, locked.create_time) < $2 AND
			NOT EXISTS (
				SELECT 1 FROM network_client_connection
				WHERE
					network_client_connection.client_id = locked.client_id AND
					network_client_connection.connected
			)
		FOR UPDATE SKIP LOCKED
	)
	RETURNING network_client.client_id, network_client.device_id
`

// Hard deletes the hosted proxies deactivated more than ReapAfterDeactivate
// before now, one short transaction per page, with their dependents after each
// commit (removeReapedNetworkClientDependents: proxy_device_config,
// proxy_client and its change rows, provide keys, certificates, the device
// once nothing else references it, and Redis state). A run stops at the end of
// proxy_device_config (passComplete, with the zero cursor), at MaxRemoveCount
// deletions or at the run budget (more).
func RemoveRetiredHostedProxyClients(
	ctx context.Context,
	now time.Time,
	cursor HostedProxyReapCursor,
	settings *HostedProxyReapSettings,
) (removedCount int64, next HostedProxyReapCursor, passComplete bool, more bool) {
	next = cursor
	horizon := now.Add(-settings.ReapAfterDeactivate).UTC()
	deadline := time.Now().Add(settings.RunBudget)
	for {
		select {
		case <-ctx.Done():
			return removedCount, next, false, true
		default:
		}
		// each examined row deletes at most one client, so a page no larger
		// than the remaining allowance never passes MaxRemoveCount
		pageCount := min(settings.PageCount, settings.MaxRemoveCount-int(removedCount))
		if pageCount <= 0 {
			return removedCount, next, false, true
		}
		var examinedCount int
		var pageNext HostedProxyReapCursor
		var batchClientIds []server.Id
		var batchDeviceIds []server.Id
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// an attempt that does not commit owns no cleanup and moves no cursor
			examinedCount = 0
			pageNext = next
			batchClientIds, batchDeviceIds = nil, nil

			reapableClientIds := []server.Id{}
			result, err := tx.Query(
				ctx,
				hostedProxyReapPageSql,
				next.ProxyId,
				horizon,
				pageCount,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var proxyId server.Id
					var clientId *server.Id
					var reapable bool
					server.Raise(result.Scan(&proxyId, &clientId, &reapable))
					examinedCount += 1
					pageNext = HostedProxyReapCursor{ProxyId: proxyId}
					if reapable && clientId != nil {
						reapableClientIds = append(reapableClientIds, *clientId)
					}
				}
			})
			if len(reapableClientIds) == 0 {
				return
			}
			result, err = tx.Query(ctx, removeHostedProxyClientsSql, reapableClientIds, horizon)
			deviceIds := map[server.Id]bool{}
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					var deviceId *server.Id
					server.Raise(result.Scan(&clientId, &deviceId))
					batchClientIds = append(batchClientIds, clientId)
					if deviceId != nil && !deviceIds[*deviceId] {
						deviceIds[*deviceId] = true
						batchDeviceIds = append(batchDeviceIds, *deviceId)
					}
				}
			})
		}, server.TxReadCommitted)
		removeReapedNetworkClientDependents(ctx, batchClientIds, batchDeviceIds)
		removedCount += int64(len(batchClientIds))
		if examinedCount < pageCount {
			// the page ran off the end of proxy_device_config
			return removedCount, HostedProxyReapCursor{}, true, false
		}
		next = pageNext
		if !time.Now().Before(deadline) {
			return removedCount, next, false, true
		}
	}
}

// When the reap's next run starts: soon while a run left work behind, a pass
// interval after a pass ends.
func HostedProxyReapNextRunAt(now time.Time, passComplete bool, settings *HostedProxyReapSettings) time.Time {
	if passComplete {
		return now.Add(settings.PassInterval)
	}
	return now.Add(settings.ContinueInterval)
}
