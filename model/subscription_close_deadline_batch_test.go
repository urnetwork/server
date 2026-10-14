// Due rows of one expiry page that share one balance close in owner batches.
// Before, each row took the balance's try-lock in its own transaction, so a
// heavy payer's page retired one row per owned turn.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// Creates count reserved contracts on the fixture's one balance, created a
// second apart from two hours before now and past their stored deadline.
func newForceCloseOwnerBatchRows(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, count int, now time.Time) []server.Id {
	t.Helper()
	contractIds := make([]server.Id, count)
	for index := range contractIds {
		contractIds[index] = createRedisAdmissionTest(ctx, f, 5).ContractId
	}
	server.Tx(ctx, func(tx server.PgTx) {
		for index, contractId := range contractIds {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=$3 WHERE contract_id=$1`,
				contractId, now.Add(-2*time.Hour).Add(time.Duration(index)*time.Second), now.Add(-time.Hour)))
		}
	}, server.TxReadCommitted, server.OptNoRetry())
	return contractIds
}

// The owner turns one balance saw for the given contracts: each admitted or
// refused turn lists the contracts whose provider totals keys it carried, so
// a turn that closes several rows lists each of them.
type forceCloseOwnerTurns struct {
	stateLock sync.Mutex
	admitted  [][]server.Id
	refused   [][]server.Id
}

func observeForceCloseOwnerTurns(ctx context.Context, balanceId server.Id, contractIds []server.Id) (context.Context, *forceCloseOwnerTurns) {
	grant := server.NewPgOwnershipKey("transfer_balance", balanceId)
	keyContractIds := map[server.PgOwnershipKey]server.Id{}
	for _, contractId := range contractIds {
		keyContractIds[task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contractId))] = contractId
	}
	turns := &forceCloseOwnerTurns{}
	observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
		if !slices.Contains(event.Keys, grant) {
			return
		}
		covered := []server.Id{}
		for _, key := range event.Keys {
			if contractId, ok := keyContractIds[key]; ok {
				covered = append(covered, contractId)
			}
		}
		if len(covered) == 0 {
			return
		}
		func() {
			turns.stateLock.Lock()
			defer turns.stateLock.Unlock()
			switch event.Kind {
			case server.PgOwnershipAdmitted:
				turns.admitted = append(turns.admitted, covered)
			case server.PgOwnershipRefused:
				turns.refused = append(turns.refused, covered)
			}
		}()
	})
	return observed, turns
}

func (self *forceCloseOwnerTurns) snapshot() (admitted [][]server.Id, refused [][]server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.admitted), slices.Clone(self.refused)
}

// Requires each contract's terminal state, in order.
func requireForceCloseOwnerBatchTerminal(t testing.TB, ctx context.Context, contractIds []server.Id, wantTerminal []bool) {
	t.Helper()
	terminal, _ := readForceCloseDeferredRows(t, ctx, contractIds)
	if !slices.Equal(terminal, wantTerminal) {
		t.Fatalf("terminal rows %v, want %v", terminal, wantTerminal)
	}
}

// One payer keeps all its contracts on one balance, as heavy payers do. Its
// page of 2*ContractDeadlineOwnerBatchLimit+1 due rows took one owned turn per
// row; it now closes in ceil(n/ContractDeadlineOwnerBatchLimit) turns, each
// covering at most one batch, every row closed in exactly one of them.
func TestForceClosePageClosesSingleBalanceDeadlineRowsInOwnerBatches(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		count := 2*ContractDeadlineOwnerBatchLimit + 1
		contractIds := newForceCloseOwnerBatchRows(t, ctx, f, count, now)
		observed, turns := observeForceCloseOwnerTurns(ctx, f.balanceId, contractIds)

		closed, cursor, _, err := forceCloseOpenContractIdsPage(observed, now, 2*count, 8, 0, 0, nil)
		if err != nil || closed != int64(count) || cursor != nil {
			t.Fatal("the page did not close every due row of the balance", closed, cursor, err)
		}
		admitted, refused := turns.snapshot()
		batchCount := (count + ContractDeadlineOwnerBatchLimit - 1) / ContractDeadlineOwnerBatchLimit
		if len(refused) != 0 || batchCount < len(admitted) {
			t.Fatalf("the page took %d owned turns and %d refusals for %d rows of one balance, want at most %d turns",
				len(admitted), len(refused), count, batchCount)
		}
		turnCounts := map[server.Id]int{}
		for _, members := range admitted {
			if ContractDeadlineOwnerBatchLimit < len(members) {
				t.Fatal("one owned turn exceeded the owner batch limit", len(members))
			}
			for _, contractId := range members {
				turnCounts[contractId] += 1
			}
		}
		wantTerminal := make([]bool, count)
		for index, contractId := range contractIds {
			if turnCounts[contractId] != 1 {
				t.Fatal("a row did not close in exactly one owned turn", index, turnCounts[contractId])
			}
			wantTerminal[index] = true
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, wantTerminal)
	})
}

// Another owner holds the balance for the whole page. The batch tries it once
// and leaves every row to the holder as an owner_busy visit, as a held row
// tried alone is left: the rows stay open, the page succeeds and its cursor
// passes them, and the next pass closes them once the balance is free.
func TestForceClosePageLeavesHeldOwnerBatchRowsToHolder(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		// An older free row on no balance closes beside the held rows.
		freeIds := newForceCloseDeferredRows(t, ctx, 1)
		count := 3
		contractIds := newForceCloseOwnerBatchRows(t, ctx, f, count, now)
		release := holdTransferBalanceTestOwner(t, ctx, []server.Id{f.balanceId})
		observed, turns := observeForceCloseOwnerTurns(ctx, f.balanceId, contractIds)

		busyBefore := forceCloseCount("owner_busy")
		closed, cursor, _, err := forceCloseOpenContractIdsPage(observed, now, 2*count+2, 4, 0, 0, nil)
		if err != nil || closed != 1 || cursor != nil {
			t.Fatal("the held batch failed the page or kept its cursor", closed, cursor, err)
		}
		if busy := forceCloseCount("owner_busy") - busyBefore; busy != float64(count) {
			t.Fatal("the held batch's rows were not left to the holder as owner_busy visits", busy)
		}
		admitted, refused := turns.snapshot()
		if len(admitted) != 0 || len(refused) != 1 || len(refused[0]) != count {
			t.Fatalf("the held balance saw %d admissions and %d refusals, want one refusal of the whole batch", len(admitted), len(refused))
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, freeIds, []bool{true})
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{false, false, false})

		release()
		closed, cursor, _, err = forceCloseOpenContractIdsPage(ctx, server.NowUtc(), 2*count+2, 4, 0, 0, nil)
		if err != nil || closed != int64(count) || cursor != nil {
			t.Fatal("the next pass did not close the rows left to the holder", closed, cursor, err)
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{true, true, true})
	})
}

// A row visited alone, here one with a pending intent, finds its balance held
// first; the page remembers the held balance, so the batch of the balance's
// other rows that follows skips without a try. One worker fixes that order.
// All four rows are left to the holder as owner_busy visits, the balance is
// tried once in all, and the next pass closes the rows once it is free.
func TestForceClosePageSkipsOwnerBatchOfABalanceARowFoundHeld(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		contractIds := newForceCloseOwnerBatchRows(t, ctx, f, 4, now)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome)
				VALUES($1,get_byte(uuid_send($1::uuid),15)%16,'settled')`, contractIds[0]))
		}, server.TxReadCommitted, server.OptNoRetry())
		release := holdTransferBalanceTestOwner(t, ctx, []server.Id{f.balanceId})
		observed, turns := observeForceCloseOwnerTurns(ctx, f.balanceId, contractIds)

		busyBefore := forceCloseCount("owner_busy")
		closed, cursor, _, err := forceCloseOpenContractIdsPage(observed, now, 8, 1, 0, 0, nil)
		if err != nil || closed != 0 || cursor != nil {
			t.Fatal("the held balance's rows failed the page or kept its cursor", closed, cursor, err)
		}
		if busy := forceCloseCount("owner_busy") - busyBefore; busy != float64(len(contractIds)) {
			t.Fatal("the rows were not all left to the holder as owner_busy visits", busy)
		}
		admitted, refused := turns.snapshot()
		if len(admitted) != 0 || len(refused) != 1 || !slices.Equal(refused[0], []server.Id{contractIds[0]}) {
			t.Fatalf("the held balance saw %d admissions and refusals %v, want one refusal of the row alone and no batch try", len(admitted), refused)
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{false, false, false, false})

		release()
		closed, cursor, _, err = forceCloseOpenContractIdsPage(ctx, server.NowUtc(), 8, 1, 0, 0, nil)
		if err != nil || closed != int64(len(contractIds)) || cursor != nil {
			t.Fatal("the next pass did not close the rows left to the holder", closed, cursor, err)
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{true, true, true, true})
	})
}

// Only the batch's first row starts before dispatch closes. Its batch also
// closes its later rows, but the receipt and cursor cover the started prefix
// alone: the cursor stops before the first row that never started, a free row
// between the batch's rows, and the page resuming there closes that row and
// finds the batch's rows closed. Nothing is skipped.
func TestForceClosePageOwnerBatchKeepsStartedPrefixCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		contractIds := newForceCloseOwnerBatchRows(t, ctx, f, 3, now)
		freeIds := newForceCloseDeferredRows(t, ctx, 1)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`,
				freeIds[0], now.Add(-2*time.Hour).Add(500*time.Millisecond)))
		}, server.TxReadCommitted, server.OptNoRetry())
		var firstCreateTime time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT create_time FROM transfer_contract WHERE contract_id=$1`, contractIds[0]).Scan(&firstCreateTime))
		})

		closed, cursor, _, err := forceCloseOpenContractIdsPageDispatched(ctx, now, 8, 1, 0, 0, nil, func() bool { return false })
		if err != nil || closed != 1 {
			t.Fatal("the started prefix did not close exactly its one row", closed, err)
		}
		if cursor == nil || cursor.Open == nil || cursor.Open.ContractId != contractIds[0] || !cursor.Open.CreateTime.Equal(firstCreateTime) ||
			cursor.OpenDone || !cursor.DisputeDone {
			t.Fatal("the cursor did not stop just after the only started row", cursor)
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{true, true, true})
		requireForceCloseOwnerBatchTerminal(t, ctx, freeIds, []bool{false})

		closed, cursor, _, err = forceCloseOpenContractIdsPageDispatched(ctx, now, 8, 1, 0, 0, cursor, nil)
		if err != nil || closed != 1 || cursor != nil {
			t.Fatal("the resumed page did not close the unstarted row and end", closed, cursor, err)
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, freeIds, []bool{true})
	})
}

// Fails the outcome claim of one contract in every transaction until dropped,
// inside an owner batch's members as in a row visited alone.
func failForceCloseOwnerBatchMember(t testing.TB, ctx context.Context, contractId server.Id) (drop func()) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
			CREATE FUNCTION synthetic_owner_batch_member_failure() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				IF NEW.contract_id = '%s'::uuid THEN
					RAISE EXCEPTION 'synthetic owner batch member failure';
				END IF;
				RETURN NEW;
			END $$;
			CREATE TRIGGER synthetic_owner_batch_member_failure
			BEFORE UPDATE OF outcome ON transfer_contract
			FOR EACH ROW WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL)
			EXECUTE FUNCTION synthetic_owner_batch_member_failure();`, contractId)))
	}, server.TxReadCommitted, server.OptNoRetry())
	dropped := false
	drop = func() {
		if dropped {
			return
		}
		dropped = true
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_owner_batch_member_failure ON transfer_contract;
				DROP FUNCTION synthetic_owner_batch_member_failure();`))
		}, server.TxReadCommitted, server.OptNoRetry())
	}
	t.Cleanup(drop)
	return drop
}

// Counts the owned turns that covered all the given rows together, and each
// row's turns alone. Any other turn fails the test.
func countForceCloseOwnerBatchTurns(t testing.TB, admitted [][]server.Id, batchIds []server.Id) (batchTurns int, aloneTurns map[server.Id]int) {
	t.Helper()
	aloneTurns = map[server.Id]int{}
	for _, members := range admitted {
		switch {
		case len(members) == len(batchIds) && !slices.ContainsFunc(members, func(id server.Id) bool { return !slices.Contains(batchIds, id) }):
			batchTurns += 1
		case len(members) == 1:
			aloneTurns[members[0]] += 1
		default:
			t.Fatal("an unexpected owned turn", members)
		}
	}
	return
}

// One member's outcome claim fails inside the batch's shared turn, which rolls
// back. Each row then closes alone: the others close, and only that row is
// deferred with its own failure, exactly as when every row was visited alone.
// It closes once the failure is gone.
func TestForceClosePageOwnerBatchFailureClosesRowsAlone(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		contractIds := newForceCloseOwnerBatchRows(t, ctx, f, 3, now)
		victim := contractIds[1]
		drop := failForceCloseOwnerBatchMember(t, ctx, victim)
		observed, turns := observeForceCloseOwnerTurns(ctx, f.balanceId, contractIds)

		closed, cursor, _, err := forceCloseOpenContractIdsPage(observed, now, 8, 4, 0, 0, nil)
		visited, ok := err.(*ForceCloseVisitError)
		if !ok || !visited.CanCheckpoint() || !visited.Progressed() || visited.ownershipBusyOnly() || cursor != nil {
			t.Fatal("the failed member did not leave a completed receipt of its own", cursor, err)
		}
		if closed != 2 || visited.AttemptedCloseCount() != 2 || !slices.Equal(visited.DeferredContractIds(), []server.Id{victim}) {
			t.Fatal("the other rows did not close alone around the failed member", closed, visited.DeferredContractIds())
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{true, false, true})
		admitted, _ := turns.snapshot()
		batchTurns, aloneTurns := countForceCloseOwnerBatchTurns(t, admitted, contractIds)
		if batchTurns != 1 || aloneTurns[contractIds[0]] != 1 || aloneTurns[contractIds[2]] != 1 || aloneTurns[victim] != 1 {
			t.Fatalf("the page took %d batch turns and %v turns alone, want one batch then each row alone", batchTurns, aloneTurns)
		}

		drop()
		closed, cursor, _, err = forceCloseOpenContractIdsPage(ctx, server.NowUtc(), 8, 4, 0, 0, nil)
		if err != nil || closed != 1 || cursor != nil {
			t.Fatal("the next pass did not close the freed row", closed, cursor, err)
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{true, true, true})
	})
}

// A row whose pending intent another worker holds stays out of its balance's
// batch. Its close takes the intent with NOWAIT, so inside the batch it failed
// every member and sent each row to close alone. Now the other rows close in
// one turn and only that row fails, alone; it closes once the intent is free.
func TestForceClosePageLeavesIntentRowsOutOfOwnerBatches(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		contractIds := newForceCloseOwnerBatchRows(t, ctx, f, 4, now)
		victim := contractIds[1]
		batchIds := []server.Id{contractIds[0], contractIds[2], contractIds[3]}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome)
				VALUES($1,get_byte(uuid_send($1::uuid),15)%16,'settled')`, victim))
		}, server.TxReadCommitted, server.OptNoRetry())
		release := holdContractCloseTestRow(t, ctx, `SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1 FOR UPDATE`, victim)
		observed, turns := observeForceCloseOwnerTurns(ctx, f.balanceId, contractIds)

		closed, cursor, _, err := forceCloseOpenContractIdsPage(observed, now, 8, 4, 0, 0, nil)
		visited, ok := err.(*ForceCloseVisitError)
		if !ok || !visited.CanCheckpoint() || !visited.Progressed() || cursor != nil {
			t.Fatal("the held intent did not leave a completed receipt of its own", cursor, err)
		}
		if closed != 3 || !slices.Equal(visited.DeferredContractIds(), []server.Id{victim}) {
			t.Fatal("the other rows did not close around the held intent", closed, visited.DeferredContractIds())
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{true, false, true, true})
		admitted, _ := turns.snapshot()
		batchTurns, aloneTurns := countForceCloseOwnerBatchTurns(t, admitted, batchIds)
		if batchTurns != 1 || aloneTurns[contractIds[0]]+aloneTurns[contractIds[2]]+aloneTurns[contractIds[3]] != 0 || 1 < aloneTurns[victim] {
			t.Fatalf("the page took %d batch turns of the other rows and %v turns alone, want one batch and the intent row alone", batchTurns, aloneTurns)
		}

		release()
		closed, cursor, _, err = forceCloseOpenContractIdsPage(ctx, server.NowUtc(), 8, 4, 0, 0, nil)
		if err != nil || closed != 1 || cursor != nil {
			t.Fatal("the next pass did not close the freed intent row", closed, cursor, err)
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{true, true, true, true})
	})
}

// The batch fails after its admission but before its members, as a lost route
// or connection fails it. Each row alone would meet the same route, so none is
// retried alone: every row stays open, deferred with that failure, and the
// next pass closes them in one batch. Each row used to be retried alone.
func TestForceClosePageLeavesRowsOpenWhenOwnerBatchFailsBeforeItsMembers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		contractIds := newForceCloseOwnerBatchRows(t, ctx, f, 3, now)
		keyContractIds := map[server.PgOwnershipKey]server.Id{}
		for _, contractId := range contractIds {
			keyContractIds[task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", contractId))] = contractId
		}
		synthetic := errors.New("synthetic owner batch route failure")
		var stateLock sync.Mutex
		batchAdmissions, aloneAdmissions := 0, 0
		observed := server.Testing_WithPgOwnershipObservation(ctx, func(event server.PgOwnershipEvent) {
			if event.Kind != server.PgOwnershipAdmitted {
				return
			}
			covered := 0
			for _, key := range event.Keys {
				if _, ok := keyContractIds[key]; ok {
					covered += 1
				}
			}
			stateLock.Lock()
			first := false
			switch {
			case len(contractIds) <= covered:
				batchAdmissions += 1
				first = batchAdmissions == 1
			case covered == 1:
				aloneAdmissions += 1
			}
			stateLock.Unlock()
			if first {
				// Before BEGIN: the batch never reaches its members.
				panic(synthetic)
			}
		})

		closed, cursor, _, err := forceCloseOpenContractIdsPage(observed, now, 8, 4, 0, 0, nil)
		visited, ok := err.(*ForceCloseVisitError)
		if !ok || !visited.CanCheckpoint() || visited.Progressed() || !errors.Is(err, synthetic) || cursor != nil {
			t.Fatal("the failed batch did not leave every row deferred with its failure", cursor, err)
		}
		if closed != 0 || !slices.Equal(visited.DeferredContractIds(), contractIds) {
			t.Fatal("a row of the failed batch was retried alone", closed, visited.DeferredContractIds())
		}
		func() {
			stateLock.Lock()
			defer stateLock.Unlock()
			if batchAdmissions != 1 || aloneAdmissions != 0 {
				t.Fatalf("the page took %d batch admissions and %d row admissions, want the one batch and none alone", batchAdmissions, aloneAdmissions)
			}
		}()
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{false, false, false})

		closed, cursor, _, err = forceCloseOpenContractIdsPage(observed, server.NowUtc(), 8, 4, 0, 0, nil)
		if err != nil || closed != int64(len(contractIds)) || cursor != nil {
			t.Fatal("the next pass did not close the rows left open", closed, cursor, err)
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{true, true, true})
	})
}

// Only open rows past their stored deadline with exactly one balance join a
// batch, at most ContractDeadlineOwnerBatchLimit to a batch, in page order;
// a row alone on its balance, not yet due, without a stored deadline, with a
// pending legacy settlement intent, or with two balances or none keeps its own
// visit, though the payer's turn brings each of them into the grouping read.
func TestForceCloseDeadlineOwnerBatchesGroupsOnlyDueSingleBalanceRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		now := server.NowUtc()
		heavy := newNetEscrowOrderingTestFixture(t, ctx)
		heavyIds := newForceCloseOwnerBatchRows(t, ctx, heavy, ContractDeadlineOwnerBatchLimit+3, now)
		single := newNetEscrowOrderingTestFixture(t, ctx)
		singleIds := newForceCloseOwnerBatchRows(t, ctx, single, 1, now)
		pendingIds := newForceCloseOwnerBatchRows(t, ctx, heavy, 2, now)
		legacyIds := newForceCloseOwnerBatchRows(t, ctx, heavy, 2, now)
		intentIds := newForceCloseOwnerBatchRows(t, ctx, heavy, 1, now)
		twoBalanceIds := newForceCloseOwnerBatchRows(t, ctx, heavy, 1, now)
		noBalanceId, err := CreateContractNoEscrow(ctx, heavy.sourceNetworkId, heavy.sourceId, heavy.destinationNetworkId, heavy.destinationId, 100)
		server.Raise(err)
		// A second grant of the payer, added after every reserved row, funds
		// a second escrow of one row.
		AddBasicTransferBalance(ctx, heavy.sourceNetworkId, 1000, now, now.Add(time.Hour))
		secondBalanceId := server.Id{}
		for _, balance := range GetActiveTransferBalances(ctx, heavy.sourceNetworkId) {
			if balance.BalanceId != heavy.balanceId {
				secondBalanceId = balance.BalanceId
			}
		}
		if secondBalanceId == (server.Id{}) {
			t.Fatal("the payer did not gain a second balance")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count) VALUES($1,$2,5)`,
				twoBalanceIds[0], secondBalanceId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=$3 WHERE contract_id=$1`,
				noBalanceId, now.Add(-2*time.Hour), now.Add(-time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=ANY($1)`, pendingIds, now.Add(time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL WHERE contract_id=ANY($1)`, legacyIds))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome)
				VALUES($1,get_byte(uuid_send($1::uuid),15)%16,'settled')`, intentIds[0]))
		}, server.TxReadCommitted, server.OptNoRetry())

		// Page order interleaves the kinds; the first batch's rows are not contiguous.
		pageIds := []server.Id{heavyIds[0], singleIds[0], pendingIds[0], heavyIds[1], legacyIds[0]}
		pageIds = append(pageIds, heavyIds[2:]...)
		pageIds = append(pageIds, pendingIds[1], legacyIds[1], intentIds[0], twoBalanceIds[0], noBalanceId)
		openContracts := make([]*contractExpiryState, len(pageIds))
		for index, contractId := range pageIds {
			openContracts[index] = &contractExpiryState{contractId: contractId}
		}
		rowBatches := forceCloseDeadlineOwnerBatches(ctx, openContracts, forceCloseRowTurnsByPayer(ctx, openContracts), now)

		batches := []*forceCloseOwnerBatch{}
		for index, batch := range rowBatches {
			if batch != nil && batch.indexes[0] == index {
				batches = append(batches, batch)
			}
		}
		if len(batches) != 2 {
			t.Fatal("the heavy balance's due rows did not form exactly two batches", len(batches))
		}
		wantIndexes := [][]int{{0, 3}, {}}
		for index := 5; index < 5+ContractDeadlineOwnerBatchLimit+1; index += 1 {
			if len(wantIndexes[0]) < ContractDeadlineOwnerBatchLimit {
				wantIndexes[0] = append(wantIndexes[0], index)
			} else {
				wantIndexes[1] = append(wantIndexes[1], index)
			}
		}
		for batchIndex, batch := range batches {
			if batch.balanceId != heavy.balanceId || !slices.Equal(batch.indexes, wantIndexes[batchIndex]) || len(batch.members) != len(batch.indexes) {
				t.Fatal("a batch held other rows than its balance's due rows in page order", batchIndex, batch.indexes)
			}
			for _, member := range batch.members {
				// The column keeps microseconds.
				if time.Microsecond <= member.Deadline.Sub(now.Add(-time.Hour)).Abs() {
					t.Fatal("a member did not carry its stored deadline", member)
				}
			}
			if !slices.IsSortedFunc(batch.members, func(a, b ContractDeadline) int { return a.ContractId.Cmp(b.ContractId) }) {
				t.Fatal("a batch's members are not in contract order")
			}
		}
		turns := forceCloseRowTurnsByPayer(ctx, openContracts)
		for _, index := range []int{len(pageIds) - 3, len(pageIds) - 2, len(pageIds) - 1} {
			if turns[index] == nil || turns[index] != turns[0] {
				t.Fatal("a due row of the payer did not share its turn, so the grouping never read it", index)
			}
		}
		for _, index := range []int{1, 2, 4, len(pageIds) - 5, len(pageIds) - 4, len(pageIds) - 3, len(pageIds) - 2, len(pageIds) - 1} {
			if rowBatches[index] != nil {
				t.Fatal("a row alone on its balance, not yet due, without a deadline, with a pending intent, or with two balances or none joined a batch", index)
			}
		}
	})
}

// The page summary counts each batch led from the started prefix once, by how
// it ended; a batch led after the prefix, or never run, is not counted. The
// summary used to count every batch led from the prefix, held or failed ones
// too, with all their rows.
func TestForceCloseOwnerBatchSummaryCountsBatchesByOutcome(t *testing.T) {
	newBatch := func(indexes ...int) *forceCloseOwnerBatch {
		return &forceCloseOwnerBatch{balanceId: server.NewId(), indexes: indexes}
	}
	closedBatch, heldBatch, failedBatch, unrunBatch, laterBatch := newBatch(0, 2), newBatch(1, 3), newBatch(4, 7), newBatch(5, 6), newBatch(8, 9)
	rowBatches := []*forceCloseOwnerBatch{closedBatch, heldBatch, closedBatch, heldBatch, failedBatch, unrunBatch, unrunBatch, failedBatch, laterBatch, laterBatch}
	batchOutcomes := make([]forceCloseOwnerBatchOutcome, len(rowBatches))
	batchOutcomes[0], batchOutcomes[1], batchOutcomes[4], batchOutcomes[8] =
		forceCloseOwnerBatchClosed, forceCloseOwnerBatchHeld, forceCloseOwnerBatchFailed, forceCloseOwnerBatchClosed
	closedCount, heldCount, failedCount := forceCloseOwnerBatchSummary(rowBatches, batchOutcomes, 8)
	if closedCount != 1 || heldCount != 1 || failedCount != 1 {
		t.Fatalf("summary counted %d closed, %d held and %d failed batches, want 1 of each", closedCount, heldCount, failedCount)
	}
	if closedCount, heldCount, failedCount := forceCloseOwnerBatchSummary(rowBatches, batchOutcomes, 0); closedCount+heldCount+failedCount != 0 {
		t.Fatal("an empty started prefix counted batches")
	}
}
