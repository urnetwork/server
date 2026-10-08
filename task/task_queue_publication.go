// Financial publishers validate already-admitted queue ownership before adding
// SQL to either a direct transaction or its batch. Admission belongs to the
// complete outer business owner, before any shared write or savepoint.
package task

import (
	"errors"

	"github.com/urnetwork/server"
)

type taskQueueOwnershipOption struct {
	tx server.PgTx
}

// This is a query-free subset assertion, never a late ownership acquisition.
// Required publications must not be omitted after their financial mutation.
func RequireQueueOwnership(tx server.PgTx) any {
	return taskQueueOwnershipOption{tx: tx}
}

// Savepoints may borrow their still-live outer owner on the same connection;
// an unrelated transaction cannot use its ownership assertion as authority.
func requireTaskPublicationBackend(tx server.PgTx, options []any) {
	for _, option := range options {
		if requirement, ok := option.(taskQueueOwnershipOption); ok {
			if tx == nil || requirement.tx == nil || tx.Conn() != requirement.tx.Conn() {
				panic(errors.New("task publication requires the admitted transaction backend"))
			}
		}
	}
}

func requirePreparedTaskOwnership(prepared preparedTask, options []any) {
	for _, option := range options {
		if requirement, ok := option.(taskQueueOwnershipOption); ok {
			key := PendingTaskOwnershipKey(prepared.taskId, prepared.runOnceKey)
			if !server.TxOwnsKeys(requirement.tx, []server.PgOwnershipKey{key}) {
				panic(errors.New("task publication requires its admitted queue owner"))
			}
		}
	}
}
