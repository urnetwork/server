// The legacy partition's early drop against PostgreSQL with the migrated schema
// (owner, 2026-11-15; db_provider_work_session_legacy_drop.go): the drop guard
// admits the legacy partition once its upper bound is past and admits nothing
// else early, the registry's own retirement keeps it until the horizon, and
// the drop step detaches and drops it in two transactions, waits less than a
// writer's lock bound behind open writes, and resumes after an interrupted
// detach. The append keeps writing through the drop: a connection admitted in
// the dropped partition retires without its network id, and every other
// connection keeps its admit's network.
package server

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// A cutover at the previous week: the legacy partition's upper bound is at
// least a week past, past the drop guard's margin, and the previous week's
// leaf is inside 35 days.
func testProviderWorkSessionLegacyCutover(t testing.TB, ctx context.Context) (previousWeek time.Time, currentWeek time.Time) {
	testProviderWorkSessionRequirePlain(t, ctx)
	currentWeek = PartitionGranularityWeekly.RangeStart(NowUtc())
	previousWeek = PartitionGranularityWeekly.AddRanges(currentWeek, -1)
	Testing_PartitionProviderWorkSessionEventsAt(ctx, previousWeek)
	return
}

// Whether a relation exists under the legacy partition's name, and whether it
// is attached to the journal.
func testProviderWorkSessionLegacyRelation(t testing.TB, ctx context.Context) (exists bool, attached bool) {
	legacy := "public." + PartitionLegacyLeafName(providerWorkSessionEventTable)
	exists = testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass($1) IS NOT NULL`, legacy)
	attached = testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM pg_inherits WHERE inhrelid = to_regclass($1) AND inhparent = 'public.provider_work_session_event'::regclass)`, legacy)
	return
}

// An open connection minted at admittedAt, with its admit written at
// admittedAt as the append wrote it then: a connection admitted before the
// cutover that is still open.
func testProviderWorkSessionOpenWithAdmit(t testing.TB, ctx context.Context, clientId Id, networkId Id, sequence int64, admittedAt time.Time) (connectionId Id) {
	connectionId = testContractIdAt(admittedAt)
	Tx(ctx, func(tx PgTx) {
		// historical fixture: the trigger would admit at now
		RaisePgResult(tx.Exec(ctx, `ALTER TABLE network_client_connection DISABLE TRIGGER provider_work_session_mutation`))
		RaisePgResult(tx.Exec(ctx, testProviderWorkSessionConnectSql, clientId, connectionId, admittedAt, make([]byte, 32), NewId()))
		RaisePgResult(tx.Exec(ctx, `ALTER TABLE network_client_connection ENABLE TRIGGER provider_work_session_mutation`))
	}, TxReadCommitted)
	testProviderWorkSessionInsertEvent(t, ctx, clientId, &networkId, &connectionId, sequence, "admit", admittedAt)
	return
}

// The drop guard's refusal of leaf with the opt-in, or "" when it admits it.
func testProviderWorkSessionGuardRefusal(ctx context.Context, leaf string) string {
	_, message := testProviderWorkSessionRolledBack(ctx, fmt.Sprintf(`SET LOCAL urnetwork.provider_work_session_retention = 'on'; SELECT provider_work_session_event_drop_guard('%s')`, leaf))
	return message
}

// The guard admits the legacy partition a week after its upper bound, and only
// with the opt-in; it still refuses a week inside 35 days, a relation that is
// not a leaf, and the legacy table once detached. The registry's own
// retirement keeps the legacy partition until the horizon after the cutover
// week: the early drop is the owner-run step only.
func TestProviderWorkSessionLegacyDropGuardAdmitsOnlyThePastLegacyPartition(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		previousWeek, _ := testProviderWorkSessionLegacyCutover(t, ctx)
		legacy := PartitionLegacyLeafName(providerWorkSessionEventTable)

		if message := testProviderWorkSessionGuardRefusal(ctx, legacy); message != "" {
			t.Fatalf("the guard refused the legacy partition a week after its upper bound: %q", message)
		}
		if _, message := testProviderWorkSessionRolledBack(ctx, fmt.Sprintf(`SELECT provider_work_session_event_drop_guard('%s')`, legacy)); !strings.Contains(message, "dropped only by their retention") {
			t.Fatalf("the guard without the opt-in: %q", message)
		}
		previous := providerWorkSessionEventLeaf(PartitionGranularityWeekly.RangeOf(previousWeek))
		if message := testProviderWorkSessionGuardRefusal(ctx, previous); !strings.Contains(message, "kept at least 35 days") {
			t.Fatalf("the guard admitted the week before the current one: %q", message)
		}
		if message := testProviderWorkSessionGuardRefusal(ctx, "network_client"); !strings.Contains(message, "kept at least 35 days") {
			t.Fatalf("the guard admitted a relation that is not a leaf: %q", message)
		}
		if _, message := testProviderWorkSessionRolledBack(ctx, fmt.Sprintf(`ALTER TABLE public.provider_work_session_event DETACH PARTITION public.%s;
			SET LOCAL urnetwork.provider_work_session_retention = 'on'; SELECT provider_work_session_event_drop_guard('%s')`, legacy, legacy)); !strings.Contains(message, "kept at least 35 days") {
			t.Fatalf("the guard admitted the detached legacy table: %q", message)
		}

		family := providerWorkSessionEventPartitionFamily
		report, _ := family.Retire(WithContractHorizon(ctx, ProviderWorkSessionEventRetentionFloor), NowUtc(), PartitionFamilyState{}, time.Now().Add(time.Minute))
		if len(report.Dropped) != 0 || len(report.Blocked) != 0 {
			t.Fatalf("the registry retired %v, blocked %v", report.Dropped, report.Blocked)
		}
		if exists, attached := testProviderWorkSessionLegacyRelation(t, ctx); !exists || !attached {
			t.Fatal("the legacy partition is gone")
		}
	})
}

// While the legacy partition still takes events (a cutover week ahead), the
// guard refuses it and the drop step, dry run or not, refuses and leaves it
// attached; the connections it holds keep their networks.
func TestProviderWorkSessionLegacyDropRefusesWhileTheLegacyPartitionTakesEvents(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testProviderWorkSessionRequirePlain(t, ctx)
		nextWeek := PartitionGranularityWeekly.AddRanges(PartitionGranularityWeekly.RangeStart(NowUtc()), 1)
		Testing_PartitionProviderWorkSessionEventsAt(ctx, nextWeek)
		legacy := PartitionLegacyLeafName(providerWorkSessionEventTable)
		clientId, networkId := testProviderWorkSessionClient(t, ctx)
		connectionId := testProviderWorkSessionConnect(t, ctx, clientId)

		if message := testProviderWorkSessionGuardRefusal(ctx, legacy); !strings.Contains(message, "kept at least 35 days") {
			t.Fatalf("the guard admitted the legacy partition before its upper bound: %q", message)
		}
		for _, dryRun := range []bool{true, false} {
			err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{DropLegacy: true, DryRun: dryRun})
			if err == nil || !strings.Contains(err.Error(), "the drop guard refuses") || !strings.Contains(err.Error(), "kept at least 35 days") {
				t.Fatalf("the drop step (dry run %t) before the upper bound: %v", dryRun, err)
			}
		}
		if exists, attached := testProviderWorkSessionLegacyRelation(t, ctx); !exists || !attached {
			t.Fatal("a refused drop detached the legacy partition")
		}

		testProviderWorkSessionDisconnect(t, ctx, connectionId)
		events := testProviderWorkSessionEvents(t, ctx, clientId)
		testProviderWorkSessionRequireContiguous(t, events)
		if len(events) != 2 || events[1].kind != "retire" || events[1].networkId == nil || *events[1].networkId != networkId || events[1].leaf != legacy {
			t.Fatalf("events %+v", events)
		}
	})
}

// After the drop the journal keeps its weeks and every append succeeds: the
// open connection admitted before the cutover retires through the real
// connection trigger without its network id, the one admitted after it keeps
// its admit's network, and a later connection admits and retires as before.
// The sequences stay contiguous from the dropped admit on. A second run finds
// nothing to drop.
func TestProviderWorkSessionLegacyDropRetiresWithoutTheLegacyPartition(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		previousWeek, currentWeek := testProviderWorkSessionLegacyCutover(t, ctx)
		clientId, networkId := testProviderWorkSessionClient(t, ctx)
		legacy := PartitionLegacyLeafName(providerWorkSessionEventTable)
		before := testProviderWorkSessionOpenWithAdmit(t, ctx, clientId, networkId, 1, previousWeek.Add(-24*time.Hour))
		after := testProviderWorkSessionConnect(t, ctx, clientId)
		if events := testProviderWorkSessionEvents(t, ctx, clientId); len(events) != 2 || events[0].leaf != legacy || events[1].leaf == legacy {
			t.Fatalf("events before the drop %+v", events)
		}

		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{DropLegacy: true}); err != nil {
			t.Fatal(err)
		}
		if exists, _ := testProviderWorkSessionLegacyRelation(t, ctx); exists {
			t.Fatal("the legacy partition was not dropped")
		}
		if !isProviderWorkSessionEventPartitioned(ctx) {
			t.Fatal("the journal is no longer partitioned")
		}
		for week := previousWeek; week.Before(PartitionGranularityWeekly.AddRanges(currentWeek, 1)); week = PartitionGranularityWeekly.NextRangeStart(week) {
			if !testProviderWorkSessionLeafExists(t, ctx, PartitionGranularityWeekly.RangeOf(week)) {
				t.Fatalf("the drop took the week of %s", week.Format(time.DateOnly))
			}
		}

		testProviderWorkSessionDisconnect(t, ctx, before)
		testProviderWorkSessionDisconnect(t, ctx, after)
		testProviderWorkSessionDisconnect(t, ctx, testProviderWorkSessionConnect(t, ctx, clientId))
		events := testProviderWorkSessionEvents(t, ctx, clientId)
		type expected struct {
			sequence int64
			kind     string
			network  bool
		}
		want := []expected{{2, "admit", true}, {3, "retire", false}, {4, "retire", true}, {5, "admit", true}, {6, "retire", true}}
		if len(events) != len(want) {
			t.Fatalf("events %+v", events)
		}
		for i, event := range events {
			if event.sequence != want[i].sequence || event.kind != want[i].kind || (event.networkId != nil) != want[i].network ||
				(event.networkId != nil && *event.networkId != networkId) || !strings.HasPrefix(event.leaf, providerWorkSessionEventTable+"_w") {
				t.Fatalf("event %d is %+v, want %+v (all %+v)", i, event, want[i], events)
			}
		}
		if open := testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM network_client_connection WHERE client_id = $1 AND connected`, clientId); open != 0 {
			t.Fatalf("%d connections stayed open", open)
		}

		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{DropLegacy: true}); err != nil {
			t.Fatalf("a second drop: %v", err)
		}
	})
}

// A run that detached the legacy partition and stopped is resumed by the next,
// which drops the detached table; a dry run changes nothing. A table under the
// legacy name that is not the detached partition is left alone.
func TestProviderWorkSessionLegacyDropResumesAfterItsDetach(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testProviderWorkSessionLegacyCutover(t, ctx)
		legacy := PartitionLegacyLeafName(providerWorkSessionEventTable)

		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{DropLegacy: true, DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if exists, attached := testProviderWorkSessionLegacyRelation(t, ctx); !exists || !attached {
			t.Fatal("a dry run detached the legacy partition")
		}
		cutover := newProviderWorkSessionCutover(DefaultProviderWorkSessionPartitionSettings(), false, testContractPartitionLogf(t))
		if err := contractPartitionExec(ctx, cutover.legacyDetachSql()); err != nil {
			t.Fatal(err)
		}
		if exists, attached := testProviderWorkSessionLegacyRelation(t, ctx); !exists || attached {
			t.Fatal("the legacy partition is not detached")
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{DropLegacy: true, DryRun: true}); err != nil {
			t.Fatal(err)
		}
		if exists, _ := testProviderWorkSessionLegacyRelation(t, ctx); !exists {
			t.Fatal("a dry run dropped the detached table")
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{DropLegacy: true}); err != nil {
			t.Fatal(err)
		}
		if exists, _ := testProviderWorkSessionLegacyRelation(t, ctx); exists {
			t.Fatal("the detached table was not dropped")
		}

		Raise(contractPartitionExec(ctx, []string{fmt.Sprintf(`CREATE TABLE public.%s (id bigint)`, quoteIdentifier(legacy))}))
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{DropLegacy: true}); err == nil || !strings.Contains(err.Error(), "left alone") {
			t.Fatalf("a drop of a table that is not the legacy partition: %v", err)
		}
		if exists, _ := testProviderWorkSessionLegacyRelation(t, ctx); !exists {
			t.Fatal("a table that is not the legacy partition was dropped")
		}
		Raise(contractPartitionExec(ctx, []string{fmt.Sprintf(`DROP TABLE public.%s`, quoteIdentifier(legacy))}))
	})
}

// A connection insert held open locks the journal's parent: the detach waits
// on it for at most its lock bound per attempt and gives up, leaving the
// legacy partition attached. A main-pool connection insert that queues behind
// the waiting detach, under its own lock bound (PgLockTimeout), still commits,
// and so does the held writer. Once the writer is gone, the drop succeeds.
func TestProviderWorkSessionLegacyDetachWaitsLessThanAWriter(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		var workers sync.WaitGroup
		defer func() {
			cancel()
			workers.Wait()
		}()
		testProviderWorkSessionLegacyCutover(t, ctx)
		clientId, networkId := testProviderWorkSessionClient(t, ctx)
		queuedClientId, _ := testProviderWorkSessionClient(t, ctx)

		writer := testProviderWorkSessionStartWriter(t, ctx, &workers, clientId)
		var writerPid uint32
		select {
		case writerPid = <-writer.ready:
		case err := <-writer.done:
			t.Fatalf("writer failed before its barrier: %v", err)
		}
		settings := DefaultProviderWorkSessionPartitionSettings()
		// the quiet-journal wait admits the held writer, so each attempt waits
		// on its lock
		settings.MaxTransactionAge = time.Hour
		settings.LockAttempts = 3
		settings.LockRetryInterval = 10 * time.Millisecond
		dropDone := make(chan error, 1)
		dropStart := time.Now()
		workers.Add(1)
		go func() {
			defer workers.Done()
			dropDone <- MigrateProviderWorkSessionEventsToPartitions(ctx, ProviderWorkSessionPartitionMigrateOptions{
				DropLegacy: true,
				Settings:   settings,
				Logf:       testContractPartitionLogf(t),
			})
		}()

		// a connection insert behind the waiting detach
		MaintenanceDb(ctx, func(conn PgConn) {
			for {
				var waiting bool
				Raise(conn.QueryRow(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity activity
					WHERE activity.query LIKE '%DETACH PARTITION%' AND $1::int = ANY (pg_blocking_pids(activity.pid)))`, writerPid).Scan(&waiting))
				if waiting {
					break
				}
				select {
				case err := <-dropDone:
					t.Fatalf("the drop ended before its detach waited on the writer: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(5 * time.Millisecond):
				}
			}
		})
		queued := testProviderWorkSessionConnect(t, ctx, queuedClientId)

		err := <-dropDone
		if !contractPartitionLockBusy(err) || !strings.Contains(err.Error(), "detach") {
			t.Fatalf("the detach behind a held writer: %v", err)
		}
		if elapsed := time.Since(dropStart); elapsed < time.Duration(settings.LockAttempts)*settings.LegacyDetachLockTimeout {
			t.Fatalf("the detach gave up after %s, before its lock bound", elapsed)
		}
		if exists, attached := testProviderWorkSessionLegacyRelation(t, ctx); !exists || !attached {
			t.Fatal("a detach that timed out changed the legacy partition")
		}
		close(writer.commit)
		if err := <-writer.done; err != nil {
			t.Fatalf("the held writer: %v", err)
		}
		testProviderWorkSessionDisconnect(t, ctx, queued)

		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{DropLegacy: true}); err != nil {
			t.Fatal(err)
		}
		if exists, _ := testProviderWorkSessionLegacyRelation(t, ctx); exists {
			t.Fatal("the legacy partition was not dropped")
		}
		events := testProviderWorkSessionEvents(t, ctx, clientId)
		if len(events) != 1 || events[0].kind != "admit" || events[0].networkId == nil || *events[0].networkId != networkId {
			t.Fatalf("the held writer's admit %+v", events)
		}
	})
}
