// A scheduled deadline enters the ordinary close owners by exact contract key.
package model

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// The caller's durable task owns the deadline. Retain the original reports
// before any billing continuation; existing intents keep their accepted outcome
// and return their actual serial owner for durable task publication.
func CloseContractAtDeadline(ctx context.Context, contractId server.Id, deadline time.Time) (owner *ContractCloseOwner, returnErr error) {
	if contractId == (server.Id{}) || deadline.IsZero() {
		return nil, fmt.Errorf("invalid scheduled contract close")
	}
	server.HandleError(func() {
		var fresh *contractExpiryState
		var pending bool
		server.Tx(ctx, func(tx server.PgTx) {
			fresh, owner, pending = nil, nil, false
			var outcome *ContractOutcome
			var sourceId, destinationId server.Id
			err := tx.QueryRow(ctx, `SELECT outcome,source_id,destination_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, contractId).Scan(&outcome, &sourceId, &destinationId)
			if err == pgx.ErrNoRows || (err == nil && outcome != nil) {
				return
			}
			server.Raise(err)
			// Capture after the contract lock: a report committed while this
			// task waited cannot postpone an already-due startup deadline.
			now := server.NowUtc()
			if now.Before(deadline) {
				server.Raise(fmt.Errorf("scheduled contract close is not due"))
			}
			// Persist only an earlier retirement deadline. Existing intent workers
			// can then enforce it under I/C without changing accepted authority here.
			capped := server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2
				WHERE contract_id=$1 AND outcome IS NULL AND (expiration_time IS NULL OR expiration_time>$2)`, contractId, deadline))
			if capped.RowsAffected() == 1 {
				contractHoleEventInTx(ctx, tx, contractId, sourceId, destinationId, "remove")
			}
			server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)`, contractId).Scan(&pending))
			if pending {
				actual, _, err := readContractCloseOwnerInConn(ctx, tx, contractId)
				server.Raise(err)
				owner = &actual
				return
			}
			fresh, err = prepareContractExpiryInTx(ctx, tx, contractId, now)
			server.Raise(err)
			if fresh == nil {
				server.Raise(fmt.Errorf("scheduled contract close did not acquire expiry ownership"))
			}
		}, server.TxReadCommitted, server.OptNoRetry())
		if fresh == nil || pending {
			return
		}
		tag := fmt.Sprintf("[scheduled-close][%s]", contractId)
		server.Raise(continueContractExpiry(ctx, tag, fresh, nil))
		// Ordinary legacy close can create an intent. A report race can also
		// create a dispute, which resumes through the same ordinary owner.
		for attempt := 0; attempt < 2; attempt++ {
			var found, terminal, disputed bool
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT outcome IS NOT NULL,dispute,
					EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
					FROM transfer_contract WHERE contract_id=$1`, contractId)
				server.WithPgResult(rows, err, func() {
					if rows.Next() {
						found = true
						server.Raise(rows.Scan(&terminal, &disputed, &pending))
					}
				})
				if pending && !terminal {
					actual, _, err := readContractCloseOwnerInConn(ctx, conn, contractId)
					server.Raise(err)
					owner = &actual
				}
			})
			if !found || terminal {
				RemoveFromStream(ctx, contractId)
				return
			}
			if pending {
				return
			}
			if attempt == 0 && disputed {
				settleExpiredContractDispute(ctx, tag, contractId, nil)
				continue
			}
			server.Raise(fmt.Errorf("scheduled contract close remains nonterminal without a settlement owner"))
		}
	}, func(err error) { returnErr = err })
	return
}
