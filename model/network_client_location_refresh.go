// Location maintenance commits bounded client cohorts without retaining a
// fleet-wide transaction while later cohorts are read, folded or written.
package model

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

const clientLocationRefreshPageSize = 256
const clientLocationRefreshPageTimeout = 5 * time.Second

// Clocks and policy belong to the whole pass, not to a client's page position.
type clientLocationReliabilityRefresh struct {
	minTime                 time.Time
	maxTime                 time.Time
	updateBlockNumber       int64
	minHandlerHeartbeatTime time.Time
	requireSubscriber       bool
	egressNow               time.Time
	egressSettings          *EgressIndexSettings
	egressHealthSql         string
	probeAdmissionSql       string
	liveStaleHandlerIds     []server.Id
	pageCounts              clientLocationRefreshCounts
	passCounts              clientLocationRefreshCounts
}

// Only committed pages contribute to the last completed pass gauges.
type clientLocationRefreshCounts struct {
	hostedChildCount int
	topLevelCount    int
	writtenRowCount  int64
}

// Capture input policy before acquiring the first page's transaction.
func newClientLocationReliabilityRefresh(ctx context.Context, minTime, maxTime time.Time) *clientLocationReliabilityRefresh {
	requireSubscriber, err := subscriberQualityPolicyEnabled()
	server.Raise(err)
	liveStaleHandlerIds := readLiveStaleNetworkClientHandlerIds(ctx)
	now := server.NowUtc()
	return &clientLocationReliabilityRefresh{
		minTime:                 minTime,
		maxTime:                 maxTime,
		updateBlockNumber:       maxTime.UTC().UnixMilli() / int64(ReliabilityBlockDuration/time.Millisecond),
		minHandlerHeartbeatTime: now.Add(-2 * NetworkClientHandlerHeartbeatTimeout),
		requireSubscriber:       requireSubscriber,
		egressNow:               now,
		egressSettings:          egressIndexSettings(),
		egressHealthSql:         providerEgressHealthWindowSql(),
		probeAdmissionSql:       providerUrlProbeAdmissionSql("provider_location"),
		liveStaleHandlerIds:     liveStaleHandlerIds,
	}
}

// Acquisition, retries and all statements share one page deadline. Server
// ceilings also end an abandoned statement or idle transaction after host loss.
// The shared transaction owner retains its existing bounded commit/rollback.
func clientLocationRefreshTx(ctx context.Context, callback func(context.Context, server.PgTx)) {
	pageCtx, cancel := context.WithTimeout(ctx, clientLocationRefreshPageTimeout)
	defer cancel()
	server.MaintenanceTx(pageCtx, func(tx server.PgTx) {
		server.SetTransactionTimeoutInTx(pageCtx, tx, clientLocationRefreshPageTimeout)
		milliseconds := strconv.FormatInt(clientLocationRefreshPageTimeout.Milliseconds(), 10)
		server.RaisePgResult(tx.Exec(pageCtx, `SELECT
			set_config('statement_timeout', $1, true),
			set_config('idle_in_transaction_session_timeout', $1, true)`, milliseconds))
		callback(pageCtx, tx)
		server.Raise(pageCtx.Err())
	}, server.TxReadCommitted)
}

// Each branch matches a declared connected-prefix index and caps candidates before the
// union. Offline history is not revisited to discover the next active client.
const clientLocationRefreshCandidatesSql = `
	SELECT client_id FROM (
		(SELECT DISTINCT client_id FROM network_client_connection
		 WHERE connected AND client_id > $1 AND client_id <= $2
		 ORDER BY client_id LIMIT $3)
		UNION
		(SELECT client_id FROM network_client_location_reliability
		 WHERE valid AND connected AND client_id > $1 AND client_id <= $2
		 ORDER BY client_id LIMIT $3)
		UNION
		(SELECT client_id FROM network_client_location_reliability
		 WHERE NOT valid AND connected AND client_id > $1 AND client_id <= $2
		 ORDER BY client_id LIMIT $3)
	) candidates ORDER BY client_id LIMIT $3`

// Cap raw disconnects before the missing-target filter. An all-existing page
// must still advance. The time-prefix index does not include connection_id:
// equal-time skew can require a sort, so the page deadline remains essential.
const clientLocationRefreshDisconnectedSql = `
	WITH raw AS MATERIALIZED (
		SELECT connection_id, client_id, disconnect_time
		FROM network_client_connection
		WHERE NOT connected AND disconnect_time >= $1 AND disconnect_time < $2
		AND (disconnect_time, connection_id) > ($3, $4)
		AND connection_id <= $5
		ORDER BY disconnect_time, connection_id LIMIT $6
	)
	SELECT raw.connection_id, raw.client_id, raw.disconnect_time,
		location.client_id IS NULL AS missing
	FROM raw LEFT JOIN network_client_location_reliability location USING (client_id)
	ORDER BY raw.disconnect_time, raw.connection_id`

// A stable primary-key cursor covers stale true hints whose location row is
// absent/offline. Filter after the raw cap: sparse hints cannot hide progress.
const clientLocationRefreshCyclesSql = `
	WITH raw AS MATERIALIZED (
		SELECT client_id, eligible FROM provider_egress_probe_cycle
		WHERE client_id > $1 AND client_id <= $2 ORDER BY client_id LIMIT $3
	)
	SELECT raw.client_id, raw.eligible, COALESCE(location.connected, false)
	FROM raw LEFT JOIN network_client_location_reliability AS location USING (client_id)
	ORDER BY raw.client_id`

// Never retain a transaction across pages. A caller must finish every pass
// before proceeding to payment/score work; errors propagate rather than marking
// a partial refresh complete. A later invocation starts a fresh bounded pass.
func updateClientLocationReliabilityPages(ctx context.Context, refresh *clientLocationReliabilityRefresh) {
	refresh.passCounts = clientLocationRefreshCounts{}
	pageTx := func(callback func(context.Context, server.PgTx)) {
		clientLocationRefreshTx(ctx, func(pageCtx context.Context, tx server.PgTx) {
			refresh.pageCounts = clientLocationRefreshCounts{}
			callback(pageCtx, tx)
		})
		refresh.passCounts.hostedChildCount += refresh.pageCounts.hostedChildCount
		refresh.passCounts.topLevelCount += refresh.pageCounts.topLevelCount
		refresh.passCounts.writtenRowCount += refresh.pageCounts.writtenRowCount
	}
	var upperClientId, upperConnectionId *server.Id
	pageTx(func(pageCtx context.Context, tx server.PgTx) {
		server.Raise(tx.QueryRow(pageCtx, `SELECT
			(SELECT client_id FROM (
				(SELECT client_id FROM network_client ORDER BY client_id DESC LIMIT 1)
				UNION ALL
				(SELECT client_id FROM network_client_location_reliability ORDER BY client_id DESC LIMIT 1)
				UNION ALL
				(SELECT client_id FROM provider_egress_probe_cycle ORDER BY client_id DESC LIMIT 1)
			) candidates ORDER BY client_id DESC LIMIT 1),
			(SELECT connection_id FROM network_client_connection ORDER BY connection_id DESC LIMIT 1)`).
			Scan(&upperClientId, &upperConnectionId))
	})
	if upperClientId != nil {
		var cursor server.Id
		firstPage := true
		for {
			clientIds := []server.Id{}
			pageTx(func(pageCtx context.Context, tx server.PgTx) {
				// Transaction retries replace their candidate snapshot.
				clientIds = clientIds[:0]
				query := clientLocationRefreshCandidatesSql
				if firstPage {
					query = strings.ReplaceAll(query, "client_id > $1", "client_id >= $1")
				}
				result, err := tx.Query(pageCtx, query, cursor, *upperClientId, clientLocationRefreshPageSize)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var clientId server.Id
						server.Raise(result.Scan(&clientId))
						clientIds = append(clientIds, clientId)
					}
				})
				if len(clientIds) != 0 {
					updateClientLocationReliabilityCohortInTx(pageCtx, tx, clientIds, refresh.minTime, refresh.maxTime, refresh.liveStaleHandlerIds, refresh)
				}
			})
			if len(clientIds) == 0 {
				break
			}
			cursor = clientIds[len(clientIds)-1]
			firstPage = false
		}
	}

	if upperConnectionId != nil && refresh.minTime.Before(refresh.maxTime) {
		cursorTime := refresh.minTime
		var cursorId server.Id
		firstPage := true
		for {
			var nextTime time.Time
			var nextId server.Id
			readCount := 0
			pageTx(func(pageCtx context.Context, tx server.PgTx) {
				readCount = 0
				clientIds := []server.Id{}
				seen := map[server.Id]bool{}
				query := clientLocationRefreshDisconnectedSql
				if firstPage {
					query = strings.Replace(query, "> ($3, $4)", ">= ($3, $4)", 1)
				}
				result, err := tx.Query(pageCtx, query,
					refresh.minTime, refresh.maxTime, cursorTime, cursorId, *upperConnectionId, clientLocationRefreshPageSize)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var clientId server.Id
						var missing bool
						server.Raise(result.Scan(&nextId, &clientId, &nextTime, &missing))
						readCount++
						if missing && !seen[clientId] {
							clientIds = append(clientIds, clientId)
							seen[clientId] = true
						}
					}
				})
				if len(clientIds) != 0 {
					updateClientLocationReliabilityCohortInTx(pageCtx, tx, clientIds, refresh.minTime, refresh.maxTime, refresh.liveStaleHandlerIds, refresh)
				}
			})
			if readCount == 0 {
				break
			}
			cursorTime, cursorId = nextTime, nextId
			firstPage = false
		}
	}

	if upperClientId != nil {
		var cursor server.Id
		firstPage := true
		for {
			var nextId server.Id
			readCount := 0
			pageTx(func(pageCtx context.Context, tx server.PgTx) {
				readCount = 0
				clientIds := []server.Id{}
				query := clientLocationRefreshCyclesSql
				if firstPage {
					query = strings.Replace(query, "client_id > $1", "client_id >= $1", 1)
				}
				result, err := tx.Query(pageCtx, query, cursor, *upperClientId, clientLocationRefreshPageSize)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var eligible, connected bool
						server.Raise(result.Scan(&nextId, &eligible, &connected))
						readCount++
						if eligible && !connected {
							clientIds = append(clientIds, nextId)
						}
					}
				})
				if len(clientIds) != 0 {
					updateClientLocationReliabilityCohortInTx(pageCtx, tx, clientIds, refresh.minTime, refresh.maxTime, refresh.liveStaleHandlerIds, refresh)
				}
			})
			if readCount == 0 {
				break
			}
			cursor = nextId
			firstPage = false
		}
	}
	clientLocationReliabilityPassClients.WithLabelValues("hosted_child").Set(float64(refresh.passCounts.hostedChildCount))
	clientLocationReliabilityPassClients.WithLabelValues("top_level").Set(float64(refresh.passCounts.topLevelCount))
	clientLocationReliabilityPassWrittenRows.Set(float64(refresh.passCounts.writtenRowCount))
}
