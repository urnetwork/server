// Orphan repair takes one endpoint fence for a bounded primary-key page. The
// minute task rediscovers deferred rows; foreground owners never wait for a
// cleanup that is itself waiting for another endpoint or connection row.
//
// Handler expiry deletes a handler only on evidence that its process ended,
// not merely that its heartbeat stopped. A heartbeat is a database write, so
// a failing database path stops the heartbeats of every live process at once:
// on 2026-10-19 a pooler livelock held them off for three hours, expiry, which
// still got through, deleted all 22 live handlers and retired their rows, and
// each connect process then shut itself down on its next heartbeat, dropping
// 74k providers. Every connect process also stamps its handler in the provider
// liveness writer set in Redis every few seconds (provider_liveness_lease.go),
// and the stamps kept coming throughout. So a stale handler whose process
// still has a current stamp is kept, and one without is deleted. When the set
// cannot vouch, because the read failed or no process stamped within the
// window, the fleet decides: stale handlers are deleted only while at least as
// many handlers still heartbeat, which holds for a dead process, a lost host
// and a restarted fleet, but not for a heartbeat path that failed for all.
// Deferral keeps a handler's rows connected; its heartbeat or the next round
// settles it.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

const networkClientHandlerRetirementPageSize = 64

// The most stale handlers one expiry round judges. A fleet has a few dozen;
// the rest wait for the next round.
const networkClientHandlerExpiryCandidateLimit = 1024

// The budget of expiry's read of the writer set. The round runs once a minute
// off the request path, so it can wait longer than a FindProviders2 read.
const networkClientHandlerStampReadTimeout = 2 * time.Second

// Stale handlers by expiry's verdict, as its metric labels them.
const (
	// deleted, with their connection rows retired
	networkClientHandlerExpiryExpired = "expired"
	// kept: the process still stamps the writer set
	networkClientHandlerExpiryDeferredStamped = "deferred_stamped"
	// kept: the writer set could not vouch, and fewer handlers heartbeat than
	// went stale
	networkClientHandlerExpiryDeferredFleet = "deferred_fleet"
)

var networkClientHandlerExpiryCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_network_client_handler_expiry_total",
	Help: "Connect handlers whose heartbeat was older than the expiry cutoff, by verdict: expired, deleted with their connection rows retired; deferred_stamped, kept because the process still stamps the provider liveness writer set; deferred_fleet, kept because the set could not vouch and fewer handlers heartbeat than went stale",
}, []string{"verdict"})

func init() {
	prometheus.MustRegister(networkClientHandlerExpiryCounter)
	for _, verdict := range []string{
		networkClientHandlerExpiryExpired,
		networkClientHandlerExpiryDeferredStamped,
		networkClientHandlerExpiryDeferredFleet,
	} {
		networkClientHandlerExpiryCounter.WithLabelValues(verdict)
	}
}

// The first 4096 clients, in order, that have an open row without a handler
// row: the result of SELECT DISTINCT client_id ... WHERE c.connected AND NOT
// EXISTS(handler) ORDER BY client_id LIMIT 4096, which read every open row
// because orphans rarely fill the limit. This skips from one handler id to the
// next on network_client_connection_connected_handler_id (migration 820),
// probes each handler row once, and reads only the open rows of absent
// handlers and of a NULL handler id, which NOT EXISTS also counts as orphaned.
// The materialized orphan set and OFFSET 0 keep each absent handler's rows a
// separate indexed scan, never a join over every open row.
const networkClientOrphanClientSql = `WITH RECURSIVE connected_handler(handler_id) AS (
 (SELECT c.handler_id FROM network_client_connection c
  WHERE c.connected AND c.handler_id IS NOT NULL ORDER BY c.handler_id LIMIT 1)
 UNION ALL
 SELECT (SELECT c.handler_id FROM network_client_connection c
  WHERE c.connected AND c.handler_id>connected_handler.handler_id ORDER BY c.handler_id LIMIT 1)
 FROM connected_handler WHERE connected_handler.handler_id IS NOT NULL
), orphan_handler AS MATERIALIZED (
 SELECT connected_handler.handler_id FROM connected_handler
 WHERE connected_handler.handler_id IS NOT NULL
 AND NOT EXISTS(SELECT 1 FROM network_client_handler h WHERE h.handler_id=connected_handler.handler_id)
)
SELECT DISTINCT orphan.client_id FROM (
 SELECT owned.client_id FROM orphan_handler CROSS JOIN LATERAL (
  SELECT c.client_id FROM network_client_connection c
  WHERE c.connected AND c.handler_id=orphan_handler.handler_id OFFSET 0
 ) AS owned
 UNION ALL
 SELECT c.client_id FROM network_client_connection c WHERE c.connected AND c.handler_id IS NULL
) AS orphan
ORDER BY orphan.client_id LIMIT 4096`

// Discovery may inspect an endpoint's connected history, but holds no endpoint
// fence. Every candidate is checked again after its row is exclusively owned.
const networkClientOrphanConnectionPageSql = `SELECT c.connection_id
 FROM network_client_connection c
 WHERE c.client_id=$1 AND c.connected
 AND NOT EXISTS(SELECT 1 FROM network_client_handler h WHERE h.handler_id=c.handler_id)
 ORDER BY c.connection_id LIMIT $2`

// A rolling connection owner may already hold a row before reaching its fence.
// Skipping that row avoids the endpoint-to-row inversion without a 40001 retry.
const networkClientOrphanConnectionLockSql = `SELECT owned.ctid::text
 FROM unnest($1::uuid[]) AS candidate(connection_id)
 CROSS JOIN LATERAL (
  SELECT c.ctid,c.client_id,c.connected FROM network_client_connection c
  WHERE c.connection_id=candidate.connection_id
  LIMIT 1 OFFSET 0 FOR UPDATE OF c SKIP LOCKED
 ) AS owned
 WHERE owned.client_id=$2 AND owned.connected`

// The preceding primary-key locks already checked client and connected state;
// those values cannot change until this transaction ends. Repeating them here
// lets a generic plan choose a client index instead of the bounded TID array.
// Only the handler's independent lifecycle needs a fresh membership check.
const networkClientOrphanConnectionRetireSql = `UPDATE network_client_connection c
 SET connected=false,disconnect_time=$1
 WHERE c.ctid=ANY($2::text[]::tid[])
 AND NOT EXISTS(SELECT 1 FROM network_client_handler h WHERE h.handler_id=c.handler_id LIMIT 1 OFFSET 0)`

// Expire the handlers that ended (see the file header), then discover orphans
// without endpoint fences and retire one bounded client page per transaction.
// Busy endpoints and excess rows remain eligible next minute.
func CloseExpiredNetworkClientHandlers(ctx context.Context, minTime time.Time) {
	ctx = providerWorkSessionContext(ctx)
	disconnectTime := server.NowUtc()
	expireNetworkClientHandlers(ctx, minTime, readNetworkClientHandlerStamps)

	// Handler deletion is not a complete orphan index: a connect may race that
	// deletion, and old processes may have left connections without any handler.
	var clientIds []server.Id
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		clientIds = nil
		rows, err := tx.Query(ctx, networkClientOrphanClientSql)
		server.Raise(err)
		defer rows.Close()
		for rows.Next() {
			var clientId server.Id
			server.Raise(rows.Scan(&clientId))
			clientIds = append(clientIds, clientId)
		}
		server.Raise(rows.Err())
	}, server.TxReadCommitted)
	for _, clientId := range clientIds {
		var connectionIds []server.Id
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			connectionIds = nil
			rows, err := tx.Query(ctx, networkClientOrphanConnectionPageSql, clientId, networkClientHandlerRetirementPageSize)
			server.Raise(err)
			defer rows.Close()
			for rows.Next() {
				var connectionId server.Id
				server.Raise(rows.Scan(&connectionId))
				connectionIds = append(connectionIds, connectionId)
			}
			server.Raise(rows.Err())
		}, server.TxReadCommitted)
		retireNetworkClientHandlerPage(ctx, clientId, connectionIds, disconnectTime)
	}
}

// A busy fence is a successful deferral, never permission to run unfenced.
// Connection state and available original receipts commit in the same page.
func retireNetworkClientHandlerPage(ctx context.Context, clientId server.Id, connectionIds []server.Id, disconnectTime time.Time) {
	if len(connectionIds) == 0 {
		return
	}
	if len(connectionIds) > networkClientHandlerRetirementPageSize {
		panic("handler retirement page exceeds its connection bound")
	}
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		retireNetworkClientHandlerPageInTx(ctx, tx, clientId, connectionIds, disconnectTime)
	}, server.TxReadCommitted, server.OptNoRetry())
}

// The caller owns the bounded page and the no-retry transaction. Journal
// objects are migration prerequisites; their SQL failures abort this owner.
func retireNetworkClientHandlerPageInTx(ctx context.Context, tx server.PgTx, clientId server.Id, connectionIds []server.Id, disconnectTime time.Time) {
	server.Raise(ctx.Err())
	var acquired bool
	err := tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock_shared(-776::bigint)`).Scan(&acquired)
	server.Raise(errors.Join(err, ctx.Err()))
	if !acquired {
		return
	}
	err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(776,('x'||substr(md5($1::uuid::text),1,8))::bit(32)::int)`, clientId).Scan(&acquired)
	server.Raise(errors.Join(err, ctx.Err()))
	if !acquired {
		return
	}
	var busy bool
	err = tx.QueryRow(ctx, `WITH owned AS (
		 SELECT client_id FROM provider_work_session_head WHERE client_id=$1 FOR UPDATE SKIP LOCKED)
		 SELECT EXISTS(SELECT 1 FROM provider_work_session_head WHERE client_id=$1)
		 AND NOT EXISTS(SELECT 1 FROM owned)`, clientId).Scan(&busy)
	server.Raise(errors.Join(err, ctx.Err()))
	if busy {
		return
	}
	_, err = tx.Exec(ctx, `SELECT set_config('urnetwork.provider_work_cooperating','1',true)`)
	server.Raise(errors.Join(err, ctx.Err()))
	var ownedAddresses []string
	rows, err := tx.Query(ctx, networkClientOrphanConnectionLockSql, connectionIds, clientId)
	server.Raise(err)
	defer rows.Close()
	for rows.Next() {
		var address string
		server.Raise(rows.Scan(&address))
		ownedAddresses = append(ownedAddresses, address)
	}
	rowErr := rows.Err()
	rows.Close()
	server.Raise(rowErr)
	if len(ownedAddresses) == 0 {
		return
	}
	server.RaisePgResult(tx.Exec(ctx, networkClientOrphanConnectionRetireSql, disconnectTime, ownedAddresses))
	providerWorkRetainSessionEventsInTx(ctx, tx, clientId)
}

// Deletes the stale handlers judged ended (see the file header) and counts every
// stale handler by verdict. readStamps says, for each handler, whether its
// process stamped the writer set within the window, and whether the set could
// vouch at all.
func expireNetworkClientHandlers(
	ctx context.Context,
	minTime time.Time,
	readStamps func(ctx context.Context, handlerIds []server.Id) (current []bool, ok bool),
) {
	var staleHandlerIds []server.Id
	var freshCount int
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		staleHandlerIds = nil
		rows, err := tx.Query(
			ctx,
			`SELECT handler_id FROM network_client_handler WHERE heartbeat_time < $1 ORDER BY heartbeat_time, handler_id LIMIT $2`,
			minTime.UTC(),
			networkClientHandlerExpiryCandidateLimit,
		)
		server.Raise(err)
		defer rows.Close()
		for rows.Next() {
			var handlerId server.Id
			server.Raise(rows.Scan(&handlerId))
			staleHandlerIds = append(staleHandlerIds, handlerId)
		}
		rowErr := rows.Err()
		rows.Close()
		server.Raise(rowErr)
		server.Raise(tx.QueryRow(
			ctx,
			`SELECT count(*) FROM network_client_handler WHERE heartbeat_time >= $1`,
			minTime.UTC(),
		).Scan(&freshCount))
	}, server.TxReadCommitted)
	if len(staleHandlerIds) == 0 {
		return
	}

	// no database connection is held across the Redis read
	stampsCurrent, stampsOk := readStamps(ctx, staleHandlerIds)
	expiredHandlerIds, deferredStampedCount, deferredFleetCount := selectExpiredNetworkClientHandlers(
		staleHandlerIds,
		freshCount,
		stampsCurrent,
		stampsOk,
	)
	expiredCount := 0
	if 0 < len(expiredHandlerIds) {
		server.MaintenanceTx(ctx, func(tx server.PgTx) {
			// a handler that heartbeat since the read is no longer stale and stays
			tag := server.RaisePgResult(tx.Exec(
				ctx,
				`DELETE FROM network_client_handler WHERE handler_id = ANY($1::uuid[]) AND heartbeat_time < $2`,
				expiredHandlerIds,
				minTime.UTC(),
			))
			expiredCount = int(tag.RowsAffected())
		}, server.TxReadCommitted)
	}
	networkClientHandlerExpiryCounter.WithLabelValues(networkClientHandlerExpiryExpired).Add(float64(expiredCount))
	networkClientHandlerExpiryCounter.WithLabelValues(networkClientHandlerExpiryDeferredStamped).Add(float64(deferredStampedCount))
	networkClientHandlerExpiryCounter.WithLabelValues(networkClientHandlerExpiryDeferredFleet).Add(float64(deferredFleetCount))
	if 0 < deferredStampedCount+deferredFleetCount {
		glog.Infof(
			"[nch]handler expiry kept %d of %d stale handlers (%d still stamp liveness, %d without fleet evidence, %d heartbeat)\n",
			deferredStampedCount+deferredFleetCount,
			len(staleHandlerIds),
			deferredStampedCount,
			deferredFleetCount,
			freshCount,
		)
	}
}

// The stale handlers to delete, and how many are kept for each reason. While the
// writer set can vouch (stampsOk), a handler is kept if its process still
// stamps (stampsCurrent, in staleHandlerIds order) and deleted if not.
// Otherwise every stale handler is deleted while at least as many handlers
// still heartbeat (freshCount), and every one is kept while fewer do.
func selectExpiredNetworkClientHandlers(
	staleHandlerIds []server.Id,
	freshCount int,
	stampsCurrent []bool,
	stampsOk bool,
) (expiredHandlerIds []server.Id, deferredStampedCount int, deferredFleetCount int) {
	if stampsOk && len(stampsCurrent) == len(staleHandlerIds) {
		for i, handlerId := range staleHandlerIds {
			if stampsCurrent[i] {
				deferredStampedCount += 1
			} else {
				expiredHandlerIds = append(expiredHandlerIds, handlerId)
			}
		}
		return
	}
	if 0 < freshCount && len(staleHandlerIds) <= freshCount {
		expiredHandlerIds = staleHandlerIds
		return
	}
	deferredFleetCount = len(staleHandlerIds)
	return
}

// Whether each handler's process stamped the provider liveness writer set within
// its window, judged as FindProviders2 judges writer coverage, and whether the
// set could vouch at all: not when the read failed or ran past its budget, or
// when no process stamped within the window.
func readNetworkClientHandlerStamps(ctx context.Context, handlerIds []server.Id) (current []bool, ok bool) {
	handlerMembers := make([]string, len(handlerIds))
	for i, handlerId := range handlerIds {
		handlerMembers[i] = handlerId.String()
	}
	var newestCmd *redis.ZSliceCmd
	var stampCmd *redis.FloatSliceCmd
	readErr := readProviderLivenessWithin(ctx, networkClientHandlerStampReadTimeout, func(readCtx context.Context, pipe redis.Pipeliner) {
		newestCmd = pipe.ZRevRangeWithScores(readCtx, providerLivenessWritersKey, 0, 0)
		stampCmd = pipe.ZMScore(readCtx, providerLivenessWritersKey, handlerMembers...)
	})
	if readErr != nil || providerLivenessZSliceFailed(newestCmd) || providerLivenessFloatSliceFailed(stampCmd) {
		return nil, false
	}
	return judgeProviderLivenessStamps(
		server.NowUtc().UnixMilli(),
		providerLivenessNewestMillis(newestCmd),
		providerLivenessStampMillis(stampCmd),
		providerLivenessWriterWindow,
	)
}
