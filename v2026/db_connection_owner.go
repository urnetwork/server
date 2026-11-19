package server

import "context"

// Reuse an explicit caller-owned connection. A nil owner is the outermost
// acquisition boundary and retains Db's normal retry and release behavior.
// The callback must not acquire PostgreSQL through another helper.
func DbInConn(ctx context.Context, conn PgConn, callback func(PgConn), options ...any) {
	if conn == nil {
		Db(ctx, callback, options...)
		return
	}
	checkPostgresAllowed(ctx)
	callback(conn)
}

// Run sequential transactions on an explicit caller-owned connection without
// reacquiring PostgreSQL. A nil owner uses Tx's normal acquisition boundary.
// The caller must hold no active transaction, and post-commit callbacks must
// also respect this connection's ownership. A lost session remains an error;
// this function cannot replace a connection carrying a session advisory lock.
func TxInConn(ctx context.Context, conn PgConn, callback func(PgTx), options ...any) {
	if conn == nil {
		Tx(ctx, callback, options...)
		return
	}
	checkPostgresAllowed(ctx)
	if conn.Conn().PgConn().TxStatus() != 'I' {
		panic("caller-owned connection already has an active transaction")
	}
	txWithConnection(ctx, func(body func(PgConn)) { body(conn) }, callback, options...)
}
