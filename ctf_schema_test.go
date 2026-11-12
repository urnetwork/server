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

// ctf_award.network_id is its own plain SQL migration, after the one that
// creates ctf_award and after the extender address ports migration that was
// the last on main before it, at its position in the list.
func TestCtfAwardNetworkMigrationIsAppended(t *testing.T) {
	version := CtfAwardNetworkMigrationVersion()
	if version < 1 || len(migrations) < version || migrations[version-1] != ctfAwardNetworkMigration {
		t.Fatalf("ctf award network migration version %d is not its position", version)
	}
	if schema := CtfSchemaMigrationVersion(); version <= schema {
		t.Fatalf("ctf award network migration %d is not after the ctf schema migration %d", version, schema)
	}
	if ports := NetworkExtenderAddressPortsSchemaMigrationVersion(); version <= ports {
		t.Fatalf("ctf award network migration %d is not after the extender address ports migration %d", version, ports)
	}
	if sqlMigrationVersion(ctfAwardNetworkSql) != version {
		t.Fatal("the ctf award network SQL is in the list more than once, or not as its migration")
	}
}

// The migration bounds its lock waits before anything else, touches ctf_award
// alone, adds only the nullable column and its index when absent, and sets
// only the rows whose network is still null: from the census member of the
// award's epoch, role and client, else the client's network_client row.
func TestCtfAwardNetworkMigrationIsBoundedAndAdditive(t *testing.T) {
	sql := ctfAwardNetworkSql
	bound := strings.Index(sql, "SET LOCAL lock_timeout = '5s';")
	if bound < 0 || strings.Index(sql, "ALTER") < bound {
		t.Error("the migration does not bound its lock wait before it alters ctf_award")
	}
	for _, statement := range []string{
		"ALTER TABLE ctf_award ADD COLUMN IF NOT EXISTS network_id uuid NULL;",
		"CREATE INDEX IF NOT EXISTS ctf_award_network ON ctf_award (deployment_key, no_id, network_id, epoch);",
		"UPDATE ctf_award SET network_id = COALESCE(",
		"WHERE network_id IS NULL;",
		"member.epoch = ctf_award.epoch AND member.role = ctf_award.role",
		"member.client_id = ctf_award.client_id",
		"WHERE network_client.client_id = ctf_award.client_id",
	} {
		if !strings.Contains(sql, statement) {
			t.Errorf("the migration lacks %q", statement)
		}
	}
	if strings.Index(sql, "FROM ctf_census_member") > strings.Index(sql, "FROM network_client") {
		t.Error("the migration prefers network_client's network to the census member's")
	}
	changed := regexp.MustCompile(`(?i)\b(?:ALTER TABLE|UPDATE|CREATE INDEX IF NOT EXISTS \w+ ON|INSERT INTO|DELETE FROM)\s+(\w+)`).FindAllStringSubmatch(sql, -1)
	if len(changed) != 3 {
		t.Fatalf("the migration changes %d relations, want ctf_award's column, its rows and its index", len(changed))
	}
	for _, match := range changed {
		if match[1] != "ctf_award" {
			t.Errorf("the migration changes %s", match[1])
		}
	}
	if regexp.MustCompile(`(?i)\b(DROP|TRUNCATE|VACUUM|ANALYZE|CLUSTER|REFERENCES|CONCURRENTLY|NOT NULL|DEFAULT)\b`).MatchString(sql) {
		t.Error("the migration does more than add a nullable column, its values and its index")
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
	for _, match := range regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+) \((.*?)\n\t\)( WITH \(fillfactor = \d+\))?;`).FindAllStringSubmatch(ctfSchemaSql, -1) {
		tables[match[1]] = match[2]
	}
	if !strings.Contains(ctfSchemaSql, "\t) WITH (fillfactor = 50);\n\n\tCREATE TABLE IF NOT EXISTS ctf_presence_outage") {
		t.Error("ctf_presence leaves no room for its rows' new versions")
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
