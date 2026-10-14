// Due rows of one expiry page that share one transfer balance close together
// through the owner batch: one owned turn per batch instead of one transaction
// per row on the balance's try-lock. Heavy payers keep their contracts on one
// balance, so their rows held that balance one close at a time, about 30 ms
// each, and one payer could not retire much more than 33 rows a second.
package model

import (
	"context"
	"slices"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

// Owner batches admit on the process's direct maintenance connections, which
// task claims, task completions and the other closers share. Main allows 32 per
// taskworker and showed at most 13 in use in an hour on 2026-10-13. A page runs
// at most this many batches at once; contract_close.yml can lower it or turn
// batching off (taskworker/work/close_expired_parallel.go).
const ForceCloseOwnerBatchParallel = 4

type forceCloseOwnerBatchParallelKey struct{}

// A sweep page under the returned context runs at most parallel owner batches
// at once. Zero turns batching off: every row is visited alone, as before
// owner batches. A value outside [0, ForceCloseOwnerBatchParallel], or none,
// keeps ForceCloseOwnerBatchParallel.
func WithForceCloseOwnerBatchParallel(ctx context.Context, parallel int) context.Context {
	return context.WithValue(ctx, forceCloseOwnerBatchParallelKey{}, parallel)
}

// The owner batch limit a page under ctx runs with, in [0, ForceCloseOwnerBatchParallel].
func forceCloseOwnerBatchParallel(ctx context.Context) int {
	if parallel, ok := ctx.Value(forceCloseOwnerBatchParallelKey{}).(int); ok && 0 <= parallel && parallel <= ForceCloseOwnerBatchParallel {
		return parallel
	}
	return ForceCloseOwnerBatchParallel
}

// A private fixture seam observes a batch that must wait for a slot. It is
// called with the batch's payer turn held and no transaction or slot.
type forceCloseOwnerBatchSlotWaitKey struct{}

func forceCloseOwnerBatchSlotWait(ctx context.Context) {
	if observe, ok := ctx.Value(forceCloseOwnerBatchSlotWaitKey{}).(func()); ok {
		observe()
	}
}

// Open rows with a stored deadline and no pending legacy settlement intent,
// with up to two of their balances, enough to tell one balance from several.
// A close takes its row's intent with NOWAIT, so an intent another worker
// holds would fail every member of a batch; alone it fails only its row. The
// lateral probes keep each contract's estimate exact, as the deadline custody
// read does.
const forceCloseDeadlineOwnerSql = `SELECT contract.contract_id,contract.expiration_time,escrow.balance_id
	FROM unnest($1::uuid[]) AS requested(contract_id)
	CROSS JOIN LATERAL (SELECT contract_id,expiration_time FROM transfer_contract
		WHERE contract_id=requested.contract_id AND outcome IS NULL AND expiration_time IS NOT NULL OFFSET 0) AS contract
	CROSS JOIN LATERAL (SELECT balance_id FROM transfer_escrow
		WHERE contract_id=contract.contract_id ORDER BY balance_id LIMIT 2) AS escrow
	WHERE NOT EXISTS(SELECT 1 FROM legacy_settlement_intent pending WHERE pending.contract_id=contract.contract_id)`

// Rows of one balance in page order. The first row leads the batch; members
// carry the stored deadlines in contract order, the order their rows lock.
type forceCloseOwnerBatch struct {
	balanceId server.Id
	indexes   []int
	members   []ContractDeadline
}

// How one owner batch ended, for the page summary.
type forceCloseOwnerBatchOutcome int

const (
	// Never started, or stopped with its page.
	forceCloseOwnerBatchUnrun forceCloseOwnerBatchOutcome = iota
	// Its owned turn committed every member.
	forceCloseOwnerBatchClosed
	// Left with its rows to the holder of its balance.
	forceCloseOwnerBatchHeld
	// Its turn failed; its rows closed alone or stayed open.
	forceCloseOwnerBatchFailed
)

// Counts the batches led from the started prefix by how they ended. Outcomes
// are indexed by each batch's first row.
func forceCloseOwnerBatchSummary(rowBatches []*forceCloseOwnerBatch, batchOutcomes []forceCloseOwnerBatchOutcome, started int) (closedCount int, heldCount int, failedCount int) {
	for index, batch := range rowBatches[:started] {
		if batch == nil || batch.indexes[0] != index {
			continue
		}
		switch batchOutcomes[index] {
		case forceCloseOwnerBatchClosed:
			closedCount += 1
		case forceCloseOwnerBatchHeld:
			heldCount += 1
		case forceCloseOwnerBatchFailed:
			failedCount += 1
		}
	}
	return
}

// Groups the page's open rows whose stored deadline passed at now by their one
// transfer balance, in page order, into batches of at most
// ContractDeadlineOwnerBatchLimit rows, and maps each batched row to its batch.
// A row with no stored deadline, a pending legacy settlement intent, no
// balance or several, or alone in its batch keeps its own visit. Rows of one balance share their payer's
// turn, so only rows that share a turn are read, and a page without any reads
// nothing. A deadline is immutable while its contract is open, so the unlocked
// read is the deadline each batch closes at. The read is only an optimization:
// if it fails, every row keeps its own visit.
func forceCloseDeadlineOwnerBatches(ctx context.Context, openContracts []*contractExpiryState, rowTurns []chan struct{}, now time.Time) (rowBatches []*forceCloseOwnerBatch) {
	rowBatches = make([]*forceCloseOwnerBatch, len(openContracts))
	turnRowCounts := map[chan struct{}]int{}
	for _, turn := range rowTurns {
		if turn != nil {
			turnRowCounts[turn] += 1
		}
	}
	contractIds := []server.Id{}
	for index, turn := range rowTurns {
		if turn != nil && 2 <= turnRowCounts[turn] {
			contractIds = append(contractIds, openContracts[index].contractId)
		}
	}
	if len(contractIds) < 2 {
		return
	}
	contractIdDeadlines := map[server.Id]time.Time{}
	contractIdBalanceIds := map[server.Id][]server.Id{}
	readOk := false
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, forceCloseDeadlineOwnerSql, contractIds)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var contractId, balanceId server.Id
					var deadline time.Time
					server.Raise(rows.Scan(&contractId, &deadline, &balanceId))
					contractIdDeadlines[contractId] = deadline
					contractIdBalanceIds[contractId] = append(contractIdBalanceIds[contractId], balanceId)
				}
			})
		}, server.OptNoRetry())
		readOk = true
	}, func(err error) {
		glog.Infof("[close-expired]owner batches unavailable, rows close alone: %v\n", err)
	})
	if !readOk {
		return
	}
	balanceIdIndexes := map[server.Id][]int{}
	balanceIds := []server.Id{}
	for index, openContract := range openContracts {
		deadline, ok := contractIdDeadlines[openContract.contractId]
		rowBalanceIds := contractIdBalanceIds[openContract.contractId]
		// No financial owner admits the zero id; such a row fails alone.
		if !ok || now.Before(deadline) || len(rowBalanceIds) != 1 || rowBalanceIds[0] == (server.Id{}) {
			continue
		}
		balanceId := rowBalanceIds[0]
		if _, ok := balanceIdIndexes[balanceId]; !ok {
			balanceIds = append(balanceIds, balanceId)
		}
		balanceIdIndexes[balanceId] = append(balanceIdIndexes[balanceId], index)
	}
	for _, balanceId := range balanceIds {
		indexes := balanceIdIndexes[balanceId]
		for offset := 0; offset < len(indexes); offset += ContractDeadlineOwnerBatchLimit {
			batchIndexes := indexes[offset:min(offset+ContractDeadlineOwnerBatchLimit, len(indexes))]
			if len(batchIndexes) < 2 {
				continue
			}
			batch := &forceCloseOwnerBatch{balanceId: balanceId, indexes: batchIndexes}
			for _, index := range batchIndexes {
				contractId := openContracts[index].contractId
				batch.members = append(batch.members, ContractDeadline{ContractId: contractId, Deadline: contractIdDeadlines[contractId]})
				rowBatches[index] = batch
			}
			slices.SortFunc(batch.members, func(a, b ContractDeadline) int { return a.ContractId.Cmp(b.ContractId) })
		}
	}
	return
}
