// An expiry page reaches contracts past their stored deadline while another
// closer often holds their payer's grant: the deadline pass, a steal or a
// scheduled close's owner batch, which retires that payer's due contracts,
// these usually among them. A page row used to lock its contract, read its
// settlement rows, intent, participants and grants, and only then try the
// grant, refusing with an error report and a stack. On Main one payer's burst
// drew about eight refusals a second and failed every sweep page.
//
// A row now reads its deadline and grants without a lock and tries the grant
// before anything else. A held grant leaves the row, and that payer's later
// rows on the same page, to the holder or a later pass: a counted, quiet skip
// that completes the row's visit, not a failure.
package model

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// The stored deadline and reservation grants of an open contract, read without
// a transaction or lock. Found is false for a closed or missing contract.
func readForceCloseDeadlineOwner(ctx context.Context, contractId server.Id) (found bool, expiration *time.Time, balanceIds []server.Id) {
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT contract.expiration_time, escrow.balance_id
			FROM (SELECT contract_id, expiration_time FROM transfer_contract
				WHERE contract_id=$1 AND outcome IS NULL OFFSET 0) AS contract
			LEFT JOIN LATERAL (SELECT balance_id FROM transfer_escrow
				WHERE contract_id=contract.contract_id ORDER BY balance_id LIMIT $2) AS escrow ON true`,
			contractId, deadlineEscrowRowLimit+1)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var balanceId *server.Id
				server.Raise(rows.Scan(&expiration, &balanceId))
				found = true
				if balanceId != nil {
					balanceIds = append(balanceIds, *balanceId)
				}
			}
		})
	}, server.OptNoRetry())
	return
}

// Reconciles the contract at its deadline under the grants read before,
// admitted with one try before any lock. Busy means another writer holds a
// grant and nothing was locked, read under lock or written.
func tryReconcileContractAtDeadline(ctx context.Context, contractId server.Id, deadline time.Time, balanceIds []server.Id) (result *ContractDeadlineReconciliation, busy bool, returnErr error) {
	if contractId == (server.Id{}) || deadline.IsZero() {
		return nil, false, fmt.Errorf("invalid scheduled contract close")
	}
	var posts []server.PostFunction
	server.HandleError(func() {
		keys := deadlineFinancialOwnershipKeys(contractId, balanceIds)
		body := func(tx server.PgTx) {
			result, posts = reconcileContractAtDeadlineWithOwnershipInTx(ctx, tx, contractId, deadline, true)
		}
		if len(keys) == 0 {
			server.Tx(ctx, body, server.TxReadCommitted, server.OptNoRetry())
		} else if !server.TryOwnedTx(ctx, keys, body, server.TxReadCommitted, server.OptNoRetry()) {
			busy = true
			return
		}
		server.RunPosts(ctx, posts...)
	}, func(err error) { result, busy, returnErr = nil, false, err })
	return
}

// A page's payer grant sets found held. Safe for concurrent use.
type forceCloseBusyOwners struct {
	stateLock sync.Mutex
	owners    map[string]bool
}

// No payer is known held at a page's start.
func newForceCloseBusyOwners() *forceCloseBusyOwners {
	return &forceCloseBusyOwners{owners: map[string]bool{}}
}

// The grant set's identity, from ids in balance order.
func forceCloseOwnerKey(balanceIds []server.Id) string {
	parts := make([]string, 0, len(balanceIds))
	for _, balanceId := range balanceIds {
		parts = append(parts, balanceId.String())
	}
	return strings.Join(parts, ",")
}

// Whether a row of this grant set already found it held.
func (self *forceCloseBusyOwners) held(owner string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.owners[owner]
}

// Records a grant set found held.
func (self *forceCloseBusyOwners) add(owner string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.owners[owner] = true
}

// A row past its stored deadline, closed under its grants or left to their
// holder. Handled is false for any other row, which takes the page's ordinary
// locked path; the ordinary path also covers a deadline that passes between
// this read and that one.
func forceCloseDueRowByOwner(ctx context.Context, contractId server.Id, busyOwners *forceCloseBusyOwners) (handled bool, reconciled *ContractDeadlineReconciliation, ownerBusy bool, returnErr error) {
	found, expiration, balanceIds := readForceCloseDeadlineOwner(ctx, contractId)
	if !found || expiration == nil || server.NowUtc().Before(*expiration) {
		return false, nil, false, nil
	}
	owner := forceCloseOwnerKey(balanceIds)
	if 0 < len(balanceIds) && busyOwners.held(owner) {
		return true, nil, true, nil
	}
	reconciled, busy, err := tryReconcileContractAtDeadline(ctx, contractId, *expiration, balanceIds)
	if err != nil {
		return true, nil, false, err
	}
	if busy {
		busyOwners.add(owner)
		return true, nil, true, nil
	}
	return true, reconciled, false, nil
}
