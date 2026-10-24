// Retired hosted identities are hard deleted a day after their deactivation
// (PARTITION-MAINTENANCE-PLAN.md Part D). A hosted proxy device mints one
// child client per candidate exit through its local authority
// (localclient.Authority, AuthNetworkClientFromParent), always under its own
// client, and retires it with RemoveNetworkClient. So a hosted identity is a
// child whose `source_client_id` is a client with a `proxy_device_config`, the
// same test as the identity churn signal (monitor/signal_identity_churn.go).
//
// Other children keep the 30-day reap with top-level clients: a child minted
// with a client token may parent its own children, whose ancestry walk
// (session.ResolveClientRootInTx) reads it, and may earn as a provider, whose
// epoch reliability joins its row (GetStEpochClientReliability). A hosted
// window has neither: its authority mints only for the proxy, and it is a
// payer behind the proxy host's shared egress. Its contracts carry their own
// network ids, so close, settlement and payout never read its row.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

type RetiredHostedNetworkClientReapSettings struct {
	// A retired window is a contract endpoint for at most its contract
	// lifespan and the close linger, about 75 minutes. Usage metering reads
	// its row about two minutes after settlement. A day leaves both far
	// behind.
	ReapAfterDeactivate time.Duration
	// retired children examined in one transaction
	BatchCount int
	// a run examines batches for this long, then returns with more to do
	RunBudget time.Duration
}

func DefaultRetiredHostedNetworkClientReapSettings() *RetiredHostedNetworkClientReapSettings {
	return &RetiredHostedNetworkClientReapSettings{
		ReapAfterDeactivate: 24 * time.Hour,
		BatchCount:          5000,
		RunBudget:           2 * time.Minute,
	}
}

// Where the reap resumes: the last retired child it examined, in the order of
// network_client_retired_child_deactivate_time. Every child is examined once.
// One it could not delete, because it is not hosted, still holds a connected
// row, or another transaction held its lock, stays for the 30-day reap.
type RetiredHostedNetworkClientCursor struct {
	DeactivateTime time.Time `json:"deactivate_time"`
	ClientId       server.Id `json:"client_id"`
}

// One page of retired children past the cursor and before the horizon, each
// with whether the reap may delete it now.
const retiredHostedNetworkClientPageSql = `
	SELECT
		network_client.client_id,
		network_client.deactivate_time,
		EXISTS (
			SELECT 1 FROM proxy_device_config
			WHERE proxy_device_config.client_id = network_client.source_client_id
		) AND NOT EXISTS (
			SELECT 1 FROM network_client_connection
			WHERE
				network_client_connection.client_id = network_client.client_id AND
				network_client_connection.connected
		)
	FROM network_client
	WHERE
		NOT network_client.active AND
		network_client.source_client_id IS NOT NULL AND
		(network_client.deactivate_time, network_client.client_id) > ($1, $2) AND
		network_client.deactivate_time < $3
	ORDER BY network_client.deactivate_time, network_client.client_id
	LIMIT $4
`

// Deletes the page's reapable rows that are still retired before the
// horizon. A row another transaction holds is skipped rather than waited for.
const removeRetiredHostedNetworkClientsSql = `
	DELETE FROM network_client
	WHERE network_client.client_id IN (
		SELECT locked.client_id
		FROM network_client AS locked
		WHERE
			locked.client_id = ANY($1) AND
			NOT locked.active AND
			locked.deactivate_time < $2
		FOR UPDATE SKIP LOCKED
	)
	RETURNING network_client.client_id, network_client.device_id
`

// Hard deletes the hosted children deactivated more than ReapAfterDeactivate
// before now, one short transaction per page, until the reap reaches the
// horizon or the run budget is spent. Each page's dependents go after its
// commit (removeReapedNetworkClientDependents), so a run that stops early
// leaves nothing half removed. more is true when the budget ended the run.
func RemoveRetiredHostedNetworkClients(
	ctx context.Context,
	now time.Time,
	cursor RetiredHostedNetworkClientCursor,
	settings *RetiredHostedNetworkClientReapSettings,
) (removedCount int64, next RetiredHostedNetworkClientCursor, more bool) {
	next = cursor
	horizon := now.Add(-settings.ReapAfterDeactivate).UTC()
	deadline := time.Now().Add(settings.RunBudget)
	for {
		select {
		case <-ctx.Done():
			return removedCount, next, true
		default:
		}
		var examinedCount int
		var pageNext RetiredHostedNetworkClientCursor
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
				retiredHostedNetworkClientPageSql,
				next.DeactivateTime.UTC(),
				next.ClientId,
				horizon,
				settings.BatchCount,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					var deactivateTime time.Time
					var reapable bool
					server.Raise(result.Scan(&clientId, &deactivateTime, &reapable))
					examinedCount += 1
					pageNext = RetiredHostedNetworkClientCursor{
						DeactivateTime: deactivateTime,
						ClientId:       clientId,
					}
					if reapable {
						reapableClientIds = append(reapableClientIds, clientId)
					}
				}
			})
			if len(reapableClientIds) == 0 {
				return
			}
			result, err = tx.Query(ctx, removeRetiredHostedNetworkClientsSql, reapableClientIds, horizon)
			// the windows of one proxy share its device
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
		next = pageNext
		removeReapedNetworkClientDependents(ctx, batchClientIds, batchDeviceIds)
		removedCount += int64(len(batchClientIds))
		if examinedCount < settings.BatchCount {
			return removedCount, next, false
		}
		if !time.Now().Before(deadline) {
			return removedCount, next, true
		}
	}
}
