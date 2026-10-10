// Rows past an explicit deadline reconcile under their payer's shared grant
// keys. One expiry page gives those rows one in-process turn per payer.
package model

import (
	"context"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

// Deadline reconciliation try-locks every reservation grant of the payer, so
// two such rows of one payer started together refuse each other and both look
// busy to the closer. Each returned slot is a one-row turn shared by one
// payer's due rows; other rows get nil and keep the page's full parallelism.
// The read is only an optimization: if it fails the page runs unserialized,
// and a refused row stays open for a later visit as any busy row does.
func forceCloseRowTurnsByPayer(ctx context.Context, openContracts []*contractExpiryState) (turns []chan struct{}) {
	turns = make([]chan struct{}, len(openContracts))
	if len(openContracts) == 0 {
		return
	}
	indexes := make(map[server.Id]int, len(openContracts))
	ids := make([]server.Id, len(openContracts))
	for index, openContract := range openContracts {
		indexes[openContract.contractId] = index
		ids[index] = openContract.contractId
	}
	payerTurns := map[server.Id]chan struct{}{}
	assigned := make([]chan struct{}, len(openContracts))
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT contract_id, COALESCE(payer_network_id, source_network_id)
				FROM transfer_contract
				WHERE contract_id = ANY($1) AND outcome IS NULL AND expiration_time <= statement_timestamp() AT TIME ZONE 'UTC'`, ids)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var contractId server.Id
					var payerId *server.Id
					server.Raise(rows.Scan(&contractId, &payerId))
					if payerId == nil {
						continue
					}
					turn, ok := payerTurns[*payerId]
					if !ok {
						turn = make(chan struct{}, 1)
						payerTurns[*payerId] = turn
					}
					assigned[indexes[contractId]] = turn
				}
			})
		}, server.OptNoRetry())
		turns = assigned
	}, func(err error) {
		glog.Infof("[close-expired]payer turns unavailable, page runs unserialized: %v\n", err)
	})
	return
}

// A private fixture seam observes a row that must wait for its payer's turn.
// The row holds no transaction or turn while it is called.
type forceCloseRowTurnWaitKey struct{}

func forceCloseRowTurnWait(ctx context.Context, contractId server.Id) {
	if observe, ok := ctx.Value(forceCloseRowTurnWaitKey{}).(func(server.Id)); ok {
		observe(contractId)
	}
}
