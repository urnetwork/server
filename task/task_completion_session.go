// Only the result collector may donate its retained execution connection.
package task

import (
	"context"

	"github.com/urnetwork/server"
)

func (self *taskClaimGuard) finalizeOwnedTx(ctx context.Context, keys []server.PgOwnershipKey, callback func(server.PgTx), options ...any) {
	options = append(append([]any{}, options...), server.TxReadCommitted, server.OptNoRetry())
	if self == nil || (self.completionSessionError() == nil && !self.sessionIdle()) {
		// A session pgx closed, which also released its locks, or one inside a
		// transaction is never lent. The claim fence still guards this owner.
		server.OwnedTx(ctx, keys, callback, options...)
		return
	}
	if self.completionSession == nil {
		self.completionSession = server.NewPgOwnedSession(self.conn)
	}
	self.completionSession.Tx(ctx, keys, callback, options...)
}

// Idle while pgx holds the session open outside a transaction. pgx closes it
// when it interrupts a statement on it.
func (self *taskClaimGuard) sessionIdle() bool {
	return self.conn != nil && !self.conn.Conn().IsClosed() && self.conn.Conn().PgConn().TxStatus() == 'I'
}

func (self *taskClaimGuard) completionSessionError() error {
	if self == nil || self.completionSession == nil {
		return nil
	}
	return self.completionSession.Err()
}
