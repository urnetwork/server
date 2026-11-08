//go:build sidecar_pending

package server

import (
	"context"
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// MG0 under the sidecar_pending build tag (§19.7, D-OPS-6).

// The pending groups end the slice, MG0 then MG1, each marker last in its group,
// so main's migrations keep their positions below them.
func TestSidecarMg0EndsTheMigrations(t *testing.T) {
	mg0 := sidecarMg0Migrations()
	if len(mg0) != sidecarMg0MigrationCount {
		t.Fatalf("MG0 holds %d migrations", len(mg0))
	}
	mg1 := sidecarMg1Migrations()
	if len(mg1) != sidecarMg1MigrationCount {
		t.Fatalf("MG1 holds %d migrations", len(mg1))
	}
	group := append(append([]any{}, mg0...), mg1...)
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
			t.Fatalf("migration %d is not the pending groups' entry %d", head+offset, offset)
		}
	}
	if !SidecarMg0Linked() || !SidecarMg1Linked() {
		t.Fatal("a build with the tag does not report MG0 and MG1 linked")
	}
	if got := CodeMigrationVersion(sidecarMg0MigrationId); got != MigrationCount()-len(mg1) {
		t.Fatalf("MG0's marker is at version %d of %d", got, MigrationCount())
	}
	if got := CodeMigrationVersion(sidecarMg1MigrationId); got != MigrationCount() {
		t.Fatalf("MG1's marker is at version %d of %d", got, MigrationCount())
	}
	// the initializer's alias sees the groups, which an init-time append would escape
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
		running := MigrationCount() - sidecarPendingMigrationCount
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

// CODESTYLE's migration rules for MG1: new tables and their indexes only.
func TestSidecarMg1IsAdditive(t *testing.T) {
	statement := regexp.MustCompile(`(?is)^\s*(CREATE TABLE (network_auth_key|network_admin_event)\b|CREATE INDEX (network_auth_key|network_admin_event)_\w+ ON (network_auth_key|network_admin_event)\b)`)
	refused := regexp.MustCompile(`(?i)\b(DROP|ALTER|RENAME|NOT VALID|TRUNCATE|UPDATE|DELETE|transaction_timeout)\b`)
	for index, migration := range sidecarMg1Migrations() {
		switch value := migration.(type) {
		case *SqlMigration:
			if !statement.MatchString(value.sql) {
				t.Errorf("entry %d is not an addition: %s", index, value.sql)
			}
			if refused.MatchString(value.sql) {
				t.Errorf("entry %d changes what the running release uses: %s", index, value.sql)
			}
		case *CodeMigration:
			if value.id != sidecarMg1MigrationId {
				t.Fatalf("entry %d runs code %s", index, value.id)
			}
		default:
			t.Fatalf("entry %d is a %T", index, migration)
		}
	}
}

// The metadata row each column of MG1's tables shows under (§2.8, I-18), with
// words of that row's sees that name it. The full guard is TEST-M1-03's; this
// keeps MG1's own columns from growing without a row in the meantime.
var sidecarMg1ColumnRows = map[string][2]string{
	"network_auth_key.auth_key_id":     {"api-11", "auth keys"},
	"network_auth_key.network_id":      {"api-11", "auth keys"},
	"network_auth_key.secret_sha256":   {"api-11", "hashed secret"},
	"network_auth_key.creator_user_id": {"api-11", "creator"},
	"network_auth_key.tag_ids":         {"api-11", "tags"},
	"network_auth_key.create_time":     {"api-11", "auth keys"},
	"network_auth_key.expire_time":     {"api-11", "expiry"},
	"network_auth_key.max_uses":        {"api-11", "uses"},
	"network_auth_key.use_count":       {"api-11", "uses"},
	"network_auth_key.ephemeral":       {"api-11", "ephemeral"},
	"network_auth_key.pre_approved":    {"api-11", "pre-approved"},

	"network_admin_event.network_id":   {"api-8", "admin events"},
	"network_admin_event.seq":          {"api-8", "per-network seq"},
	"network_admin_event.event_time":   {"api-8", "admin events"},
	"network_admin_event.actor_kind":   {"api-8", "actor"},
	"network_admin_event.actor_id":     {"api-8", "actor"},
	"network_admin_event.action":       {"api-8", "action"},
	"network_admin_event.target_kind":  {"api-8", "target"},
	"network_admin_event.target_id":    {"api-8", "target"},
	"network_admin_event.result":       {"api-8", "result"},
	"network_admin_event.request_id":   {"api-8", "request id"},
	"network_admin_event.prev_sha256":  {"api-8", "hash chain"},
	"network_admin_event.chain_sha256": {"api-8", "hash chain"},
}

// Each MG1 column maps to a row of §2.8's list whose store names its table and
// whose sees says the mapped words, and each mapped column exists.
func TestSidecarMg1ColumnsMapToMetadataRows(t *testing.T) {
	listPath := "../sidecar/metadata_list.yml"
	listBytes, err := os.ReadFile(listPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("no sidecar checkout beside the server (%s)", listPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	// a row is its id line and the indented lines under it
	rowText := func(rowId string) string {
		lines := strings.Split(string(listBytes), "\n")
		for i, line := range lines {
			if line != rowId+":" {
				continue
			}
			row := []string{}
			for _, next := range lines[i+1:] {
				if !strings.HasPrefix(next, "  ") {
					break
				}
				row = append(row, next)
			}
			return strings.Join(row, "\n")
		}
		return ""
	}

	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		columns := map[string]bool{}
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT table_name || '.' || column_name
				FROM information_schema.columns
				WHERE table_name IN ('network_auth_key', 'network_admin_event')
			`)
			WithPgResult(result, err, func() {
				for result.Next() {
					var column string
					Raise(result.Scan(&column))
					columns[column] = true
				}
			})
		})
		for column := range columns {
			mapped, ok := sidecarMg1ColumnRows[column]
			if !ok {
				t.Errorf("%s maps to no row of §2.8's list", column)
				continue
			}
			row := rowText(mapped[0])
			table, _, _ := strings.Cut(column, ".")
			if !strings.Contains(row, table) {
				t.Errorf("%s: row %s does not store %s", column, mapped[0], table)
			}
			if !strings.Contains(row, mapped[1]) {
				t.Errorf("%s: row %s does not say %q", column, mapped[0], mapped[1])
			}
		}
		for column := range sidecarMg1ColumnRows {
			if !columns[column] {
				t.Errorf("%s is mapped but does not exist", column)
			}
		}
	})
}
