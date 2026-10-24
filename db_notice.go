// Delivers the notices PostgreSQL sends during one statement (its INFO,
// NOTICE and WARNING responses) to a handler registered for that statement,
// such as the VACUUM (VERBOSE) summaries of db_maintenance_hot_vacuum.go. pgx
// takes one notice handler per pool configuration, so a pool that delivers
// notices dispatches each one to the handler its connection carries, and a
// connection without a handler drops them as before. Only the maintenance pool
// delivers notices; the main pool's configuration is unchanged.
//
// pgx calls the handler on the goroutine reading the connection, which is the
// one running the statement, so the registration needs no lock.
package server

import (
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The `PgConn.CustomData` key of a connection's notice handler.
const pgNoticeHandlerKey = "github.com/urnetwork/server.noticeHandler"

// Dispatches each notice a connection of the pool receives to the handler
// registered on that connection, keeping any handler the config already has.
func configurePgPoolNotices(config *pgxpool.Config) {
	onNotice := config.ConnConfig.OnNotice
	config.ConnConfig.OnNotice = func(conn *pgconn.PgConn, notice *pgconn.Notice) {
		if handler, ok := conn.CustomData()[pgNoticeHandlerKey].(func(*pgconn.Notice)); ok {
			handler(notice)
		}
		if onNotice != nil {
			onNotice(conn, notice)
		}
	}
}

// Registers handler for the notices conn receives while callback runs, and
// only then. Only a connection of a pool configured by configurePgPoolNotices
// delivers any.
func withPgNotices(conn *pgconn.PgConn, handler func(*pgconn.Notice), callback func()) {
	data := conn.CustomData()
	data[pgNoticeHandlerKey] = handler
	defer delete(data, pgNoticeHandlerKey)
	callback()
}
