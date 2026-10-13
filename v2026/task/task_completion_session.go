// Only the result collector may donate its retained execution connection.
package task

import (
	"context"

	"github.com/urnetwork/server/v2026"
)

func (self *taskClaimGuard) finalizeOwnedTx(ctx context.Context, keys []server.PgOwnershipKey, callback func(server.PgTx), options ...any) {
	options = append(append([]any{}, options...), server.TxReadCommitted, server.OptNoRetry())
	if self == nil {
		server.OwnedTx(ctx, keys, callback, options...)
		return
	}
	if self.completionSession == nil {
		self.completionSession = server.NewPgOwnedSession(self.conn)
	}
	self.completionSession.Tx(ctx, keys, callback, options...)
}

func (self *taskClaimGuard) completionSessionError() error {
	if self == nil || self.completionSession == nil {
		return nil
	}
	return self.completionSession.Err()
}
