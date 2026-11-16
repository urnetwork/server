// The due-contract delete pass is planned and bounded like the horizon
// reaper's batches, so a statistics misestimate on the dependent tables cannot
// turn one batch into a long sequential scan inside one transaction.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// Due, deletable contracts: closed long before the straggler horizon, with a
// reap_time in the past and no payment, and a close from each party.
func createDueContractReapTestContracts(t testing.TB, ctx context.Context, now time.Time, count int) []server.Id {
	t.Helper()
	fixture := newNetEscrowOrderingTestFixture(t, ctx)
	ids := make([]server.Id, count)
	for index := range ids {
		var err error
		ids[index], err = CreateContractNoEscrow(ctx, fixture.sourceNetworkId, fixture.sourceId,
			fixture.destinationNetworkId, fixture.destinationId, 0)
		server.Raise(err)
	}
	old := now.Add(-400 * 24 * time.Hour)
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract
 SET create_time=$2,close_time=$2,reap_time=$2 WHERE contract_id=ANY($1)`, ids, old))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
 (contract_id,party,used_transfer_byte_count,checkpoint)
 SELECT contract_id,party,0,false FROM unnest($1::uuid[]) AS contract_id
 CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, ids))
	})
	return ids
}

func dueContractReapTestRemaining(t testing.TB, ctx context.Context, ids []server.Id) (contracts int, closes int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1)),
 (SELECT count(*) FROM contract_close WHERE contract_id=ANY($1))`, ids).Scan(&contracts, &closes))
	})
	return
}

func dueContractReapTestStops(stop string) float64 {
	return testutil.ToFloat64(dueContractReapStopCounter.WithLabelValues(stop))
}

// The scan node types of each relation an EXPLAIN (FORMAT JSON) plan reads.
func dueContractReapTestScans(t testing.TB, plan string) map[string][]string {
	t.Helper()
	var roots []map[string]any
	if err := json.Unmarshal([]byte(plan), &roots); err != nil {
		t.Fatal("unreadable plan", err)
	}
	scans := map[string][]string{}
	var walk func(node map[string]any)
	walk = func(node map[string]any) {
		nodeType, _ := node["Node Type"].(string)
		if relation, ok := node["Relation Name"].(string); ok && strings.HasSuffix(nodeType, " Scan") {
			scans[relation] = append(scans[relation], nodeType)
		}
		children, _ := node["Plans"].([]any)
		for _, child := range children {
			if childNode, ok := child.(map[string]any); ok {
				walk(childNode)
			}
		}
	}
	for _, root := range roots {
		if node, ok := root["Plan"].(map[string]any); ok {
			walk(node)
		}
	}
	return scans
}

// Main's misestimate, at its extreme: statistics that make every contract id
// match every escrow, which costs a sequential scan cheapest. The batch's
// dependents delete still reads each table with a bitmap scan of its contract
// id key, under the reaper's statement and lock bounds, and deletes the
// contracts.
func TestRemoveDueContractBatchesPlansBitmapScansWithinBounds(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		ids := createDueContractReapTestContracts(t, ctx, now, 3)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_escrow ALTER COLUMN contract_id SET (n_distinct = 1)`))
			server.RaisePgResult(tx.Exec(ctx, `ANALYZE transfer_escrow`))
		})

		var plans []string
		var settings []map[string]string
		dueContractReapBatchObserved = func(ctx context.Context, tx server.PgTx, candidateIds []server.Id) {
			var plan string
			server.Raise(tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+dueContractDeleteDependentsSql, candidateIds).Scan(&plan))
			plans = append(plans, plan)
			batchSettings := map[string]string{}
			for _, name := range []string{"statement_timeout", "lock_timeout", "enable_seqscan", "enable_indexscan", "jit"} {
				var value string
				server.Raise(tx.QueryRow(ctx, `SELECT current_setting($1)`, name).Scan(&value))
				batchSettings[name] = value
			}
			settings = append(settings, batchSettings)
		}
		defer func() { dueContractReapBatchObserved = nil }()

		removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 100)

		if len(plans) != 1 {
			t.Fatalf("batches observed = %d, want 1", len(plans))
		}
		scans := dueContractReapTestScans(t, plans[0])
		for _, relation := range []string{"contract_close_report", "contract_close", "transfer_escrow", "transfer_escrow_sweep", "contract_extender"} {
			if !slices.Equal(scans[relation], []string{"Bitmap Heap Scan"}) {
				t.Errorf("dependents delete reads %s with %v, want one bitmap heap scan", relation, scans[relation])
			}
		}
		want := map[string]string{
			"statement_timeout": "30s",
			"lock_timeout":      "2s",
			"enable_seqscan":    "off",
			"enable_indexscan":  "off",
			"jit":               "off",
		}
		for name, value := range want {
			if settings[0][name] != value {
				t.Errorf("batch %s = %q, want %q", name, settings[0][name], value)
			}
		}
		if contracts, closes := dueContractReapTestRemaining(t, ctx, ids); contracts != 0 || closes != 0 {
			t.Errorf("due contracts left = %d with %d closes, want none", contracts, closes)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE transfer_escrow ALTER COLUMN contract_id RESET (n_distinct)`))
		})
	})
}

// The run deletes its due contracts in batches of the pass's batch size, not
// of the assign passes' 50,000.
func TestRemoveCompletedContractsDeletesDueContractsInBoundedBatches(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		ids := createDueContractReapTestContracts(t, ctx, now, 5)

		defer func(batchSize int) { dueContractReapBatchSize = batchSize }(dueContractReapBatchSize)
		dueContractReapBatchSize = 2
		var sizes []int
		dueContractReapBatchObserved = func(ctx context.Context, tx server.PgTx, candidateIds []server.Id) {
			sizes = append(sizes, len(candidateIds))
		}
		defer func() { dueContractReapBatchObserved = nil }()

		RemoveCompletedContracts(ctx, now.Add(-CompletedContractExpiration))

		if !slices.Equal(sizes, []int{2, 2, 1}) {
			t.Errorf("delete batches = %v, want [2 2 1]", sizes)
		}
		if contracts, closes := dueContractReapTestRemaining(t, ctx, ids); contracts != 0 || closes != 0 {
			t.Errorf("due contracts left = %d with %d closes, want none", contracts, closes)
		}
	})
}

// A run whose budget is spent starts no delete batch: the delete pass checks
// the run's budget before each batch, not only after one.
func TestRemoveCompletedContractsStartsNoDeleteBatchPastRunBudget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		ids := createDueContractReapTestContracts(t, ctx, now, 2)

		defer func(budget time.Duration) { removeCompletedContractsRunBudget = budget }(removeCompletedContractsRunBudget)
		removeCompletedContractsRunBudget = -time.Minute
		before := dueContractReapTestStops(dueContractReapStopBudget)
		RemoveCompletedContracts(ctx, now.Add(-CompletedContractExpiration))
		if contracts, _ := dueContractReapTestRemaining(t, ctx, ids); contracts != len(ids) {
			t.Fatalf("a spent run deleted %d of %d due contracts", len(ids)-contracts, len(ids))
		}
		if stops := dueContractReapTestStops(dueContractReapStopBudget) - before; stops != 1 {
			t.Errorf("budget stops = %v, want 1", stops)
		}

		removeCompletedContractsRunBudget = 20 * time.Minute
		RemoveCompletedContracts(ctx, now.Add(-CompletedContractExpiration))
		if contracts, _ := dueContractReapTestRemaining(t, ctx, ids); contracts != 0 {
			t.Errorf("a fresh run left %d due contracts", contracts)
		}
	})
}

// Another transaction holds a due contract's row lock. The batch waits the
// lock bound, not the holder: it rolls back and the pass stops without a
// raise, so the run finishes, and a later pass deletes the contract.
func TestRemoveDueContractBatchesStopsAtLockWaitBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
		defer cancel()
		now := server.NowUtc()
		ids := createDueContractReapTestContracts(t, ctx, now, 1)

		const holdTime = 15 * time.Second
		locked := make(chan struct{})
		release := make(chan struct{})
		holderDone := make(chan any, 1)
		go func() {
			holderDone <- server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, ids[0]))
					close(locked)
					select {
					case <-release:
					case <-time.After(holdTime):
					}
				}, server.TxReadCommitted, server.OptNoRetry())
			})
		}()
		select {
		case <-locked:
		case r := <-holderDone:
			t.Fatal("the holder did not take its lock", r)
		}

		before := dueContractReapTestStops(dueContractReapStopLockTimeout)
		start := time.Now()
		var stop string
		r := server.HandleError(func() {
			stop = newDueContractReaper(now, now.Add(-StragglerContractExpiration), 10, now.Add(time.Hour)).removeDue(ctx)
		})
		elapsed := time.Since(start)
		close(release)
		if holderErr := <-holderDone; holderErr != nil {
			t.Fatal("the holder failed", holderErr)
		}

		if r != nil {
			t.Fatal("the lock wait bound raised out of the pass", r)
		}
		if stop != dueContractReapStopLockTimeout {
			t.Errorf("pass stop = %q, want %q", stop, dueContractReapStopLockTimeout)
		}
		if holdTime/2 <= elapsed {
			t.Errorf("the pass waited %s on a held lock, past its %s bound", elapsed, dueContractReapLockTimeout)
		}
		if stops := dueContractReapTestStops(dueContractReapStopLockTimeout) - before; stops != 1 {
			t.Errorf("lock wait stops = %v, want 1", stops)
		}
		if contracts, closes := dueContractReapTestRemaining(t, ctx, ids); contracts != 1 || closes != 2 {
			t.Fatalf("the rolled back batch left %d contracts with %d closes, want 1 with 2", contracts, closes)
		}

		removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 10)
		if contracts, _ := dueContractReapTestRemaining(t, ctx, ids); contracts != 0 {
			t.Errorf("a later pass left %d due contracts", contracts)
		}
	})
}

// The pass reads at most its rate of contracts a second.
func TestRemoveDueContractBatchesPacesToRate(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		ids := createDueContractReapTestContracts(t, ctx, now, 6)

		defer func(rate float64) { dueContractReapMaxRowsPerSecond = rate }(dueContractReapMaxRowsPerSecond)
		// three batches of two contracts, four seconds each
		dueContractReapMaxRowsPerSecond = 0.5
		start := time.Now()
		removeDueContractBatches(ctx, now, now.Add(-StragglerContractExpiration), 2)
		elapsed := time.Since(start)
		if contracts, _ := dueContractReapTestRemaining(t, ctx, ids); contracts != 0 {
			t.Fatalf("the paced pass left %d due contracts", contracts)
		}
		if elapsed < 10*time.Second {
			t.Errorf("six contracts at half a contract a second took %s, want at least 10s", elapsed)
		}
	})
}

// Only the pass's own bounds stop it quietly. A serialization failure, a
// cancellation of the pass's context or a value that is no error still raise.
func TestDueContractReapBoundStop(t *testing.T) {
	live := context.Background()
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		r    any
		want string
	}{
		{"lock wait bound", live, &pgconn.PgError{Code: "55P03"}, dueContractReapStopLockTimeout},
		{"statement bound", live, &pgconn.PgError{Code: "57014"}, dueContractReapStopStatementTimeout},
		{"wrapped statement bound", live, fmt.Errorf("batch: %w", &pgconn.PgError{Code: "57014"}), dueContractReapStopStatementTimeout},
		{"serialization failure", live, &pgconn.PgError{Code: "40001"}, ""},
		{"canceled pass", canceled, &pgconn.PgError{Code: "57014"}, ""},
		{"context end", live, context.Canceled, ""},
		{"no error", live, "Done", ""},
		{"plain error", live, errors.New("synthetic"), ""},
	}
	for _, c := range cases {
		if got := dueContractReapBoundStop(c.ctx, c.r); got != c.want {
			t.Errorf("%s: stop = %q, want %q", c.name, got, c.want)
		}
	}
}
