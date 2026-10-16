package model

import (
	"context"

	"github.com/urnetwork/server"
)

// The subscriber policy as a request naming its providers applies it in
// Quality (owner decision 2026-10-15, FP2FIX.md "Named Quality requests"). A
// named provider still needs a live connection, and ARIN risk on a live
// connection still refuses it, but its live connections need not be ARIN
// quality verified. Connect's fixed-IP redial names the exit it lost; refusing
// a non-quality exit there changed the user's IP, though discovery hands the
// same provider out as a borrowed Speed or Online member.
//
// offline holds the providers with no live connection, risky those with ARIN
// risk on a live connection. With subscriber policy two off, a named request
// reads nothing here, as before.
//
// No refusal is cached: a provider that reconnects is admitted by the next
// redial, and every admission reads current facts on the primary anyway. The
// read keeps its own query and row shape, so a change to the native Quality
// read (getProviderSubscriberExclusions) cannot change it.
func getProviderNamedSubscriberExclusions(ctx context.Context, clientIds []server.Id) (offline map[server.Id]bool, risky map[server.Id]bool, returnErr error) {
	enabled, err := subscriberQualityPolicyEnabled()
	if err != nil || !enabled {
		return nil, nil, err
	}
	pending := make([]server.Id, 0, len(clientIds))
	seen := make(map[server.Id]bool, len(clientIds))
	for _, id := range clientIds {
		if !seen[id] {
			seen[id] = true
			pending = append(pending, id)
		}
	}
	offline = map[server.Id]bool{}
	risky = map[server.Id]bool{}
	if len(pending) == 0 {
		return offline, risky, nil
	}
	// Candidate chunks bound both the query and result size.
	const chunkSize = providerQualityValidationBatchSize
	server.Db(ctx, func(conn server.PgConn) {
		for start := 0; start < len(pending); start += chunkSize {
			chunk := pending[start:min(start+chunkSize, len(pending))]
			observeSubscriberEligibilityEvent("sql_batch", 1)
			rows, err := conn.Query(ctx, providerNamedSubscriberExclusionsSql, chunk, server.NowUtc().Add(-2*NetworkClientHandlerHeartbeatTimeout))
			if err != nil {
				returnErr = err
				return
			}
			func() {
				defer rows.Close()
				for rows.Next() {
					var id server.Id
					var risk bool
					if err := rows.Scan(&id, &risk); err != nil {
						returnErr = err
						return
					}
					if risk {
						risky[id] = true
					} else {
						offline[id] = true
					}
				}
				returnErr = rows.Err()
			}()
			if returnErr != nil {
				return
			}
		}
	}, server.OptReadOnly(), server.OptNoRetry())
	if returnErr != nil {
		return nil, nil, returnErr
	}
	observeSubscriberEligibilityEvent("offline", len(offline))
	return offline, risky, nil
}

// No live connection, or ARIN risk on a live one; ARIN quality verification is
// not read. The live relation (connected, on a handler with a current
// heartbeat) is the native Quality read's, text for text
// (TestNamedSubscriberQueryKeepsTheQualityLiveRelation). An unclassified live
// connection is not a risk here, as in the arin_risk column of that read.
const providerNamedSubscriberExclusionsSql = `
	WITH candidates AS MATERIALIZED (
		SELECT DISTINCT unnest($1::uuid[]) AS client_id
	), connections AS MATERIALIZED (
		SELECT connection.client_id, connection.connection_id, connection.handler_id
		FROM candidates
		JOIN network_client_connection AS connection ON connection.client_id = candidates.client_id AND connection.connected
	), live AS MATERIALIZED (
		SELECT connections.client_id, connections.connection_id
		FROM connections
		JOIN network_client_handler AS handler ON handler.handler_id = connections.handler_id
			AND handler.heartbeat_time >= $2
	)
	SELECT candidates.client_id, bool_or(COALESCE(location.arin_risk, false)) AS arin_risk
	FROM candidates
	LEFT JOIN live ON live.client_id = candidates.client_id
	LEFT JOIN network_client_location AS location ON location.connection_id = live.connection_id
	GROUP BY candidates.client_id
	HAVING count(live.connection_id) = 0 OR bool_or(COALESCE(location.arin_risk, false))
`
