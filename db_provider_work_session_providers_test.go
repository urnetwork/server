// The append for provider clients only, without a database
// (db_provider_work_session_providers.go). The DB tests are in
// db_provider_work_session_providers_db_test.go.
package server

import (
	"strings"
	"testing"
)

// The append for provider clients only is the partition preparation's append
// with only the two provider gates added and the endpoint fence and head moved
// after the lookups that decide; it is the last migration that publishes the
// append, follows the legacy drop guard, bounds its lock wait and changes no
// table.
func TestProviderWorkSessionProviderAppendMigration(t *testing.T) {
	version := ProviderWorkSessionProviderAppendMigrationVersion()
	if version == 0 || version <= ProviderWorkSessionEventLegacyDropGuardMigrationVersion() {
		t.Fatalf("provider append version %d, legacy drop guard version %d", version, ProviderWorkSessionEventLegacyDropGuardMigrationVersion())
	}
	for index, migration := range migrations[version:] {
		if sql, ok := migration.(*SqlMigration); ok && strings.Contains(sql.sql, "provider_work_session_append") {
			t.Fatalf("migration %d republishes the append after the provider append", version+index+1)
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(providerWorkSessionProviderAppendSql), "SET LOCAL lock_timeout = '5s';") {
		t.Error("the provider append does not bound its lock wait")
	}
	if count := strings.Count(providerWorkSessionProviderAppendSql, "$session_append$"); count != 2 {
		t.Fatalf("the delimiter occurs %d times", count)
	}
	for _, statement := range []string{"ALTER TABLE", "LOCK TABLE", "CREATE INDEX", "DROP ", "CREATE TRIGGER", "DELETE", "UPDATE provider"} {
		if strings.Contains(providerWorkSessionProviderAppendSql, statement) {
			t.Fatalf("the migration runs %s", statement)
		}
	}

	const provider = "EXISTS(SELECT 1 FROM provide_key WHERE client_id=client AND provide_mode IN (2,3))"
	retireGate := "   IF NOT " + provider + " THEN\n    RETURN NULL;\n   END IF;\n"
	admitGate := "  IF NOT " + provider + " THEN\n   RETURN NULL;\n  END IF;\n"
	head := ` PERFORM provider_work_endpoint_lock(client);
 INSERT INTO provider_work_session_head(client_id,sequence) VALUES(client,1)
 ON CONFLICT(client_id) DO UPDATE SET sequence=provider_work_session_head.sequence+1
 RETURNING sequence INTO next_sequence;
`
	const begin = "BEGIN\n"
	const event = " INSERT INTO provider_work_session_event("
	providerAppend := ProviderWorkSessionProviderAppendBodySql()
	for _, part := range []string{retireGate, admitGate, head, event} {
		if strings.Count(providerAppend, part) != 1 {
			t.Fatalf("the provider append holds %q %d times:\n%s", part, strings.Count(providerAppend, part), providerAppend)
		}
	}
	if strings.Index(providerAppend, retireGate) > strings.Index(providerAppend, admitGate) || strings.Index(providerAppend, admitGate) > strings.Index(providerAppend, head) {
		t.Fatalf("the gates do not precede the endpoint fence and the head:\n%s", providerAppend)
	}
	restored := strings.Replace(strings.Replace(providerAppend, retireGate, "", 1), admitGate, "", 1)
	restored = strings.Replace(strings.Replace(restored, head, "", 1), begin, begin+head, 1)
	if _, sessionAppend, _, _ := ProviderWorkSessionPartitionBodiesSql(); restored != sessionAppend {
		t.Fatalf("the provider append differs from the partition preparation's beyond its gates and their order:\n%s", restored)
	}
	// the gate's retire branch is inside the bounded lookup's miss, before the
	// unbounded fallback
	retire := providerAppend[strings.Index(providerAppend, "IF event_kind='retire' THEN"):strings.Index(providerAppend, " ELSE\n")]
	if !strings.Contains(retire, "  IF NOT FOUND THEN\n"+retireGate+"   SELECT network_id INTO owner_network FROM provider_work_session_event\n") {
		t.Fatalf("the retire gate is not between the bounded lookup's miss and the fallback:\n%s", retire)
	}
}
