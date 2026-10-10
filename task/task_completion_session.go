// Only the result collector may donate its retained execution connection.
package task

import (
	"context"

	"github.com/urnetwork/server"
)

func (self *taskClaimGuard) finalizeOwnedTx(ctx context.Context, keys []server.PgOwnershipKey, callback func(server.PgTx)) {
	if self == nil {
		server.OwnedTx(ctx, keys, callback, server.TxReadCommitted, server.OptNoRetry())
		return
	}
	if self.completionSession == nil {
		self.completionSession = server.NewPgOwnedSession(self.conn)
	}
	self.completionSession.Tx(ctx, keys, callback, server.TxReadCommitted, server.OptNoRetry())
}

func (self *taskClaimGuard) completionSessionError() error {
	if self == nil || self.completionSession == nil {
		return nil
	}
	return self.completionSession.Err()
}
