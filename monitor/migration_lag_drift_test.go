// The lag.dev drift test's four contracts through the public migration
// signal: a code head ahead of the database is migration lag, never schema
// drift, and from each migration's version every published shape fault is
// drift owned by exactly the migrations that published the shape. Every
// table, column, index and key each migration adds is pinned by its own
// contract.
package monitor

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// The drift test's migrations, in order: its tables with lag_track's kind,
// the calibration, the client country, and the run's k.
const (
	lagDriftTestTables = iota
	lagDriftTestCalibration
	lagDriftTestCountry
	lagDriftTestRunK
)

func lagDriftTestVersions(t testing.TB) [4]int {
	t.Helper()
	versions := [4]int{
		server.LagDriftSchemaMigrationVersion(),
		server.LagDriftCalibrationSchemaMigrationVersion(),
		server.LagDriftClientIpCountrySchemaMigrationVersion(),
		server.LagDriftRunKSchemaMigrationVersion(),
	}
	for index, version := range versions {
		if version == 0 || (0 < index && version != versions[index-1]+1) {
			t.Fatalf("the drift migrations are at %v, want four consecutive versions", versions)
		}
	}
	return versions
}

func lagDriftTestOwners(t testing.TB) [4]string {
	t.Helper()
	var owners [4]string
	for index, version := range lagDriftTestVersions(t) {
		owners[index] = fmt.Sprintf("%s@v%d", migrationCurrentSuffixTestContract(t, version).artifact.name, version)
	}
	return owners
}

// Each contract pins every table, column, key and index it publishes, so a
// rolled-back shape of any of them is drift its migration owns.
func TestMigrationsLagDriftContractsNameTheirArtifacts(t *testing.T) {
	for _, test := range []struct {
		contract snMainnetMigrationContract
		want     []string
	}{
		{lagDriftMigrationContract, []string{
			"public.lag_track", "'kind'", "'drift_test_id'", "'drift_run'",
			"public.lag_drift_test", "'token_hash'", "'client_ip_hash'", "'parent_origins'", "'run_count'", "'result_json'",
			"PRIMARY KEY (drift_test_id)", "UNIQUE (token_hash)",
			"lag_drift_test_network_id_created_at", "lag_drift_test_created_at", "lag_drift_test_client_ip_hash_created_at", "client_ip_hash IS NOT NULL",
			"public.lag_drift_run", "'track_id'", "'state'", "'extenders_json'", "'measured_ms'",
			"PRIMARY KEY (drift_test_id, run)", "UNIQUE (track_id)",
			"lag_drift_run_open_started_at", "<> ''complete''",
			"public.lag_drift_network", "'disable_time'", "'update_time'", "PRIMARY KEY (network_id)"}},
		{lagDriftCalibrationMigrationContract, []string{
			"public.lag_drift_calibration", "'calibration_id'", "'kappa'", "'sigma0_ms'", "'relay_threshold_ms'", "'summary_json'", "'withdrawn_at'",
			"PRIMARY KEY (calibration_id)", "lag_drift_calibration_calibrated_at", "withdrawn_at IS NULL"}},
		{lagDriftClientIpCountryMigrationContract, []string{"public.lag_drift_test", "'client_ip_country_code'", "'client_ip_country_name'"}},
		{lagDriftRunKMigrationContract, []string{"public.lag_drift_run", "'k'", "'k_early'"}},
	} {
		for _, want := range test.want {
			if !strings.Contains(test.contract.query, want) {
				t.Fatalf("the %s contract does not pin %s", test.contract.artifact.name, want)
			}
		}
	}
}

// A database one version behind a contract reads it as absent and only warns
// that it is behind; at its version an absent artifact pages, and a present
// one raises nothing but lag.
func TestMigrationsLagDriftPendingWarnsWithoutPaging(t *testing.T) {
	owners := lagDriftTestOwners(t)
	for index, version := range lagDriftTestVersions(t) {
		artifact := migrationCurrentSuffixTestContract(t, version).artifact
		if artifact.rowColumn != version-589 || artifact.removedVersion != 0 {
			t.Fatalf("contract at %d is %+v", version, artifact)
		}
		for _, test := range []struct {
			name           string
			head           int
			present, drift bool
		}{
			{name: fmt.Sprintf("pending %s", artifact.name), head: version - 1},
			{name: fmt.Sprintf("missing published %s", artifact.name), head: version, drift: true},
			{name: fmt.Sprintf("published %s", artifact.name), head: version, present: true},
		} {
			row := syntheticMigrationArtifactRow(test.head)
			row[artifact.rowColumn] = "f"
			if test.present {
				row[artifact.rowColumn] = "t"
			}
			source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
				if strings.Contains(query, "FROM migration_catalog") {
					return syntheticMigrationCatalogRows(test.head), nil
				}
				return []Row{row}, nil
			}}
			alerts, err := NewMigrationsSignal().Run(context.Background(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			verifyCohortCensusRequireAlerts(t, test.name, test.head, alerts, test.drift, owners[index])
		}
	}
}

// The tables the drift migrations publish or extend.
var lagDriftTestCatalogTables = []string{"lag_track", "lag_drift_test", "lag_drift_run", "lag_drift_network", "lag_drift_calibration"}

// The drift tables' shape as the database has it: each table, column, index
// that backs no key, and key or check (NOT NULL is the column's), with the
// statement that removes it.
func lagDriftTestCatalog(t testing.TB, ctx context.Context) map[string]string {
	t.Helper()
	catalog := map[string]string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT 'table ' || c.relname, format('ALTER TABLE public.%I SET UNLOGGED', c.relname)
			FROM pg_class c
			WHERE c.relnamespace = 'public'::regnamespace AND c.relname = ANY($1::text[])
			UNION ALL
			SELECT 'column ' || c.relname || '.' || a.attname, format('ALTER TABLE public.%I DROP COLUMN %I', c.relname, a.attname)
			FROM pg_attribute a
			JOIN pg_class c ON c.oid = a.attrelid
			WHERE c.relnamespace = 'public'::regnamespace AND c.relname = ANY($1::text[])
			AND 0 < a.attnum AND NOT a.attisdropped
			UNION ALL
			SELECT 'index ' || c.relname || '.' || i.relname, format('DROP INDEX public.%I', i.relname)
			FROM pg_index x
			JOIN pg_class i ON i.oid = x.indexrelid
			JOIN pg_class c ON c.oid = x.indrelid
			WHERE c.relnamespace = 'public'::regnamespace AND c.relname = ANY($1::text[])
			AND NOT EXISTS (SELECT 1 FROM pg_constraint k WHERE k.conrelid = x.indrelid AND k.conindid = x.indexrelid)
			UNION ALL
			SELECT 'constraint ' || c.relname || ' ' || pg_get_constraintdef(k.oid), format('ALTER TABLE public.%I DROP CONSTRAINT %I', c.relname, k.conname)
			FROM pg_constraint k
			JOIN pg_class c ON c.oid = k.conrelid
			WHERE c.relnamespace = 'public'::regnamespace AND c.relname = ANY($1::text[])
			AND k.contype <> 'n'
		`, lagDriftTestCatalogTables)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var entry, removal string
				server.Raise(result.Scan(&entry, &removal))
				catalog[entry] = removal
			}
		})
	})
	return catalog
}

// Whether the database admits a contract's own predicate.
func lagDriftTestAdmits(t testing.TB, ctx context.Context, tx server.PgTx, prefix string, query string) bool {
	t.Helper()
	var admitted bool
	if err := tx.QueryRow(ctx, prefix+"SELECT "+query).Scan(&admitted); err != nil {
		t.Fatal(err)
	}
	return admitted
}

// The real schema before and at each version, then rolled-back shape faults,
// each drift owned by exactly the migrations that published the shape; then
// everything each migration adds, removed in turn and rolled back, refused by
// that migration's own contract.
func TestMigrationsLagDriftPublishedSchemaAndFaults(t *testing.T) {
	versions := lagDriftTestVersions(t)
	owners := lagDriftTestOwners(t)
	faults := []struct {
		name   string
		sql    string
		owners []int
	}{
		{"track kind absent", `ALTER TABLE lag_track DROP COLUMN kind`, []int{lagDriftTestTables}},
		{"track kind without its default", `ALTER TABLE lag_track ALTER COLUMN kind DROP DEFAULT`, []int{lagDriftTestTables}},
		{"track kind nullable", `ALTER TABLE lag_track ALTER COLUMN kind DROP NOT NULL`, []int{lagDriftTestTables}},
		{"track drift run widened", `ALTER TABLE lag_track ALTER COLUMN drift_run TYPE integer`, []int{lagDriftTestTables}},
		{"drift tests absent", `DROP TABLE lag_drift_test`, []int{lagDriftTestTables, lagDriftTestCountry}},
		{"drift tests not durable", `ALTER TABLE lag_drift_test SET UNLOGGED`, []int{lagDriftTestTables}},
		{"drift tests without their key", `ALTER TABLE lag_drift_test DROP CONSTRAINT lag_drift_test_pkey`, []int{lagDriftTestTables}},
		{"token hash not unique", `ALTER TABLE lag_drift_test DROP CONSTRAINT lag_drift_test_token_hash`, []int{lagDriftTestTables}},
		{"address hash required", `ALTER TABLE lag_drift_test ALTER COLUMN client_ip_hash SET NOT NULL`, []int{lagDriftTestTables}},
		{"parent origins widened", `ALTER TABLE lag_drift_test ALTER COLUMN parent_origins TYPE varchar(512)[]`, []int{lagDriftTestTables}},
		{"run count without its default", `ALTER TABLE lag_drift_test ALTER COLUMN run_count DROP DEFAULT`, []int{lagDriftTestTables}},
		{"address hash index over every test", `DROP INDEX lag_drift_test_client_ip_hash_created_at; CREATE INDEX lag_drift_test_client_ip_hash_created_at ON lag_drift_test (created_at)`, []int{lagDriftTestTables}},
		{"runs absent", `DROP TABLE lag_drift_run`, []int{lagDriftTestTables, lagDriftTestRunK}},
		{"runs without their key", `ALTER TABLE lag_drift_run DROP CONSTRAINT lag_drift_run_pkey`, []int{lagDriftTestTables}},
		{"run track not unique", `ALTER TABLE lag_drift_run DROP CONSTRAINT lag_drift_run_track_id`, []int{lagDriftTestTables}},
		{"run extenders nullable", `ALTER TABLE lag_drift_run ALTER COLUMN extenders_json DROP NOT NULL`, []int{lagDriftTestTables}},
		{"open runs index over every run", `DROP INDEX lag_drift_run_open_started_at; CREATE INDEX lag_drift_run_open_started_at ON lag_drift_run (started_at)`, []int{lagDriftTestTables}},
		{"network settings absent", `DROP TABLE lag_drift_network`, []int{lagDriftTestTables}},
		{"network settings without their key", `ALTER TABLE lag_drift_network DROP CONSTRAINT lag_drift_network_pkey`, []int{lagDriftTestTables}},
		{"calibrations absent", `DROP TABLE lag_drift_calibration`, []int{lagDriftTestCalibration}},
		{"calibrations not durable", `ALTER TABLE lag_drift_calibration SET UNLOGGED`, []int{lagDriftTestCalibration}},
		{"kappa nullable", `ALTER TABLE lag_drift_calibration ALTER COLUMN kappa DROP NOT NULL`, []int{lagDriftTestCalibration}},
		{"calibration summary not jsonb", `ALTER TABLE lag_drift_calibration ALTER COLUMN summary_json TYPE json`, []int{lagDriftTestCalibration}},
		{"live calibration index over withdrawn ones", `DROP INDEX lag_drift_calibration_calibrated_at; CREATE INDEX lag_drift_calibration_calibrated_at ON lag_drift_calibration (calibrated_at, calibration_id)`, []int{lagDriftTestCalibration}},
		{"client country absent", `ALTER TABLE lag_drift_test DROP COLUMN client_ip_country_code`, []int{lagDriftTestCountry}},
		{"client country name narrowed", `ALTER TABLE lag_drift_test ALTER COLUMN client_ip_country_name TYPE varchar(128)`, []int{lagDriftTestCountry}},
		{"client country with an invented default", `ALTER TABLE lag_drift_test ALTER COLUMN client_ip_country_code SET DEFAULT 'ZZ'`, []int{lagDriftTestCountry}},
		{"run k absent", `ALTER TABLE lag_drift_run DROP COLUMN k`, []int{lagDriftTestRunK}},
		{"run k_early narrowed", `ALTER TABLE lag_drift_run ALTER COLUMN k_early TYPE real`, []int{lagDriftTestRunK}},
		{"run k required", `ALTER TABLE lag_drift_run ALTER COLUMN k SET NOT NULL`, []int{lagDriftTestRunK}},
	}
	// What each migration adds: its tables with their columns, indexes and
	// keys, or its columns alone.
	wantAdded := [4]int{
		lagDriftTestTables:      3 + (1 + 18 + 3 + 2) + (1 + 18 + 1 + 2) + (1 + 4 + 0 + 1),
		lagDriftTestCalibration: 1 + 11 + 1 + 1,
		lagDriftTestCountry:     2,
		lagDriftTestRunK:        2,
	}
	heads := []int{versions[lagDriftTestTables] - 1}
	heads = append(heads, versions[:]...)
	rows := map[int]Row{}
	alertsAt := map[int][]Alert{}
	catalogs := map[int]map[string]string{}
	var observations [][]Alert
	var added [4]int
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		for _, head := range heads {
			server.ApplyDbMigrationsUpTo(t.Context(), head)
			source := &migrationPingDatabaseSource{}
			alerts, err := NewMigrationsSignal().Run(t.Context(), syntheticSettings(source))
			if err != nil {
				t.Fatal(err)
			}
			rows[head], alertsAt[head] = source.artifactRow, alerts
			catalogs[head] = lagDriftTestCatalog(t, t.Context())
		}
		for _, fault := range faults {
			snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
				if _, err := tx.Exec(ctx, fault.sql); err != nil {
					t.Fatalf("apply %s: %v", fault.name, err)
				}
				alerts, err := NewMigrationsSignal().Run(ctx, syntheticSettings(&snMainnetMigrationTxSource{tx: tx}))
				if err != nil {
					t.Fatalf("inspect %s: %v", fault.name, err)
				}
				observations = append(observations, alerts)
			})
		}
		prefix := snMainnetMigrationProbePrefix(t)
		for index, version := range versions {
			contract := migrationCurrentSuffixTestContract(t, version)
			for _, entry := range slices.Sorted(maps.Keys(catalogs[version])) {
				if _, published := catalogs[version-1][entry]; published {
					continue
				}
				added[index]++
				snMainnetMigrationFaultTx(t, func(ctx context.Context, tx server.PgTx) {
					if !lagDriftTestAdmits(t, ctx, tx, prefix, contract.query) {
						t.Fatalf("the %s contract refuses the schema at its version", contract.artifact.name)
					}
					if _, err := tx.Exec(ctx, catalogs[version][entry]); err != nil {
						t.Fatalf("remove %s: %v", entry, err)
					}
					if lagDriftTestAdmits(t, ctx, tx, prefix, contract.query) {
						t.Fatalf("the %s contract admits the schema without its %s", contract.artifact.name, entry)
					}
				})
			}
		}
		if _, drift := migrationPingDatabaseCheck(t, t.Context()); drift != "" {
			t.Fatalf("rolled-back drift test faults left drift: %s", drift)
		}
	})
	for _, head := range heads {
		if rows[head] == nil {
			t.Fatalf("the schema at %d was not observed", head)
		}
		for index, version := range versions {
			column := migrationCurrentSuffixTestContract(t, version).artifact.rowColumn
			want := "f"
			if version <= head {
				want = "t"
			}
			if rows[head][column] != want {
				t.Fatalf("%s reads %q at %d, want %q", owners[index], rows[head][column], head, want)
			}
		}
		verifyCohortCensusRequireAlerts(t, fmt.Sprintf("drift schema at %d", head), head, alertsAt[head], false, owners[lagDriftTestTables])
	}
	if added != wantAdded {
		t.Fatalf("the drift migrations add %v tables, columns, indexes and keys, want %v", added, wantAdded)
	}
	if len(observations) != len(faults) {
		t.Fatalf("observed %d drift test faults, want %d", len(observations), len(faults))
	}
	for index, alerts := range observations {
		alert := requireAlertClass(t, alerts, "migration-schema-drift")
		if alert.Severity != SeverityPage {
			t.Fatalf("%s does not page", faults[index].name)
		}
		for owner := range owners {
			named, owns := strings.Contains(alert.Observed, owners[owner]), slices.Contains(faults[index].owners, owner)
			if named != owns {
				t.Fatalf("%s: the drift names %s=%t, want %t: %s", faults[index].name, owners[owner], named, owns, alert.Observed)
			}
		}
	}
}
