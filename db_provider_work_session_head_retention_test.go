// The head retention guard, without a database and against PostgreSQL with
// the migrated schema (db_provider_work_session_head_retention.go).
package server

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// The retention guard is the v776 head guard with only the retention clause
// added before its refusal, naming the opt-in; it is the last migration that
// publishes the guard, follows the append for provider clients only and bounds
// its lock wait.
func TestProviderWorkSessionHeadRetentionGuardMigration(t *testing.T) {
	version := ProviderWorkSessionHeadRetentionGuardMigrationVersion()
	if version == 0 || version <= ProviderWorkSessionProviderAppendMigrationVersion() {
		t.Fatalf("head retention guard version %d, provider append version %d", version, ProviderWorkSessionProviderAppendMigrationVersion())
	}
	for index, migration := range migrations[version:] {
		if sql, ok := migration.(*SqlMigration); ok && strings.Contains(sql.sql, "provider_work_session_head_guard") {
			t.Fatalf("migration %d republishes the head guard after the retention guard", version+index+1)
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(providerWorkSessionHeadRetentionGuardSql), "SET LOCAL lock_timeout = '5s';") {
		t.Error("the retention guard does not bound its lock wait")
	}
	if count := strings.Count(providerWorkSessionHeadRetentionGuardSql, "$head_guard$"); count != 2 {
		t.Fatalf("the delimiter occurs %d times", count)
	}
	refusal := " RAISE EXCEPTION 'provider work journal birth cannot be reset';"
	original := ProviderWorkSessionHeadGuardBodySql()
	if strings.Count(original, refusal) != 1 || strings.Replace(original, refusal, providerWorkSessionHeadRetentionClauseSql+refusal, 1) != ProviderWorkSessionHeadRetentionGuardBodySql() {
		t.Fatal("the retention guard differs from the v776 head guard beyond the retention clause")
	}
	if strings.Count(providerWorkSessionHeadRetentionClauseSql, fmt.Sprintf("current_setting('%s',true)='on'", ProviderWorkSessionHeadRetentionSetting)) != 1 {
		t.Fatal("the retention clause does not read the retention's opt-in")
	}
	for _, check := range []string{
		"NOT EXISTS(SELECT 1 FROM provider_work_session_event WHERE client_id=OLD.client_id)",
		"NOT EXISTS(SELECT 1 FROM provider_work_session_receipt WHERE client_id=OLD.client_id)",
		"TG_OP='DELETE'",
	} {
		if strings.Count(providerWorkSessionHeadRetentionClauseSql, check) != 1 {
			t.Fatalf("the retention clause lacks %q", check)
		}
	}
}

// Through the real guard: a head goes only from a transaction that opted in,
// and only once its client has no event left in the journal; an update other
// than +1 stays refused, with or without the opt-in.
func TestProviderWorkSessionHeadGuardAdmitsOnlyRetentionDeletes(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		optIn := fmt.Sprintf("SET LOCAL %s = 'on'; ", ProviderWorkSessionHeadRetentionSetting)
		const refused = "provider work journal birth cannot be reset"

		// a provider with an event in the journal
		journaled, _ := testProviderWorkSessionClient(t, ctx)
		testProviderWorkSessionConnect(t, ctx, journaled)
		for _, prefix := range []string{"", optIn} {
			// statements with literals: a multi-statement string takes no parameters
			if _, message := testProviderWorkSessionRolledBack(ctx, prefix+fmt.Sprintf(`DELETE FROM provider_work_session_head WHERE client_id = '%s'`, journaled)); message != refused {
				t.Fatalf("delete of a head with events (opt-in %t): %q", prefix != "", message)
			}
			if _, message := testProviderWorkSessionRolledBack(ctx, prefix+fmt.Sprintf(`UPDATE provider_work_session_head SET sequence = sequence + 2 WHERE client_id = '%s'`, journaled)); message != refused {
				t.Fatalf("update past +1 (opt-in %t): %q", prefix != "", message)
			}
		}

		// a head whose client's events are gone
		idle := NewId()
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_session_head (client_id, sequence) VALUES ($1, 2)`, idle))
		})
		deleteIdle := fmt.Sprintf(`DELETE FROM provider_work_session_head WHERE client_id = '%s'`, idle)
		if _, message := testProviderWorkSessionRolledBack(ctx, deleteIdle); message != refused {
			t.Fatalf("delete without the opt-in: %q", message)
		}
		if _, message := testProviderWorkSessionRolledBack(ctx, optIn+deleteIdle); message != "" {
			t.Fatalf("the retention's delete of an idle head: %q", message)
		}
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, optIn+deleteIdle))
		})
		if remaining := testProviderWorkSessionHeadSequence(t, ctx, idle); remaining != 0 {
			t.Fatalf("the idle head is still at %d", remaining)
		}
		if sequence := testProviderWorkSessionHeadSequence(t, ctx, journaled); sequence != 1 {
			t.Fatalf("the journaled head moved to %d", sequence)
		}
	})
}
