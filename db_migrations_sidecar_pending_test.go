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

// The pending groups under the sidecar_pending build tag (§19.7, D-OPS-6).

// Each pending group by name, in §19.7's order: the order the slice's tail holds
// them in, and the order Gn-outs promote them in.
func sidecarPendingGroups() []struct {
	name     string
	markerId string
	group    []any
} {
	return []struct {
		name     string
		markerId string
		group    []any
	}{
		{name: "MG0", markerId: sidecarMg0MigrationId, group: sidecarMg0Migrations()},
		{name: "MG1", markerId: sidecarMg1MigrationId, group: sidecarMg1Migrations()},
		{name: "MG2", markerId: sidecarMg2MigrationId, group: sidecarMg2Migrations()},
		{name: "MG5", markerId: sidecarMg5MigrationId, group: sidecarMg5Migrations()},
		{name: "MG6", markerId: sidecarMg6MigrationId, group: sidecarMg6Migrations()},
		{name: "MG7", markerId: sidecarMg7MigrationId, group: sidecarMg7Migrations()},
		{name: "MG8", markerId: sidecarMg8MigrationId, group: sidecarMg8Migrations()},
		{name: "MG9", markerId: sidecarMg9MigrationId, group: sidecarMg9Migrations()},
	}
}

// The pending groups end the slice in order, each marker last in its group, so
// main's migrations keep their positions below them, and each marker reports its
// group linked at the version its position gives.
func TestSidecarGroupsEndTheMigrations(t *testing.T) {
	pending := sidecarPendingMigrations()
	head := len(migrations) - len(pending)
	offset := 0
	for _, group := range sidecarPendingGroups() {
		if len(group.group) == 0 {
			t.Fatalf("%s is empty, with no marker", group.name)
		}
		marker, ok := group.group[len(group.group)-1].(*CodeMigration)
		if !ok || marker.id != group.markerId {
			t.Fatalf("%s does not end with its marker %s", group.name, group.markerId)
		}
		for index, migration := range group.group {
			identity, err := migrationIdentity(migration)
			if err != nil {
				t.Fatal(err)
			}
			linked, err := MigrationIdentity(head + offset + index)
			if err != nil {
				t.Fatal(err)
			}
			if linked != identity {
				t.Fatalf("migration %d is not %s's entry %d", head+offset+index, group.name, index)
			}
		}
		offset += len(group.group)
		if got := CodeMigrationVersion(group.markerId); got != head+offset {
			t.Fatalf("%s's marker is at version %d, not %d", group.name, got, head+offset)
		}
	}
	if offset != len(pending) {
		t.Fatalf("the groups hold %d migrations and the pending tail %d", offset, len(pending))
	}
	for _, linked := range []func() bool{
		SidecarMg0Linked, SidecarMg1Linked, SidecarMg2Linked, SidecarMg5Linked,
		SidecarMg6Linked, SidecarMg7Linked, SidecarMg8Linked, SidecarMg9Linked,
	} {
		if !linked() {
			t.Fatal("a build with the tag does not report every group linked")
		}
	}
	// the initializer's alias sees the groups, which an init-time append would escape
	if len(migrationCatalogMigrations) != len(migrations) {
		t.Fatalf("the catalog sees %d migrations of %d", len(migrationCatalogMigrations), len(migrations))
	}
}

// Every group passes the static check (sidecarGroupFindings): no drop, rename or
// truncation, no SET NOT NULL, and an existing table indexed only online.
func TestSidecarGroupsAreAdditive(t *testing.T) {
	for _, group := range sidecarPendingGroups() {
		for _, finding := range sidecarGroupFindings(group.name, group.group) {
			t.Error(finding)
		}
	}
}

// A database that applies every pending group reaches the binary's head, with
// each group's marker recorded and each table the groups create in place
// (FS-54's tagged case).
func TestSidecarGroupsApplyOnAPrivateDatabase(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if got := DbVersion(ctx); got != MigrationCount() {
			t.Fatalf("version = %d of %d", got, MigrationCount())
		}
		tables := []string{}
		for _, group := range sidecarPendingGroups() {
			for _, migration := range group.group {
				if value, ok := migration.(*SqlMigration); ok {
					for _, match := range sidecarGroupCreateTablePattern.FindAllStringSubmatch(value.sql, -1) {
						tables = append(tables, strings.ToLower(match[1]))
					}
				}
			}
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			for _, table := range tables {
				var found *string
				Raise(conn.QueryRow(ctx, `SELECT to_regclass($1)::text`, table).Scan(&found))
				if found == nil {
					t.Errorf("%s is not in the migrated database", table)
				}
			}
		})
	})
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
		running := MigrationCount() - len(sidecarPendingMigrations())
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

// CODESTYLE's migration rules for MG1's §7 part: new tables and their indexes
// only.
func TestSidecarMg1TeamPartIsAdditive(t *testing.T) {
	statement := regexp.MustCompile(`(?is)^\s*(CREATE TABLE (network_auth_key|network_admin_event)\b|CREATE INDEX (network_auth_key|network_admin_event)_\w+ ON (network_auth_key|network_admin_event)\b)`)
	refused := regexp.MustCompile(`(?i)\b(DROP|ALTER|RENAME|NOT VALID|TRUNCATE|UPDATE|DELETE|transaction_timeout)\b`)
	for index, migration := range sidecarMg1TeamMigrations() {
		switch value := migration.(type) {
		case *SqlMigration:
			if !statement.MatchString(value.sql) {
				t.Errorf("entry %d is not an addition: %s", index, value.sql)
			}
			if refused.MatchString(value.sql) {
				t.Errorf("entry %d changes what the running release uses: %s", index, value.sql)
			}
		default:
			t.Fatalf("entry %d is a %T", index, migration)
		}
	}
}

// CODESTYLE's migration rules for MG1's §5 part: new tables and their indexes
// only.
func TestSidecarMg1CorePartIsAdditive(t *testing.T) {
	statement := regexp.MustCompile(`(?is)^\s*(CREATE TABLE (sidecar_identity|sidecar_identity_binding)\b|CREATE (UNIQUE )?INDEX (sidecar_identity|sidecar_identity_binding)_\w+ ON (sidecar_identity|sidecar_identity_binding)\b)`)
	refused := regexp.MustCompile(`(?i)\b(DROP|ALTER|RENAME|NOT VALID|TRUNCATE|UPDATE|DELETE|REFERENCES|transaction_timeout)\b`)
	migrations := sidecarMg1CoreMigrations()
	if len(migrations) == 0 {
		t.Fatal("MG1's §5 part is empty")
	}
	for index, migration := range migrations {
		switch value := migration.(type) {
		case *SqlMigration:
			if !statement.MatchString(value.sql) {
				t.Errorf("entry %d is not an addition: %s", index, value.sql)
			}
			if refused.MatchString(value.sql) {
				t.Errorf("entry %d changes what the running release uses, or ties a row to the reaper: %s", index, value.sql)
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

	"sidecar_identity.sidecar_id":          {"api-5", "sidecar_id"},
	"sidecar_identity.network_id":          {"api-5", "network_id"},
	"sidecar_identity.identity_public_key": {"api-5", "identity public key"},
	"sidecar_identity.identity_alg":        {"api-5", "algorithm"},
	"sidecar_identity.status":              {"api-5", "status"},
	"sidecar_identity.create_time":         {"api-5", "create and revoke times"},
	"sidecar_identity.revoke_time":         {"api-5", "create and revoke times"},

	"sidecar_identity_binding.sidecar_id":        {"api-5", "append-only bindings"},
	"sidecar_identity_binding.generation":        {"api-5", "generation"},
	"sidecar_identity_binding.client_id":         {"api-5", "client_id"},
	"sidecar_identity_binding.client_public_key": {"api-5", "client public key"},
	"sidecar_identity_binding.signatures":        {"api-5", "dual signatures"},
	"sidecar_identity_binding.create_time":       {"api-5", "time"},
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
				WHERE table_name IN ('network_auth_key', 'network_admin_event', 'sidecar_identity', 'sidecar_identity_binding')
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

// CODESTYLE's migration rules for MG2's §8 part: its two new tables and an index
// on one, and network_client's one nullable column, added with its lock wait
// bounded.
func TestSidecarMg2AclPartIsAdditive(t *testing.T) {
	statement := regexp.MustCompile(`(?is)^\s*(CREATE TABLE (network_acl_head|network_acl_ack)\b|CREATE INDEX network_acl_ack_\w+ ON network_acl_ack\b|SET LOCAL lock_timeout = '2s';\s*ALTER TABLE network_client ADD COLUMN acl_subject_client_id uuid NULL\s*$)`)
	refused := regexp.MustCompile(`(?i)\b(DROP|RENAME|NOT VALID|NOT NULL DEFAULT gen|TRUNCATE|UPDATE|DELETE|REFERENCES|transaction_timeout)\b`)
	migrations := sidecarMg2AclMigrations()
	if len(migrations) != 4 {
		t.Fatalf("MG2's §8 part holds %d migrations", len(migrations))
	}
	for index, migration := range migrations {
		switch value := migration.(type) {
		case *SqlMigration:
			if !statement.MatchString(value.sql) {
				t.Errorf("entry %d is not an addition: %s", index, value.sql)
			}
			if refused.MatchString(value.sql) {
				t.Errorf("entry %d changes what the running release uses: %s", index, value.sql)
			}
		default:
			t.Fatalf("entry %d is a %T", index, migration)
		}
	}
}

// The metadata row each column of MG2's §8 part shows under (§2.8, I-18), with
// words of that row's sees that name it.
var sidecarMg2AclColumnRows = map[string][2]string{
	"network_acl_head.network_id":   {"api-7", "network_acl_head"},
	"network_acl_head.version":      {"api-7", "each network's newest position"},
	"network_acl_head.chain":        {"api-7", "chain digest"},
	"network_acl_head.has_document": {"api-7", "has_document"},

	"network_acl_ack.client_id": {"api-15", "per client"},
	"network_acl_ack.version":   {"api-15", "the projection version it applied"},
	"network_acl_ack.ack_time":  {"api-15", "and when"},

	"network_client.acl_subject_client_id": {"api-4", "acl_subject_client_id"},
}

// A row of §2.8's list: its id line and the indented lines under it, or "".
func sidecarMetadataRowText(listBytes []byte, rowId string) string {
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

// Each column MG2's §8 part adds maps to a row of §2.8's list whose store names
// its table, or its column on an existing table, and whose sees says the mapped
// words; each mapped column exists.
func TestSidecarMg2AclColumnsMapToMetadataRows(t *testing.T) {
	listPath := "../sidecar/metadata_list.yml"
	listBytes, err := os.ReadFile(listPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("no sidecar checkout beside the server (%s)", listPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		columns := map[string]bool{}
		MaintenanceDb(ctx, func(conn PgConn) {
			result, err := conn.Query(ctx, `
				SELECT table_name || '.' || column_name
				FROM information_schema.columns
				WHERE
					table_name IN ('network_acl_head', 'network_acl_ack') OR
					(table_name = 'network_client' AND column_name = 'acl_subject_client_id')
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
			mapped, ok := sidecarMg2AclColumnRows[column]
			if !ok {
				t.Errorf("%s maps to no row of §2.8's list", column)
				continue
			}
			row := sidecarMetadataRowText(listBytes, mapped[0])
			table, columnName, _ := strings.Cut(column, ".")
			stored := table
			if table == "network_client" {
				stored = table + "." + columnName
			}
			if !strings.Contains(row, stored) {
				t.Errorf("%s: row %s does not store %s", column, mapped[0], stored)
			}
			if !strings.Contains(row, mapped[1]) {
				t.Errorf("%s: row %s does not say %q", column, mapped[0], mapped[1])
			}
		}
		for column := range sidecarMg2AclColumnRows {
			if !columns[column] {
				t.Errorf("%s is mapped but does not exist", column)
			}
		}
	})
}
