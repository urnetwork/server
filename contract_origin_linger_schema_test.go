package server

import (
	"strings"
	"testing"
	"time"
)

// Pure: the side table's migration is in the list, bounds its lock waits,
// creates the published trigger only inside its catalog and emptiness checks,
// and the operator's statements create the same trigger with the state row,
// complete the warmup later.
func TestContractOriginLingerMigrationShape(t *testing.T) {
	version := ContractOriginLingerMigrationVersion()
	if !(0 < version && version <= MigrationCount()) {
		t.Fatalf("the side table's migration is at version %d of %d", version, MigrationCount())
	}
	sql := contractOriginLingerSchemaSql
	if !strings.HasPrefix(strings.TrimSpace(sql), "SET LOCAL lock_timeout = '5s';") {
		t.Fatal("the migration does not bound its lock waits")
	}
	if strings.Count(sql, ContractOriginLingerTriggerSql) != 1 || strings.Count(sql, "CREATE TRIGGER") != 1 {
		t.Fatal("the migration does not create exactly the published trigger")
	}
	absent := strings.Index(sql, "IF NOT EXISTS (")
	empty := strings.Index(sql, "IF FOUND THEN")
	trigger := strings.Index(sql, "CREATE TRIGGER")
	if !(0 < absent && absent < empty && empty < trigger) {
		t.Fatal("the migration creates the trigger outside its catalog and emptiness checks")
	}
	// no existing table changes shape or rows; the function's upsert and the
	// trigger's events are not statements of the migration
	for _, statement := range []string{"DROP ", "ALTER TABLE", "UPDATE transfer_contract SET", "DELETE FROM", "TRUNCATE"} {
		if strings.Contains(sql, statement) {
			t.Fatalf("the migration runs %q", statement)
		}
	}
	if !strings.HasPrefix(ContractOriginLingerOperatorSql, ContractOriginLingerTriggerSql+";") ||
		!strings.Contains(ContractOriginLingerOperatorSql, "INSERT INTO contract_origin_linger_state") ||
		!strings.Contains(ContractOriginLingerOperatorSql, "+ interval '75 minutes'") {
		t.Fatalf("the operator's statements are not the published trigger with a 75-minute state row: %q", ContractOriginLingerOperatorSql)
	}
}

// The trigger as the database renders it.
func contractOriginLingerTestTriggerDefinition(t testing.TB) (definition string) {
	t.Helper()
	ctx := t.Context()
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `
			SELECT pg_get_triggerdef(oid) FROM pg_trigger
			WHERE tgrelid = 'public.transfer_contract'::regclass AND tgname = $1 AND NOT tgisinternal
		`, ContractOriginLingerTriggerName)
		WithPgResult(result, err, func() {
			if result.Next() {
				Raise(result.Scan(&definition))
			}
		})
	})
	return
}

// The state row's complete time less the database clock, and whether the row
// exists.
func contractOriginLingerTestCompleteIn(t testing.TB) (completeIn time.Duration, present bool) {
	t.Helper()
	ctx := t.Context()
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `
			SELECT EXTRACT(EPOCH FROM complete_time - clock_timestamp() AT TIME ZONE 'UTC')::float8
			FROM contract_origin_linger_state
		`)
		WithPgResult(result, err, func() {
			if result.Next() {
				var seconds float64
				Raise(result.Scan(&seconds))
				completeIn = time.Duration(seconds * float64(time.Second))
				present = true
			}
		})
	})
	return
}

// DB-backed: on a new database, whose transfer_contract has no contract, the
// migration creates the trigger and a state row complete at once.
func TestContractOriginLingerMigrationCreatesTheTriggerOnANewDatabase(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		definition := contractOriginLingerTestTriggerDefinition(t)
		if !strings.Contains(definition, "AFTER INSERT OR UPDATE OF outcome, dispute, close_time, expiration_time ON public.transfer_contract FOR EACH ROW WHEN") ||
			!strings.HasSuffix(definition, "EXECUTE FUNCTION contract_origin_linger_capture()") {
			t.Fatalf("the new database's trigger is %q", definition)
		}
		completeIn, present := contractOriginLingerTestCompleteIn(t)
		if !present || 0 < completeIn {
			t.Fatalf("the new database's side table completes in %s (present %t), want complete", completeIn, present)
		}
	})
}

// DB-backed: on a database with contracts the migration leaves the trigger to
// the operator, and completes while a writer holds transfer_contract, where
// CREATE TRIGGER would wait out its lock bound. The operator's statements then
// create the trigger with the state row, complete 75 minutes later, and the
// migration's statements run again leave both as they are.
func TestContractOriginLingerMigrationLeavesTheTriggerToTheOperator(t *testing.T) {
	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		version := ContractOriginLingerMigrationVersion()
		ApplyDbMigrationsUpTo(ctx, version-1)
		contractId := NewId()
		MaintenanceDb(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(ctx, `
				INSERT INTO transfer_contract (contract_id, source_network_id, source_id, destination_network_id, destination_id, transfer_byte_count)
				VALUES ($1, gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), 1024)
			`, contractId))
		}, OptReadWrite(), OptNoRetry())

		// a writer holds ROW EXCLUSIVE on transfer_contract across the migration
		held := make(chan struct{})
		release := make(chan struct{})
		writerDone := make(chan struct{})
		go func() {
			defer close(writerDone)
			MaintenanceTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET priority = priority WHERE contract_id = $1`, contractId))
				close(held)
				<-release
			}, OptNoRetry())
		}()
		<-held
		start := time.Now()
		ApplyDbMigrationsUpTo(ctx, version)
		close(release)
		<-writerDone
		if waited := time.Since(start); 4*time.Second < waited {
			t.Fatalf("the migration waited %s behind the writer", waited)
		}
		if definition := contractOriginLingerTestTriggerDefinition(t); definition != "" {
			t.Fatalf("the migration created the trigger on a database with contracts: %q", definition)
		}
		if _, present := contractOriginLingerTestCompleteIn(t); present {
			t.Fatal("the migration wrote a state row without the trigger")
		}

		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '500ms'`))
			RaisePgResult(tx.Exec(ctx, ContractOriginLingerOperatorSql))
		}, OptNoRetry())
		created := contractOriginLingerTestTriggerDefinition(t)
		if !strings.HasSuffix(created, "EXECUTE FUNCTION contract_origin_linger_capture()") {
			t.Fatalf("the operator's statements created %q", created)
		}
		completeIn, present := contractOriginLingerTestCompleteIn(t)
		if !present || completeIn < 74*time.Minute || 76*time.Minute < completeIn {
			t.Fatalf("the operator's state row completes in %s (present %t), want 75 minutes", completeIn, present)
		}

		// the migration's statements again, as a database the operator
		// prepared first would run them
		MaintenanceTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, contractOriginLingerSchemaSql))
		}, OptNoRetry())
		if again := contractOriginLingerTestTriggerDefinition(t); again != created {
			t.Fatalf("the migration's statements changed the trigger to %q", again)
		}
		if again, _ := contractOriginLingerTestCompleteIn(t); again < 74*time.Minute {
			t.Fatalf("the migration's statements moved the complete time to %s from now", again)
		}
	})
}
