// The operator's compaction of the contract tables' legacy partition
// (CONTRACT-PARTITION-CUTOVER-NOW.md 4.6): its bounds, and against PostgreSQL
// what it rewrites, keeps and refuses.
package server

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// Pure: the defaults are S1's admission, lock wait and statistics gate, with
// the legacy's own bounds: 100,000 live rows a leaf, a 5 s count and a 3 s
// rewrite. Bounds that would hold a leaf's lock past a few seconds, wait
// longer for it than S1, or name another table are refused.
func TestContractLegacyCompactionSettings(t *testing.T) {
	settings := DefaultContractLegacyCompactionSettings()
	retirement := DefaultContractDayRetirementSettings()
	connect.AssertEqual(t, settings.MaxCompactRows, int64(100000))
	connect.AssertEqual(t, settings.CountTimeout, 5*time.Second)
	connect.AssertEqual(t, settings.CompactTimeout, 3*time.Second)
	connect.AssertEqual(t, settings.LockTimeout, retirement.LockTimeout)
	connect.AssertEqual(t, settings.LockSettings, retirement.LockSettings)
	connect.AssertEqual(t, settings.ProbeGateRows, retirement.ProbeGateRows)
	connect.AssertEqual(t, settings.CompactAboveBytes, retirement.CompactAboveBytes)
	connect.AssertEqual(t, settings.DryRun, false)
	connect.AssertEqual(t, settings.Validate(), nil)
	// the work under a leaf's lock stays under a few seconds, past S1's
	// 10,000 rows and 500 ms
	if retirement.MaxCompactRows >= settings.MaxCompactRows || retirement.CompactTimeout >= settings.CompactTimeout {
		t.Fatalf("the legacy's bounds are no wider than S1's: %d rows, %s", settings.MaxCompactRows, settings.CompactTimeout)
	}
	// its S1 settings carry the bounds
	derived := settings.retirementSettings()
	connect.AssertEqual(t, derived.MaxCompactRows, settings.MaxCompactRows)
	connect.AssertEqual(t, derived.ProbeTimeout, settings.CountTimeout)
	connect.AssertEqual(t, derived.CompactTimeout, settings.CompactTimeout)
	connect.AssertEqual(t, derived.LockTimeout, retirement.LockTimeout)

	for _, test := range []struct {
		name   string
		mutate func(*ContractLegacyCompactionSettings)
	}{
		{"no rows", func(s *ContractLegacyCompactionSettings) { s.MaxCompactRows = 0 }},
		{"past 250,000 rows", func(s *ContractLegacyCompactionSettings) { s.MaxCompactRows = 250001 }},
		{"no rewrite bound", func(s *ContractLegacyCompactionSettings) { s.CompactTimeout = 0 }},
		{"a rewrite bound past 10 s", func(s *ContractLegacyCompactionSettings) { s.CompactTimeout = 11 * time.Second }},
		{"no count bound", func(s *ContractLegacyCompactionSettings) { s.CountTimeout = 0 }},
		{"a count bound past a minute", func(s *ContractLegacyCompactionSettings) { s.CountTimeout = 2 * time.Minute }},
		{"a lock wait past S1's", func(s *ContractLegacyCompactionSettings) { s.LockTimeout = 2 * time.Second }},
		{"no admission age", func(s *ContractLegacyCompactionSettings) { s.LockSettings.MaxTransactionAge = 0 }},
		{"a statistics gate below the bound", func(s *ContractLegacyCompactionSettings) { s.ProbeGateRows = 1000 }},
		{"another table", func(s *ContractLegacyCompactionSettings) { s.Tables = []string{"network_client"} }},
	} {
		settings := DefaultContractLegacyCompactionSettings()
		test.mutate(settings)
		if settings.Validate() == nil {
			t.Errorf("%s was accepted", test.name)
		}
	}
	settings.Tables = []string{"contract_close"}
	connect.AssertEqual(t, settings.Validate(), nil)
}

// The legacy partition of a cut over database, filled with contracts minted
// the day before the cutover: kept contracts with every row, and contracts the
// reaper deleted, vacuumed as autovacuum would (TRUNCATE false, as on Main),
// so no autovacuum holds a leaf later. Returns the kept ones.
func testContractLegacyFill(t testing.TB, ctx context.Context, cutoverDay time.Time, kept int, reaped int) (keptIds []Id) {
	t.Helper()
	minted := cutoverDay.Add(-24 * time.Hour)
	for i := 0; i < kept+reaped; i += 1 {
		contractId := testContractIdAt(minted.Add(time.Duration(i) * time.Second))
		// a kept contract holds an unsettled escrow, as the reaper keeps for
		// good; a reaped one was settled
		testInsertContractRows(t, ctx, contractId, i < reaped)
		if i < reaped {
			testReapContract(t, ctx, contractId)
		} else {
			keptIds = append(keptIds, contractId)
		}
	}
	testVacuumContractDay(t, ctx, PartitionRange{Legacy: true, Upper: cutoverDay})
	return
}

// The legacy leaves by table: their bytes of heap and TOAST.
func testContractLegacyBytes(t testing.TB, ctx context.Context) map[string]int64 {
	t.Helper()
	leaves, err := readContractDayLeaves(ctx, PartitionRange{Legacy: true})
	if err != nil {
		t.Fatal(err)
	}
	bytes := map[string]int64{}
	for _, leaf := range leaves {
		if !leaf.exists {
			t.Fatalf("%s is missing", leaf.name)
		}
		bytes[leaf.table] = leaf.bytes()
	}
	return bytes
}

// The legacy partition holds 40 contracts the reaper keeps and the storage of
// 400 it deleted. S1 leaves it, its kept rows past S1's bound (20 here, as
// 10,000 is to Main's 60k unsettled contracts). The operator's compaction,
// under a bound of 100 (as 100,000 is to them): a dry run counts the 40 rows
// a leaf and rewrites nothing; a bound below them refuses every leaf; then
// every leaf is rewritten down to its kept rows, smaller, each kept
// contract's six rows unchanged and the legacy partition still attached; a
// second run finds every leaf compact already. One table alone can be named.
func TestContractLegacyCompactionKeepsEveryKeptRow(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testContractDays(t, ctx)
		cutoverDay := day(0).Lower
		kept := testContractLegacyFill(t, ctx, cutoverDay, 40, 400)
		before := map[Id][]string{}
		for _, contractId := range kept {
			before[contractId] = testContractRows(t, ctx, contractId)
		}
		bytesBefore := testContractLegacyBytes(t, ctx)
		logs := []string{}
		logf := func(format string, args ...any) {
			logs = append(logs, fmt.Sprintf(format, args...))
		}

		// S1 at the legacy's expiry keeps it whole past its own bound
		s1 := testContractDaySettings()
		s1.MaxCompactRows = 20
		s1Report := &ContractPartitionReport{Blocked: map[string]string{}, Kept: map[string]int64{}}
		retireExpiredContractDays(WithContractHorizon(ctx, ContractDayRetentionFloor), cutoverDay.Add(ContractDayRetentionFloor+s1.RetireDelay), s1, time.Now().Add(time.Minute), s1Report)
		for _, table := range ContractPartitionTables {
			if reason := s1Report.Blocked[ContractLegacyPartitionName(table)]; !strings.Contains(reason, "more than 20 live rows") {
				t.Fatalf("S1 did not keep %s whole past its bound: %q", ContractLegacyPartitionName(table), reason)
			}
		}
		connect.AssertEqual(t, testContractLegacyBytes(t, ctx), bytesBefore)

		settings := DefaultContractLegacyCompactionSettings()
		settings.MaxCompactRows = 100
		settings.CompactAboveBytes = 0
		// a dry run counts and rewrites nothing
		settings.DryRun = true
		report, err := CompactContractLegacyPartition(ctx, settings, logf)
		testCloseEvidenceNoError(t, err)
		connect.AssertEqual(t, report.CutoverDay, cutoverDay)
		connect.AssertEqual(t, len(report.Leaves), len(ContractPartitionTables))
		for _, leaf := range report.Leaves {
			if leaf.Compacted || leaf.LiveRows != int64(len(kept)) || !strings.HasPrefix(leaf.Reason, "dry run") {
				t.Fatalf("a dry run of %s: %+v", leaf.Leaf, leaf)
			}
		}
		connect.AssertEqual(t, testContractLegacyBytes(t, ctx), bytesBefore)

		// a bound below the kept rows refuses every leaf
		settings.DryRun = false
		settings.MaxCompactRows = 39
		report, err = CompactContractLegacyPartition(ctx, settings, logf)
		testCloseEvidenceNoError(t, err)
		for _, leaf := range report.Leaves {
			if leaf.Compacted || !strings.Contains(leaf.Reason, "more than 39 live rows") {
				t.Fatalf("a bound below the kept rows compacted %s: %+v", leaf.Leaf, leaf)
			}
		}
		connect.AssertEqual(t, testContractLegacyBytes(t, ctx), bytesBefore)

		// one table alone
		settings.MaxCompactRows = 100
		settings.Tables = []string{"contract_close"}
		report, err = CompactContractLegacyPartition(ctx, settings, logf)
		testCloseEvidenceNoError(t, err)
		connect.AssertEqual(t, len(report.Leaves), 1)
		connect.AssertEqual(t, report.Leaves[0].Table, "contract_close")
		connect.AssertEqual(t, report.Leaves[0].Compacted, true)
		afterOne := testContractLegacyBytes(t, ctx)
		for _, table := range ContractPartitionTables {
			if table != "contract_close" && afterOne[table] != bytesBefore[table] {
				t.Fatalf("naming contract_close rewrote %s", table)
			}
		}

		// every leaf, smallest first
		settings.Tables = nil
		report, err = CompactContractLegacyPartition(ctx, settings, logf)
		testCloseEvidenceNoError(t, err)
		connect.AssertEqual(t, len(report.Leaves), len(ContractPartitionTables))
		for i, leaf := range report.Leaves {
			if 0 < i && leaf.BytesBefore < report.Leaves[i-1].BytesBefore {
				t.Fatalf("%s came after a larger leaf", leaf.Leaf)
			}
			if !leaf.Compacted || leaf.LiveRows != int64(len(kept)) || leaf.Reason != "" {
				t.Fatalf("%s was not compacted down to its kept rows: %+v", leaf.Leaf, leaf)
			}
		}
		bytesAfter := testContractLegacyBytes(t, ctx)
		for _, table := range ContractPartitionTables {
			if bytesBefore[table] <= bytesAfter[table] {
				t.Errorf("%s's legacy leaf did not shrink: %d bytes, then %d", table, bytesBefore[table], bytesAfter[table])
			}
		}
		for _, contractId := range kept {
			connect.AssertEqual(t, testContractRows(t, ctx, contractId), before[contractId])
		}
		for _, table := range ContractPartitionTables {
			connect.AssertEqual(t, testContractPartitionScalar[bool](t, ctx,
				`SELECT EXISTS (SELECT 1 FROM pg_inherits WHERE inhparent = to_regclass('public.' || $1) AND inhrelid = to_regclass('public.' || $2))`,
				table, ContractLegacyPartitionName(table)), true)
		}

		// a second run, at S1's size: compact already
		settings.CompactAboveBytes = DefaultContractLegacyCompactionSettings().CompactAboveBytes
		report, err = CompactContractLegacyPartition(ctx, settings, logf)
		testCloseEvidenceNoError(t, err)
		for _, leaf := range report.Leaves {
			if leaf.Compacted || !strings.HasPrefix(leaf.Reason, "compact already") {
				t.Fatalf("a compact leaf was rewritten again: %+v", leaf)
			}
		}
		connect.AssertEqual(t, slices.ContainsFunc(logs, func(line string) bool { return strings.HasPrefix(line, "compacted ") }), true)
	})
}

// A legacy leaf that a vacuum-like transaction holds is reported and left
// untouched, without the compaction queueing for its lock; the other leaves
// are compacted. Before the cutover there is no legacy partition to compact.
func TestContractLegacyCompactionDoesNotQueueBehindALeafLock(t *testing.T) {
	DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := CompactContractLegacyPartition(ctx, DefaultContractLegacyCompactionSettings(), func(string, ...any) {}); err == nil {
			t.Fatal("the compaction ran on plain contract tables")
		}
		day := testContractDays(t, ctx)
		testContractLegacyFill(t, ctx, day(0).Lower, 5, 50)
		held := ContractLegacyPartitionName("transfer_escrow")

		holderPid := make(chan uint32, 1)
		release := make(chan struct{})
		holderDone := make(chan error, 1)
		go func() {
			holderDone <- contractPartitionTx(ctx, func(tx PgTx) {
				RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`LOCK TABLE public.%s IN SHARE UPDATE EXCLUSIVE MODE`, quoteIdentifier(held))))
				holderPid <- tx.Conn().PgConn().PID()
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
		}()
		var pid uint32
		select {
		case pid = <-holderPid:
		case err := <-holderDone:
			t.Fatalf("the leaf lock holder failed: %v", err)
		}
		defer func() {
			close(release)
			<-holderDone
		}()
		// older than the admission's transaction age by the database clock
		age := DefaultContractPartitionLockSettings().MaxTransactionAge + time.Second
		MaintenanceDb(ctx, func(conn PgConn) {
			for {
				var old bool
				Raise(conn.QueryRow(ctx, `SELECT clock_timestamp() - xact_start > make_interval(secs => $2) FROM pg_stat_activity WHERE pid = $1`, pid, age.Seconds()).Scan(&old))
				if old {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
		})

		settings := DefaultContractLegacyCompactionSettings()
		settings.CompactAboveBytes = 0
		started := time.Now()
		report, err := CompactContractLegacyPartition(ctx, settings, func(string, ...any) {})
		testCloseEvidenceNoError(t, err)
		if elapsed := time.Since(started); 30*time.Second < elapsed {
			t.Fatalf("the compaction took %s beside a held leaf", elapsed)
		}
		for _, leaf := range report.Leaves {
			if leaf.Leaf == held {
				if leaf.Compacted || !strings.Contains(leaf.Reason, "waits, without queueing, for") {
					t.Fatalf("the held leaf: %+v", leaf)
				}
				continue
			}
			if !leaf.Compacted {
				t.Fatalf("%s was not compacted beside the held leaf: %+v", leaf.Leaf, leaf)
			}
		}
	})
}
