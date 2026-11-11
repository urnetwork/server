// Capture the flag's migration (ctf_schema.go), read back from the migration
// list.
package server

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The migration is a plain SQL migration appended after every migration that
// was on main before it, and its version is its position.
func TestCtfSchemaMigrationIsAppended(t *testing.T) {
	version := CtfSchemaMigrationVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != ctfSchemaMigration {
		t.Fatalf("ctf schema migration version %d is not its position", version)
	}
	if ipv6 := migrationVersion(providerEgressIpv6PlaceMigration); version <= ipv6 {
		t.Fatalf("ctf schema migration %d is not after the IPv6 place migration %d", version, ipv6)
	}
	if sqlMigrationVersion(ctfSchemaSql) != version {
		t.Fatal("the ctf schema SQL is in the list more than once, or not as its migration")
	}
}

// The tables the census, the driver and the api read and write, each created
// only when absent after a five-second lock bound, and nothing else: no table
// that exists is altered, rewritten, sampled or deleted from.
func TestCtfSchemaMigrationCreatesOnlyItsTablesAndIsBounded(t *testing.T) {
	sql := ctfSchemaSql
	want := []string{
		"ctf_presence_sample", "ctf_presence", "ctf_presence_outage",
		"ctf_census", "ctf_census_object", "ctf_census_member", "ctf_census_pot", "ctf_census_exclusion",
		"ctf_census_commitment", "ctf_draw",
		"ctf_epoch", "ctf_award", "ctf_pot", "ctf_pending_award",
		"ctf_ledger", "ctf_recycle",
	}
	created := []string{}
	for _, match := range regexp.MustCompile(`CREATE TABLE IF NOT EXISTS (\w+) \(`).FindAllStringSubmatch(sql, -1) {
		created = append(created, match[1])
	}
	if !slices.Equal(created, want) {
		t.Fatalf("ctf schema creates %v, want %v", created, want)
	}
	if strings.Count(sql, "CREATE TABLE") != len(want) {
		t.Fatal("ctf schema creates a table that is not created only when absent")
	}
	indexes := regexp.MustCompile(`CREATE INDEX IF NOT EXISTS \w+ ON (\w+) `).FindAllStringSubmatch(sql, -1)
	if strings.Count(sql, "CREATE INDEX") != len(indexes) || strings.Contains(sql, "UNIQUE INDEX") {
		t.Fatal("ctf schema creates an index that is not created only when absent")
	}
	for _, index := range indexes {
		if !slices.Contains(want, index[1]) {
			t.Errorf("ctf schema indexes %s, which it does not create", index[1])
		}
	}
	bound := strings.Index(sql, "SET LOCAL lock_timeout = '5s';")
	if bound < 0 || strings.Index(sql, "CREATE") < bound {
		t.Error("ctf schema does not bound its lock wait before it creates anything")
	}
	if regexp.MustCompile(`(?i)\b(ALTER|UPDATE|INSERT|ANALYZE|VACUUM|DELETE|TRUNCATE|DROP|CLUSTER|REFERENCES|CONCURRENTLY)\b`).MatchString(sql) {
		t.Error("ctf schema touches more than its own new tables")
	}
}

// Every table is keyed by the deployment and the operator, so a replaced
// deployment or a second operator never mixes with another's rows, and each
// keeps the shape the model writes.
func TestCtfSchemaKeysAndShapes(t *testing.T) {
	tables := map[string]string{}
	for _, match := range regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+) \((.*?)\n\t\);`).FindAllStringSubmatch(ctfSchemaSql, -1) {
		tables[match[1]] = match[2]
	}
	if len(tables) != 16 {
		t.Fatalf("parsed %d ctf tables, want 16", len(tables))
	}
	for name, body := range tables {
		if !strings.Contains(body, "deployment_key varchar(96) NOT NULL,") || !strings.Contains(body, "no_id bigint NOT NULL CHECK (no_id > 0),") {
			t.Errorf("%s is not keyed by the deployment and the operator", name)
		}
		if name != "ctf_presence_outage" && !strings.Contains(body, "PRIMARY KEY (deployment_key, no_id, ") {
			t.Errorf("%s's primary key does not lead with the deployment and the operator", name)
		}
	}
	for table, markers := range map[string][]string{
		"ctf_presence": {
			"samples bytea NOT NULL CHECK (octet_length(samples) = 84),",
			"PRIMARY KEY (deployment_key, no_id, epoch, role, client_id, country_code),",
		},
		"ctf_presence_sample": {"CHECK (0 <= sample_index AND sample_index < 672)"},
		"ctf_census": {
			"census_hash bytea NOT NULL CHECK (octet_length(census_hash) = 32),",
			"excused_samples smallint[] NOT NULL,",
			"PRIMARY KEY (deployment_key, no_id, epoch),",
		},
		"ctf_census_member": {
			"failed integer NOT NULL",
			"CHECK (candidate = (reason = 0)),",
			"CHECK (candidate = (failed = 0)),",
			"coldkey bytea NULL CHECK (octet_length(coldkey) = 32),",
		},
		"ctf_census_exclusion": {"CHECK (1 <= reason AND reason <= 10)", "count integer NOT NULL CHECK (count > 0),"},
		"ctf_draw":             {"signature bytea NOT NULL CHECK (octet_length(signature) = 48),"},
		"ctf_award":            {"PRIMARY KEY (deployment_key, no_id, epoch, role, country_code, rank)"},
		"ctf_pending_award":    {"amount numeric(78,0) NOT NULL CHECK (amount > 0),"},
		"ctf_ledger":           {"CHECK (shadow = (status = 'shadow'))"},
		"ctf_recycle":          {"PRIMARY KEY (deployment_key, no_id, block_number, log_index)"},
	} {
		for _, marker := range markers {
			if !strings.Contains(tables[table], marker) {
				t.Errorf("%s lacks %q", table, marker)
			}
		}
	}
}
