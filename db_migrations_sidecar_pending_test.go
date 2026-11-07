//go:build sidecar_pending

package server

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// MG0 under the sidecar_pending build tag (§19.7, D-OPS-6).

// The group ends the slice, the marker last, so main's migrations keep their
// positions below it.
func TestSidecarMg0EndsTheMigrations(t *testing.T) {
	group := sidecarMg0Migrations()
	if len(group) != sidecarMg0MigrationCount {
		t.Fatalf("MG0 holds %d migrations", len(group))
	}
	head := len(migrations) - len(group)
	identities := make([]string, 0, len(group))
	for _, migration := range group {
		identity, err := migrationIdentity(migration)
		if err != nil {
			t.Fatal(err)
		}
		identities = append(identities, identity)
	}
	for offset, identity := range identities {
		linked, err := MigrationIdentity(head + offset)
		if err != nil {
			t.Fatal(err)
		}
		if linked != identity {
			t.Fatalf("migration %d is not MG0's entry %d", head+offset, offset)
		}
	}
	if !SidecarMg0Linked() {
		t.Fatal("a build with the tag does not report MG0 linked")
	}
	if got := CodeMigrationVersion(sidecarMg0MigrationId); got != MigrationCount() {
		t.Fatalf("the marker is at version %d of %d", got, MigrationCount())
	}
	// the initializer's alias sees the group, which an init-time append would escape
	if len(migrationCatalogMigrations) != len(migrations) {
		t.Fatalf("the catalog sees %d migrations of %d", len(migrationCatalogMigrations), len(migrations))
	}
}

// Readiness needs the head, MG0 included, so a binary that links it is unready
// on a database that lacks its tables (§19.4, §19.7's code row).
func TestSidecarMg0IsInTheBinarysRequiredHead(t *testing.T) {
	if got := MinimumRuntimeMigrationVersion(); got != MigrationCount() {
		t.Fatalf("the binary requires head %d of %d", got, MigrationCount())
	}
}

// CODESTYLE's migration rules: the group may add tables, columns and indexes. It
// may not touch what the running release reads or writes.
func TestSidecarMg0IsAdditive(t *testing.T) {
	statement := regexp.MustCompile(`(?is)^\s*(CREATE TABLE network_client_owner\b|SET LOCAL lock_timeout = '2s';\s+ALTER TABLE \w+\s+ADD COLUMN\b|CREATE INDEX (CONCURRENTLY )?auth_code_api_key_id\b)`)
	refused := regexp.MustCompile(`(?i)\b(DROP COLUMN|DROP TABLE|ALTER COLUMN|RENAME|SET NOT NULL|NOT VALID|TRUNCATE|UPDATE|DELETE|transaction_timeout)\b`)
	required := regexp.MustCompile(`(?i)ADD COLUMN \w+ [\w ]*NOT NULL`)
	for index, migration := range sidecarMg0Migrations() {
		sqls := []string{}
		switch value := migration.(type) {
		case *SqlMigration:
			sqls = append(sqls, value.sql)
		case *OnlineSqlMigration:
			// the restart step drops only what an interrupted build of this index left
			if value.recoverySql != "DROP INDEX CONCURRENTLY IF EXISTS auth_code_api_key_id" {
				t.Fatalf("entry %d recovers with %q", index, value.recoverySql)
			}
			if !strings.Contains(value.sql, "CONCURRENTLY") {
				t.Fatalf("entry %d locks an existing table: %q", index, value.sql)
			}
			sqls = append(sqls, value.sql, value.auditSql)
		case *CodeMigration:
			if value.id != sidecarMg0MigrationId {
				t.Fatalf("entry %d runs code %s", index, value.id)
			}
		default:
			t.Fatalf("entry %d is a %T", index, migration)
		}
		for _, sql := range sqls {
			if !statement.MatchString(sql) {
				t.Errorf("entry %d is not an addition: %s", index, sql)
			}
			if refused.MatchString(sql) || required.MatchString(sql) {
				t.Errorf("entry %d changes what the running release uses: %s", index, sql)
			}
		}
	}
}

// What the running release does to the two tables MG0 alters, in its own shapes.
// A release from before MG0 names no new column, so each must work on either side
// of the group: this is the release that runs while MG0 applies, and the one a
// rollback restarts on the migrated schema (FS-55).
func requireRunningReleaseShapes(t testing.TB, ctx context.Context) {
	t.Helper()
	networkId := NewId()
	authCodeId := NewId()
	apiKeyId := NewId()
	now := NowUtc()
	MaintenanceTx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `
			INSERT INTO auth_code (
				auth_code_id, network_id, user_id, auth_code, create_time, end_time,
				uses, remaining_uses, principal, origin_session_id
			) VALUES ($1, $2, $3, $4, $5, $6, 1, 1, '', NULL)
		`, authCodeId, networkId, NewId(), authCodeId.String(), now, now.Add(time.Minute)))
		var remaining int
		Raise(tx.QueryRow(ctx, `
			SELECT remaining_uses FROM auth_code WHERE auth_code = $1 AND network_id = $2 AND active FOR UPDATE
		`, authCodeId.String(), networkId).Scan(&remaining))
		RaisePgResult(tx.Exec(ctx, `DELETE FROM auth_code WHERE auth_code_id = $1`, authCodeId))

		RaisePgResult(tx.Exec(ctx, `
			INSERT INTO account_api_key (api_key_id, network_id, api_key, name) VALUES ($1, $2, $3, $4)
		`, apiKeyId, networkId, apiKeyId.String(), "shape"))
		var name string
		var createTime time.Time
		Raise(tx.QueryRow(ctx, `
			SELECT name, create_time FROM account_api_key WHERE api_key_id = $1 AND network_id = $2
		`, apiKeyId, networkId).Scan(&name, &createTime))
		RaisePgResult(tx.Exec(ctx, `DELETE FROM account_api_key WHERE api_key_id = $1 AND network_id = $2`, apiKeyId, networkId))
	})
}

// The release that is running when MG0 applies keeps working, before and after,
// and nothing of the group is required of a row that release writes.
func TestSidecarMg0RunsBesideTheRunningRelease(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		running := MigrationCount() - sidecarMg0MigrationCount
		ApplyDbMigrationsUpTo(ctx, running)
		requireRunningReleaseShapes(t, ctx)

		var ownerTable *string
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT to_regclass('network_client_owner')::text`).Scan(&ownerTable))
		})
		if ownerTable != nil {
			t.Fatal("the running release's schema already holds MG0")
		}

		ApplyDbMigrations(ctx)
		if got := DbVersion(ctx); got != MigrationCount() {
			t.Fatalf("version = %d of %d", got, MigrationCount())
		}
		requireRunningReleaseShapes(t, ctx)

		columns := map[string]string{}
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT table_name || '.' || column_name, is_nullable
				FROM information_schema.columns
				WHERE
					(table_name = 'account_api_key' AND column_name IN ('scopes', 'expire_time', 'creator_user_id')) OR
					(table_name = 'auth_code' AND column_name = 'api_key_id')
			`)
			WithPgResult(result, err, func() {
				for result.Next() {
					var column string
					var nullable string
					Raise(result.Scan(&column, &nullable))
					columns[column] = nullable
				}
			})
		})
		want := []string{"account_api_key.creator_user_id", "account_api_key.expire_time", "account_api_key.scopes", "auth_code.api_key_id"}
		got := []string{}
		for column, nullable := range columns {
			got = append(got, column)
			if nullable != "YES" {
				t.Errorf("%s is NOT NULL", column)
			}
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Fatalf("columns = %v, want %v", got, want)
		}
	})
}

// §7.7's owner columns, with a client id and the marks that default to none.
func TestNetworkClientOwnerColumns(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		columns := []string{}
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT column_name
				FROM information_schema.columns
				WHERE table_name = 'network_client_owner'
				ORDER BY ordinal_position
			`)
			WithPgResult(result, err, func() {
				for result.Next() {
					var column string
					Raise(result.Scan(&column))
					columns = append(columns, column)
				}
			})
		})
		want := []string{
			"client_id", "owner_user_id", "member_id", "sponsor_user_id", "auth_key_id",
			"tagged", "ephemeral", "admin_refused", "strict_exp", "acl_enforcing", "declares_no_egress",
		}
		if !slices.Equal(columns, want) {
			t.Fatalf("columns = %v, want %v", columns, want)
		}

		clientId := NewId()
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client_owner (client_id) VALUES ($1)`, clientId))
			var adminRefused, strictExp bool
			Raise(tx.QueryRow(ctx, `SELECT admin_refused, strict_exp FROM network_client_owner WHERE client_id = $1`, clientId).Scan(&adminRefused, &strictExp))
			if adminRefused || strictExp {
				t.Fatalf("a bare owner row is marked: %t %t", adminRefused, strictExp)
			}
		})
	})
}
