//go:build sidecar_pending

package server

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// CODESTYLE's migration rules for §12's parts (DNS-M5-09, DNS-M6-01, DNS-M6-02, DNS-M6-06): each entry creates one of
// §12.14's tables, or an index on one, or adds one nullable column with no default to a table an earlier
// part creates, and touches nothing the running release reads or writes; kinds and states are varchar,
// never ENUM, and nothing references another table. An index on a table an earlier group creates is built
// online (§19.7), in exactly the forms its part lists.
func TestSidecarDnsPartsAreAdditive(t *testing.T) {
	refused := regexp.MustCompile(`(?i)\b(DROP|RENAME|ALTER|NOT VALID|TRUNCATE|UPDATE|DELETE|REFERENCES|ENUM|transaction_timeout)\b`)
	refusedButAlter := regexp.MustCompile(`(?i)\b(DROP|RENAME|NOT VALID|TRUNCATE|UPDATE|DELETE|REFERENCES|ENUM|transaction_timeout|SET NOT NULL|DEFAULT)\b`)
	normalize := func(sql string) string {
		return strings.Join(strings.Fields(sql), " ")
	}
	parts := []struct {
		name       string
		migrations []any
		statement  *regexp.Regexp
		// the part adds nullable columns, which ALTER names
		allowAlter bool
		// the part's online index builds in order, each its restart step, its concurrent build and the
		// audit's transactional form, whitespace aside
		online [][3]string
	}{
		{
			name:       "MG2's §12 part",
			migrations: sidecarMg2DnsMigrations(),
			statement:  regexp.MustCompile(`(?is)^\s*(CREATE TABLE dns_label\b|CREATE INDEX dns_label_\w+ ON dns_label\b)`),
		},
		{
			name:       "MG5's §12 part",
			migrations: sidecarMg5DnsMigrations(),
			statement: regexp.MustCompile(`(?is)^\s*(CREATE TABLE (dns_root|network_dns_zone|network_dns_record|dns_zone_journal|network_dns_custom_host|dns_custom_host_map|dns_custom_host_map_applied|dns_restore_incident)\b|` +
				`CREATE INDEX (network_dns_zone|network_dns_record|dns_zone_journal|network_dns_custom_host|dns_custom_host_map|dns_restore_incident)_\w+ ON (network_dns_zone|network_dns_record|dns_zone_journal|network_dns_custom_host|dns_custom_host_map|dns_restore_incident)\b)`),
		},
		{
			// nullable columns on tables earlier parts create, an index on MG6's own network_domain, and one on
			// MG5's network_dns_zone, which holds rows by the time MG6 applies, so it is built online
			name:       "MG6's §12 part",
			migrations: sidecarMg6DnsMigrations(),
			statement: regexp.MustCompile(`(?is)^\s*(ALTER TABLE dns_label ADD COLUMN certified_time timestamp NULL\s*$|` +
				`ALTER TABLE network_domain ADD COLUMN acme_token varchar\(26\) NULL\s*$|` +
				`CREATE UNIQUE INDEX network_domain_acme_token ON network_domain \(acme_token\) WHERE acme_token IS NOT NULL\s*$|` +
				`ALTER TABLE network_dns_zone ADD COLUMN report_token varchar\(26\) NULL\s*$)`),
			allowAlter: true,
			online: [][3]string{{
				`DROP INDEX CONCURRENTLY IF EXISTS network_dns_zone_report_token`,
				`CREATE UNIQUE INDEX CONCURRENTLY network_dns_zone_report_token ON network_dns_zone (report_token) WHERE report_token IS NOT NULL`,
				`CREATE UNIQUE INDEX network_dns_zone_report_token ON network_dns_zone (report_token) WHERE report_token IS NOT NULL`,
			}},
		},
	}
	for _, part := range parts {
		if len(part.migrations) == 0 {
			t.Fatalf("%s is empty", part.name)
		}
		online := [][3]string{}
		for index, migration := range part.migrations {
			switch value := migration.(type) {
			case *SqlMigration:
				if !part.statement.MatchString(value.sql) {
					t.Errorf("%s entry %d is not an addition: %s", part.name, index, value.sql)
				}
				partRefused := refused
				if part.allowAlter {
					partRefused = refusedButAlter
				}
				if partRefused.MatchString(value.sql) {
					t.Errorf("%s entry %d changes what the running release uses: %s", part.name, index, value.sql)
				}
			case *OnlineSqlMigration:
				online = append(online, [3]string{normalize(value.recoverySql), normalize(value.sql), normalize(value.auditSql)})
			default:
				t.Fatalf("%s entry %d is a %T", part.name, index, migration)
			}
		}
		if !slices.Equal(online, part.online) {
			t.Errorf("%s builds online %q, want %q", part.name, online, part.online)
		}
	}
}

// MG6's report_token index on the zones MG5's table holds by then, past what an interrupted concurrent build
// of it left (§19.7; plan-DNS-rua-token): the build ends valid with the index's name, column and predicate,
// keeps every zone and mints no token; a token then names one zone, any number of zones have none, and
// GetDnsNetworkForReportToken's lookup reads through the index.
func TestSidecarMg6ReportTokenIndexBuildsOnlineOverMg5Zones(t *testing.T) {
	indexSql := ""
	for _, migration := range sidecarMg6DnsMigrations() {
		if value, ok := migration.(*OnlineSqlMigration); ok && strings.Contains(value.sql, "network_dns_zone_report_token") {
			indexSql = value.sql
		}
	}
	if indexSql == "" {
		t.Fatal("MG6's §12 part builds no network_dns_zone_report_token online")
	}
	// the build's 1-based version: a database at version-1 has every entry before it, report_token's column
	// included
	version := migrationVersionOf(indexSql)
	(&TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		ApplyDbMigrationsUpTo(ctx, version-1)

		// zones written while MG5's binary runs, all in one root
		root := "zones.example"
		createTime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		networkIds := []Id{NewId(), NewId(), NewId(), NewId()}
		MaintenanceTx(ctx, func(tx PgTx) {
			for _, networkId := range networkIds {
				RaisePgResult(tx.Exec(ctx, `INSERT INTO network_dns_zone (root, network_id, create_time) VALUES ($1, $2, $3)`, root, networkId, createTime))
			}
		})
		// what an interrupted build leaves, an invalid index of the name: a concurrent unique build over the
		// zones' one root fails its scan and leaves exactly that
		MaintenanceDb(ctx, func(conn PgConn) {
			if _, err := conn.Exec(ctx, `CREATE UNIQUE INDEX CONCURRENTLY network_dns_zone_report_token ON network_dns_zone (root)`); err == nil {
				t.Fatal("the interrupted build's control did not fail")
			}
			var valid bool
			Raise(conn.QueryRow(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'network_dns_zone_report_token'::regclass`).Scan(&valid))
			if valid {
				t.Fatal("the failed concurrent build left no invalid index")
			}
		}, OptReadWrite(), OptNoRetry())

		ApplyDbMigrations(ctx)
		if got := DbVersion(ctx); got != MigrationCount() {
			t.Fatalf("version = %d of %d", got, MigrationCount())
		}

		MaintenanceDb(ctx, func(conn PgConn) {
			var definition string
			var valid, ready, live bool
			Raise(conn.QueryRow(ctx, `
				SELECT pg_get_indexdef(catalog.indexrelid), catalog.indisvalid, catalog.indisready, catalog.indislive
				FROM pg_index AS catalog
				WHERE catalog.indexrelid = 'network_dns_zone_report_token'::regclass AND catalog.indrelid = 'network_dns_zone'::regclass
			`).Scan(&definition, &valid, &ready, &live))
			want := "CREATE UNIQUE INDEX network_dns_zone_report_token ON public.network_dns_zone USING btree (report_token) WHERE (report_token IS NOT NULL)"
			if definition != want || !valid || !ready || !live {
				t.Fatalf("index %q valid/ready/live %t/%t/%t, want %q", definition, valid, ready, live, want)
			}
			var zoneCount, tokenCount int
			Raise(conn.QueryRow(ctx, `SELECT count(*), count(report_token) FROM network_dns_zone WHERE root = $1`, root).Scan(&zoneCount, &tokenCount))
			if zoneCount != len(networkIds) || tokenCount != 0 {
				t.Fatalf("%d zones with %d tokens, want %d with none", zoneCount, tokenCount, len(networkIds))
			}
		}, OptReadOnly(), OptNoRetry())

		// a token names one zone; the other two zones keep none
		token := strings.Repeat("a", 26)
		otherToken := strings.Repeat("b", 26)
		MaintenanceDb(ctx, func(conn PgConn) {
			setToken := func(networkId Id, token string) error {
				_, err := conn.Exec(ctx, `UPDATE network_dns_zone SET report_token = $3 WHERE root = $1 AND network_id = $2`, root, networkId, token)
				return err
			}
			Raise(setToken(networkIds[0], token))
			var pgErr *pgconn.PgError
			if err := setToken(networkIds[1], token); !errors.As(err, &pgErr) || pgErr.Code != "23505" || pgErr.ConstraintName != "network_dns_zone_report_token" {
				t.Fatalf("a second zone took a zone's token: %v", err)
			}
			Raise(setToken(networkIds[1], otherToken))
		}, OptReadWrite(), OptNoRetry())

		MaintenanceTx(ctx, func(tx PgTx) {
			var noneCount int
			Raise(tx.QueryRow(ctx, `SELECT count(*) FROM network_dns_zone WHERE root = $1 AND report_token IS NULL`, root).Scan(&noneCount))
			if noneCount != 2 {
				t.Fatalf("%d zones have no token, want 2", noneCount)
			}
			// the lookup's own shape, with sequential scans off so the plan shows whether the index serves it
			RaisePgResult(tx.Exec(ctx, `SET LOCAL enable_seqscan = off`))
			plan := []string{}
			result, err := tx.Query(ctx, `EXPLAIN SELECT network_id FROM network_dns_zone WHERE report_token = $1`, token)
			WithPgResult(result, err, func() {
				for result.Next() {
					var line string
					Raise(result.Scan(&line))
					plan = append(plan, line)
				}
			})
			if !strings.Contains(strings.Join(plan, "\n"), "network_dns_zone_report_token") {
				t.Fatalf("the token lookup does not read the index:\n%s", strings.Join(plan, "\n"))
			}
			var networkId Id
			Raise(tx.QueryRow(ctx, `SELECT network_id FROM network_dns_zone WHERE report_token = $1`, token).Scan(&networkId))
			if networkId != networkIds[0] {
				t.Fatalf("the token names %s, want %s", networkId, networkIds[0])
			}
		})
	})
}
