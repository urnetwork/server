// Scheduled closes that share one financial owner retire in two owned turns.
// The first admits the primary contract's owner without waiting, closes it and
// reads the owner's due siblings inside the same transaction; a held owner is
// refused before any read or write. The second admits the owner again with the
// siblings' publication keys and closes them together. Each member runs the
// ordinary locked deadline reconciliation. A failed sibling batch rolls back and
// each sibling is retried alone, so one bad contract cannot hold the rest.
package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/task"
)

// Bounds the contracts closed per owner turn pair and the balances of one owner.
const ContractDeadlineOwnerBatchLimit = 64

// Unsettled reservations visited per owner balance when looking for siblings.
const contractDeadlineOwnerCandidateScanLimit = 4 * ContractDeadlineOwnerBatchLimit

// A due scheduled retirement and its authoritative deadline.
type ContractDeadline struct {
	ContractId server.Id
	Deadline   time.Time
}

// An unlocked committed read taken before admission. Missing and terminal are
// final; an open contract is rechecked under its locks before any write.
type ContractDeadlineCustody struct {
	Found      bool
	Outcome    *ContractOutcome
	BalanceIds []server.Id
}

// Selects the due candidates and their authoritative deadlines. It runs inside
// the owned transaction and must only read through the supplied query.
type ContractDeadlineDueFunc func(ctx context.Context, query server.PgCanQuery, contractIds []server.Id) []ContractDeadline

// Busy means the owner was held and nothing was read or written.
type ContractOwnerDeadlineResult struct {
	Reconciliation *ContractDeadlineReconciliation
	Busy           bool
	SiblingsClosed int
}

// Lateral probes keep each contract's estimate exact under the escrow table's
// retained statistics; the bound matches the deadline escrow read.
var contractDeadlineCustodySql = fmt.Sprintf(`SELECT requested.contract_id,contract.contract_id IS NOT NULL,contract.outcome,escrow.balance_id
	FROM unnest($1::uuid[]) AS requested(contract_id)
	LEFT JOIN LATERAL (SELECT contract_id,outcome FROM transfer_contract
		WHERE contract_id=requested.contract_id OFFSET 0) AS contract ON true
	LEFT JOIN LATERAL (SELECT balance_id FROM transfer_escrow
		WHERE contract_id=requested.contract_id ORDER BY balance_id LIMIT %d) AS escrow
		ON contract.contract_id IS NOT NULL AND contract.outcome IS NULL`, deadlineEscrowRowLimit+1)

// Visits the oldest unsettled reservations of each owner balance through the
// partial balance index. Each open candidate also returns its complete balance
// set, so only contracts the owner's keys fully cover are kept.
var contractDeadlineOwnerCandidatesSql = fmt.Sprintf(`SELECT candidate.contract_id,escrow.balance_id
	FROM unnest($1::uuid[]) AS owner(balance_id)
	CROSS JOIN LATERAL (SELECT contract_id FROM transfer_escrow
		WHERE balance_id=owner.balance_id AND settled=false AND contract_id<>$2
		ORDER BY contract_id LIMIT %d) AS candidate
	CROSS JOIN LATERAL (SELECT contract_id FROM transfer_contract
		WHERE contract_id=candidate.contract_id AND outcome IS NULL OFFSET 0) AS open_contract
	CROSS JOIN LATERAL (SELECT balance_id FROM transfer_escrow
		WHERE contract_id=candidate.contract_id ORDER BY balance_id LIMIT %d) AS escrow`,
	contractDeadlineOwnerCandidateScanLimit, ContractDeadlineOwnerBatchLimit+1)

// Reads the committed state and escrow balances of each requested contract
// without a transaction or lock. Absent ids are returned as not found.
func ReadContractDeadlineCustody(ctx context.Context, contractIds []server.Id) (custody map[server.Id]*ContractDeadlineCustody, returnErr error) {
	custody = make(map[server.Id]*ContractDeadlineCustody, len(contractIds))
	if len(contractIds) == 0 {
		return
	}
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, contractDeadlineCustodySql, contractIds)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var contractId server.Id
					var found bool
					var outcome *ContractOutcome
					var balanceId *server.Id
					server.Raise(rows.Scan(&contractId, &found, &outcome, &balanceId))
					state := custody[contractId]
					if state == nil {
						state = &ContractDeadlineCustody{Found: found, Outcome: outcome}
						custody[contractId] = state
					}
					if balanceId != nil {
						state.BalanceIds = append(state.BalanceIds, *balanceId)
					}
				}
			})
		}, server.OptNoRetry())
	}, func(err error) { custody, returnErr = nil, err })
	for _, state := range custody {
		slices.SortFunc(state.BalanceIds, server.Id.Cmp)
		state.BalanceIds = slices.Compact(state.BalanceIds)
	}
	return
}

// Closes the due primary contract under its owner balances without waiting
// for admission. While that owner is admitted, due reads the owner's queued
// siblings; a second owned turn then closes up to one batch of them. A held
// owner is busy for the primary; for the siblings it only ends the batch.
func ReconcileContractOwnerAtDeadline(ctx context.Context, balanceIds []server.Id, primary ContractDeadline, due ContractDeadlineDueFunc) (*ContractOwnerDeadlineResult, error) {
	if err := validateContractDeadlineOwner(balanceIds, []ContractDeadline{primary}); err != nil {
		return nil, err
	}
	discover := func(tx server.PgTx) []ContractDeadline {
		if due == nil || ContractDeadlineOwnerBatchLimit < len(balanceIds) {
			return nil
		}
		return readContractDeadlineOwnerSiblingsInTx(ctx, tx, balanceIds, primary.ContractId, due)
	}
	// Both turns share the coalesced projections, refreshed once at the end.
	postCtx, finishLegacyPosts := withLegacySettlementPostBatch(ctx)
	refresh := &deadlineNetEscrowRefreshBatch{balanceIdSet: map[server.Id]bool{}}
	postCtx = context.WithValue(postCtx, deadlineNetEscrowRefreshBatchKey{}, refresh)
	defer func() {
		refresh.finish(ctx)
		finishLegacyPosts()
	}()
	results, siblings, busy, _, err := reconcileContractOwnerBatchAtDeadline(ctx, postCtx, balanceIds, []ContractDeadline{primary}, discover)
	if err != nil {
		return nil, err
	}
	if busy {
		return &ContractOwnerDeadlineResult{Busy: true}, nil
	}
	result := &ContractOwnerDeadlineResult{Reconciliation: results[primary.ContractId]}
	if len(siblings) == 0 {
		return result, nil
	}
	newlyClosed := func(closed *ContractDeadlineReconciliation) bool {
		return closed != nil && !closed.AlreadyClosed && !closed.Missing
	}
	results, _, busy, entered, err := reconcileContractOwnerBatchAtDeadline(ctx, postCtx, balanceIds, siblings, nil)
	switch {
	case err == nil && !busy:
		for _, sibling := range siblings {
			if newlyClosed(results[sibling.ContractId]) {
				result.SiblingsClosed++
			}
		}
	case err != nil && entered:
		// A failed shared transaction retries each sibling in its own turn.
		for _, sibling := range siblings {
			results, _, busy, _, err := reconcileContractOwnerBatchAtDeadline(ctx, postCtx, balanceIds, []ContractDeadline{sibling}, nil)
			if busy {
				break
			}
			if err == nil && newlyClosed(results[sibling.ContractId]) {
				result.SiblingsClosed++
			}
		}
	}
	// A held owner or a failed admission leaves siblings to their own closes.
	return result, nil
}

// Rejects empty, oversized, duplicate or zero identities before any admission.
func validateContractDeadlineOwner(balanceIds []server.Id, members []ContractDeadline) error {
	if len(balanceIds) == 0 || deadlineEscrowRowLimit < len(balanceIds) || len(members) == 0 || ContractDeadlineOwnerBatchLimit < len(members) {
		return errors.New("invalid scheduled contract owner batch")
	}
	seen := make(map[server.Id]bool, len(balanceIds)+len(members))
	for _, balanceId := range balanceIds {
		if balanceId == (server.Id{}) || seen[balanceId] {
			return errors.New("invalid scheduled contract owner balance")
		}
		seen[balanceId] = true
	}
	clear(seen)
	for _, member := range members {
		if member.ContractId == (server.Id{}) || member.Deadline.IsZero() || seen[member.ContractId] {
			return errors.New("invalid scheduled contract close")
		}
		seen[member.ContractId] = true
	}
	return nil
}

// Open candidates on the owner balances that the owner's keys fully cover,
// filtered by their own due authority and bounded to one sibling batch.
func readContractDeadlineOwnerSiblingsInTx(ctx context.Context, tx server.PgTx, balanceIds []server.Id, primaryId server.Id, due ContractDeadlineDueFunc) []ContractDeadline {
	owned := make(map[server.Id]bool, len(balanceIds))
	for _, balanceId := range balanceIds {
		owned[balanceId] = true
	}
	candidateBalanceIds := map[server.Id][]server.Id{}
	rows, err := tx.Query(ctx, contractDeadlineOwnerCandidatesSql, balanceIds, primaryId)
	server.WithPgResult(rows, err, func() {
		for rows.Next() {
			var candidateId, balanceId server.Id
			server.Raise(rows.Scan(&candidateId, &balanceId))
			if !slices.Contains(candidateBalanceIds[candidateId], balanceId) {
				candidateBalanceIds[candidateId] = append(candidateBalanceIds[candidateId], balanceId)
			}
		}
	})
	candidateIds := []server.Id{}
	for candidateId, candidateBalances := range candidateBalanceIds {
		covered := len(candidateBalances) <= ContractDeadlineOwnerBatchLimit
		for _, balanceId := range candidateBalances {
			covered = covered && owned[balanceId]
		}
		if covered {
			candidateIds = append(candidateIds, candidateId)
		}
	}
	if len(candidateIds) == 0 {
		return nil
	}
	slices.SortFunc(candidateIds, server.Id.Cmp)
	candidates := make(map[server.Id]bool, len(candidateIds))
	for _, candidateId := range candidateIds {
		candidates[candidateId] = true
	}
	siblings := []ContractDeadline{}
	for _, sibling := range due(ctx, tx, candidateIds) {
		if candidates[sibling.ContractId] && !sibling.Deadline.IsZero() {
			siblings = append(siblings, sibling)
			delete(candidates, sibling.ContractId)
		}
	}
	slices.SortFunc(siblings, func(a, b ContractDeadline) int { return a.ContractId.Cmp(b.ContractId) })
	return siblings[:min(len(siblings), ContractDeadlineOwnerBatchLimit-1)]
}

// One owned transaction for the complete member set. Entered reports whether
// the business callback began, which separates an admission or route failure
// from a failed member. Discovery reads precede every row lock. Member posts
// run only after an acknowledged commit; postCtx carries their coalescers.
func reconcileContractOwnerBatchAtDeadline(ctx context.Context, postCtx context.Context, balanceIds []server.Id, members []ContractDeadline, discover func(server.PgTx) []ContractDeadline) (results map[server.Id]*ContractDeadlineReconciliation, discovered []ContractDeadline, busy bool, entered bool, returnErr error) {
	keys := transferBalanceOwnershipKeys(balanceIds)
	contractIds := make([]server.Id, 0, len(members))
	for _, member := range members {
		keys = append(keys, task.RunOnceOwnershipKey(task.RunOnce("legacy_provider_totals", member.ContractId)))
		contractIds = append(contractIds, member.ContractId)
	}
	var posts []server.PostFunction
	server.HandleError(func() {
		admitted := server.TryOwnedTx(ctx, keys, func(tx server.PgTx) {
			entered = true
			results, discovered, posts = map[server.Id]*ContractDeadlineReconciliation{}, nil, nil
			if discover != nil {
				discovered = discover(tx)
			}
			found := map[server.Id]bool{}
			outcomes := map[server.Id]*ContractOutcome{}
			// Committed terminal state is final, so these members need no lock.
			rows, err := tx.Query(ctx, `SELECT contract_id,outcome FROM transfer_contract WHERE contract_id=ANY($1)`, contractIds)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var contractId server.Id
					var outcome *ContractOutcome
					server.Raise(rows.Scan(&contractId, &outcome))
					found[contractId], outcomes[contractId] = true, outcome
				}
			})
			for _, member := range members {
				contractId := member.ContractId
				switch {
				case !found[contractId]:
					results[contractId] = &ContractDeadlineReconciliation{ContractId: contractId, Missing: true}
				case outcomes[contractId] != nil:
					results[contractId] = &ContractDeadlineReconciliation{ContractId: contractId, Outcome: *outcomes[contractId], AlreadyClosed: true}
				default:
					result, memberPosts := reconcileContractAtDeadlineWithOwnershipInTx(postCtx, tx, contractId, member.Deadline, true)
					results[contractId] = result
					posts = append(posts, memberPosts...)
				}
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if !admitted {
			results, busy = nil, true
			return
		}
		for offset := 0; offset < len(posts); offset += 8 {
			server.RunPosts(ctx, posts[offset:min(offset+8, len(posts))]...)
		}
	}, func(err error) { results, discovered, returnErr = nil, nil, err })
	return
}

// Every member of one owner batch refreshes the same balance mirrors. Collect
// them during the posts and refresh each balance once from committed state.
type deadlineNetEscrowRefreshBatch struct {
	stateLock    sync.Mutex
	balanceIdSet map[server.Id]bool
}

type deadlineNetEscrowRefreshBatchKey struct{}

// Records one member's mirrors; concurrent posts share the batch.
func (self *deadlineNetEscrowRefreshBatch) add(balanceIds []server.Id) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, balanceId := range balanceIds {
		self.balanceIdSet[balanceId] = true
	}
}

// Refreshes every recorded balance once from committed state.
func (self *deadlineNetEscrowRefreshBatch) finish(ctx context.Context) {
	var balanceIds []server.Id
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for balanceId := range self.balanceIdSet {
			balanceIds = append(balanceIds, balanceId)
		}
		clear(self.balanceIdSet)
	}()
	if len(balanceIds) == 0 {
		return
	}
	slices.SortFunc(balanceIds, server.Id.Cmp)
	server.RunPosts(ctx, func() any { refreshNetEscrow(ctx, balanceIds); return nil })
}

// A single deadline close keeps its direct refresh; a batch member defers it
// to the batch's one refresh after the shared commit.
func deadlineNetEscrowRefreshPost(ctx context.Context, balanceIds []server.Id) server.PostFunction {
	if batch, _ := ctx.Value(deadlineNetEscrowRefreshBatchKey{}).(*deadlineNetEscrowRefreshBatch); batch != nil {
		return func() any { batch.add(balanceIds); return nil }
	}
	return func() any { refreshNetEscrow(ctx, balanceIds); return nil }
}
