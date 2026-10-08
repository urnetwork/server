// Orphan repair takes one endpoint fence for a bounded primary-key page. The
// minute task rediscovers deferred rows; foreground owners never wait for a
// cleanup that is itself waiting for another endpoint or connection row.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

const networkClientHandlerRetirementPageSize = 64

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

// Discover orphans without endpoint fences, then retire one bounded client page
// per transaction. Busy endpoints and excess rows remain eligible next minute.
func CloseExpiredNetworkClientHandlers(ctx context.Context, minTime time.Time) {
	ctx = providerWorkSessionContext(ctx)
	disconnectTime := server.NowUtc()
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_handler WHERE heartbeat_time < $1`, minTime.UTC()))
	}, server.TxReadCommitted)

	// Handler deletion is not a complete orphan index: a connect may race that
	// deletion, and old processes may have left connections without any handler.
	var clientIds []server.Id
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		clientIds = nil
		rows, err := tx.Query(ctx, `SELECT DISTINCT client_id FROM network_client_connection c
		 WHERE c.connected AND NOT EXISTS(SELECT 1 FROM network_client_handler h WHERE h.handler_id=c.handler_id)
		 ORDER BY client_id LIMIT 4096`)
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

// Optional journal tables retain their rollout behavior. A busy fence is a
// distinct successful deferral, never permission to run an unfenced fallback.
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

// The caller owns the bounded page and the no-retry transaction. Only a
// missing head table permits pre-journal compatibility after savepoint rollback.
func retireNetworkClientHandlerPageInTx(ctx context.Context, tx server.PgTx, clientId server.Id, connectionIds []server.Id, disconnectTime time.Time) {
	busy := false
	stage := "bridge"
	var fenceErr error
	fenced := providerWorkOptionalSchemaInTx(ctx, tx, func(optional server.PgTx) (returnErr error) {
		defer func() { fenceErr = returnErr }()
		var acquired bool
		if err := optional.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock_shared(-776::bigint)`).Scan(&acquired); err != nil {
			return err
		}
		if !acquired {
			busy = true
			return nil
		}
		stage = "endpoint"
		if err := optional.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock(776,('x'||substr(md5($1::uuid::text),1,8))::bit(32)::int)`, clientId).Scan(&acquired); err != nil {
			return err
		}
		if !acquired {
			busy = true
			return nil
		}
		stage = "head"
		if err := optional.QueryRow(ctx, `WITH owned AS (
		 SELECT client_id FROM provider_work_session_head WHERE client_id=$1 FOR UPDATE SKIP LOCKED)
		 SELECT EXISTS(SELECT 1 FROM provider_work_session_head WHERE client_id=$1)
		 AND NOT EXISTS(SELECT 1 FROM owned)`, clientId).Scan(&busy); err != nil {
			return err
		}
		if busy {
			return nil
		}
		stage = "cooperating"
		_, err := optional.Exec(ctx, `SELECT set_config('urnetwork.provider_work_cooperating','1',true)`)
		return err
	})
	if !fenced {
		var pgErr *pgconn.PgError
		if stage != "head" || !errors.As(fenceErr, &pgErr) || pgErr.Code != "42P01" {
			if fenceErr == nil {
				fenceErr = errors.New("handler retirement fence is unavailable")
			}
			server.Raise(fenceErr)
		}
	}
	if busy {
		return
	}
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
