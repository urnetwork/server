package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The attested fixture deliberately uses a non-superuser. PostgreSQL forbids
// that role from disabling its own FK-internal triggers. Project one changed
// pg_trigger row into the real catalog query instead; no privilege escalation,
// catalog writes, or production predicate substitution is involved.
type migrationFKTriggerProjectionSource struct {
	migrationPingDatabaseSource
	oid     uint32
	enabled string
}

func (source *migrationFKTriggerProjectionSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	if strings.Contains(query, "FROM migration_catalog") {
		return source.migrationPingDatabaseSource.PostgreSQL(ctx, query)
	}
	trimmed := strings.TrimSpace(query)
	if !strings.HasPrefix(trimmed, "WITH ") {
		return nil, fmt.Errorf("migration catalog projection requires the source WITH query")
	}
	if source.enabled != "D" && source.enabled != "R" && source.enabled != "A" {
		return nil, fmt.Errorf("invalid test trigger state")
	}
	projection := fmt.Sprintf(`pg_trigger AS (
 SELECT actual.oid, actual.tgrelid, actual.tgname, actual.tgfoid, actual.tgtype,
  CASE WHEN actual.oid=%d THEN '%s'::"char" ELSE actual.tgenabled END AS tgenabled,
  actual.tgisinternal, actual.tgconstraint, actual.tgdeferrable, actual.tginitdeferred,
  actual.tgnargs, actual.tgattr, actual.tgqual, actual.tgargs, actual.tgoldtable, actual.tgnewtable
 FROM pg_catalog.pg_trigger actual
 )`, source.oid, source.enabled)
	return source.migrationPingDatabaseSource.PostgreSQL(ctx, "WITH "+projection+", "+strings.TrimPrefix(trimmed, "WITH "))
}

func checkMigrationForeignKeyEnabledCatalog(t testing.TB, ctx context.Context, child, parent, artifact string) {
	t.Helper()
	var triggers []uint32
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT t.oid FROM pg_catalog.pg_trigger t
 JOIN pg_constraint k ON k.oid=t.tgconstraint
 WHERE k.conrelid=to_regclass('public.'||$1) AND k.confrelid=to_regclass('public.'||$2)
 AND k.contype='f' AND t.tgisinternal ORDER BY t.oid`, child, parent)
		server.Raise(err)
		defer rows.Close()
		for rows.Next() {
			var oid uint32
			server.Raise(rows.Scan(&oid))
			triggers = append(triggers, oid)
		}
		server.Raise(rows.Err())
	})
	if len(triggers) != 4 {
		t.Fatalf("FK catalog fixture has %d triggers, want four independent child/parent guards", len(triggers))
	}
	for index, oid := range triggers {
		for _, enabled := range []string{"D", "R", "A"} {
			source := &migrationFKTriggerProjectionSource{oid: oid, enabled: enabled}
			alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			drift := ""
			for _, alert := range alerts {
				if alert.Class == "migration-schema-drift" {
					drift = alert.Markdown()
				}
			}
			if enabled == "A" {
				if drift != "" {
					t.Fatalf("always-enabled FK guard %d reports drift: %s", index, drift)
				}
			} else if !strings.Contains(drift, artifact) {
				t.Fatalf("inactive FK guard %d/%s did not identify %s: %s", index, enabled, artifact, drift)
			}
		}
	}
	if _, drift := migrationPingDatabaseCheck(t, ctx); drift != "" {
		t.Fatalf("unchanged native catalog after projections reports drift: %s", drift)
	}
}
