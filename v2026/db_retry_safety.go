// Proves whether a complete database callback stayed before its first socket
// write. Error-local retry flags cannot describe earlier callback statements.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Owns only a byte counter for one physical connection, not SQL or a global
// connection registry. Atomic access also tolerates pgx's asynchronous cleanup.
type pgWriteTrackedConn struct {
	net.Conn
	writtenByteCount atomic.Uint64
}

// Counts bytes accepted by the underlying transport, including partial writes.
// A zero-byte failure remains distinguishable from a lost statement reply.
func (self *pgWriteTrackedConn) Write(p []byte) (int, error) {
	n, err := self.Conn.Write(p)
	if 0 < n {
		self.writtenByteCount.Add(uint64(n))
	}
	return n, err
}

// Wraps beneath TLS so pgx still sees *tls.Conn for SCRAM channel binding.
// No additional socket, goroutine, admission limit, or protocol query is added.
func configurePgPoolWriteTracking(config *pgxpool.Config) {
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network string, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return conn, err
		}
		return &pgWriteTrackedConn{Conn: conn}, nil
	}
}

// Captures one callback's transport boundary. Unknown custom wrappers cannot
// supply proof and therefore fail closed instead of authorizing a replay.
type pgWriteSnapshot struct {
	conn             *pgWriteTrackedConn
	writtenByteCount uint64
}

// Finds the tracked raw socket through pgx's optional outer TLS connection.
func snapshotPgWrites(conn net.Conn) pgWriteSnapshot {
	if tlsConn, ok := conn.(*tls.Conn); ok {
		conn = tlsConn.NetConn()
	}
	trackedConn, ok := conn.(*pgWriteTrackedConn)
	if !ok {
		return pgWriteSnapshot{}
	}
	return pgWriteSnapshot{conn: trackedConn, writtenByteCount: trackedConn.writtenByteCount.Load()}
}

// Checks before disposal can write Terminate. Earlier asynchronous disposal
// can conservatively prevent a retry, but cannot hide application bytes.
func (self pgWriteSnapshot) unchanged() bool {
	return self.conn != nil && self.writtenByteCount == self.conn.writtenByteCount.Load()
}

// Only an explicit rollback outcome authorizes a whole-transaction replay.
// A lost response, timeout, or unknown completion may already have committed.
// A commit postgres turned into a rollback (`pgx.ErrTxCommitRollback`) means
// the callback went on after a failed statement; it is replayed only when the
// statement error recorded for it (see `txAbortedError`) is itself transient.
func canRetryCommitError(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && isTransientError(pgErr)
}
