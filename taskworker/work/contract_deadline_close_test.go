// Contracts that carry a stored deadline have no queued close. They retire at
// that deadline through owner batches: as siblings of a queued close, through
// a refused or terminal claim, and through the continuous deadline pass.
package work

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// An id minted at the given time, as an API process mints a contract id.
func contractIdMintedAt(at time.Time) server.Id {
	id := model.ContractIdTimeBound(at)
	entropy := server.NewId()
	copy(id[6:], entropy[6:])
	return id
}

// Contracts minted at mintedAt, a millisecond apart, created then with the
// given stored deadline (nil for legacy rows) and the fixture's escrow and
// bilateral final reports. None has a queued close.
func (self *scheduledCloseOwnerFixture) addDeadlineContracts(ctx context.Context, payer *scheduledCloseOwnerPayer,
	mintedAt time.Time, count int, deadline *time.Time) []server.Id {
	return self.addDeadlineContractsCreatedAt(ctx, payer, mintedAt, mintedAt, count, deadline)
}

// As addDeadlineContracts, with the database creation clock at createdAt, as
// it is after an admission wait.
func (self *scheduledCloseOwnerFixture) addDeadlineContractsCreatedAt(ctx context.Context, payer *scheduledCloseOwnerPayer,
	mintedAt time.Time, createdAt time.Time, count int, deadline *time.Time) []server.Id {
	ids := make([]server.Id, count)
	for index := range ids {
		ids[index] = contractIdMintedAt(mintedAt.Add(time.Duration(index) * time.Millisecond))
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
			contract_id,source_network_id,source_id,destination_network_id,destination_id,
			transfer_byte_count,payer_network_id,usage_origin_is_source,create_time,expiration_time)
			SELECT contract_id,$2,$3,$4,$5,$6,$2,true,$7,$8 FROM unnest($1::uuid[]) AS candidate(contract_id)`,
			ids, payer.networkId, payer.clientId, self.providerNetworkId, self.providerClientId, scheduledCloseOwnerEscrowBytes, createdAt, deadline))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count)
			SELECT contract_id,$2,$3 FROM unnest($1::uuid[]) AS candidate(contract_id)`, ids, payer.balanceId, scheduledCloseOwnerEscrowBytes))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close (contract_id,party,used_transfer_byte_count,close_time,checkpoint)
			SELECT contract_id,party,$2,$3,false FROM unnest($1::uuid[]) AS candidate(contract_id)
			CROSS JOIN (VALUES ('source'),('destination')) AS report(party)`, ids, scheduledCloseOwnerUsedBytes, createdAt.Add(time.Minute)))
	})
	payer.contractIds = append(payer.contractIds, ids...)
	return ids
}

// Every listed contract settled its escrow at exactly the given deadline.
func requireContractsSettledAt(t testing.TB, ctx context.Context, contractIds []server.Id, deadline time.Time) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var exact int
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1) AND settled AND settle_time=$2`,
			contractIds, deadline).Scan(&exact))
		if exact != len(contractIds) {
			t.Fatalf("%d of %d contracts settled at their stored deadline %s", exact, len(contractIds), deadline)
		}
	})
}

// A run at now that may start every page and owner.
func runContractDeadlinePass(t testing.TB, ctx context.Context, args *CloseContractDeadlinesArgs, now time.Time) *CloseContractDeadlinesResult {
	t.Helper()
	return runContractDeadlinePassWithin(t, ctx, args, now, -1)
}

// A run at now that may start allowance pages and owners in total, or every
// one for a negative allowance.
func runContractDeadlinePassWithin(t testing.TB, ctx context.Context, args *CloseContractDeadlinesArgs, now time.Time, allowance int) *CloseContractDeadlinesResult {
	t.Helper()
	result, err := closeContractDeadlines(ctx, args, now, func() bool {
		if allowance < 0 {
			return true
		}
		allowance--
		return 0 <= allowance
	})
	if err != nil {
		t.Fatal("deadline pass failed", err)
	}
	return result
}

// Production shape: a payer's grant owns one legacy contract the startup pass
// queued and, issued after that pass, contracts that carry their own deadline
// and have no queued close. The queued close's owner batch retires its due
// siblings at their stored deadlines in the same two owner turns, while a
// sibling whose deadline has not passed and an unqueued legacy sibling stay
// open. On the unchanged worker only the queued contract closed: a sibling
// without a queued close was never due.
func TestScheduledContractCloseOwnerBatchRetiresSiblingsAtTheirStoredDeadlines(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		payer := f.newPayer(t, t.Context(), "synthetic-stored-deadline-sibling-payer")
		queued := f.addLegacyContracts(t.Context(), payer, 1)
		events := newScheduledCloseOwnerEvents(payer)
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), events.observe)
		queuedDeadline := publishScheduledCloses(t, ctx)

		mintedAt := server.NowUtc().Truncate(time.Millisecond).Add(-90 * time.Minute)
		deadline := mintedAt.Add(model.DefaultContractExpiration)
		due := f.addDeadlineContracts(t.Context(), payer, mintedAt, 5, &deadline)
		future := server.NowUtc().Truncate(time.Microsecond).Add(time.Hour)
		f.addDeadlineContracts(t.Context(), payer, mintedAt, 2, &future)
		f.addDeadlineContracts(t.Context(), payer, mintedAt, 1, nil)

		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || len(retried) != 0 || len(posts) != 0 {
			t.Fatal("queued close did not finish", finished, retried, posts, err)
		}
		result := readScheduledCloseResultFields(t, ctx)[queued[0]]
		if siblings, _ := result["owner_batch_closed"].(float64); int(siblings) != len(due) {
			t.Fatalf("queued close retired %v stored-deadline siblings; want %d", result["owner_batch_closed"], len(due))
		}
		requireScheduledCloseOwnerSettled(t, ctx, payer, 1+len(due))
		requireContractsSettledAt(t, ctx, queued, queuedDeadline)
		requireContractsSettledAt(t, ctx, due, deadline)
		if admitted, waiting, refused := events.counts(payer.balanceId); admitted != 2 || waiting != 0 || refused != 0 {
			t.Fatal("siblings did not close in the queued close's owner batch", admitted, waiting, refused)
		}
	})
}

// A terminal head claim spends itself on another owner. The newest contracts
// carry their own deadline and have no queued close, so on the unchanged
// worker the steal sampled only queued closes and retired an old queued
// owner while the recently due owner stayed open. Now the claim retires the
// recently due owner's batch at its stored deadlines first.
func TestScheduledContractCloseTerminalClaimRetiresRecentlyDueOwnerFirst(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		settled := f.newPayer(t, t.Context(), "synthetic-recent-steal-settled-payer")
		f.addLegacyContracts(t.Context(), settled, 1)
		queuedBehind := f.newPayer(t, t.Context(), "synthetic-recent-steal-queued-payer")
		f.addLegacyContracts(t.Context(), queuedBehind, model.ContractDeadlineOwnerBatchLimit)
		recent := f.newPayer(t, t.Context(), "synthetic-recent-steal-due-payer")
		events := newScheduledCloseOwnerEvents(settled, queuedBehind, recent)
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), events.observe)
		deadline := publishScheduledCloses(t, ctx)
		if closed, err := model.ReconcileContractAtDeadline(t.Context(), settled.contractIds[0], deadline); err != nil || closed.AlreadyClosed {
			t.Fatal("fixture did not settle the head contract before its queued close", err)
		}
		// The retained queue still holds closes that outlived their contracts.
		requeueRetiredScheduledClose(t, ctx, settled.contractIds[0], deadline)

		// Issued after the startup pass, so none has a queued close.
		mintedAt := server.NowUtc().Truncate(time.Millisecond).Add(-90 * time.Minute)
		recentDeadline := mintedAt.Add(model.DefaultContractExpiration)
		f.addDeadlineContracts(t.Context(), recent, mintedAt, model.ContractDeadlineOwnerBatchLimit, &recentDeadline)
		sliceStart := scheduledCloseRecentSliceStart
		scheduledCloseRecentSliceStart = func(time.Time, time.Duration) time.Time { return mintedAt.Add(-time.Millisecond) }
		defer func() { scheduledCloseRecentSliceStart = sliceStart }()

		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || len(retried) != 0 || len(posts) != 0 {
			t.Fatal("terminal head claim failed", finished, retried, posts, err)
		}
		results := readScheduledCloseResultFields(t, ctx)
		reconciliation, _ := results[settled.contractIds[0]]["reconciliation"].(map[string]any)
		if len(results) != 1 || reconciliation["already_closed"] != true {
			t.Fatal("the claim was not the terminal queue head", results)
		}
		requireScheduledCloseOwnerSettled(t, ctx, recent, model.ContractDeadlineOwnerBatchLimit)
		requireContractsSettledAt(t, ctx, recent.contractIds, recentDeadline)
		requireScheduledCloseOwnerSettled(t, ctx, queuedBehind, 0)
		if admitted, waiting, refused := events.counts(recent.balanceId); admitted != 2 || waiting != 0 || refused != 0 {
			t.Fatal("recently due owner did not close through one owner batch", admitted, waiting, refused)
		}
		if admitted, waiting, refused := events.counts(queuedBehind.balanceId); admitted != 0 || waiting != 0 || refused != 0 {
			t.Fatal("the claim worked an old queued owner before the recently due one", admitted, waiting, refused)
		}
	})
}

// The last hour and the rest of the last day hold nothing due, as once the
// steals keep up with new deadlines. A terminal claim then retires an owner
// from the older stored-deadline backlog before an old queued owner. On the
// unchanged worker it retired the old queued owner and left the backlog open.
func TestScheduledContractCloseTerminalClaimRetiresDeadlineBacklogBeforeQueue(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		settled := f.newPayer(t, t.Context(), "synthetic-backlog-steal-settled-payer")
		f.addLegacyContracts(t.Context(), settled, 1)
		queuedBehind := f.newPayer(t, t.Context(), "synthetic-backlog-steal-queued-payer")
		f.addLegacyContracts(t.Context(), queuedBehind, model.ContractDeadlineOwnerBatchLimit)
		backlog := f.newPayer(t, t.Context(), "synthetic-backlog-steal-due-payer")
		events := newScheduledCloseOwnerEvents(settled, queuedBehind, backlog)
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), events.observe)
		deadline := publishScheduledCloses(t, ctx)
		if closed, err := model.ReconcileContractAtDeadline(t.Context(), settled.contractIds[0], deadline); err != nil || closed.AlreadyClosed {
			t.Fatal("fixture did not settle the head contract before its queued close", err)
		}
		// The retained queue still holds closes that outlived their contracts.
		requeueRetiredScheduledClose(t, ctx, settled.contractIds[0], deadline)

		// Issued two days ago, after the startup pass's position, without queued closes.
		mintedAt := server.NowUtc().Truncate(time.Millisecond).Add(-48 * time.Hour)
		backlogDeadline := mintedAt.Add(model.DefaultContractExpiration)
		f.addDeadlineContracts(t.Context(), backlog, mintedAt, model.ContractDeadlineOwnerBatchLimit, &backlogDeadline)
		// Only the window that holds the backlog reads it; a newer window
		// reads an empty slice at its start.
		windowsRead := 0
		sliceStart := scheduledCloseRecentSliceStart
		scheduledCloseRecentSliceStart = func(windowStart time.Time, span time.Duration) time.Time {
			windowsRead++
			if !mintedAt.Before(windowStart) && mintedAt.Before(windowStart.Add(span)) {
				return mintedAt.Add(-time.Millisecond)
			}
			return windowStart
		}
		defer func() { scheduledCloseRecentSliceStart = sliceStart }()

		worker := startupClosureWorker(ctx, NewScheduledContractClosureTaskTarget())
		defer worker.Close()
		finished, retried, posts, err := worker.EvalTasks(1)
		if err != nil || len(finished) != 1 || len(retried) != 0 || len(posts) != 0 {
			t.Fatal("terminal head claim failed", finished, retried, posts, err)
		}
		requireScheduledCloseOwnerSettled(t, ctx, backlog, model.ContractDeadlineOwnerBatchLimit)
		requireContractsSettledAt(t, ctx, backlog.contractIds, backlogDeadline)
		requireScheduledCloseOwnerSettled(t, ctx, queuedBehind, 0)
		if windowsRead != 3 {
			t.Fatal("the steal did not read the hour, the day and the lookback in turn", windowsRead)
		}
		if admitted, waiting, refused := events.counts(backlog.balanceId); admitted != 2 || waiting != 0 || refused != 0 {
			t.Fatal("backlog owner did not close through one owner batch", admitted, waiting, refused)
		}
		if admitted, waiting, refused := events.counts(queuedBehind.balanceId); admitted != 0 || waiting != 0 || refused != 0 {
			t.Fatal("the claim worked an old queued owner before the deadline backlog", admitted, waiting, refused)
		}
	})
}

// The first run starts at the deadline edge. The next run retires every
// contract whose stored deadline passed since, one owner batch per owner:
// a wide owner needs a second batch, which its retry runs, and a contract not
// yet due waits for a retry until its deadline. A legacy contract without a
// stored deadline is never touched. Every close settles at its own deadline.
func TestContractDeadlinePassRetiresNewDeadlinesThroughOwnerBatches(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		mintedAt := server.NowUtc().Truncate(time.Millisecond)
		deadline := mintedAt.Add(model.DefaultContractExpiration)
		lateDeadline := deadline.Add(3 * time.Hour)
		const wideCount = model.ContractDeadlineOwnerBatchLimit + 6
		wide := f.newPayer(t, t.Context(), "synthetic-deadline-pass-wide-payer")
		f.addDeadlineContracts(t.Context(), wide, mintedAt, wideCount, &deadline)
		narrow := f.newPayer(t, t.Context(), "synthetic-deadline-pass-narrow-payer")
		f.addDeadlineContracts(t.Context(), narrow, mintedAt, 3, &deadline)
		legacy := f.newPayer(t, t.Context(), "synthetic-deadline-pass-legacy-payer")
		f.addDeadlineContracts(t.Context(), legacy, mintedAt, 2, nil)
		late := f.newPayer(t, t.Context(), "synthetic-deadline-pass-late-payer")
		f.addDeadlineContracts(t.Context(), late, mintedAt, 2, &lateDeadline)
		events := newScheduledCloseOwnerEvents(wide, narrow, legacy, late)
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), events.observe)
		requireOpen := func(closed map[*scheduledCloseOwnerPayer]int) {
			t.Helper()
			for _, payer := range []*scheduledCloseOwnerPayer{wide, narrow, legacy, late} {
				requireScheduledCloseOwnerSettled(t, ctx, payer, closed[payer])
			}
		}

		// The contracts are minted after the first run's edge.
		first := runContractDeadlinePass(t, ctx, &CloseContractDeadlinesArgs{}, deadline)
		if first.Next.Fresh != model.ContractIdTimeBound(mintedAt.Add(-contractDeadlineCloseEdgeMargin)) ||
			first.Next.Backlog != nil || first.Next.Retry != nil || first.Next.Pending != nil || first.Closed != 0 || first.Exhausted {
			t.Fatalf("first run did not start at the edge with an empty backlog: %+v", first)
		}
		requireOpen(nil)

		second := runContractDeadlinePass(t, ctx, &first.Next, deadline.Add(2*time.Minute))
		if second.Closed != model.ContractDeadlineOwnerBatchLimit+3 || second.Next.Pending == nil || second.Next.Retry != nil {
			t.Fatalf("new deadlines did not close one batch per owner and keep the rest for a retry: %+v", second)
		}
		requireOpen(map[*scheduledCloseOwnerPayer]int{wide: model.ContractDeadlineOwnerBatchLimit, narrow: 3})
		for _, payer := range []*scheduledCloseOwnerPayer{wide, narrow} {
			if admitted, waiting, refused := events.counts(payer.balanceId); admitted != 2 || waiting != 0 || refused != 0 {
				t.Fatal("owner did not close through one owner batch", admitted, waiting, refused)
			}
		}

		third := runContractDeadlinePass(t, ctx, &second.Next, deadline.Add(3*time.Minute))
		if third.Closed != wideCount-model.ContractDeadlineOwnerBatchLimit || third.Dropped != 0 || third.Next.Retry != nil || third.Next.Pending == nil {
			t.Fatalf("retry did not close the wide owner's second batch and keep the late contracts: %+v", third)
		}
		requireOpen(map[*scheduledCloseOwnerPayer]int{wide: wideCount, narrow: 3})
		if admitted, _, _ := events.counts(wide.balanceId); admitted != 4 {
			t.Fatal("wide owner's retry was not one more owner batch", admitted)
		}

		fourth := runContractDeadlinePass(t, ctx, &third.Next, lateDeadline.Add(2*time.Minute))
		if fourth.Closed != 2 || fourth.Next.Retry != nil || fourth.Next.Pending != nil {
			t.Fatalf("late contracts did not close once due: %+v", fourth)
		}
		requireOpen(map[*scheduledCloseOwnerPayer]int{wide: wideCount, narrow: 3, late: 2})
		requireContractsSettledAt(t, ctx, append(append([]server.Id{}, wide.contractIds...), narrow.contractIds...), deadline)
		requireContractsSettledAt(t, ctx, late.contractIds, lateDeadline)
		if admitted, waiting, refused := events.counts(legacy.balanceId); admitted != 0 || waiting != 0 || refused != 0 {
			t.Fatal("deadline pass worked a legacy owner without a stored deadline", admitted, waiting, refused)
		}
	})
}

// The first run's backlog pass starts at the deadline edge. New deadlines
// close first: with budget for one page the next run closes them and leaves
// the backlog position unchanged. The backlog then walks down across runs from
// its retained position and closes the due contracts it meets, the legacy ones
// at their creation plus the default lifetime. It passes a full page of legacy
// contracts and closes the older contracts below it; the unchanged pass ended
// its walk at that page and never read them.
func TestContractDeadlinePassClosesNewDeadlinesBeforeTheBacklog(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		newMintedAt := server.NowUtc().Truncate(time.Millisecond)
		backlogMintedAt := newMintedAt.Add(-5 * time.Hour)
		backlogDeadline := backlogMintedAt.Add(model.DefaultContractExpiration)
		newDeadline := newMintedAt.Add(model.DefaultContractExpiration)
		older := f.newPayer(t, t.Context(), "synthetic-deadline-backlog-payer")
		f.addDeadlineContracts(t.Context(), older, backlogMintedAt, 3, &backlogDeadline)
		legacyMintedAt := backlogMintedAt.Add(-time.Hour)
		legacyDeadline := legacyMintedAt.Add(model.DefaultContractExpiration)
		legacy := f.newPayer(t, t.Context(), "synthetic-deadline-backlog-legacy-payer")
		f.addDeadlineContracts(t.Context(), legacy, legacyMintedAt, 2*contractDeadlineClosePageSize, nil)
		ancientMintedAt := backlogMintedAt.Add(-2 * time.Hour)
		ancientDeadline := ancientMintedAt.Add(model.DefaultContractExpiration)
		ancient := f.newPayer(t, t.Context(), "synthetic-deadline-ancient-payer")
		f.addDeadlineContracts(t.Context(), ancient, ancientMintedAt, 2, &ancientDeadline)
		newer := f.newPayer(t, t.Context(), "synthetic-deadline-new-payer")
		f.addDeadlineContracts(t.Context(), newer, newMintedAt, 3, &newDeadline)
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)

		edge := model.ContractIdTimeBound(newMintedAt.Add(-contractDeadlineCloseEdgeMargin))
		first := runContractDeadlinePassWithin(t, ctx, &CloseContractDeadlinesArgs{}, newDeadline, 0)
		if first.Next.Fresh != edge || first.Next.Backlog == nil || first.Next.Backlog.Upper != edge || !first.Exhausted {
			t.Fatalf("first run did not start the backlog at the edge: %+v", first)
		}

		// One fresh page and its one owner, then the budget ends.
		second := runContractDeadlinePassWithin(t, ctx, &first.Next, newDeadline.Add(2*time.Minute), 2)
		requireScheduledCloseOwnerSettled(t, ctx, newer, 3)
		requireScheduledCloseOwnerSettled(t, ctx, older, 0)
		if second.Next.Backlog == nil || second.Next.Backlog.Upper != edge || !second.Exhausted {
			t.Fatalf("new deadlines did not close before the backlog moved: %+v", second)
		}

		// The empty fresh read, one backlog page and its two owners: the
		// three older contracts and the newest legacy ones that fill the page.
		third := runContractDeadlinePassWithin(t, ctx, &second.Next, newDeadline.Add(3*time.Minute), 4)
		requireScheduledCloseOwnerSettled(t, ctx, older, 3)
		requireContractsSettledAt(t, ctx, older.contractIds, backlogDeadline)
		requireScheduledCloseOwnerSettled(t, ctx, legacy, contractDeadlineClosePageSize-3)
		if third.Next.Backlog == nil || third.Next.Backlog.Upper.Cmp(edge) >= 0 || third.Next.Backlog.Upper.Cmp(older.contractIds[0]) >= 0 {
			t.Fatalf("backlog did not retain its position below the page it closed: %+v", third)
		}

		// A full page of legacy contracts, then the last of them and the
		// ancient contracts below.
		fourth := runContractDeadlinePass(t, ctx, &third.Next, newDeadline.Add(4*time.Minute))
		requireScheduledCloseOwnerSettled(t, ctx, ancient, 2)
		requireContractsSettledAt(t, ctx, ancient.contractIds, ancientDeadline)
		requireScheduledCloseOwnerSettled(t, ctx, legacy, 2*contractDeadlineClosePageSize)
		requireContractsSettledAt(t, ctx, legacy.contractIds, legacyDeadline)
		if fourth.Next.Backlog != nil || fourth.Closed != contractDeadlineClosePageSize+5 || fourth.Exhausted {
			t.Fatalf("backlog did not walk past the full legacy page to the contracts below: %+v", fourth)
		}
	})
}

// The budget ends after the first of a page's two owners started. The page is
// not complete, so the run keeps every contract of it for the next run, which
// closes the owner that never started instead of skipping it.
func TestContractDeadlinePassRereadsAPageItsBudgetCutShort(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		mintedAt := server.NowUtc().Truncate(time.Millisecond)
		deadline := mintedAt.Add(model.DefaultContractExpiration)
		payers := []*scheduledCloseOwnerPayer{
			f.newPayer(t, t.Context(), "synthetic-deadline-cut-payer-0"),
			f.newPayer(t, t.Context(), "synthetic-deadline-cut-payer-1"),
		}
		for _, payer := range payers {
			f.addDeadlineContracts(t.Context(), payer, mintedAt, 2, &deadline)
		}
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		first := runContractDeadlinePass(t, ctx, &CloseContractDeadlinesArgs{}, deadline)

		// One fresh page and one of its two owners.
		second := runContractDeadlinePassWithin(t, ctx, &first.Next, deadline.Add(2*time.Minute), 2)
		closedPayers := 0
		for _, payer := range payers {
			server.Db(ctx, func(conn server.PgConn) {
				var closed int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1) AND outcome IS NOT NULL`,
					payer.contractIds).Scan(&closed))
				if closed == len(payer.contractIds) {
					closedPayers++
				} else if closed != 0 {
					t.Fatal("an owner batch closed part of its owner", closed)
				}
			})
		}
		if closedPayers != 1 || !second.Exhausted || second.Visited != 0 || second.Next.Pending == nil {
			t.Fatalf("budget cut did not keep the page for the next run: closed owners=%d %+v", closedPayers, second)
		}

		third := runContractDeadlinePass(t, ctx, &second.Next, deadline.Add(3*time.Minute))
		if third.Closed != 2 || third.Next.Retry != nil || third.Next.Pending != nil {
			t.Fatalf("next run did not close the owner the budget cut off: %+v", third)
		}
		for _, payer := range payers {
			requireScheduledCloseOwnerSettled(t, ctx, payer, 2)
			requireContractsSettledAt(t, ctx, payer.contractIds, deadline)
		}
	})
}

// A page read that outlasts its timeout stops the run without failing it. The
// new deadlines it could not read wait for a retry and the run's positions are
// kept, so the next run closes them.
func TestContractDeadlinePassKeepsItsPositionsWhenAReadTimesOut(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		mintedAt := server.NowUtc().Truncate(time.Millisecond)
		deadline := mintedAt.Add(model.DefaultContractExpiration)
		payer := f.newPayer(t, t.Context(), "synthetic-deadline-read-timeout-payer")
		f.addDeadlineContracts(t.Context(), payer, mintedAt, 3, &deadline)
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		first := runContractDeadlinePass(t, ctx, &CloseContractDeadlinesArgs{}, deadline)

		readTimeout := contractDeadlineCloseReadTimeout
		contractDeadlineCloseReadTimeout = time.Nanosecond
		second := runContractDeadlinePass(t, ctx, &first.Next, deadline.Add(2*time.Minute))
		contractDeadlineCloseReadTimeout = readTimeout
		requireScheduledCloseOwnerSettled(t, ctx, payer, 0)
		if second.Exhausted || second.Visited != 0 || second.Next.Fresh == first.Next.Fresh || second.Next.Pending == nil ||
			second.Next.Pending.Lower != first.Next.Fresh || second.Next.Pending.Upper != second.Next.Fresh {
			t.Fatalf("timed-out read did not keep the new deadlines for a retry: %+v", second)
		}

		third := runContractDeadlinePass(t, ctx, &second.Next, deadline.Add(3*time.Minute))
		if third.Closed != 3 || third.Next.Retry != nil || third.Next.Pending != nil {
			t.Fatalf("retry did not close the contracts the timed-out read missed: %+v", third)
		}
		requireScheduledCloseOwnerSettled(t, ctx, payer, 3)
		requireContractsSettledAt(t, ctx, payer.contractIds, deadline)
	})
}

// Production shape: the rollout left a run of legacy contracts, rows without a
// stored deadline, above contracts that carry one, and the startup pass queued
// none of them. The unchanged pass ended its backlog walk at the first full
// page of them, so it closed neither those legacy contracts nor any contract
// below. The walk now passes the page: every contract below closes at its
// stored deadline, and the legacy contracts at their creation plus the default
// lifetime, through their owner's batch and alone, including those of an owner
// whose other contracts carry a deadline.
func TestContractDeadlinePassBacklogClosesPastAFullPageOfLegacyContracts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		now := server.NowUtc().Truncate(time.Millisecond)
		legacyMintedAt := now.Add(-60 * time.Hour)
		legacyDeadline := legacyMintedAt.Add(model.DefaultContractExpiration)
		strandedMintedAt := now.Add(-100 * time.Hour)
		strandedDeadline := strandedMintedAt.Add(model.DefaultContractExpiration)
		mixedMintedAt := strandedMintedAt.Add(2 * time.Minute)
		mixedDeadline := mixedMintedAt.Add(model.DefaultContractExpiration)
		// Two legacy owners fill more than a page between them.
		legacyPayers := []*scheduledCloseOwnerPayer{
			f.newPayer(t, t.Context(), "synthetic-legacy-page-payer-0"),
			f.newPayer(t, t.Context(), "synthetic-legacy-page-payer-1"),
		}
		for _, payer := range legacyPayers {
			f.addDeadlineContracts(t.Context(), payer, legacyMintedAt, 70, nil)
		}
		// One owner has legacy contracts in the run and stranded ones below it.
		mixed := f.newPayer(t, t.Context(), "synthetic-legacy-page-mixed-payer")
		mixedLegacyIds := f.addDeadlineContracts(t.Context(), mixed, legacyMintedAt, 3, nil)
		mixedStrandedIds := f.addDeadlineContracts(t.Context(), mixed, mixedMintedAt, 4, &mixedDeadline)
		strandedPayers := []*scheduledCloseOwnerPayer{
			f.newPayer(t, t.Context(), "synthetic-legacy-page-stranded-payer-0"),
			f.newPayer(t, t.Context(), "synthetic-legacy-page-stranded-payer-1"),
		}
		f.addDeadlineContracts(t.Context(), strandedPayers[0], strandedMintedAt, 40, &strandedDeadline)
		f.addDeadlineContracts(t.Context(), strandedPayers[1], strandedMintedAt.Add(time.Minute), 50, &strandedDeadline)
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)

		// The backlog's first page holds only legacy contracts.
		edge := model.ContractIdTimeBound(now.Add(-model.DefaultContractExpiration - contractDeadlineCloseEdgeMargin))
		page, err := model.ReadOpenContractDeadlines(ctx, model.ContractIdTimeBound(now.Add(-contractDeadlineCloseLookback)), edge,
			contractDeadlineClosePageSize, true)
		server.Raise(err)
		for _, contract := range page {
			if contract.ExpirationTime != nil {
				t.Fatal("fixture did not put a full page of legacy contracts at the top of the backlog")
			}
		}
		if len(page) != contractDeadlineClosePageSize {
			t.Fatal("fixture backlog page is not full", len(page))
		}

		result := runContractDeadlinePass(t, ctx, &CloseContractDeadlinesArgs{}, now)
		for _, payer := range strandedPayers {
			requireScheduledCloseOwnerSettled(t, ctx, payer, len(payer.contractIds))
			requireContractsSettledAt(t, ctx, payer.contractIds, strandedDeadline)
		}
		requireScheduledCloseOwnerSettled(t, ctx, mixed, len(mixed.contractIds))
		requireContractsSettledAt(t, ctx, mixedStrandedIds, mixedDeadline)
		requireContractsSettledAt(t, ctx, mixedLegacyIds, legacyDeadline)
		for _, payer := range legacyPayers {
			requireScheduledCloseOwnerSettled(t, ctx, payer, len(payer.contractIds))
			requireContractsSettledAt(t, ctx, payer.contractIds, legacyDeadline)
		}
		if result.Next.Backlog != nil || result.Next.Pending != nil || result.Exhausted || result.Dropped != 0 || result.Closed != 2*70+3+4+40+50 {
			t.Fatalf("backlog did not close every contract on its way to the lookback: %+v", result)
		}
	})
}

// Production shape: a position written by the earlier release after its walk
// ended at a page of legacy contracts. A startup merge keeps a pending run's
// arguments, so on the unchanged pass no run walked the contracts below that
// page again. The first run of this release reads a position without a version
// and walks the backlog again from the new-deadline position, past the legacy
// page and down to the lookback. Its successor carries the version, so the
// next run does not walk again.
func TestContractDeadlinePassRearmsTheBacklogOfAPositionWithoutVersion(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		now := server.NowUtc().Truncate(time.Millisecond)
		strandedMintedAt := now.Add(-100 * time.Hour)
		strandedDeadline := strandedMintedAt.Add(model.DefaultContractExpiration)
		stranded := f.newPayer(t, t.Context(), "synthetic-rearm-stranded-payer")
		f.addDeadlineContracts(t.Context(), stranded, strandedMintedAt, 40, &strandedDeadline)
		legacyMintedAt := now.Add(-60 * time.Hour)
		legacyDeadline := legacyMintedAt.Add(model.DefaultContractExpiration)
		legacy := f.newPayer(t, t.Context(), "synthetic-rearm-legacy-payer")
		f.addDeadlineContracts(t.Context(), legacy, legacyMintedAt, contractDeadlineClosePageSize+2, nil)
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)

		key := task.RunOnce("close_contract_deadlines").String()
		readPass := func() (taskId server.Id, args map[string]any, argsJson string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT task_id,args_json FROM pending_task WHERE run_once_key=$1`, key).Scan(&taskId, &argsJson))
			})
			server.Raise(json.Unmarshal([]byte(argsJson), &args))
			return
		}
		startup := func() {
			server.Tx(ctx, func(tx server.PgTx) {
				clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
				defer clientSession.Cancel()
				ScheduleCloseContractDeadlines(clientSession, tx)
			})
		}
		// The earlier release's position: new deadlines reached two hours ago,
		// a few seconds of them kept for a retry, and no backlog.
		fresh := model.ContractIdTimeBound(now.Add(-2 * time.Hour))
		position := fmt.Sprintf(`{"fresh":"%s","pending":{"lower":"%s","upper":"%s"}}`,
			fresh, model.ContractIdTimeBound(now.Add(-2*time.Hour-10*time.Second)), fresh)
		startup()
		taskId, _, _ := readPass()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE pending_task SET args_json=$2 WHERE task_id=$1`, taskId, position))
		})
		// A restart merges into the pending run and keeps its position.
		startup()
		if _, _, argsJson := readPass(); argsJson != position {
			t.Fatal("startup replaced the pending run's position", argsJson)
		}

		worker := startupClosureWorker(ctx, NewContractDeadlineCloseTaskTarget())
		defer worker.Close()
		evalPass := func(taskId server.Id) {
			t.Helper()
			makeCloseRetryTaskDue(ctx, taskId)
			finished, retried, posts, err := worker.EvalTasks(1)
			if err != nil || len(finished) != 1 || finished[0] != taskId || len(retried)+len(posts) != 0 {
				t.Fatal("deadline pass run did not finish", finished, retried, posts, err)
			}
		}
		evalPass(taskId)
		requireScheduledCloseOwnerSettled(t, ctx, stranded, len(stranded.contractIds))
		requireContractsSettledAt(t, ctx, stranded.contractIds, strandedDeadline)
		requireScheduledCloseOwnerSettled(t, ctx, legacy, len(legacy.contractIds))
		requireContractsSettledAt(t, ctx, legacy.contractIds, legacyDeadline)
		server.Db(ctx, func(conn server.PgConn) {
			var resultJson string
			server.Raise(conn.QueryRow(ctx, `SELECT result_json FROM finished_task WHERE task_id=$1`, taskId).Scan(&resultJson))
			var result map[string]any
			server.Raise(json.Unmarshal([]byte(resultJson), &result))
			if result["closed"] != float64(len(stranded.contractIds)+len(legacy.contractIds)) || result["legacy_closed"] != float64(len(legacy.contractIds)) {
				t.Fatal("the run's stored result does not count the contracts it closed", resultJson)
			}
		})

		successorId, successor, successorJson := readPass()
		if successorId == taskId || successor["version"] != float64(1) || successor["backlog_start"] != fresh.String() ||
			successor["backlog"] != nil || successor["retry"] != nil || successor["pending"] != nil {
			t.Fatal("successor did not carry the version with its walk done", successorJson)
		}
		evalPass(successorId)
		if _, later, laterJson := readPass(); later["version"] != float64(1) || later["backlog_start"] != fresh.String() || later["backlog"] != nil {
			t.Fatal("a run with a versioned position walked the backlog again", laterJson)
		}
	})
}

// The backlog walk ends at its lookback, past a full page of legacy contracts.
// The unchanged pass ended at that page, so contracts just inside the lookback
// stayed open; now they close, while contracts minted before the lookback,
// with or without a stored deadline, are never read.
func TestContractDeadlinePassBacklogEndsAtItsLookback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		now := server.NowUtc().Truncate(time.Millisecond)
		insideMintedAt := now.Add(-contractDeadlineCloseLookback + 2*time.Hour)
		insideDeadline := insideMintedAt.Add(model.DefaultContractExpiration)
		inside := f.newPayer(t, t.Context(), "synthetic-lookback-inside-payer")
		f.addDeadlineContracts(t.Context(), inside, insideMintedAt, 3, &insideDeadline)
		beyondMintedAt := now.Add(-contractDeadlineCloseLookback - 2*time.Hour)
		beyondDeadline := beyondMintedAt.Add(model.DefaultContractExpiration)
		beyond := f.newPayer(t, t.Context(), "synthetic-lookback-beyond-payer")
		f.addDeadlineContracts(t.Context(), beyond, beyondMintedAt, 3, &beyondDeadline)
		beyondLegacy := f.newPayer(t, t.Context(), "synthetic-lookback-beyond-legacy-payer")
		f.addDeadlineContracts(t.Context(), beyondLegacy, beyondMintedAt, 2, nil)
		legacyMintedAt := now.Add(-7 * 24 * time.Hour)
		legacy := f.newPayer(t, t.Context(), "synthetic-lookback-legacy-payer")
		f.addDeadlineContracts(t.Context(), legacy, legacyMintedAt, contractDeadlineClosePageSize+2, nil)
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)

		result := runContractDeadlinePass(t, ctx, &CloseContractDeadlinesArgs{}, now)
		requireScheduledCloseOwnerSettled(t, ctx, inside, len(inside.contractIds))
		requireContractsSettledAt(t, ctx, inside.contractIds, insideDeadline)
		requireScheduledCloseOwnerSettled(t, ctx, legacy, len(legacy.contractIds))
		requireContractsSettledAt(t, ctx, legacy.contractIds, legacyMintedAt.Add(model.DefaultContractExpiration))
		requireScheduledCloseOwnerSettled(t, ctx, beyond, 0)
		requireScheduledCloseOwnerSettled(t, ctx, beyondLegacy, 0)
		if result.Next.Backlog != nil || result.Next.Pending != nil || result.Exhausted || result.Visited != len(inside.contractIds)+len(legacy.contractIds) {
			t.Fatalf("backlog did not end at its lookback: %+v", result)
		}
	})
}

// An admission wait can start a legacy contract's database clock after its id
// was minted, so a legacy contract near the backlog's start may not have
// reached the end of its lifetime when the backlog reads it. The backlog keeps
// it for a retry, which closes it once due. The unchanged pass skipped it and
// kept nothing.
func TestContractDeadlinePassRetriesALegacyContractTheBacklogFoundNotDue(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		now := server.NowUtc().Truncate(time.Millisecond)
		mintedAt := now.Add(-model.DefaultContractExpiration - contractDeadlineCloseEdgeMargin - 30*time.Second)
		createdAt := mintedAt.Add(2 * time.Minute)
		deadline := createdAt.Add(model.DefaultContractExpiration)
		payer := f.newPayer(t, t.Context(), "synthetic-legacy-admission-wait-payer")
		f.addDeadlineContractsCreatedAt(t.Context(), payer, mintedAt, createdAt, 2, nil)
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)

		first := runContractDeadlinePass(t, ctx, &CloseContractDeadlinesArgs{}, now)
		requireScheduledCloseOwnerSettled(t, ctx, payer, 0)
		if first.Next.Backlog != nil || first.Next.Pending == nil || first.Visited != 0 {
			t.Fatalf("backlog did not keep the legacy contracts it found not yet due: %+v", first)
		}

		second := runContractDeadlinePass(t, ctx, &first.Next, deadline.Add(time.Second))
		requireScheduledCloseOwnerSettled(t, ctx, payer, len(payer.contractIds))
		requireContractsSettledAt(t, ctx, payer.contractIds, deadline)
		if second.Next.Retry != nil || second.Next.Pending != nil || second.Closed != len(payer.contractIds) {
			t.Fatalf("retry did not close the legacy contracts once due: %+v", second)
		}
	})
}

// A held owner refuses the batch a legacy contract leads, as it refuses any
// owner batch. Its legacy contracts then close alone, each waiting for the
// owner's admission, so a held owner does not leave them to the expiry sweep.
// The holder releases once the first of them waits. The unchanged pass never
// closed a legacy contract.
func TestContractDeadlinePassClosesTheLegacyContractsOfAHeldOwnerAlone(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		now := server.NowUtc().Truncate(time.Millisecond)
		mintedAt := now.Add(-60 * time.Hour)
		payer := f.newPayer(t, t.Context(), "synthetic-held-legacy-payer")
		f.addDeadlineContracts(t.Context(), payer, mintedAt, 3, nil)
		balanceKey := server.NewPgOwnershipKey("transfer_balance", payer.balanceId)
		events := newScheduledCloseOwnerEvents(payer)
		admittedHolder, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var releaseOnce sync.Once
		releaseHolder := func() { releaseOnce.Do(func() { close(release) }) }
		ctx := server.Testing_WithPgOwnershipObservation(model.WithProviderWorkSessionSource(t.Context(), nil), func(event server.PgOwnershipEvent) {
			events.observe(event)
			if event.Kind == server.PgOwnershipWaiting && !event.TransactionScoped && slices.Contains(event.Keys, balanceKey) {
				releaseHolder()
			}
		})
		go func() {
			defer close(holderDone)
			server.OwnedTx(t.Context(), []server.PgOwnershipKey{balanceKey}, func(tx server.PgTx) {
				close(admittedHolder)
				<-release
			}, server.TxReadCommitted, server.OptNoRetry())
		}()
		<-admittedHolder
		defer func() {
			releaseHolder()
			<-holderDone
		}()

		result := runContractDeadlinePass(t, ctx, &CloseContractDeadlinesArgs{}, now)
		requireScheduledCloseOwnerSettled(t, ctx, payer, len(payer.contractIds))
		requireContractsSettledAt(t, ctx, payer.contractIds, mintedAt.Add(model.DefaultContractExpiration))
		if admitted, waiting, refused := events.counts(payer.balanceId); refused != 1 || waiting == 0 || admitted != len(payer.contractIds) {
			t.Fatal("held owner's legacy contracts did not close alone after the refused batch", admitted, waiting, refused)
		}
		if result.Next.Backlog != nil || result.Next.Pending != nil || result.Dropped != 0 || result.Closed != len(payer.contractIds) {
			t.Fatalf("held owner's legacy contracts were left open: %+v", result)
		}
	})
}

// A position without a version whose walk had not ended yet. The earlier
// release skipped the legacy contracts on the pages it passed, so continuing
// from its retained position would leave those open. The first run of this
// release walks again from the new-deadline position, closing them, and keeps
// the earlier walk's lower bound, which lies below its own lookback.
func TestContractDeadlinePassRearmsAnUnfinishedWalkFromTheNewDeadlinePosition(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newScheduledCloseOwnerFixture(t.Context())
		now := server.NowUtc().Truncate(time.Millisecond)
		skippedMintedAt := now.Add(-60 * time.Hour)
		skipped := f.newPayer(t, t.Context(), "synthetic-unfinished-walk-skipped-legacy-payer")
		f.addDeadlineContracts(t.Context(), skipped, skippedMintedAt, 5, nil)
		strandedMintedAt := now.Add(-100 * time.Hour)
		strandedDeadline := strandedMintedAt.Add(model.DefaultContractExpiration)
		stranded := f.newPayer(t, t.Context(), "synthetic-unfinished-walk-stranded-payer")
		f.addDeadlineContracts(t.Context(), stranded, strandedMintedAt, 10, &strandedDeadline)
		deepMintedAt := now.Add(-contractDeadlineCloseLookback - 30*time.Minute)
		deepDeadline := deepMintedAt.Add(model.DefaultContractExpiration)
		deep := f.newPayer(t, t.Context(), "synthetic-unfinished-walk-deep-payer")
		f.addDeadlineContracts(t.Context(), deep, deepMintedAt, 3, &deepDeadline)
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)

		// The earlier walk stands between the skipped legacy contracts and
		// the stranded ones, with the lower bound of a first run an hour
		// older than this one.
		fresh := model.ContractIdTimeBound(now.Add(-2 * time.Hour))
		position := fmt.Sprintf(`{"fresh":"%s","backlog":{"lower":"%s","upper":"%s"}}`, fresh,
			model.ContractIdTimeBound(now.Add(-contractDeadlineCloseLookback-time.Hour)), model.ContractIdTimeBound(now.Add(-80*time.Hour)))
		var args CloseContractDeadlinesArgs
		server.Raise(json.Unmarshal([]byte(position), &args))

		result := runContractDeadlinePass(t, ctx, &args, now)
		requireScheduledCloseOwnerSettled(t, ctx, skipped, len(skipped.contractIds))
		requireContractsSettledAt(t, ctx, skipped.contractIds, skippedMintedAt.Add(model.DefaultContractExpiration))
		requireScheduledCloseOwnerSettled(t, ctx, stranded, len(stranded.contractIds))
		requireContractsSettledAt(t, ctx, stranded.contractIds, strandedDeadline)
		requireScheduledCloseOwnerSettled(t, ctx, deep, len(deep.contractIds))
		requireContractsSettledAt(t, ctx, deep.contractIds, deepDeadline)
		nextJson, err := json.Marshal(&result.Next)
		server.Raise(err)
		var next map[string]any
		server.Raise(json.Unmarshal(nextJson, &next))
		if next["version"] != float64(1) || next["backlog_start"] != fresh.String() || next["backlog"] != nil || result.Exhausted {
			t.Fatal("the walk from the new-deadline position did not finish with the version", string(nextJson))
		}
	})
}
