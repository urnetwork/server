// Subscriber fixture batching preserves original facts and session events.
package model

import (
	"testing"

	"github.com/urnetwork/server/v2026"
)

// Real admission transaction ids prove bounded commits while duplicate input
// and an existing revoked member retain the original no-overwrite contract.
func TestSubscriberFixtureBatchesPreserveExistingFacts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		scores := make([]*ClientScore, 1025)
		ids := make([]server.Id, len(scores))
		for i := range scores {
			ids[i] = server.NewId()
			scores[i] = &ClientScore{ClientId: ids[i]}
		}
		writeSubscriberFactsForScores(ctx, scores[:1])
		var originalHandler, originalLocation server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(tx.QueryRow(ctx, `SELECT c.handler_id,l.city_location_id
 FROM network_client_connection c JOIN network_client_location l USING(client_id,connection_id)
 WHERE c.client_id=$1`, ids[0]).Scan(&originalHandler, &originalLocation))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false WHERE client_id=$1`, ids[0]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_location SET arin_quality_verified=false WHERE client_id=$1`, ids[0]))
		})
		// The final range includes a new member and duplicates of both the
		// revoked member and an already admitted member from the first range.
		writeSubscriberFactsForScores(ctx, append(scores, scores[0], scores[511]))
		server.Db(ctx, func(conn server.PgConn) {
			var rows, connected, locations, handlers, cities, timestamps, admitted, transactions, maximumAdmits int64
			server.Raise(conn.QueryRow(ctx, `WITH admission_batches AS (
 SELECT transaction_id,count(*) AS admits FROM provider_work_session_event
 WHERE client_id=ANY($1::uuid[]) AND client_id<>$2 AND kind='admit' GROUP BY transaction_id
)
SELECT count(*),count(*) FILTER(WHERE c.connected),count(l.connection_id),
 count(DISTINCT c.handler_id) FILTER(WHERE c.client_id<>$2),
 count(DISTINCT l.city_location_id) FILTER(WHERE c.client_id<>$2),
 count(DISTINCT c.connect_time) FILTER(WHERE c.client_id<>$2),
 (SELECT COALESCE(sum(admits),0)::bigint FROM admission_batches),(SELECT count(*) FROM admission_batches),
 (SELECT COALESCE(max(admits),0) FROM admission_batches)
FROM network_client_connection c LEFT JOIN network_client_location l USING(client_id,connection_id)
WHERE c.client_id=ANY($1::uuid[])`, ids, ids[0]).Scan(&rows, &connected, &locations, &handlers, &cities, &timestamps, &admitted, &transactions, &maximumAdmits))
			if rows != 1025 || connected != 1024 || locations != 1025 || handlers != 1 || cities != 1 || timestamps != 1 || admitted != 1024 || transactions != 3 || maximumAdmits > 512 {
				t.Fatalf("subscriber fixture lost bounded original facts: rows=%d connected=%d locations=%d handlers=%d cities=%d timestamps=%d admitted=%d transactions=%d maximum_admits=%d", rows, connected, locations, handlers, cities, timestamps, admitted, transactions, maximumAdmits)
			}
			var revoked bool
			server.Raise(conn.QueryRow(ctx, `SELECT NOT c.connected AND NOT l.arin_quality_verified
 AND c.handler_id=$2 AND l.city_location_id=$3
 AND (SELECT count(*) FROM provider_work_session_event e WHERE e.client_id=c.client_id AND e.kind='admit')=1
FROM network_client_connection c JOIN network_client_location l USING(client_id,connection_id)
WHERE c.client_id=$1`, ids[0], originalHandler, originalLocation).Scan(&revoked))
			if !revoked {
				t.Fatal("subscriber fixture replaced an existing revoked fact")
			}
		})
	})
}
