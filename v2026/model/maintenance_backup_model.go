package model

import (
	"context"

	"github.com/urnetwork/server/v2026"
)

// A pg_dump snapshot holds the cleanup horizon for its whole transaction, not
// only its current COPY. Defer optional scans as soon as that snapshot exists;
// mandatory consistency repairs and explicit operator backfills are not gated.
// Scope to this database and exclude the observing backend. An application name
// alone (for example an idle connection left after a dump) is not a held snapshot.
const postgresLogicalBackupSnapshotActiveSQL = `
	SELECT EXISTS (
		SELECT 1
		FROM pg_stat_activity
		WHERE datid = (SELECT oid FROM pg_database WHERE datname = current_database())
			AND pid <> pg_backend_pid()
			AND backend_type = 'client backend'
			AND application_name = 'pg_dump'
			AND xact_start IS NOT NULL
			AND backend_xmin IS NOT NULL
	)
`

func PostgresLogicalBackupSnapshotActive(ctx context.Context) (active bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, postgresLogicalBackupSnapshotActiveSQL)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&active))
			}
		})
	})
	return
}
