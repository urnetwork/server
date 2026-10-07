// Statement transition tables count actual lease writes without timing or
// protocol assumptions. Every fixture identity and target is synthetic.
package task

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func seedTaskClaimLeaseRows(ctx context.Context, count int) []server.Id {
	clientSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", nil)
	defer clientSession.Cancel()
	ids := make([]server.Id, count)
	server.Tx(ctx, func(tx server.PgTx) {
		for i := range count {
			// Distinct eligibility blocks make the selected prefix exact;
			// the production candidate order has no within-block ID tie-break.
			ids[i] = ScheduleTaskInTx(tx, claimProfileAllowed, &claimProfileArgs{}, clientSession,
				RunAt(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i*2*BlockSizeSeconds)*time.Second)))
		}
		server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET claim_time=$2,release_time=$2 WHERE task_id=ANY($1)`,
			ids, time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)))
	})
	return ids
}

func installTaskClaimLeaseObserver(ctx context.Context) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE synthetic_claim_lease_statements (
			entry_id bigserial PRIMARY KEY, task_ids uuid[] NOT NULL)`))
		server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_observe_claim_lease() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			INSERT INTO synthetic_claim_lease_statements (task_ids)
			SELECT COALESCE(array_agg(task_id ORDER BY task_id),'{}'::uuid[]) FROM claimed_rows;
			RETURN NULL;
		END $$`))
		server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER synthetic_claim_lease_statement
			AFTER UPDATE ON pending_task REFERENCING NEW TABLE AS claimed_rows
			FOR EACH STATEMENT EXECUTE FUNCTION synthetic_observe_claim_lease()`))
	})
}

func readTaskClaimLeaseStatements(ctx context.Context, conn server.PgConn) (statements [][]server.Id) {
	rows, err := conn.Query(ctx, `SELECT task_ids FROM synthetic_claim_lease_statements ORDER BY entry_id`)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var ids []server.Id
			server.Raise(rows.Scan(&ids))
			statements = append(statements, ids)
		}
	})
	return
}

func requireTaskClaimLeaseStatements(t testing.TB, ctx context.Context, expected []server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		statements := readTaskClaimLeaseStatements(ctx, conn)
		wantStatements := 0
		if len(expected) != 0 {
			wantStatements = 1
		}
		if len(statements) != wantStatements {
			t.Fatalf("selected lease writes were not coalesced: statements=%d want=%d selected=%d", len(statements), wantStatements, len(expected))
		}
		if len(expected) != 0 {
			want := slices.Clone(expected)
			slices.SortFunc(want, func(a, b server.Id) int { return a.Cmp(b) })
			if !slices.Equal(statements[0], want) {
				t.Fatal("lease statement changed a refused/unselected row or omitted an exact owner")
			}
		}
	})
}

func taskClaimLeaseWorker(ctx context.Context, limit int) *TaskWorker {
	settings := DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	settings.TargetClaimLimits = map[string]int{functionName(claimProfileAllowed): limit}
	worker := NewTaskWorker(ctx, settings)
	worker.AddTargets(NewTaskTarget(claimProfileAllowed))
	return worker
}

// The retained owner-drain geometry is 64 exact claims per pass. Its untouched
// tail remains byte-for-byte unchanged, while every selected owner is exclusive.
func TestTaskClaimLeaseBatchUpdatesSelectedOwnersOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ids := seedTaskClaimLeaseRows(ctx, 68)
		before := GetTasks(ctx, ids...)
		installTaskClaimLeaseObserver(ctx)
		worker := taskClaimLeaseWorker(ctx, 64)
		defer worker.Close()
		claimed, guard, err := worker.takeTasks(64)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || len(claimed) != 64 || len(guard.admissionKVs) != 64 {
			t.Fatalf("claim did not retain the exact bounded cohort: claimed=%d error=%v", len(claimed), err)
		}
		after := GetTasks(ctx, ids...)
		var claimTime time.Time
		for i, id := range ids {
			if i >= 64 {
				if !reflect.DeepEqual(before[id], after[id]) || claimed[id] != nil {
					t.Fatal("unselected tail changed during lease publication")
				}
				continue
			}
			if claimed[id] == nil || after[id] == nil || !after[id].ClaimTime.After(before[id].ClaimTime) || after[id].ReleaseTime.Sub(after[id].ClaimTime) != TaskLeaseTimeout {
				t.Fatal("selected row lost its exact identity or lease duration")
			}
			if claimTime.IsZero() {
				claimTime = after[id].ClaimTime
			}
			if !after[id].ClaimTime.Equal(claimTime) {
				t.Fatal("one cohort received different lease origins")
			}
			unchanged := *after[id]
			unchanged.ClaimTime, unchanged.ReleaseTime = before[id].ClaimTime, before[id].ReleaseTime
			if !reflect.DeepEqual(before[id], &unchanged) {
				t.Fatal("claim lease publication changed payload, retry or scheduling state")
			}
		}
		peer, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer peer.Release()
		defer func() { server.RaisePgResult(peer.Exec(ctx, `SELECT pg_advisory_unlock_all()`)) }()
		for _, id := range ids[:64] {
			var acquired bool
			server.Raise(peer.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, taskAdvisoryLockKey(id)).Scan(&acquired))
			if acquired {
				t.Fatal("selected lease lacked its exclusive advisory owner")
			}
		}
		// All identities, metadata and actual ownership qualify before the
		// intended baseline RED: 64 lease statements instead of one.
		requireTaskClaimLeaseStatements(t, ctx, ids[:64])
	})
}

func TestTaskClaimLeaseBatchPreservesAdvisoryRefusal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ids := seedTaskClaimLeaseRows(ctx, 5)
		before := GetTasks(ctx, ids[0])
		owner, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Release()
		defer func() { server.RaisePgResult(owner.Exec(ctx, `SELECT pg_advisory_unlock_all()`)) }()
		server.RaisePgResult(owner.Exec(ctx, `SELECT pg_advisory_lock($1)`, taskAdvisoryLockKey(ids[0])))
		installTaskClaimLeaseObserver(ctx)
		worker := taskClaimLeaseWorker(ctx, 4)
		defer worker.Close()
		claimed, guard, err := worker.takeTasks(4)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard == nil || len(claimed) != 4 || claimed[ids[0]] != nil || len(guard.admissionKVs) != 4 {
			t.Fatalf("advisory refusal changed selected capacity: claimed=%d error=%v", len(claimed), err)
		}
		for _, id := range ids[1:] {
			if claimed[id] == nil {
				t.Fatal("refusal hid a later eligible owner")
			}
		}
		if !reflect.DeepEqual(before, GetTasks(ctx, ids[0])) {
			t.Fatal("refused task acquired a timestamp mutation")
		}
		requireTaskClaimLeaseStatements(t, ctx, ids[1:])
	})
}

// Both a returned error and canceled COMMIT follow the same real rollback path.
// The in-transaction observer proves all selected writes had already happened;
// another worker then acquires every original task without lease repair.
func requireTaskClaimLeaseRollback(t *testing.T, cancelCommit bool) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		claimCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		ids := seedTaskClaimLeaseRows(ctx, 16)
		before := GetTasks(ctx, ids...)
		installTaskClaimLeaseObserver(ctx)
		worker := taskClaimLeaseWorker(claimCtx, 16)
		defer worker.Close()
		want := errors.New("synthetic failure after exact lease publication")
		calls := 0
		observedOwnedWrites := false
		worker.claimBeforeCommit = func(guard *taskClaimGuard) error {
			calls++
			observed := []server.Id{}
			for _, statement := range readTaskClaimLeaseStatements(ctx, guard.conn) {
				observed = append(observed, statement...)
			}
			slices.SortFunc(observed, func(a, b server.Id) int { return a.Cmp(b) })
			expected := slices.Clone(ids)
			slices.SortFunc(expected, func(a, b server.Id) int { return a.Cmp(b) })
			if !slices.Equal(observed, expected) || len(guard.admissionKVs) != 16 {
				return errors.New("synthetic fault missed the exact owned lease writes")
			}
			observedOwnedWrites = true
			if cancelCommit {
				cancel()
				return nil
			}
			return want
		}
		claimed, guard, err := worker.takeTasks(16)
		if guard != nil {
			defer guard.release()
		}
		if err == nil || (!cancelCommit && !errors.Is(err, want)) || (cancelCommit && claimCtx.Err() != context.Canceled) || !observedOwnedWrites || guard != nil || len(claimed) != 0 || calls != 1 {
			t.Fatalf("failed claim changed return/retry ownership: claimed=%d calls=%d error=%v", len(claimed), calls, err)
		}
		if !reflect.DeepEqual(before, GetTasks(ctx, ids...)) {
			t.Fatal("failed claim left durable lease or payload changes")
		}
		requireTaskClaimLeaseStatements(t, ctx, nil)
		worker.stateLock.Lock()
		reserved := len(worker.claimTargetCounts)
		worker.stateLock.Unlock()
		if reserved != 0 {
			t.Fatal("failed claim retained local target admission")
		}
		peer := taskClaimLeaseWorker(ctx, 16)
		defer peer.Close()
		reclaimed, peerGuard, err := peer.takeTasks(16)
		if peerGuard != nil {
			defer peerGuard.release()
		}
		if err != nil || peerGuard == nil || len(reclaimed) != 16 {
			t.Fatalf("rollback leaked advisory or timestamp ownership: claimed=%d error=%v", len(reclaimed), err)
		}
		for _, id := range ids {
			if reclaimed[id] == nil {
				t.Fatal("rollback lost an original pending identity")
			}
		}
	})
}

func TestTaskClaimLeaseBatchRollbackReturnsAllOwners(t *testing.T) {
	requireTaskClaimLeaseRollback(t, false)
}

func TestTaskClaimLeaseBatchCanceledCommitReturnsAllOwners(t *testing.T) {
	requireTaskClaimLeaseRollback(t, true)
}

func TestTaskClaimLeaseBatchEmptySelectionWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ids := seedTaskClaimLeaseRows(ctx, 2)
		before := GetTasks(ctx, ids...)
		owner, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Release()
		defer func() { server.RaisePgResult(owner.Exec(ctx, `SELECT pg_advisory_unlock_all()`)) }()
		for _, id := range ids {
			server.RaisePgResult(owner.Exec(ctx, `SELECT pg_advisory_lock($1)`, taskAdvisoryLockKey(id)))
		}
		installTaskClaimLeaseObserver(ctx)
		worker := taskClaimLeaseWorker(ctx, 2)
		defer worker.Close()
		claimed, guard, err := worker.takeTasks(2)
		if guard != nil {
			defer guard.release()
		}
		if err != nil || guard != nil || len(claimed) != 0 || !reflect.DeepEqual(before, GetTasks(ctx, ids...)) {
			t.Fatalf("empty selection changed claims or return values: claimed=%d error=%v", len(claimed), err)
		}
		requireTaskClaimLeaseStatements(t, ctx, nil)
	})
}
