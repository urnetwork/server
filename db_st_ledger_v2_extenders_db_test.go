// The SN25 extender pool's migration (db_st_ledger_v2_extenders.go) on a real
// database: applied after the migrations before it, beside a running release
// that names the columns it writes, and again.
package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// One column as information_schema shows it.
type stExtenderMigrationColumn struct {
	name, dataType, udtName string
	length, precision       int
	scale                   int
	nullable                bool
	hasDefault              bool
}

// names nil reads every column of table.
func stExtenderMigrationColumns(t testing.TB, ctx context.Context, table string, names ...string) []stExtenderMigrationColumn {
	t.Helper()
	columns := []stExtenderMigrationColumn{}
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `
			SELECT column_name, data_type, udt_name,
				coalesce(character_maximum_length, 0), coalesce(numeric_precision, 0), coalesce(numeric_scale, 0),
				is_nullable = 'YES', column_default IS NOT NULL
			FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = $1 AND ($2::text[] IS NULL OR column_name = ANY($2::text[]))
			ORDER BY ordinal_position
		`, table, names)
		WithPgResult(result, err, func() {
			for result.Next() {
				var column stExtenderMigrationColumn
				Raise(result.Scan(&column.name, &column.dataType, &column.udtName, &column.length, &column.precision, &column.scale, &column.nullable, &column.hasDefault))
				columns = append(columns, column)
			}
		})
	}, OptReadOnly(), OptNoRetry())
	return columns
}

func stExtenderMigrationPrimaryKey(t testing.TB, ctx context.Context, table string) []string {
	t.Helper()
	key := []string{}
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `
			SELECT a.attname
			FROM pg_index AS i
			JOIN pg_attribute AS a ON a.attrelid = i.indrelid AND a.attnum = ANY(i.indkey)
			WHERE i.indrelid = to_regclass('public.' || $1) AND i.indisprimary
			ORDER BY array_position(i.indkey::int2[], a.attnum)
		`, table)
		WithPgResult(result, err, func() {
			for result.Next() {
				var name string
				Raise(result.Scan(&name))
				key = append(key, name)
			}
		})
	}, OptReadOnly(), OptNoRetry())
	return key
}

// Every row of the pool's state, in key order, as text.
func stExtenderMigrationState(t testing.TB, ctx context.Context) []string {
	t.Helper()
	state := []string{}
	MaintenanceDb(ctx, func(conn PgConn) {
		for _, sql := range []string{
			`SELECT row(r.*)::text FROM st_ledger_v2_epoch_rows AS r ORDER BY deployment_key, no_id, epoch`,
			`SELECT row(c.*)::text FROM st_ledger_v2_extender_census AS c ORDER BY deployment_key, no_id, epoch`,
			`SELECT row(a.*)::text FROM st_amendments AS a ORDER BY deployment_key`,
			`SELECT row(s.*)::text FROM st_ledger_v2_extender_shadow AS s ORDER BY deployment_key, no_id, epoch`,
			`SELECT row(p.*)::text FROM st_platform_endpoint AS p ORDER BY client_id, register_time`,
		} {
			result, err := conn.Query(ctx, sql)
			WithPgResult(result, err, func() {
				for result.Next() {
					var row string
					Raise(result.Scan(&row))
					state = append(state, row)
				}
			})
		}
	}, OptReadOnly(), OptNoRetry())
	return state
}

func stExtenderMigrationCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return fmt.Sprintf("%v", err)
}

// Applied after the migrations before it, the migration creates its four
// tables with the columns, keys and checks the model reads, and adds the six
// nullable columns without defaults to the epoch rows, which the running
// release's rows read as NULL. The release's insert, which names its columns,
// still stores. Run again it changes nothing, and the head still applies.
func TestStLedgerV2ExtenderMigrationOnTheDatabase(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		version := sqlMigrationVersion(StLedgerV2ExtenderSchemaSql)
		if version < 1 || migrations[version-1] != stLedgerV2ExtenderMigration {
			t.Fatalf("the extender pool migration is not in the migration list (version %d)", version)
		}
		const key = "945:0x2000000000000000000000000000000000000002"
		// the insert of the release that predates the columns
		insertRunningRelease := func(epoch int64, chunks []string, declaredBy *string) {
			MaintenanceTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `
					INSERT INTO st_ledger_v2_epoch_rows (deployment_key, no_id, epoch, provider_chunks, declared_by, create_time)
					VALUES ($1, 1, $2, $3, $4, $5)
				`, key, epoch, chunks, declaredBy, NowUtc()))
			})
		}
		regclass := func(name string) bool {
			var exists bool
			MaintenanceDb(ctx, func(conn PgConn) {
				Raise(conn.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&exists))
			}, OptReadOnly(), OptNoRetry())
			return exists
		}

		componentColumns := []string{"extender_bytes", "extender_census_chunks", "extender_intra_platform_bytes", "extender_intra_network_bytes", "extender_contracts", "extender_payers"}
		ApplyDbMigrationsUpTo(ctx, version-1)
		if regclass("st_amendments") || regclass("st_ledger_v2_extender_census") || regclass("st_ledger_v2_extender_shadow") || regclass("st_platform_endpoint") ||
			len(stExtenderMigrationColumns(t, ctx, "st_ledger_v2_epoch_rows", componentColumns...)) != 0 {
			t.Fatal("the extender pool's state exists before its migration")
		}
		declared := "operator@example"
		insertRunningRelease(5, []string{"p5"}, nil)
		insertRunningRelease(6, nil, &declared)

		ApplyDbMigrationsUpTo(ctx, version)
		if got, want := stExtenderMigrationColumns(t, ctx, "st_ledger_v2_epoch_rows", componentColumns...), []stExtenderMigrationColumn{
			{name: "extender_bytes", dataType: "numeric", udtName: "numeric", precision: 20, scale: 0, nullable: true},
			{name: "extender_census_chunks", dataType: "ARRAY", udtName: "_text", nullable: true},
			{name: "extender_intra_platform_bytes", dataType: "numeric", udtName: "numeric", precision: 20, scale: 0, nullable: true},
			{name: "extender_intra_network_bytes", dataType: "numeric", udtName: "numeric", precision: 20, scale: 0, nullable: true},
			{name: "extender_contracts", dataType: "bigint", udtName: "int8", precision: 64, nullable: true},
			{name: "extender_payers", dataType: "bigint", udtName: "int8", precision: 64, nullable: true},
		}; !slices.Equal(got, want) {
			t.Fatalf("epoch rows columns %+v, want %+v", got, want)
		}
		if got, want := stExtenderMigrationColumns(t, ctx, "st_ledger_v2_extender_shadow"), []stExtenderMigrationColumn{
			{name: "deployment_key", dataType: "character varying", udtName: "varchar", length: 96},
			{name: "no_id", dataType: "bigint", udtName: "int8", precision: 64},
			{name: "epoch", dataType: "bigint", udtName: "int8", precision: 64},
			{name: "report_ref", dataType: "character varying", udtName: "varchar", length: 71},
			{name: "report", dataType: "bytea", udtName: "bytea"},
			{name: "create_time", dataType: "timestamp without time zone", udtName: "timestamp"},
		}; !slices.Equal(got, want) {
			t.Fatalf("st_ledger_v2_extender_shadow columns %+v, want %+v", got, want)
		}
		if got, want := stExtenderMigrationColumns(t, ctx, "st_platform_endpoint"), []stExtenderMigrationColumn{
			{name: "client_id", dataType: "uuid", udtName: "uuid"},
			{name: "network_id", dataType: "uuid", udtName: "uuid"},
			{name: "kind", dataType: "character varying", udtName: "varchar", length: 16},
			{name: "register_time", dataType: "timestamp without time zone", udtName: "timestamp"},
			{name: "registered_by", dataType: "character varying", udtName: "varchar", length: 256},
			{name: "revoke_time", dataType: "timestamp without time zone", udtName: "timestamp", nullable: true},
			{name: "revoked_by", dataType: "character varying", udtName: "varchar", length: 256, nullable: true},
		}; !slices.Equal(got, want) {
			t.Fatalf("st_platform_endpoint columns %+v, want %+v", got, want)
		}
		if key := stExtenderMigrationPrimaryKey(t, ctx, "st_ledger_v2_extender_shadow"); !slices.Equal(key, []string{"deployment_key", "no_id", "epoch"}) {
			t.Fatalf("st_ledger_v2_extender_shadow key %v", key)
		}
		if key := stExtenderMigrationPrimaryKey(t, ctx, "st_platform_endpoint"); !slices.Equal(key, []string{"client_id", "register_time"}) {
			t.Fatalf("st_platform_endpoint key %v", key)
		}
		if got, want := stExtenderMigrationColumns(t, ctx, "st_amendments"), []stExtenderMigrationColumn{
			{name: "deployment_key", dataType: "character varying", udtName: "varchar", length: 96},
			{name: "raw", dataType: "bytea", udtName: "bytea"},
			{name: "update_time", dataType: "timestamp without time zone", udtName: "timestamp"},
		}; !slices.Equal(got, want) {
			t.Fatalf("st_amendments columns %+v, want %+v", got, want)
		}
		if got, want := stExtenderMigrationColumns(t, ctx, "st_ledger_v2_extender_census"), []stExtenderMigrationColumn{
			{name: "deployment_key", dataType: "character varying", udtName: "varchar", length: 96},
			{name: "no_id", dataType: "bigint", udtName: "int8", precision: 64},
			{name: "epoch", dataType: "bigint", udtName: "int8", precision: 64},
			{name: "census", dataType: "bytea", udtName: "bytea"},
			{name: "census_rows", dataType: "bigint", udtName: "int8", precision: 64},
			{name: "capture_time", dataType: "timestamp without time zone", udtName: "timestamp"},
		}; !slices.Equal(got, want) {
			t.Fatalf("st_ledger_v2_extender_census columns %+v, want %+v", got, want)
		}
		if key := stExtenderMigrationPrimaryKey(t, ctx, "st_amendments"); !slices.Equal(key, []string{"deployment_key"}) {
			t.Fatalf("st_amendments key %v", key)
		}
		if key := stExtenderMigrationPrimaryKey(t, ctx, "st_ledger_v2_extender_census"); !slices.Equal(key, []string{"deployment_key", "no_id", "epoch"}) {
			t.Fatalf("st_ledger_v2_extender_census key %v", key)
		}

		// the running release's rows have no component, and its insert still
		// stores
		insertRunningRelease(7, []string{}, nil)
		var rows, components int
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `
				SELECT count(*), count(*) FILTER (WHERE extender_bytes IS NOT NULL OR extender_census_chunks IS NOT NULL
					OR extender_intra_platform_bytes IS NOT NULL OR extender_intra_network_bytes IS NOT NULL OR extender_contracts IS NOT NULL OR extender_payers IS NOT NULL)
				FROM st_ledger_v2_epoch_rows
			`).Scan(&rows, &components))
		}, OptReadOnly(), OptNoRetry())
		if rows != 3 || components != 0 {
			t.Fatalf("%d rows, %d with an extender component; want 3 and none", rows, components)
		}

		// the new release's writes: Z up to 2^64 - 1, exactly
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `
				INSERT INTO st_ledger_v2_epoch_rows (deployment_key, no_id, epoch, provider_chunks, declared_by, create_time, extender_bytes, extender_census_chunks,
					extender_intra_platform_bytes, extender_intra_network_bytes, extender_contracts, extender_payers)
				VALUES ($1, 1, 8, ARRAY['p8'], NULL, $2, $3::text::numeric, ARRAY['c8'], $4::text::numeric, $5::text::numeric, 9223372036854775807, 1)
			`, key, NowUtc(), "18446744073709551615", "18446744073709551614", "1"))
			RaisePgResult(tx.Exec(ctx, `
				INSERT INTO st_ledger_v2_extender_shadow (deployment_key, no_id, epoch, report_ref, report, create_time)
				VALUES ($1, 1, 6, $2, $3, $4)
			`, key, "sha256:"+strings.Repeat("ab", 32), []byte("{}"), NowUtc()))
			RaisePgResult(tx.Exec(ctx, `
				INSERT INTO st_platform_endpoint (client_id, network_id, kind, register_time, registered_by)
				VALUES ('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002', 'tlsedge', $1, 'operator@example')
			`, NowUtc()))
			RaisePgResult(tx.Exec(ctx, `
				INSERT INTO st_ledger_v2_extender_census (deployment_key, no_id, epoch, census, census_rows, capture_time)
				VALUES ($1, 1, 8, $2, 1, $3)
			`, key, []byte{1, 2, 3}, NowUtc()))
			RaisePgResult(tx.Exec(ctx, `INSERT INTO st_amendments (deployment_key, raw, update_time) VALUES ($1, $2, $3)`, key, []byte("sn: 25\n"), NowUtc()))
		})
		var z string
		MaintenanceDb(ctx, func(conn PgConn) {
			Raise(conn.QueryRow(ctx, `SELECT extender_bytes::text FROM st_ledger_v2_epoch_rows WHERE deployment_key = $1 AND epoch = 8`, key).Scan(&z))
		}, OptReadOnly(), OptNoRetry())
		if z != "18446744073709551615" {
			t.Fatalf("Z read back as %s", z)
		}
		// and the checks refuse what the model never writes
		for name, c := range map[string]struct {
			sql  string
			code string
		}{
			"no_id 0":                 {`INSERT INTO st_ledger_v2_extender_census VALUES ($1, 0, 9, ''::bytea, 0, now())`, "23514"},
			"epoch 0":                 {`INSERT INTO st_ledger_v2_extender_census VALUES ($1, 1, 0, ''::bytea, 0, now())`, "23514"},
			"negative rows":           {`INSERT INTO st_ledger_v2_extender_census VALUES ($1, 1, 9, ''::bytea, -1, now())`, "23514"},
			"no census":               {`INSERT INTO st_ledger_v2_extender_census VALUES ($1, 1, 9, NULL, 0, now())`, "23502"},
			"a second census":         {`INSERT INTO st_ledger_v2_extender_census VALUES ($1, 1, 8, ''::bytea, 0, now())`, "23505"},
			"no amendments file":      {`INSERT INTO st_amendments VALUES ($1 || 'x', NULL, now())`, "23502"},
			"a Z past 20 digits":      {`UPDATE st_ledger_v2_epoch_rows SET extender_bytes = 184467440737095516150 WHERE deployment_key = $1 AND epoch = 8`, "22003"},
			"a second shadow":         {`INSERT INTO st_ledger_v2_extender_shadow VALUES ($1, 1, 6, 'x', ''::bytea, now())`, "23505"},
			"a shadow of no_id 0":     {`INSERT INTO st_ledger_v2_extender_shadow VALUES ($1, 0, 6, 'x', ''::bytea, now())`, "23514"},
			"another kind":            {`INSERT INTO st_platform_endpoint VALUES ('00000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-000000000002', 'egress', now(), $1)`, "23514"},
			"no registrant":           {`INSERT INTO st_platform_endpoint VALUES ('00000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-000000000002', 'smtpedge', now(), '' || left($1, 0))`, "23514"},
			"revoked before it began": {`INSERT INTO st_platform_endpoint VALUES ('00000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-000000000002', 'mxedge', now(), $1, now() - interval '1 second', $1)`, "23514"},
			"revoked by no one":       {`INSERT INTO st_platform_endpoint VALUES ('00000000-0000-0000-0000-000000000003', '00000000-0000-0000-0000-000000000002', 'mxedge', now() - interval '1 hour', $1, now(), NULL)`, "23514"},
		} {
			MaintenanceDb(ctx, func(conn PgConn) {
				_, err := conn.Exec(ctx, c.sql, key)
				if code := stExtenderMigrationCode(err); code != c.code {
					t.Fatalf("%s: %s, want SQLSTATE %s", name, code, c.code)
				}
			}, OptNoRetry())
		}

		before := stExtenderMigrationState(t, ctx)
		if len(before) != 8 || !strings.Contains(before[3], "18446744073709551615") {
			t.Fatalf("state %v", before)
		}
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, StLedgerV2ExtenderSchemaSql))
		}, OptNoRetry())
		if after := stExtenderMigrationState(t, ctx); !slices.Equal(after, before) {
			t.Fatalf("running the migration again changed the state:\n%v\n%v", before, after)
		}

		// and the head still applies from here
		ApplyDbMigrations(ctx)
		if after := stExtenderMigrationState(t, ctx); !slices.Equal(after, before) {
			t.Fatalf("the migrations after it changed the state:\n%v\n%v", before, after)
		}
	})
}
