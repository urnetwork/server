// Contracts that carry a stored deadline have no queued close. They retire at
// that deadline through owner batches: as siblings of a queued close, through
// a refused or terminal claim, and through the continuous deadline pass.
package work

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
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
	ids := make([]server.Id, count)
	for index := range ids {
		ids[index] = contractIdMintedAt(mintedAt.Add(time.Duration(index) * time.Millisecond))
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
			contract_id,source_network_id,source_id,destination_network_id,destination_id,
			transfer_byte_count,payer_network_id,usage_origin_is_source,create_time,expiration_time)
			SELECT contract_id,$2,$3,$4,$5,$6,$2,true,$7,$8 FROM unnest($1::uuid[]) AS candidate(contract_id)`,
			ids, payer.networkId, payer.clientId, self.providerNetworkId, self.providerClientId, scheduledCloseOwnerEscrowBytes, mintedAt, deadline))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id,balance_id,balance_byte_count)
			SELECT contract_id,$2,$3 FROM unnest($1::uuid[]) AS candidate(contract_id)`, ids, payer.balanceId, scheduledCloseOwnerEscrowBytes))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close (contract_id,party,used_transfer_byte_count,close_time,checkpoint)
			SELECT contract_id,party,$2,$3,false FROM unnest($1::uuid[]) AS candidate(contract_id)
			CROSS JOIN (VALUES ('source'),('destination')) AS report(party)`, ids, scheduledCloseOwnerUsedBytes, mintedAt.Add(time.Minute)))
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
// its retained position, closes the due contracts it meets and ends at the
// first full page of legacy contracts, so older contracts below that page are
// never read.
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
		legacy := f.newPayer(t, t.Context(), "synthetic-deadline-backlog-legacy-payer")
		f.addDeadlineContracts(t.Context(), legacy, backlogMintedAt.Add(-time.Hour), 2*contractDeadlineClosePageSize, nil)
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

		// The empty fresh read, one backlog page and its one owner.
		third := runContractDeadlinePassWithin(t, ctx, &second.Next, newDeadline.Add(3*time.Minute), 3)
		requireScheduledCloseOwnerSettled(t, ctx, older, 3)
		requireContractsSettledAt(t, ctx, older.contractIds, backlogDeadline)
		if third.Next.Backlog == nil || third.Next.Backlog.Upper.Cmp(edge) >= 0 || third.Next.Backlog.Upper.Cmp(older.contractIds[0]) >= 0 {
			t.Fatalf("backlog did not retain its position below the page it closed: %+v", third)
		}

		fourth := runContractDeadlinePass(t, ctx, &third.Next, newDeadline.Add(4*time.Minute))
		if fourth.Next.Backlog != nil || fourth.Closed != 0 {
			t.Fatalf("backlog did not end at the full legacy page: %+v", fourth)
		}
		requireScheduledCloseOwnerSettled(t, ctx, ancient, 0)
		requireScheduledCloseOwnerSettled(t, ctx, legacy, 0)
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
