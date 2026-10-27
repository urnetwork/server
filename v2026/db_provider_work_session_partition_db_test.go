// The session journal's weekly partitions against PostgreSQL with the migrated
// schema (PARTITION-MAINTENANCE-PLAN.md C4): the cutover and its restarts,
// rollback and abandonment, the guards, the receipt reference and the drop.
// Each test that needs a particular layout builds it from the plain table, and
// skips when URNETWORK_TEST_PROVIDER_WORK_SESSION_PARTITIONS already cut the
// database over.
package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Skips a test that starts from the plain journal.
func testProviderWorkSessionRequirePlain(t testing.TB, ctx context.Context) {
	if isProviderWorkSessionEventPartitioned(ctx) {
		t.Skip("the test starts from the plain journal")
	}
}

// A cutover command run whose settings keep a lock wait observable for the
// whole test deadline, even when the test process is descheduled.
func testProviderWorkSessionMigrate(t testing.TB, ctx context.Context, options ProviderWorkSessionPartitionMigrateOptions) error {
	if options.Settings == nil {
		settings := DefaultProviderWorkSessionPartitionSettings()
		settings.MaxTransactionAge = time.Minute
		settings.LockTimeout = time.Minute
		settings.StatementTimeout = time.Minute
		options.Settings = settings
	}
	if options.Logf == nil {
		options.Logf = testContractPartitionLogf(t)
	}
	return MigrateProviderWorkSessionEventsToPartitions(ctx, options)
}

// A client with a network, so its admits carry the network.
func testProviderWorkSessionClient(t testing.TB, ctx context.Context) (clientId Id, networkId Id) {
	clientId, networkId = NewId(), NewId()
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`, clientId, networkId))
	})
	return
}

const testProviderWorkSessionConnectSql = `INSERT INTO network_client_connection
	(client_id, connection_id, connect_time, connection_host, connection_service, connection_block,
	 client_address_hash, client_address_port, handler_id)
	VALUES ($1, $2, $3, 'synthetic.example', 'test', 'test', $4, 10001, $5)`

// Opens a connection through the real row trigger, which appends its admit.
func testProviderWorkSessionConnect(t testing.TB, ctx context.Context, clientId Id) (connectionId Id) {
	connectionId = NewId()
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, testProviderWorkSessionConnectSql, clientId, connectionId, NowUtc(), make([]byte, 32), NewId()))
	}, TxReadCommitted)
	return
}

// Closes a connection through the real row trigger, which appends its retire.
func testProviderWorkSessionDisconnect(t testing.TB, ctx context.Context, connectionId Id) {
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected = false, disconnect_time = $2 WHERE connection_id = $1`, connectionId, NowUtc()))
	}, TxReadCommitted)
}

// One appended event as stored, with the leaf that holds it.
type testProviderWorkSessionEvent struct {
	sequence  int64
	kind      string
	networkId *Id
	leaf      string
}

func testProviderWorkSessionEvents(t testing.TB, ctx context.Context, clientId Id) (events []testProviderWorkSessionEvent) {
	MaintenanceDb(ctx, func(conn PgConn) {
		result, err := conn.Query(ctx, `SELECT sequence, kind, network_id, tableoid::regclass::text FROM provider_work_session_event WHERE client_id = $1 ORDER BY sequence`, clientId)
		WithPgResult(result, err, func() {
			for result.Next() {
				var event testProviderWorkSessionEvent
				Raise(result.Scan(&event.sequence, &event.kind, &event.networkId, &event.leaf))
				events = append(events, event)
			}
		})
	})
	return
}

// The client's sequences are exactly 1..n.
func testProviderWorkSessionRequireContiguous(t testing.TB, events []testProviderWorkSessionEvent) {
	t.Helper()
	for i, event := range events {
		if event.sequence != int64(i+1) {
			t.Fatalf("sequence %d at position %d: %+v", event.sequence, i, events)
		}
	}
}

// An event written directly at observedAt, past the append's clock, with the
// head the append would leave.
func testProviderWorkSessionInsertEvent(t testing.TB, ctx context.Context, clientId Id, networkId *Id, connectionId *Id, sequence int64, kind string, observedAt time.Time) {
	Tx(ctx, func(tx PgTx) {
		RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO provider_work_session_head (client_id, sequence) VALUES ($1, $2) ON CONFLICT (client_id) DO UPDATE SET sequence = provider_work_session_head.sequence + 1`,
			clientId, sequence,
		))
		RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO provider_work_session_event (client_id, sequence, network_id, connection_id, kind, observed_at, extender_id, transaction_id)
			VALUES ($1, $2, $3, $4, $5, $6, NULL, txid_current())`,
			clientId, sequence, networkId, connectionId, kind, observedAt,
		))
	}, TxReadCommitted)
}

// The SQLSTATE and message a callback raised, or "".
func testProviderWorkSessionFailure(callback func()) (code string, message string) {
	failure := testContractPartitionRecover(callback)
	if failure == nil {
		return "", ""
	}
	if err, ok := failure.(error); ok {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) {
			return pgErr.Code, pgErr.Message
		}
		return "", err.Error()
	}
	return "", fmt.Sprintf("%v", failure)
}

// Runs statement in a transaction that is rolled back, and returns what it
// raised.
func testProviderWorkSessionRolledBack(ctx context.Context, statement string, args ...any) (code string, message string) {
	errRolledBack := errors.New("rolled back")
	code, message = testProviderWorkSessionFailure(func() {
		Raise(contractPartitionTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, statement, args...))
			panic(errRolledBack)
		}))
	})
	if message == errRolledBack.Error() {
		return "", ""
	}
	return
}

// An admit in the last minute of one week and its retire in the next: the
// bounded lookup reaches back across the week boundary and the retire keeps
// the admit's network; each event is in its own week's partition.
func TestProviderWorkSessionPartitionAdmitRetireAcrossWeekBoundary(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testProviderWorkSessionRequirePlain(t, ctx)
		currentWeek := PartitionGranularityWeekly.RangeStart(NowUtc())
		previousWeek := PartitionGranularityWeekly.AddRanges(currentWeek, -1)
		Testing_PartitionProviderWorkSessionEventsAt(ctx, previousWeek)

		clientId, networkId := testProviderWorkSessionClient(t, ctx)
		admitAt := currentWeek.Add(-time.Minute)
		connectionId := testContractIdAt(admitAt)
		testProviderWorkSessionInsertEvent(t, ctx, clientId, &networkId, &connectionId, 1, "admit", admitAt)
		var retireSequence int64
		Tx(ctx, func(tx PgTx) {
			Raise(tx.QueryRow(ctx, `SELECT provider_work_session_append($1, $2, 'retire', NULL)`, clientId, connectionId).Scan(&retireSequence))
		}, TxReadCommitted)
		if retireSequence != 2 {
			t.Fatalf("retire sequence %d", retireSequence)
		}
		events := testProviderWorkSessionEvents(t, ctx, clientId)
		testProviderWorkSessionRequireContiguous(t, events)
		if len(events) != 2 || events[1].kind != "retire" || events[1].networkId == nil || *events[1].networkId != networkId {
			t.Fatalf("the retire lost its admit's network: %+v", events)
		}
		if events[0].leaf != providerWorkSessionEventLeaf(PartitionGranularityWeekly.RangeOf(previousWeek)) ||
			events[1].leaf != providerWorkSessionEventLeaf(PartitionGranularityWeekly.RangeOf(currentWeek)) {
			t.Fatalf("events are not in their weeks' partitions: %+v", events)
		}

		// the same through the real connection trigger, within the week
		connected := testProviderWorkSessionConnect(t, ctx, clientId)
		testProviderWorkSessionDisconnect(t, ctx, connected)
		events = testProviderWorkSessionEvents(t, ctx, clientId)
		testProviderWorkSessionRequireContiguous(t, events)
		if len(events) != 4 || events[2].kind != "admit" || events[3].kind != "retire" || events[3].networkId == nil || *events[3].networkId != networkId {
			t.Fatalf("a connection's retire lost its admit's network: %+v", events)
		}

		// a connection id from no clock still finds its admit, through the
		// unbounded fallback
		unordered := Id{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x70, 0x00, 0x80}
		testProviderWorkSessionInsertEvent(t, ctx, clientId, &networkId, &unordered, 5, "admit", admitAt)
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `SELECT provider_work_session_append($1, $2, 'retire', NULL)`, clientId, unordered))
		}, TxReadCommitted)
		events = testProviderWorkSessionEvents(t, ctx, clientId)
		if len(events) != 6 || events[5].networkId == nil || *events[5].networkId != networkId {
			t.Fatalf("the fallback lost the admit's network: %+v", events)
		}
	})
}

// The relations a plan reads, from EXPLAIN (FORMAT JSON, ANALYZE), with the
// literals bound in place of $1, $2, ... generic prepares the statement and
// forces its generic plan, as a function's cached statement can switch to, so
// only pruning at executor startup applies. The session is discarded after.
func testProviderWorkSessionPlanRelations(t testing.TB, ctx context.Context, generic bool, statement string, types string, literals ...string) (relations []string, plan string) {
	conn, err := AcquireMaintenanceDbConn(ctx)
	Raise(err)
	defer discardPgConnection(ctx, conn)
	const explain = "EXPLAIN (FORMAT JSON, ANALYZE, TIMING OFF, SUMMARY OFF) "
	if generic {
		RaisePgResult(conn.Exec(ctx, fmt.Sprintf("PREPARE pws_prune_probe (%s) AS %s", types, statement)))
		RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode = force_generic_plan`))
		Raise(conn.QueryRow(ctx, explain+fmt.Sprintf("EXECUTE pws_prune_probe(%s)", strings.Join(literals, ", "))).Scan(&plan))
	} else {
		bound := statement
		for i := len(literals) - 1; 0 <= i; i -= 1 {
			bound = strings.ReplaceAll(bound, fmt.Sprintf("$%d", i+1), literals[i])
		}
		Raise(conn.QueryRow(ctx, explain+bound).Scan(&plan))
	}
	for _, part := range strings.Split(plan, `"Relation Name": "`)[1:] {
		relations = append(relations, part[:strings.Index(part, `"`)])
	}
	return
}

// The retire lookup and the receipt reference read only the weeks their
// bounds reach, with literal values and with the generic plan a function's
// cached statement can switch to.
func TestProviderWorkSessionPartitionRetireLookupPrunes(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testProviderWorkSessionRequirePlain(t, ctx)
		currentWeek := PartitionGranularityWeekly.RangeStart(NowUtc())
		cutoverWeek := PartitionGranularityWeekly.AddRanges(currentWeek, -3)
		Testing_PartitionProviderWorkSessionEventsAt(ctx, cutoverWeek)

		clientId, networkId := testProviderWorkSessionClient(t, ctx)
		// a connection minted two days into the current week, or at its start
		mintedAt := currentWeek.Add(2 * 24 * time.Hour)
		if NowUtc().Before(mintedAt) {
			mintedAt = currentWeek
		}
		connectionId := testContractIdAt(mintedAt)
		testProviderWorkSessionInsertEvent(t, ctx, clientId, &networkId, &connectionId, 1, "admit", mintedAt)
		floorWeek := PartitionGranularityWeekly.RangeStart(mintedAt.Add(-24 * time.Hour))

		const lookup = `SELECT network_id FROM provider_work_session_event
			WHERE client_id = $1 AND connection_id = $2 AND kind = 'admit'
			AND observed_at >= provider_work_session_admit_floor($2) ORDER BY sequence DESC LIMIT 1`
		_, sessionAppend, receiptReference, _ := ProviderWorkSessionPartitionBodiesSql()
		if !strings.Contains(sessionAppend, "AND observed_at>=provider_work_session_admit_floor(connection) ORDER BY sequence DESC LIMIT 1") {
			t.Fatal("the published retire lookup is not the bounded lookup tested here")
		}
		literals := []string{"'" + clientId.String() + "'::uuid", "'" + connectionId.String() + "'::uuid"}
		for _, generic := range []bool{false, true} {
			relations, plan := testProviderWorkSessionPlanRelations(t, ctx, generic, lookup, "uuid, uuid", literals...)
			for _, relation := range relations {
				r, ok := providerWorkSessionEventPartitionFamily.ParseLeafName(providerWorkSessionEventTable, relation)
				if !ok || r.Legacy || r.Lower.Before(floorWeek) {
					t.Fatalf("generic=%t: the lookup read %s below its floor week %s:\n%s", generic, relation, floorWeek.Format(time.DateOnly), plan)
				}
			}
			if len(relations) == 0 || (generic && !strings.Contains(plan, `"Subplans Removed"`)) {
				t.Fatalf("generic=%t: the lookup did not prune:\n%s", generic, plan)
			}
		}

		const reference = `SELECT 1 FROM provider_work_session_event WHERE client_id = $1 AND sequence = $2
			AND observed_at >= (now() AT TIME ZONE 'UTC') - interval '1 day'`
		if !strings.Contains(receiptReference, "AND observed_at>=(now() AT TIME ZONE 'UTC')-interval '1 day'") {
			t.Fatal("the published receipt reference is not the bounded lookup tested here")
		}
		for _, generic := range []bool{false, true} {
			relations, plan := testProviderWorkSessionPlanRelations(t, ctx, generic, reference, "uuid, bigint", literals[0], "1::bigint")
			for _, relation := range relations {
				if r, ok := providerWorkSessionEventPartitionFamily.ParseLeafName(providerWorkSessionEventTable, relation); !ok || r.Legacy || r.Upper.Before(NowUtc().Add(-24*time.Hour)) {
					t.Fatalf("generic=%t: the receipt reference read %s:\n%s", generic, relation, plan)
				}
			}
			if !strings.Contains(plan, `"Subplans Removed"`) {
				t.Fatalf("generic=%t: the receipt reference did not prune:\n%s", generic, plan)
			}
		}
	})
}

// A receipt needs its event before and after the cutover, in the legacy
// partition and in a week's partition; the cutover drops the foreign key and
// keeps every receipt.
func TestProviderWorkSessionPartitionReceiptReferenceAcrossCutover(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testProviderWorkSessionRequirePlain(t, ctx)
		currentWeek := PartitionGranularityWeekly.RangeStart(NowUtc())
		lastWeek := PartitionGranularityWeekly.AddRanges(currentWeek, -1).Add(time.Hour)
		clientId, networkId := testProviderWorkSessionClient(t, ctx)
		testProviderWorkSessionInsertEvent(t, ctx, clientId, &networkId, nil, 1, "baseline", lastWeek)
		testProviderWorkSessionInsertEvent(t, ctx, clientId, &networkId, nil, 2, "baseline", lastWeek)

		insertReceipt := func(sequence int64, hash byte) (code string) {
			code, _ = testProviderWorkSessionFailure(func() {
				hashBytes := make([]byte, 32)
				hashBytes[0] = hash
				Tx(ctx, func(tx PgTx) {
					RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_session_receipt (client_id, sequence, receipt_hash, original) VALUES ($1, $2, $3, '\x01'::bytea)`, clientId, sequence, hashBytes))
				}, OptNoRetry())
			})
			return
		}
		if code := insertReceipt(1, 1); code != "" {
			t.Fatalf("a receipt for an event was refused: %s", code)
		}
		if code := insertReceipt(99, 2); code != "23503" {
			t.Fatalf("a receipt without its event: %q, want 23503", code)
		}

		Testing_PartitionProviderWorkSessionEventsAt(ctx, currentWeek)
		if testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conrelid = 'provider_work_session_receipt'::regclass AND contype = 'f')`) {
			t.Fatal("the cutover kept the receipt foreign key")
		}
		if key := testContractPartitionScalar[string](t, ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'provider_work_session_event'::regclass AND contype = 'p'`); key != providerWorkSessionEventIdentityKey {
			t.Fatalf("primary key %s", key)
		}
		if count := testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM provider_work_session_receipt WHERE client_id = $1`, clientId); count != 1 {
			t.Fatalf("the cutover changed the receipts: %d", count)
		}
		// an event in the legacy partition, older than the bounded lookup
		if code := insertReceipt(2, 3); code != "" {
			t.Fatalf("a receipt for a legacy event was refused: %s", code)
		}
		// an event in this week's partition
		var sequence int64
		Tx(ctx, func(tx PgTx) {
			Raise(tx.QueryRow(ctx, `SELECT provider_work_session_append($1, NULL, 'baseline', NULL)`, clientId).Scan(&sequence))
		}, TxReadCommitted)
		if code := insertReceipt(sequence, 4); code != "" {
			t.Fatalf("a receipt for a weekly event was refused: %s", code)
		}
		if code := insertReceipt(sequence+1, 5); code != "23503" {
			t.Fatalf("a receipt without its event after the cutover: %q, want 23503", code)
		}
		events := testProviderWorkSessionEvents(t, ctx, clientId)
		if events[0].leaf != PartitionLegacyLeafName(providerWorkSessionEventTable) || events[2].leaf != providerWorkSessionEventLeaf(PartitionGranularityWeekly.RangeOf(currentWeek)) {
			t.Fatalf("events %+v", events)
		}
	})
}

// UPDATE, DELETE and TRUNCATE stay refused on the partitioned journal: through
// the parent, on the legacy partition, on a week's partition created at the
// cutover and on one the registry's runway creates later.
func TestProviderWorkSessionPartitionGuardsRefuseMutation(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testProviderWorkSessionRequirePlain(t, ctx)
		currentWeek := PartitionGranularityWeekly.RangeStart(NowUtc())
		clientId, networkId := testProviderWorkSessionClient(t, ctx)
		testProviderWorkSessionInsertEvent(t, ctx, clientId, &networkId, nil, 1, "baseline", currentWeek.Add(-time.Hour))
		Testing_PartitionProviderWorkSessionEventsAt(ctx, currentWeek)
		Tx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, `SELECT provider_work_session_append($1, NULL, 'baseline', NULL)`, clientId))
		}, TxReadCommitted)
		// the runway creates a week beyond the cutover's
		report := providerWorkSessionEventPartitionFamily.EnsureRunway(ctx, PartitionGranularityWeekly.AddRanges(currentWeek, 2))
		if len(report.Errors) != 0 || len(report.Created) == 0 {
			t.Fatalf("runway %+v", report)
		}
		legacy := PartitionLegacyLeafName(providerWorkSessionEventTable)
		week := providerWorkSessionEventLeaf(PartitionGranularityWeekly.RangeOf(currentWeek))
		later := report.Created[len(report.Created)-1]
		for _, statement := range []string{
			`UPDATE provider_work_session_event SET kind = 'admit' WHERE client_id = $1`,
			`DELETE FROM provider_work_session_event WHERE client_id = $1`,
			`UPDATE provider_work_session_event SET observed_at = observed_at - interval '1 day' WHERE client_id = $1 AND sequence = 2`,
			fmt.Sprintf(`UPDATE public.%s SET kind = 'admit' WHERE client_id = $1`, quoteIdentifier(legacy)),
			fmt.Sprintf(`DELETE FROM public.%s WHERE client_id = $1`, quoteIdentifier(week)),
		} {
			if _, message := testProviderWorkSessionRolledBack(ctx, statement, clientId); message != "provider work originals are append-only" {
				t.Errorf("%s: %q", statement, message)
			}
		}
		for _, table := range []string{providerWorkSessionEventTable, legacy, week, later} {
			if _, message := testProviderWorkSessionRolledBack(ctx, fmt.Sprintf("TRUNCATE public.%s", quoteIdentifier(table))); message != "provider work originals are append-only" {
				t.Errorf("TRUNCATE %s: %q", table, message)
			}
			// the row guard is cloned onto every leaf
			if table != providerWorkSessionEventTable && !testContractPartitionScalar[bool](t, ctx, `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = to_regclass($1) AND tgname = 'original_guard' AND tgparentid <> 0)`, "public."+table) {
				t.Errorf("%s has no row guard", table)
			}
		}
		if count := len(testProviderWorkSessionEvents(t, ctx, clientId)); count != 2 {
			t.Fatalf("a guard let %d of 2 events change", count)
		}
	})
}

// A cutover at fromWeek with one event in each of the weeks from fromWeek
// on, through the current one, and one in the legacy partition.
func testProviderWorkSessionOldWeeks(t testing.TB, ctx context.Context, fromWeek time.Time) (clientId Id) {
	testProviderWorkSessionRequirePlain(t, ctx)
	clientId, networkId := testProviderWorkSessionClient(t, ctx)
	sequence := int64(1)
	testProviderWorkSessionInsertEvent(t, ctx, clientId, &networkId, nil, sequence, "baseline", fromWeek.Add(-time.Hour))
	Testing_PartitionProviderWorkSessionEventsAt(ctx, fromWeek)
	for week := fromWeek; !NowUtc().Before(week); week = PartitionGranularityWeekly.NextRangeStart(week) {
		r := PartitionGranularityWeekly.RangeOf(week)
		if _, err := providerWorkSessionEventPartitionFamily.EnsureRangeFor(ctx, r.Lower); err != nil {
			t.Fatal(err)
		}
		sequence += 1
		testProviderWorkSessionInsertEvent(t, ctx, clientId, &networkId, nil, sequence, "baseline", week.Add(time.Hour))
	}
	return
}

// Whether the leaf of r exists.
func testProviderWorkSessionLeafExists(t testing.TB, ctx context.Context, r PartitionRange) bool {
	return testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+providerWorkSessionEventLeaf(r))
}

// A drop without the opt-in is refused, whatever the leaf's age; with it, the
// database's clock still refuses a leaf less than 13 weeks past.
func TestProviderWorkSessionPartitionDropRequiresOptIn(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		currentWeek := PartitionGranularityWeekly.RangeStart(NowUtc())
		oldWeek := PartitionGranularityWeekly.AddRanges(currentWeek, -14)
		testProviderWorkSessionOldWeeks(t, ctx, oldWeek)
		old := PartitionGranularityWeekly.RangeOf(oldWeek)
		recent := PartitionGranularityWeekly.RangeOf(PartitionGranularityWeekly.AddRanges(currentWeek, -12))

		withoutOptIn := *providerWorkSessionEventPartitionFamily
		withoutOptIn.DropGuardSetting = ""
		if _, err := withoutOptIn.DropRange(ctx, old); err == nil || !strings.Contains(err.Error(), "dropped only by their retention") {
			t.Fatalf("a drop without the opt-in: %v", err)
		}
		if _, message := testProviderWorkSessionRolledBack(ctx, `SELECT provider_work_session_event_drop_guard($1)`, providerWorkSessionEventLeaf(old)); !strings.Contains(message, "dropped only by their retention") {
			t.Fatalf("the guard called without the opt-in: %q", message)
		}
		if _, err := providerWorkSessionEventPartitionFamily.DropRange(ctx, recent); err == nil || !strings.Contains(err.Error(), "kept 13 weeks") {
			t.Fatalf("a drop of a week 12 weeks past: %v", err)
		}
		for _, r := range []PartitionRange{old, recent} {
			if !testProviderWorkSessionLeafExists(t, ctx, r) {
				t.Fatalf("a refused drop removed %s", providerWorkSessionEventLeaf(r))
			}
		}
		if _, message := testProviderWorkSessionRolledBack(ctx, `SET LOCAL urnetwork.provider_work_session_retention = 'on'; SELECT provider_work_session_event_drop_guard('network_client')`); !strings.Contains(message, "kept 13 weeks") {
			t.Fatalf("the guard admitted a relation that is not a leaf: %q", message)
		}
	})
}

// The registry's retirement keeps a week 12 weeks and 6 days past and drops
// the one 13 weeks and 6 days past, with the legacy partition before it; a
// pass whose clock runs ahead of the database's is refused by the guard.
func TestProviderWorkSessionPartitionRetentionDropsOnlyThirteenWeeks(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		currentWeek := PartitionGranularityWeekly.RangeStart(NowUtc())
		oldWeek := PartitionGranularityWeekly.AddRanges(currentWeek, -14)
		clientId := testProviderWorkSessionOldWeeks(t, ctx, oldWeek)
		old := PartitionGranularityWeekly.RangeOf(oldWeek)
		kept := PartitionGranularityWeekly.RangeOf(old.Upper)
		family := providerWorkSessionEventPartitionFamily

		now := kept.Upper.AddDate(0, 0, 12*7+6)
		report, _ := family.Retire(ctx, now, PartitionFamilyState{}, time.Now().Add(time.Minute))
		if len(report.Blocked) != 0 {
			t.Fatalf("blocked %v", report.Blocked)
		}
		if !slices.Equal(report.Dropped, []string{PartitionLegacyLeafName(providerWorkSessionEventTable), providerWorkSessionEventLeaf(old)}) {
			t.Fatalf("dropped %v", report.Dropped)
		}
		if testProviderWorkSessionLeafExists(t, ctx, old) || !testProviderWorkSessionLeafExists(t, ctx, kept) {
			t.Fatal("the drop took the wrong week")
		}
		events := testProviderWorkSessionEvents(t, ctx, clientId)
		if len(events) == 0 || events[0].leaf != providerWorkSessionEventLeaf(kept) {
			t.Fatalf("remaining events %+v", events)
		}

		// the pass's clock says the kept week is 13 weeks past; the database's
		// does not, and refuses
		report, _ = family.Retire(ctx, kept.Upper.AddDate(0, 0, 13*7), PartitionFamilyState{}, time.Now().Add(time.Minute))
		if blocker := report.Blocked[providerWorkSessionEventLeaf(kept)]; !strings.Contains(blocker, "kept 13 weeks") || len(report.Dropped) != 0 {
			t.Fatalf("a drop ahead of the database's clock: %+v", report)
		}
		if !testProviderWorkSessionLeafExists(t, ctx, kept) {
			t.Fatal("the refused drop removed the week")
		}
	})
}

// A writer holds an open transaction with a connection insert; the cutover
// step must wait on it (pg_blocking_pids names the writer), and the writer
// then commits. Each goroutine owns one connection; cancellation joins both.
type testProviderWorkSessionWriter struct {
	ready  chan uint32
	commit chan struct{}
	done   chan error
}

func testProviderWorkSessionStartWriter(t testing.TB, ctx context.Context, workers *sync.WaitGroup, clientId Id) *testProviderWorkSessionWriter {
	writer := &testProviderWorkSessionWriter{ready: make(chan uint32, 1), commit: make(chan struct{}), done: make(chan error, 1)}
	workers.Add(1)
	go func() {
		defer workers.Done()
		writer.done <- contractPartitionTx(ctx, func(tx PgTx) {
			RaisePgResult(tx.Exec(ctx, testProviderWorkSessionConnectSql, clientId, NewId(), NowUtc(), make([]byte, 32), NewId()))
			writer.ready <- tx.Conn().PgConn().PID()
			select {
			case <-writer.commit:
			case <-ctx.Done():
				panic(ctx.Err())
			}
		})
	}()
	return writer
}

// Waits until a backend running a statement that contains fragment is blocked
// by pid, then lets the writer commit.
func (self *testProviderWorkSessionWriter) releaseWhenBlocking(t testing.TB, ctx context.Context, fragment string, stepDone <-chan error) {
	var pid uint32
	select {
	case pid = <-self.ready:
	case err := <-self.done:
		t.Fatalf("writer failed before its barrier: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	MaintenanceDb(ctx, func(conn PgConn) {
		for {
			var blocked bool
			Raise(conn.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity activity
				WHERE activity.query LIKE '%' || $2 || '%' AND $1::int = ANY (pg_blocking_pids(activity.pid)))`, pid, fragment).Scan(&blocked))
			if blocked {
				break
			}
			select {
			case err := <-stepDone:
				t.Fatalf("the step ended before it waited on the writer: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
	close(self.commit)
	if err := <-self.done; err != nil {
		t.Fatalf("writer commit: %v", err)
	}
}

// Prepare and finalize while connections arrive: the identity build waits for
// a writer's open insert and indexes it, the swap waits for another's, and
// connections keep appending through the cutover, their sequences contiguous
// and their retires keeping the admit's network.
func TestProviderWorkSessionPartitionCutoverWithConnectionsArriving(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		var workers sync.WaitGroup
		defer func() {
			cancel()
			workers.Wait()
		}()
		testProviderWorkSessionRequirePlain(t, ctx)
		cutoverWeek := PartitionGranularityWeekly.AddRanges(PartitionGranularityWeekly.RangeStart(NowUtc()), 2)
		clientIds := []Id{}
		networkIds := map[Id]Id{}
		for range 4 {
			clientId, networkId := testProviderWorkSessionClient(t, ctx)
			clientIds = append(clientIds, clientId)
			networkIds[clientId] = networkId
		}
		// connections opened before the cutover and closed after it
		before := map[Id]Id{}
		for _, clientId := range clientIds {
			before[clientId] = testProviderWorkSessionConnect(t, ctx, clientId)
		}

		// the identity build waits for an open insert
		writer := testProviderWorkSessionStartWriter(t, ctx, &workers, clientIds[0])
		prepareDone := make(chan error, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			prepareDone <- testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{CutoverWeek: cutoverWeek})
		}()
		writer.releaseWhenBlocking(t, ctx, providerWorkSessionEventIdentityIndexName, prepareDone)
		if err := <-prepareDone; err != nil {
			t.Fatalf("prepare: %v", err)
		}
		if !testContractPartitionScalar[bool](t, ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'provider_work_session_event_pkey'::regclass`) {
			t.Fatal("the identity primary key is not valid")
		}

		// connections between prepare and finalize
		for _, clientId := range clientIds[1:] {
			testProviderWorkSessionDisconnect(t, ctx, testProviderWorkSessionConnect(t, ctx, clientId))
		}

		// the swap waits for an open insert
		writer = testProviderWorkSessionStartWriter(t, ctx, &workers, clientIds[1])
		finalizeDone := make(chan error, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			finalizeDone <- testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Finalize: true})
		}()
		writer.releaseWhenBlocking(t, ctx, "LOCK TABLE", finalizeDone)
		if err := <-finalizeDone; err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if !isProviderWorkSessionEventPartitioned(ctx) {
			t.Fatal("finalize left the journal plain")
		}

		// connections after the swap, and the retires of those from before it
		for _, clientId := range clientIds {
			testProviderWorkSessionDisconnect(t, ctx, before[clientId])
			testProviderWorkSessionDisconnect(t, ctx, testProviderWorkSessionConnect(t, ctx, clientId))
		}
		legacy := PartitionLegacyLeafName(providerWorkSessionEventTable)
		for _, clientId := range clientIds {
			events := testProviderWorkSessionEvents(t, ctx, clientId)
			testProviderWorkSessionRequireContiguous(t, events)
			admits, retires := 0, 0
			for _, event := range events {
				if event.leaf != legacy {
					t.Fatalf("an event before the cutover week is in %s", event.leaf)
				}
				if event.networkId == nil || *event.networkId != networkIds[clientId] {
					t.Fatalf("an event lost its network: %+v", events)
				}
				switch event.kind {
				case "admit":
					admits += 1
				case "retire":
					retires += 1
				}
			}
			// the writers' connections stay open
			if open := testContractPartitionScalar[int64](t, ctx, `SELECT count(*) FROM network_client_connection WHERE client_id = $1 AND connected`, clientId); int64(admits-retires) != open {
				t.Fatalf("client has %d admits, %d retires and %d open connections", admits, retires, open)
			}
		}
	})
}

// Each step of an interrupted cutover resumes: an identity build canceled
// while it waits for a writer is dropped and rebuilt, a validation that cannot
// take its lock leaves the unvalidated bound for the next run, a lost week's
// partition is created again, and finalize run again after its swap only
// analyzes.
func TestProviderWorkSessionPartitionInterruptedCutoverRestarts(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		var workers sync.WaitGroup
		defer func() {
			cancel()
			workers.Wait()
		}()
		testProviderWorkSessionRequirePlain(t, ctx)
		cutoverWeek := PartitionGranularityWeekly.AddRanges(PartitionGranularityWeekly.RangeStart(NowUtc()), 2)
		clientId, _ := testProviderWorkSessionClient(t, ctx)
		testProviderWorkSessionConnect(t, ctx, clientId)

		// a build canceled while it waits for a writer leaves an invalid index;
		// while the build still runs, prepare refuses to drop it
		otherId, _ := testProviderWorkSessionClient(t, ctx)
		writer := testProviderWorkSessionStartWriter(t, ctx, &workers, otherId)
		buildDone := make(chan error, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, build := providerWorkSessionIdentityIndexSql()
			buildDone <- providerWorkSessionScan(ctx, time.Minute, []string{build})
		}()
		writerPid := <-writer.ready
		var buildPid int32
		MaintenanceDb(ctx, func(conn PgConn) {
			for {
				// the build reports its index once the index is visible, and
				// then waits for the writer
				Raise(conn.QueryRow(ctx, `SELECT COALESCE((SELECT progress.pid FROM pg_stat_progress_create_index progress
					WHERE progress.index_relid = to_regclass('public.provider_work_session_event_identity')
					AND $1::int = ANY (pg_blocking_pids(progress.pid))), 0)`, writerPid).Scan(&buildPid))
				if buildPid != 0 {
					return
				}
				select {
				case err := <-buildDone:
					t.Fatalf("the build ended before it waited on the writer: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{CutoverWeek: cutoverWeek}); err == nil || !strings.Contains(err.Error(), "being built") {
			t.Fatalf("prepare beside a running build: %v", err)
		}
		if !testContractPartitionScalar[bool](t, ctx, `SELECT pg_cancel_backend($1)`, buildPid) {
			t.Fatal("the build could not be canceled")
		}
		if err := <-buildDone; err == nil {
			t.Fatal("the canceled build succeeded")
		}
		close(writer.commit)
		if err := <-writer.done; err != nil {
			t.Fatal(err)
		}
		if valid := testContractPartitionScalar[*bool](t, ctx, `SELECT (SELECT indisvalid FROM pg_index WHERE indexrelid = to_regclass('public.provider_work_session_event_identity'))`); valid == nil || *valid {
			t.Fatalf("the canceled build left valid=%v", valid)
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{CutoverWeek: cutoverWeek}); err != nil {
			t.Fatalf("prepare after the canceled build: %v", err)
		}
		shape, staged := readProviderWorkSessionShapes(ctx)
		if providerWorkSessionPrimaryKey(shape) != providerWorkSessionEventIdentityKey || staged == nil || shape.legacyBound == nil || !shape.legacyBound.validated {
			t.Fatalf("prepare did not rebuild and finish: key %q", providerWorkSessionPrimaryKey(shape))
		}

		// the bound added but not validated, as a run stopped before validation
		// leaves it; a validation that cannot take its lock leaves it so again
		Raise(contractPartitionExec(ctx, []string{
			fmt.Sprintf("ALTER TABLE public.%s DROP CONSTRAINT %s", quoteIdentifier(providerWorkSessionEventTable), quoteIdentifier(providerWorkSessionLegacyBound())),
			providerWorkSessionLegacyBoundSql(cutoverWeek),
		}))
		holderReady := make(chan struct{})
		holderCommit := make(chan struct{})
		holderDone := make(chan error, 1)
		workers.Add(1)
		go func() {
			defer workers.Done()
			holderDone <- contractPartitionTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, `LOCK TABLE provider_work_session_event IN SHARE UPDATE EXCLUSIVE MODE`))
				close(holderReady)
				select {
				case <-holderCommit:
				case <-ctx.Done():
					panic(ctx.Err())
				}
			})
		}()
		select {
		case <-holderReady:
		case err := <-holderDone:
			t.Fatalf("the lock holder failed: %v", err)
		}
		settings := DefaultProviderWorkSessionPartitionSettings()
		settings.ValidateLockTimeout = 50 * time.Millisecond
		settings.ValidateAttempts = 1
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{CutoverWeek: cutoverWeek, Settings: settings}); err == nil {
			t.Fatal("prepare validated through a held lock")
		}
		close(holderCommit)
		if err := <-holderDone; err != nil {
			t.Fatal(err)
		}
		shape, _ = readProviderWorkSessionShapes(ctx)
		if shape.legacyBound == nil || shape.legacyBound.validated {
			t.Fatalf("the failed validation left bound %+v", shape.legacyBound)
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Finalize: true}); err == nil {
			t.Fatal("finalize with an unvalidated bound was accepted")
		}

		// one of the cutover's weeks lost: prepare creates it again, and
		// validates the bound
		last := providerWorkSessionEventLeaf(PartitionGranularityWeekly.RangeOf(PartitionGranularityWeekly.AddRanges(cutoverWeek, providerWorkSessionEventPartitionFamily.AheadRanges)))
		Raise(contractPartitionExec(ctx, []string{
			fmt.Sprintf("ALTER TABLE public.%s DETACH PARTITION public.%s", quoteIdentifier(providerWorkSessionStagedParent()), quoteIdentifier(last)),
			fmt.Sprintf("DROP TABLE public.%s", quoteIdentifier(last)),
		}))
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{CutoverWeek: cutoverWeek}); err != nil {
			t.Fatalf("prepare again: %v", err)
		}
		shape, _ = readProviderWorkSessionShapes(ctx)
		if shape.legacyBound == nil || !shape.legacyBound.validated || !testContractPartitionScalar[bool](t, ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+last) {
			t.Fatal("prepare did not resume")
		}

		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Finalize: true}); err != nil {
			t.Fatalf("finalize: %v", err)
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Finalize: true}); err != nil {
			t.Fatalf("finalize again after the swap: %v", err)
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{CutoverWeek: cutoverWeek}); err == nil {
			t.Fatal("prepare of a partitioned journal was accepted")
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Abort: true}); err == nil {
			t.Fatal("abort of a partitioned journal was accepted")
		}
		for _, id := range []Id{clientId, otherId} {
			for _, event := range testProviderWorkSessionEvents(t, ctx, id) {
				if event.leaf != PartitionLegacyLeafName(providerWorkSessionEventTable) {
					t.Fatalf("event %+v is not in the legacy partition", event)
				}
			}
		}
	})
}

// Dry runs change nothing; before the cutover week every event written since
// the swap is in the legacy partition, and rollback returns them all to the
// plain table with its guards; an event in a week's partition ends the
// rollback window. Abort removes a prepared cutover.
func TestProviderWorkSessionPartitionRollbackKeepsWrites(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testProviderWorkSessionRequirePlain(t, ctx)
		cutoverWeek := PartitionGranularityWeekly.AddRanges(PartitionGranularityWeekly.RangeStart(NowUtc()), 2)
		clientId, networkId := testProviderWorkSessionClient(t, ctx)
		testProviderWorkSessionConnect(t, ctx, clientId)

		for _, options := range []ProviderWorkSessionPartitionMigrateOptions{
			{CutoverWeek: cutoverWeek, DryRun: true},
			{Finalize: true, DryRun: true},
			{Abort: true, DryRun: true},
		} {
			_ = testProviderWorkSessionMigrate(t, ctx, options)
		}
		if shape, staged := readProviderWorkSessionShapes(ctx); staged != nil || shape.legacyBound != nil || providerWorkSessionPrimaryKey(shape) != providerWorkSessionEventSequenceKey {
			t.Fatal("a dry run changed the journal")
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Finalize: true}); err == nil {
			t.Fatal("finalize of an unprepared journal was accepted")
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{CutoverWeek: cutoverWeek}); err != nil {
			t.Fatal(err)
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Abort: true}); err != nil {
			t.Fatal(err)
		}
		if _, staged := readProviderWorkSessionShapes(ctx); staged != nil {
			t.Fatal("abort kept the staged parent")
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{CutoverWeek: cutoverWeek}); err != nil {
			t.Fatal(err)
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Finalize: true}); err != nil {
			t.Fatal(err)
		}
		testProviderWorkSessionConnect(t, ctx, clientId)
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Rollback: true, DryRun: true}); err != nil || !isProviderWorkSessionEventPartitioned(ctx) {
			t.Fatalf("rollback dry run: %v", err)
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Rollback: true}); err != nil {
			t.Fatal(err)
		}
		if isProviderWorkSessionEventPartitioned(ctx) {
			t.Fatal("rollback left the journal partitioned")
		}
		events := testProviderWorkSessionEvents(t, ctx, clientId)
		testProviderWorkSessionRequireContiguous(t, events)
		if len(events) != 2 || events[1].leaf != providerWorkSessionEventTable {
			t.Fatalf("rollback lost an event written after the swap: %+v", events)
		}
		for _, statement := range []string{`DELETE FROM provider_work_session_event WHERE client_id = $1`, `TRUNCATE provider_work_session_event`} {
			args := []any{clientId}
			if strings.HasPrefix(statement, "TRUNCATE") {
				args = nil
			}
			if _, message := testProviderWorkSessionRolledBack(ctx, statement, args...); message != "provider work originals are append-only" {
				t.Fatalf("%s after rollback: %q", statement, message)
			}
		}

		// once a week's partition holds an event, rollback refuses
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{CutoverWeek: cutoverWeek}); err != nil {
			t.Fatal(err)
		}
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Finalize: true}); err != nil {
			t.Fatal(err)
		}
		testProviderWorkSessionInsertEvent(t, ctx, clientId, &networkId, nil, 3, "baseline", cutoverWeek.Add(time.Hour))
		if err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Rollback: true}); err == nil || !isProviderWorkSessionEventPartitioned(ctx) {
			t.Fatalf("rollback with an event in a week's partition: %v", err)
		}
		if count := len(testProviderWorkSessionEvents(t, ctx, clientId)); count != 3 {
			t.Fatalf("a refused rollback changed the events: %d", count)
		}
	})
}

// The runway pass abandons a prepared cutover once its week is less than the
// abandon margin away, and not before.
func TestProviderWorkSessionPartitionAbandonsStaleCutover(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testProviderWorkSessionRequirePlain(t, ctx)
		cutoverWeek := PartitionGranularityWeekly.AddRanges(PartitionGranularityWeekly.RangeStart(NowUtc()), 2)
		var deadline time.Time
		err := testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{
			CutoverWeek: cutoverWeek,
			Logf: func(format string, args ...any) {
				if value, found := strings.CutPrefix(fmt.Sprintf(format, args...), "prepared; run --finalize before "); found {
					var parseErr error
					deadline, parseErr = time.Parse(time.RFC3339, value)
					Raise(parseErr)
				}
			},
		})
		if err != nil || deadline.IsZero() {
			t.Fatalf("prepare did not report its deadline: %v", err)
		}
		if !deadline.Equal(cutoverWeek.Add(-DefaultProviderWorkSessionPartitionSettings().AbandonMargin)) {
			t.Fatalf("deadline %s", deadline)
		}
		family := providerWorkSessionEventPartitionFamily
		if report := family.EnsureRunway(ctx, deadline.Add(-time.Minute)); len(report.Errors) != 0 {
			t.Fatalf("runway %+v", report)
		}
		if _, prepared, err := preparedProviderWorkSessionCutoverWeek(ctx); err != nil || !prepared {
			t.Fatalf("abandoned before the deadline: %v", err)
		}
		if report := family.EnsureRunway(ctx, deadline.Add(time.Minute)); len(report.Errors) != 0 {
			t.Fatalf("runway %+v", report)
		}
		if _, prepared, err := preparedProviderWorkSessionCutoverWeek(ctx); err != nil || prepared {
			t.Fatalf("not abandoned after the deadline: %v", err)
		}
		if _, staged := readProviderWorkSessionShapes(ctx); staged != nil {
			t.Fatal("the abandoned cutover kept its staged parent")
		}
		// a connection still appends to the plain journal
		clientId, _ := testProviderWorkSessionClient(t, ctx)
		testProviderWorkSessionConnect(t, ctx, clientId)
	})
}

// The schema audit reads clean through the cutover: building the identity
// index, prepared, partitioned and rolled back.
func TestAuditSchemaCleanThroughTheProviderWorkSessionCutover(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testProviderWorkSessionRequirePlain(t, ctx)
		requireClean := func(state string) {
			if result := AuditSchema(ctx); result.Diff.HasDifferences() {
				t.Fatalf("%s drifts:\n%s", state, result.Diff.Report())
			}
		}
		_, build := providerWorkSessionIdentityIndexSql()
		Raise(providerWorkSessionScan(ctx, time.Minute, []string{build}))
		requireClean("an identity build")
		cutoverWeek := PartitionGranularityWeekly.AddRanges(PartitionGranularityWeekly.RangeStart(NowUtc()), 2)
		Raise(testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{CutoverWeek: cutoverWeek}))
		requireClean("a prepared cutover")
		Raise(testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Finalize: true}))
		requireClean("the partitioned journal")
		Raise(testProviderWorkSessionMigrate(t, ctx, ProviderWorkSessionPartitionMigrateOptions{Rollback: true}))
		requireClean("a rolled back cutover")
	})
}

// The generator keeps a client's sequences unique on the partitioned journal,
// where the primary key alone would not: concurrent appends of one client get
// distinct, contiguous sequences, and the head refuses to go back.
func TestProviderWorkSessionPartitionSequencesStayUnique(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		testProviderWorkSessionRequirePlain(t, ctx)
		currentWeek := PartitionGranularityWeekly.RangeStart(NowUtc())
		clientId, networkId := testProviderWorkSessionClient(t, ctx)
		// the legacy partition holds the client's first event
		testProviderWorkSessionInsertEvent(t, ctx, clientId, &networkId, nil, 1, "baseline", currentWeek.Add(-time.Hour))
		Testing_PartitionProviderWorkSessionEventsAt(ctx, currentWeek)

		var workers sync.WaitGroup
		for range 8 {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for range 4 {
					// a busy endpoint is a 40001 the transaction retries
					Tx(ctx, func(tx PgTx) {
						RaisePgResult(tx.Exec(ctx, `SELECT provider_work_session_append($1, NULL, 'baseline', NULL)`, clientId))
					}, TxReadCommitted)
				}
			}()
		}
		workers.Wait()
		events := testProviderWorkSessionEvents(t, ctx, clientId)
		testProviderWorkSessionRequireContiguous(t, events)
		if len(events) != 1+8*4 {
			t.Fatalf("%d events", len(events))
		}
		for _, statement := range []string{
			`UPDATE provider_work_session_head SET sequence = sequence - 1 WHERE client_id = $1`,
			`UPDATE provider_work_session_head SET sequence = sequence + 2 WHERE client_id = $1`,
			`DELETE FROM provider_work_session_head WHERE client_id = $1`,
		} {
			if _, message := testProviderWorkSessionRolledBack(ctx, statement, clientId); message != "provider work journal birth cannot be reset" {
				t.Fatalf("%s: %q", statement, message)
			}
		}
		if _, message := testProviderWorkSessionRolledBack(ctx, `TRUNCATE provider_work_session_head`); message != "provider work originals are append-only" {
			t.Fatalf("truncate head: %q", message)
		}
	})
}
