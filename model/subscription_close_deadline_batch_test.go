// Due rows of one expiry page that share one balance close in owner batches.
// Before, each row took the balance's try-lock in its own transaction, so a
// heavy payer's page retired one row per owned turn.
package model

import (
	"context"
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

// Another owner holds the balance for the whole page. The batch is refused
// once, and each of its rows is deferred as a busy row: open with its custody,
// in the receipt, behind a cursor the walk keeps moving. The next pass closes
// them once the balance is free; none is dropped.
func TestForceClosePageDefersHeldOwnerBatchRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		now := server.NowUtc()
		// An older free row on no balance closes, so the page progresses.
		freeIds := newForceCloseDeferredRows(t, ctx, 1)
		count := 3
		contractIds := newForceCloseOwnerBatchRows(t, ctx, f, count, now)
		release := holdTransferBalanceTestOwner(t, ctx, []server.Id{f.balanceId})
		observed, turns := observeForceCloseOwnerTurns(ctx, f.balanceId, contractIds)

		closed, cursor, _, err := forceCloseOpenContractIdsPage(observed, now, 2*count+2, 4, 0, 0, nil)
		visited, ok := err.(*ForceCloseVisitError)
		if !ok || !visited.CanCheckpoint() || !forceClosePageCanAdvance(err) || !visited.Progressed() || !visited.ownershipBusyOnly() {
			t.Fatal("the held batch did not leave a busy-only receipt the walk continues past", err)
		}
		if closed != 1 || visited.AttemptedCloseCount() != 1 || cursor != nil {
			t.Fatal("the page did not close its free row and pass its held rows", closed, cursor)
		}
		if !slices.Equal(visited.DeferredContractIds(), contractIds) {
			t.Fatal("the receipt did not defer exactly the held rows in page order", visited.DeferredContractIds())
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
			t.Fatal("the next pass did not close the deferred rows", closed, cursor, err)
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{true, true, true})
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

// One member's pending intent is held by another owner, so its close fails
// and the batch's shared turn rolls back. Each row then closes alone: the
// others close, and only that row is deferred with its own failure, exactly
// as when every row was visited alone. It closes once the intent is free.
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
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome)
				VALUES($1,get_byte(uuid_send($1::uuid),15)%16,'settled')`, victim))
		}, server.TxReadCommitted, server.OptNoRetry())
		release := holdContractCloseTestRow(t, ctx, `SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1 FOR UPDATE`, victim)
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
		batchTurns, aloneTurns := 0, 0
		for _, members := range admitted {
			switch {
			case len(members) == len(contractIds):
				batchTurns += 1
			case len(members) == 1 && members[0] != victim:
				aloneTurns += 1
			default:
				t.Fatal("an unexpected owned turn", members)
			}
		}
		if batchTurns != 1 || aloneTurns != 2 {
			t.Fatalf("the page took %d batch turns and %d turns alone, want one batch then two alone", batchTurns, aloneTurns)
		}

		release()
		closed, cursor, _, err = forceCloseOpenContractIdsPage(ctx, server.NowUtc(), 8, 4, 0, 0, nil)
		if err != nil || closed != 1 || cursor != nil {
			t.Fatal("the next pass did not close the freed row", closed, cursor, err)
		}
		requireForceCloseOwnerBatchTerminal(t, ctx, contractIds, []bool{true, true, true})
	})
}

// Only open rows past their stored deadline with exactly one balance join a
// batch, at most ContractDeadlineOwnerBatchLimit to a batch, in page order;
// a row alone on its balance, not yet due or without a stored deadline keeps
// its own visit.
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
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=ANY($1)`, pendingIds, now.Add(time.Hour)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL WHERE contract_id=ANY($1)`, legacyIds))
		}, server.TxReadCommitted, server.OptNoRetry())

		// Page order interleaves the kinds; the first batch's rows are not contiguous.
		pageIds := []server.Id{heavyIds[0], singleIds[0], pendingIds[0], heavyIds[1], legacyIds[0]}
		pageIds = append(pageIds, heavyIds[2:]...)
		pageIds = append(pageIds, pendingIds[1], legacyIds[1])
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
		for _, index := range []int{1, 2, 4, len(pageIds) - 2, len(pageIds) - 1} {
			if rowBatches[index] != nil {
				t.Fatal("a row alone on its balance, not yet due or without a deadline joined a batch", index)
			}
		}
	})
}
