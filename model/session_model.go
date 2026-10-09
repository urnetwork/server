// Cleanup rechecks session ownership while holding each client row lock.
package model

import (
	"context"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const sessionCleanupBatchSize = 128

func CleanupSessionOperation(ctx context.Context, operation session.SessionCleanupOperation) (complete bool, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				returnErr = err
			} else {
				panic(recovered)
			}
		}
	}()
	func() {
		server.Tx(ctx, func(tx server.PgTx) {
			rows, err := tx.Query(ctx, `SELECT client_id FROM network_client WHERE network_id=$1 AND session_id=ANY($2) AND active ORDER BY client_id LIMIT $3 FOR UPDATE`, operation.NetworkId, operation.TargetSessionIds, sessionCleanupBatchSize)
			ids := []server.Id{}
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var id server.Id
					server.Raise(rows.Scan(&id))
					ids = append(ids, id)
				}
			})
			// The predicate was rechecked after waiting for locks. A newer rebind wins.
			_, err = deactivateLockedNetworkClientsInTx(ctx, tx, ids, operation.NetworkId)
			server.Raise(err)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE auth_code SET remaining_uses=0 WHERE network_id=$1 AND origin_session_id=ANY($2) AND active`, operation.NetworkId, operation.TargetSessionIds))
			server.Raise(tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM network_client WHERE network_id=$1 AND session_id=ANY($2) AND active)`, operation.NetworkId, operation.TargetSessionIds).Scan(&complete))
			if complete {
				server.Raise(session.CompleteSessionOperationInTx(ctx, tx, operation.NetworkId, operation.OperationId))
			}
		}, server.TxReadCommitted)
	}()
	return
}

func RecoverSessionOperations(ctx context.Context, limit int) error {
	operations, err := session.PendingSessionOperations(ctx, limit)
	if err != nil {
		return err
	}
	for _, op := range operations {
		result, err := session.EnforceSessionOperation(ctx, op.NetworkId, op.OperationId)
		if err != nil {
			continue
		}
		if result.State != "enforced" {
			continue
		}
		op.TargetSessionIds = result.TargetSessionIds
		if _, err = CleanupSessionOperation(ctx, op); err != nil {
			return err
		}
	}
	return nil
}
