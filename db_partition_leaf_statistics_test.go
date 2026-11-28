// Regressions for the empty partition leaves that maintenance recorded
// analyzed empty (2026-11-25 00:01Z). PostgreSQL plans a table it has never
// analyzed (pg_class.reltuples -1) at ten pages at least, and one an ANALYZE or
// VACUUM found empty (reltuples 0) at its size, so a plan made while such a
// leaf is empty reads it with a sequential scan costed at zero. The services
// cache generic plans (pgx prepared statements), and those plans kept the scan
// once the day opened and its rows landed: every contract lookup read the
// whole new leaf until autovacuum analyzed it about 15 minutes later. The daily
// maintenance's final ANALYZE took every empty leaf of the days ahead that way,
// and the churn tables' VACUUM of a partitioned parent takes them every five
// minutes. The root-cause tests use only DbMaintenance, VacuumHotTables, the
// partition maintenance passes and SQL, so they ran unchanged on the code
// before the fix and failed there.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
)

// pg_class.reltuples of a public relation: -1 never analyzed, 0 analyzed
// empty.
func testLeafReltuples(t testing.TB, ctx context.Context, relation string) float64 {
	t.Helper()
	return testRegistryScalar[float64](t, ctx, `SELECT reltuples::float8 FROM pg_class WHERE oid = to_regclass($1)`, "public."+quoteIdentifier(relation))
}

// How a plan reads one relation, from EXPLAIN (FORMAT JSON).
type testPlanScan struct {
	nodeType  string
	totalCost float64
}

// Whether the scan reads the relation through an index: an index scan, an
// index-only scan, or a heap read through an index's bitmap.
func (self testPlanScan) indexed() bool {
	return self.nodeType == "Index Scan" || self.nodeType == "Index Only Scan" || self.nodeType == "Bitmap Heap Scan"
}

// The scan of each relation the plans read, by relation name.
func testPlanScans(t testing.TB, raw string) map[string]testPlanScan {
	t.Helper()
	type planNode struct {
		NodeType     string     `json:"Node Type"`
		RelationName string     `json:"Relation Name"`
		TotalCost    float64    `json:"Total Cost"`
		Plans        []planNode `json:"Plans"`
	}
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &plans); err != nil {
		t.Fatal(err)
	}
	scans := map[string]testPlanScan{}
	var visit func(node planNode)
	visit = func(node planNode) {
		if node.RelationName != "" {
			scans[node.RelationName] = testPlanScan{nodeType: node.NodeType, totalCost: node.TotalCost}
		}
		for _, child := range node.Plans {
			visit(child)
		}
	}
	for _, plan := range plans {
		visit(plan.Plan)
	}
	return scans
}

// The daily maintenance's final ANALYZE (SKIP_LOCKED, the whole database)
// takes every leaf, the empty leaves of the days ahead too. After it, each of
// those still reads never analyzed, so a plan made before its day opens reads
// it at ten pages, through its primary key; a day's leaf that holds a row
// keeps the statistics the analyze took.
func TestDbMaintenanceFinalAnalyzeLeavesEmptyDayLeavesNeverAnalyzed(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Testing_PartitionContracts(ctx)
		day := ContractPartitionDayStart(NowUtc())
		// a row in each of the day's leaves
		testInsertContractRows(t, ctx, testContractIdAt(day.Add(time.Minute)), true)
		aheadLeaves := []string{}
		for _, table := range ContractPartitionTables {
			for i := 1; i <= ContractPartitionAheadDays; i += 1 {
				aheadLeaves = append(aheadLeaves, ContractPartitionName(table, day.AddDate(0, 0, i)))
			}
		}
		for _, leaf := range aheadLeaves {
			if reltuples := testLeafReltuples(t, ctx, leaf); reltuples != -1 {
				t.Fatalf("fixture: %s reads reltuples %v before the maintenance, want -1 (never analyzed)", leaf, reltuples)
			}
		}

		outcome := DbMaintenance(ctx, 0, &DbMaintenanceOptions{Analyze: true})
		if outcome.Stopped != "" {
			t.Fatalf("analyze-only outcome = %+v", *outcome)
		}
		for _, leaf := range aheadLeaves {
			if reltuples := testLeafReltuples(t, ctx, leaf); reltuples != -1 {
				t.Errorf("%s reads reltuples %v after the final analyze, want -1 (never analyzed): a plan made now reads it with a sequential scan costed at zero", leaf, reltuples)
			}
		}
		for _, table := range ContractPartitionTables {
			leaf := ContractPartitionName(table, day)
			if reltuples := testLeafReltuples(t, ctx, leaf); reltuples <= 0 {
				t.Errorf("%s holds a row but reads reltuples %v after the final analyze", leaf, reltuples)
			}
		}
	})
}

// The plans a service caches. After the final analyze, the generic plan of a
// contract lookup reads each empty day ahead through its primary key, never
// with a sequential scan costed at zero; and the generic plan a prepared
// lookup caches then still reads a day through its key once a contract of
// that day landed, as rows land when the day opens.
func TestDbMaintenanceGenericPlanReadsEmptyDayLeavesThroughTheirKeys(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		Testing_PartitionContracts(ctx)
		day := ContractPartitionDayStart(NowUtc())
		outcome := DbMaintenance(ctx, 0, &DbMaintenanceOptions{Analyze: true})
		if outcome.Stopped != "" {
			t.Fatalf("analyze-only outcome = %+v", *outcome)
		}

		conn, err := AcquireMaintenanceDbConn(ctx)
		Raise(err)
		defer discardPgConnection(ctx, conn)
		const lookup = `SELECT 1 FROM transfer_contract WHERE contract_id = $1`
		// the simple protocol, so $1 stays the generic plan's parameter
		results, err := conn.Conn().PgConn().Exec(ctx, `EXPLAIN (GENERIC_PLAN, FORMAT JSON) `+lookup).ReadAll()
		Raise(err)
		if len(results) != 1 || len(results[0].Rows) != 1 {
			t.Fatalf("EXPLAIN (GENERIC_PLAN) returned %d results", len(results))
		}
		raw := string(results[0].Rows[0][0])
		scans := testPlanScans(t, raw)
		for i := 1; i <= ContractPartitionAheadDays; i += 1 {
			leaf := ContractPartitionName("transfer_contract", day.AddDate(0, 0, i))
			scan, ok := scans[leaf]
			if !ok {
				t.Fatalf("the generic plan does not read %s: %s", leaf, raw)
			}
			if !scan.indexed() {
				t.Errorf("the generic plan reads empty %s with a %s (total cost %.2f), want its primary key", leaf, scan.nodeType, scan.totalCost)
			}
		}

		// a prepared lookup caches its generic plan while the day is empty
		openDay := day.AddDate(0, 0, 2)
		leaf := ContractPartitionName("transfer_contract", openDay)
		contractId := testContractIdAt(openDay.Add(time.Hour))
		RaisePgResult(conn.Exec(ctx, `SET plan_cache_mode = force_generic_plan`))
		RaisePgResult(conn.Exec(ctx, `PREPARE synthetic_contract_lookup (uuid) AS `+lookup))
		RaisePgResult(conn.Exec(ctx, fmt.Sprintf(`EXECUTE synthetic_contract_lookup('%s')`, contractId)))
		// the day's rows land, which invalidates no cached plan
		testInsertContractRows(t, ctx, contractId, false)
		connect.AssertEqual(t, testContractPartitionOf(t, ctx, "transfer_contract", contractId), leaf)
		Raise(conn.QueryRow(ctx, fmt.Sprintf(`EXPLAIN (FORMAT JSON) EXECUTE synthetic_contract_lookup('%s')`, contractId)).Scan(&raw))
		scan, ok := testPlanScans(t, raw)[leaf]
		if !ok {
			t.Fatalf("the cached generic plan does not read %s: %s", leaf, raw)
		}
		if !scan.indexed() {
			t.Errorf("the generic plan cached while %s was empty reads it, now that it holds a row, with a %s (total cost %.2f), want its primary key", leaf, scan.nodeType, scan.totalCost)
		}
	})
}

// The churn tables' vacuum (VacuumHotTables, every five minutes) vacuums a
// partitioned table's every leaf, and a VACUUM of an empty leaf records it
// analyzed empty as an ANALYZE does: transfer_debit_journal's days ahead read
// so at every midnight. After a run they read never analyzed again.
func TestVacuumHotTablesLeavesEmptyDayLeavesNeverAnalyzed(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := NowUtc()
		Testing_PartitionTransferDebitJournal(ctx, TestingTransferDebitJournalCutoverDay(now))
		family := transferDebitJournalPartitionFamily
		day := family.Granularity.RangeStart(now)
		aheadLeaves := []string{}
		for i := 1; i <= TransferDebitJournalAheadDays; i += 1 {
			aheadLeaves = append(aheadLeaves, family.LeafName(transferDebitJournalTable, family.RangeAt(day.AddDate(0, 0, i))))
		}
		for _, leaf := range aheadLeaves {
			if reltuples := testLeafReltuples(t, ctx, leaf); reltuples != -1 {
				t.Fatalf("fixture: %s reads reltuples %v before the vacuum, want -1 (never analyzed)", leaf, reltuples)
			}
		}

		// the index pass is not due, so the run only vacuums
		state := VacuumHotTables(ctx, DbHotVacuumState{IndexPassTime: time.Now()}, testDbHotVacuumSettings(transferDebitJournalTable))
		if state.Tables[transferDebitJournalTable].VacuumTime.IsZero() {
			t.Fatalf("the run did not vacuum %s: %+v", transferDebitJournalTable, state)
		}
		for _, leaf := range aheadLeaves {
			if reltuples := testLeafReltuples(t, ctx, leaf); reltuples != -1 {
				t.Errorf("%s reads reltuples %v after the churn tables' vacuum, want -1 (never analyzed): a plan made now reads it with a sequential scan costed at zero", leaf, reltuples)
			}
		}
	})
}

// The contract task analyzes a day's leaves a minute after the day opens, once
// they hold rows, rather than leaving them to autovacuum, whose busy workers
// took about 15 minutes on 2026-11-25. Before the minute nothing is analyzed,
// an opened day that holds no row yet is left never analyzed, and a day is
// analyzed once. The day is in the past, so the statistics clock of an
// analyze, which is real, is after the minute.
func TestContractPartitionMaintenanceAnalyzesAnOpenedDayAMinuteIn(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		today := ContractPartitionDayStart(NowUtc())
		Testing_PartitionContractsFrom(ctx, today.AddDate(0, 0, -3))
		day := today.AddDate(0, 0, -2)
		testInsertContractRows(t, ctx, testContractIdAt(day.Add(30*time.Second)), true)
		leaf := ContractPartitionName("transfer_contract", day)
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf), float64(-1))

		report, cursor := MaintainContractPartitions(ctx, day.Add(30*time.Second), ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, slices.Contains(report.Analyzed, leaf), false)
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf), float64(-1))

		report, cursor = MaintainContractPartitions(ctx, day.Add(time.Minute), cursor, time.Minute)
		for _, table := range ContractPartitionTables {
			if !slices.Contains(report.Analyzed, ContractPartitionName(table, day)) {
				t.Errorf("a minute into its day, %s was not analyzed: %v", ContractPartitionName(table, day), report.Analyzed)
			}
		}
		if reltuples := testLeafReltuples(t, ctx, leaf); reltuples <= 0 {
			t.Errorf("%s holds a row but reads reltuples %v a minute into its day", leaf, reltuples)
		}

		// analyzed once
		report, cursor = MaintainContractPartitions(ctx, day.Add(2*time.Minute), cursor, time.Minute)
		connect.AssertEqual(t, slices.Contains(report.Analyzed, leaf), false)

		// the next day opens without a row: its leaves stay never analyzed
		next := day.AddDate(0, 0, 1)
		nextLeaf := ContractPartitionName("transfer_contract", next)
		report, _ = MaintainContractPartitions(ctx, next.Add(time.Minute), cursor, time.Minute)
		connect.AssertEqual(t, slices.Contains(report.Analyzed, nextLeaf), false)
		connect.AssertEqual(t, testLeafReltuples(t, ctx, nextLeaf), float64(-1))
	})
}

// The registry's retention task analyzes the days of the families it
// analyzes a minute after they open, as the contract task does its own:
// transfer_debit_journal's leaf of a day that holds a debit.
func TestPartitionRetentionAnalyzesAnOpenedDayAMinuteIn(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := NowUtc()
		Testing_PartitionTransferDebitJournal(ctx, TestingTransferDebitJournalCutoverDay(now))
		family := transferDebitJournalPartitionFamily
		// in the past, so the statistics clock of an analyze is after the minute
		day := family.Granularity.AddRanges(family.Granularity.RangeStart(now), -2)
		testDebitJournalInsert(t, ctx, testContractIdAt(day.Add(30*time.Second)), testDebitJournalBalance())
		leaf := family.LeafName(transferDebitJournalTable, family.RangeAt(day))
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf), float64(-1))

		report, _ := MaintainPartitionRetention(ctx, day.Add(time.Minute), PartitionMaintenanceState{}, time.Minute)
		if !slices.Contains(report.Analyzed[family.Name], leaf) {
			t.Errorf("a minute into its day, %s was not analyzed: %v", leaf, report.Analyzed[family.Name])
		}
		if reltuples := testLeafReltuples(t, ctx, leaf); reltuples <= 0 {
			t.Errorf("%s holds a debit but reads reltuples %v a minute into its day", leaf, reltuples)
		}
	})
}

// After an ANALYZE of a family's parent records every empty leaf analyzed
// empty, as the database-wide ANALYZE does, the clear marks each empty leaf of
// a range that has not ended never analyzed again, the current range's too,
// and leaves alone a leaf that holds a row and the empty leaf of a range that
// has ended. A leaf another session holds is passed over at once, without a
// place in its lock queue, and the next pass clears it once the holder is
// gone; a pass with nothing to clear clears nothing.
func TestPartitionRegistryClearsEmptyLeafStatisticsWithoutQueuing(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := testRegistryEventFamily(t, ctx)
		// autovacuum must not analyze the leaves behind the test's back
		family.LeafReloptions = []string{"autovacuum_enabled=false"}
		// a synthetic month out, so a real midnight moves nothing
		now := PartitionGranularityDaily.RangeStart(NowUtc().AddDate(0, 1, 0)).Add(13 * time.Hour)
		connect.AssertEqual(t, len(family.EnsureRunway(ctx, now.AddDate(0, 0, -1)).Errors), 0)
		connect.AssertEqual(t, len(family.EnsureRunway(ctx, now).Errors), 0)
		leaf := func(days int) string {
			return family.LeafName("synthetic_registry_event", family.RangeAt(now.AddDate(0, 0, days)))
		}
		// a row ahead of its day
		testRegistryExec(t, ctx, `INSERT INTO synthetic_registry_event (event_id, observed_at) VALUES ($1, $2)`, NewId(), now.AddDate(0, 0, 1))
		testRegistryExec(t, ctx, `ANALYZE synthetic_registry_event`)
		for _, days := range []int{-1, 0, 2, 3} {
			connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(days)), float64(0))
		}
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(1)), float64(1))

		_, release := testDbMaintenanceHoldVacuumLock(t, ctx, leaf(3))
		defer release()
		var cleared, locked []string
		var errs []error
		testDbMaintenanceRunWithoutQueuing(t, ctx, func() *DbMaintenanceOutcome {
			cleared, locked, errs = family.ClearEmptyLeafStatistics(ctx, now)
			return &DbMaintenanceOutcome{}
		}, release, nil, nil, leaf(3))
		connect.AssertEqual(t, len(errs), 0)
		connect.AssertEqual(t, cleared, []string{leaf(0), leaf(2)})
		connect.AssertEqual(t, locked, []string{leaf(3)})
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(0)), float64(-1))
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(2)), float64(-1))
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(3)), float64(0))
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(1)), float64(1))
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(-1)), float64(0))

		release()
		cleared, locked, errs = family.ClearEmptyLeafStatistics(ctx, now)
		connect.AssertEqual(t, len(errs), 0)
		connect.AssertEqual(t, cleared, []string{leaf(3)})
		connect.AssertEqual(t, len(locked), 0)
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(3)), float64(-1))

		cleared, locked, errs = family.ClearEmptyLeafStatistics(ctx, now)
		connect.AssertEqual(t, len(errs)+len(cleared)+len(locked), 0)
	})
}

// An opened leaf is analyzed once it holds rows, OpenLeafAnalyzeDelay into
// its range and not before, and once; an opened leaf that holds no row is
// left never analyzed, and one an ANALYZE recorded analyzed empty is analyzed
// again once it holds a row. The family's AnalyzeAdmission holds an analyze
// for a later run. The days are in the past, so the statistics clock of an
// analyze, which is real, is after each day's delay.
func TestPartitionRegistryAnalyzesOpenLeavesOnceTheyHoldRows(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := testRegistryEventFamily(t, ctx)
		family.LeafReloptions = []string{"autovacuum_enabled=false"}
		day := PartitionGranularityDaily.RangeStart(NowUtc().AddDate(0, 0, -5))
		connect.AssertEqual(t, len(family.EnsureRunway(ctx, day).Errors), 0)
		leaf := func(days int) string {
			return family.LeafName("synthetic_registry_event", family.RangeAt(day.AddDate(0, 0, days)))
		}
		insert := func(at time.Time) {
			testRegistryExec(t, ctx, `INSERT INTO synthetic_registry_event (event_id, observed_at) VALUES ($1, $2)`, NewId(), at)
		}
		deadline := time.Now().Add(time.Minute)
		delay := DefaultPartitionFamilySettings().OpenLeafAnalyzeDelay

		insert(day.Add(10 * time.Second))
		connect.AssertEqual(t, len(family.AnalyzeOpenLeaves(ctx, day.Add(delay-time.Second), deadline)), 0)
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(0)), float64(-1))
		connect.AssertEqual(t, family.AnalyzeOpenLeaves(ctx, day.Add(delay), deadline), []string{leaf(0)})
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(0)), float64(1))
		connect.AssertEqual(t, len(family.AnalyzeOpenLeaves(ctx, day.Add(2*delay), deadline)), 0)

		// the next day opens empty
		next := day.AddDate(0, 0, 1)
		connect.AssertEqual(t, len(family.AnalyzeOpenLeaves(ctx, next.Add(delay), deadline)), 0)
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(1)), float64(-1))
		// an ANALYZE takes it empty, then its first row lands
		testRegistryExec(t, ctx, fmt.Sprintf(`ANALYZE %s`, quoteIdentifier(leaf(1))))
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(1)), float64(0))
		insert(next.Add(time.Hour))
		connect.AssertEqual(t, family.AnalyzeOpenLeaves(ctx, next.Add(2*time.Hour), deadline), []string{leaf(1)})
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(1)), float64(1))

		// the admission holds an analyze
		third := day.AddDate(0, 0, 2)
		insert(third.Add(time.Second))
		waiting := *family
		admissions := []string{}
		waiting.AnalyzeAdmission = func(ctx context.Context, relation string, isLeaf bool) string {
			admissions = append(admissions, relation)
			return "a synthetic holder"
		}
		connect.AssertEqual(t, len(waiting.AnalyzeOpenLeaves(ctx, third.Add(delay), deadline)), 0)
		connect.AssertEqual(t, admissions, []string{leaf(2)})
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf(2)), float64(-1))
		connect.AssertEqual(t, family.AnalyzeOpenLeaves(ctx, third.Add(delay), deadline), []string{leaf(2)})
	})
}

// Waits until the statistics counters show a change to relation since its last
// ANALYZE: each backend publishes its counts within seconds of its commit.
func testWaitLeafChanged(t testing.TB, ctx context.Context, relation string) {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); ; {
		changed := testRegistryScalar[int64](t, ctx, `SELECT COALESCE((SELECT n_mod_since_analyze FROM pg_stat_user_tables WHERE relid = to_regclass($1)), 0)`, "public."+quoteIdentifier(relation))
		if 0 < changed {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the statistics counters show no change to %s", relation)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// The day's analyze is never held by a lock it would not wait for. A process
// that starts or exits between the admission's pg_locks and pg_stat_activity
// has no activity row, and reads as a session the admission cannot see. On
// Main at 2026-11-28 00:01Z one such session held ACCESS SHARE on the new
// day's transfer_contract leaf (a read, which ANALYZE's SHARE UPDATE EXCLUSIVE
// never waits for), and the admission held the analyze for a later run, 15
// minutes on. The leaf was read without statistics until autovacuum analyzed
// it at 00:12:51Z, and the hot reads of open contracts read its whole primary
// key on every call. No test session can be a process without an activity
// row, so the admission's read is shown one, as Main's read saw it. The test
// uses only MaintainContractPartitions and the admission's observed locks,
// which the code before the fix has too.
func TestContractPartitionMaintenanceAnalyzesAnOpenedDayBesideAnUnseenReader(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		today := ContractPartitionDayStart(NowUtc())
		Testing_PartitionContractsFrom(ctx, today.AddDate(0, 0, -3))
		day := today.AddDate(0, 0, -2)
		testInsertContractRows(t, ctx, testContractIdAt(day.Add(30*time.Second)), true)
		leaf := ContractPartitionName("transfer_contract", day)
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf), float64(-1))

		admissions := 0
		contractScopeLocksObserved = func(scope *contractLockScope, locks []contractScopeLock) []contractScopeLock {
			if slices.Contains(scope.relations, "public."+leaf) {
				admissions += 1
				locks = append(locks, contractScopeLock{pid: 4243, relation: leaf, mode: "AccessShareLock", granted: true, transactionAge: -1})
			}
			return locks
		}
		defer func() {
			contractScopeLocksObserved = nil
		}()

		report, _ := MaintainContractPartitions(ctx, day.Add(time.Minute), ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, 1 <= admissions, true)
		if !slices.Contains(report.Analyzed, leaf) {
			t.Fatalf("a minute into its day, %s was not analyzed beside a read of a session the admission cannot see: %v", leaf, report.Analyzed)
		}
		if reltuples := testLeafReltuples(t, ctx, leaf); reltuples <= 0 {
			t.Errorf("%s holds a row but reads reltuples %v a minute into its day", leaf, reltuples)
		}
	})
}

// A lock the day's analyze would wait for (SHARE UPDATE EXCLUSIVE, as a
// vacuum's) still holds it, and the run then reports the leaf: the task runs
// again after its retry interval rather than its cadence, through the day's
// first hour, and the family's published pending leaves and their wait say
// so. Once the holder is gone the next run analyzes the leaf, and nothing is
// pending.
func TestContractPartitionMaintenanceRetriesAnOpenedDayLeftUnanalyzed(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		today := ContractPartitionDayStart(NowUtc())
		Testing_PartitionContractsFrom(ctx, today.AddDate(0, 0, -3))
		day := today.AddDate(0, 0, -2)
		testInsertContractRows(t, ctx, testContractIdAt(day.Add(30*time.Second)), true)
		leaf := ContractPartitionName("transfer_contract", day)
		testWaitLeafChanged(t, ctx, leaf)

		contractScopeLocksObserved = func(scope *contractLockScope, locks []contractScopeLock) []contractScopeLock {
			if slices.Contains(scope.relations, "public."+leaf) {
				locks = append(locks, contractScopeLock{pid: 4244, relation: leaf, mode: "ShareUpdateExclusiveLock", granted: true, transactionAge: -1})
			}
			return locks
		}
		defer func() {
			contractScopeLocksObserved = nil
		}()
		family := contractPartitionFamily.Name
		pending := func() float64 {
			return testutil.ToFloat64(partitionOpenLeavesUnanalyzedGauge.WithLabelValues(family))
		}
		wait := func() float64 {
			return testutil.ToFloat64(partitionOpenLeafUnanalyzedWaitGauge.WithLabelValues(family))
		}

		// 00:03, two minutes past the day's analyze time
		at := day.Add(3 * time.Minute)
		report, cursor := MaintainContractPartitions(ctx, at, ContractRetentionCursor{}, time.Minute)
		connect.AssertEqual(t, slices.Contains(report.Analyzed, leaf), false)
		connect.AssertEqual(t, testLeafReltuples(t, ctx, leaf), float64(-1))
		connect.AssertEqual(t, report.OpenLeafRetry, true)
		connect.AssertEqual(t, pending(), float64(1))
		connect.AssertEqual(t, wait(), float64(120))
		connect.AssertEqual(t, testutil.ToFloat64(partitionOpenLeafSampleTimeGauge.WithLabelValues(family)), float64(at.Unix()))

		// past the retry window the cadence retries, and the leaf stays pending
		late := day.Add(time.Minute + DefaultPartitionFamilySettings().OpenLeafRetryWindow)
		report, cursor = MaintainContractPartitions(ctx, late, cursor, time.Minute)
		connect.AssertEqual(t, slices.Contains(report.Analyzed, leaf), false)
		connect.AssertEqual(t, report.OpenLeafRetry, false)
		connect.AssertEqual(t, pending(), float64(1))
		connect.AssertEqual(t, wait(), DefaultPartitionFamilySettings().OpenLeafRetryWindow.Seconds())

		contractScopeLocksObserved = nil
		report, _ = MaintainContractPartitions(ctx, day.Add(4*time.Minute), cursor, time.Minute)
		if !slices.Contains(report.Analyzed, leaf) {
			t.Fatalf("with its holder gone, %s was not analyzed: %v", leaf, report.Analyzed)
		}
		connect.AssertEqual(t, report.OpenLeafRetry, false)
		connect.AssertEqual(t, pending(), float64(0))
		connect.AssertEqual(t, wait(), float64(0))
	})
}

// An opened leaf the pass leaves unanalyzed is pending once it holds rows: the
// pass reports it with its wait past the range's analyze time, and asks for a
// retry within the family's retry window only. An opened leaf that holds no
// row is left without being pending, and an analyzed one is not pending.
func TestPartitionRegistryReportsOpenedLeavesLeftUnanalyzed(t *testing.T) {
	env := DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		family := testRegistryEventFamily(t, ctx)
		family.LeafReloptions = []string{"autovacuum_enabled=false"}
		day := PartitionGranularityDaily.RangeStart(NowUtc().AddDate(0, 0, -5))
		connect.AssertEqual(t, len(family.EnsureRunway(ctx, day).Errors), 0)
		leaf := func(days int) string {
			return family.LeafName("synthetic_registry_event", family.RangeAt(day.AddDate(0, 0, days)))
		}
		deadline := time.Now().Add(time.Minute)
		delay := DefaultPartitionFamilySettings().OpenLeafAnalyzeDelay
		window := DefaultPartitionFamilySettings().OpenLeafRetryWindow
		waiting := *family
		waiting.AnalyzeAdmission = func(ctx context.Context, relation string, isLeaf bool) string {
			return "a synthetic holder"
		}

		// the day opens without a row: nothing is pending
		report := waiting.AnalyzeOpenLeavesReport(ctx, day.Add(delay), deadline)
		connect.AssertEqual(t, len(report.Analyzed)+len(report.Pending), 0)
		connect.AssertEqual(t, report.Retry, false)

		testRegistryExec(t, ctx, `INSERT INTO synthetic_registry_event (event_id, observed_at) VALUES ($1, $2)`, NewId(), day.Add(10*time.Second))
		testWaitLeafChanged(t, ctx, leaf(0))
		report = waiting.AnalyzeOpenLeavesReport(ctx, day.Add(delay+90*time.Second), deadline)
		connect.AssertEqual(t, len(report.Analyzed), 0)
		connect.AssertEqual(t, report.Pending, []string{leaf(0)})
		connect.AssertEqual(t, report.PendingWait, 90*time.Second)
		connect.AssertEqual(t, report.Retry, true)
		connect.AssertEqual(t, testutil.ToFloat64(partitionOpenLeavesUnanalyzedGauge.WithLabelValues(family.Name)), float64(1))

		report = waiting.AnalyzeOpenLeavesReport(ctx, day.Add(delay+window), deadline)
		connect.AssertEqual(t, report.Pending, []string{leaf(0)})
		connect.AssertEqual(t, report.Retry, false)

		report = family.AnalyzeOpenLeavesReport(ctx, day.Add(delay+window), deadline)
		connect.AssertEqual(t, report.Analyzed, []string{leaf(0)})
		connect.AssertEqual(t, len(report.Pending), 0)
		connect.AssertEqual(t, report.Retry, false)
		connect.AssertEqual(t, testutil.ToFloat64(partitionOpenLeavesUnanalyzedGauge.WithLabelValues(family.Name)), float64(0))
		connect.AssertEqual(t, len(waiting.AnalyzeOpenLeavesReport(ctx, day.Add(delay+window+time.Minute), deadline).Pending), 0)
	})
}
