// The ordinary expiry sweep and explicit-ID repair share the same report continuation.
package model

import (
	"context"
	"errors"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

// Canonical close-report receipts keep their existing owner. Ordinary expiry
// follows CloseContract unchanged; the scoped adapter supplies no report ID and
// adds its expected-payer/report-state fence around that same report owner.
func closeContractWithExpiryScope(ctx context.Context, scope *contractExpiryRepairScope,
	contractId, clientId server.Id, usedTransferByteCount ByteCount, checkpoint bool,
) (returnErr error) {
	if scope == nil {
		return CloseContract(ctx, contractId, clientId, usedTransferByteCount, checkpoint)
	}
	if usedTransferByteCount < 0 {
		return errors.New("invalid used transfer byte count")
	}
	var terminalReplay bool
	contractExpiryContinuationTx(ctx, contractId, scope, func(tx server.PgTx) {
		_, terminalReplay, returnErr = applyContractCloseReportInTx(ctx, tx, contractId, clientId, usedTransferByteCount, checkpoint, nil)
		server.Raise(returnErr)
	})
	if terminalReplay {
		return nil
	}
	closed, err := settleContractWithExpiryScope(ctx, contractId, scope)
	if err != nil {
		return err
	}
	if closed && scope.redis == nil {
		RemoveFromStream(ctx, contractId)
	}
	return nil
}

// Claim only a current dispute. Failed settlement must roll back its clear,
// leaving the reservation disputed rather than eligible for quarantine.
func settleExpiredContractDispute(ctx context.Context, tag string, contractId server.Id, scope *contractExpiryRepairScope) {
	var posts []func() any
	resolved := false
	contractExpiryContinuationTx(ctx, contractId, scope, func(tx server.PgTx) {
		posts = nil
		resolved = false
		// Keep the dispute and reservation intact while legacy work is
		// queued. The worker clears it only in the debit/outcome transaction;
		// an accounting rejection rolls that clear back with everything else.
		var owned bool
		var expiryOwned bool
		var retained []byte
		rows, queryErr := tx.Query(ctx, `SELECT usage_unverified,provider_usage FROM transfer_contract WHERE contract_id=$1 AND dispute AND outcome IS NULL FOR UPDATE`, contractId)
		server.WithPgResult(rows, queryErr, func() {
			owned = rows.Next()
			if owned {
				server.Raise(rows.Scan(&expiryOwned, &retained))
			}
		})
		if !owned {
			return
		}
		if !expiryOwned {
			panic(errors.New("disputed expiry lacks retained ownership"))
		}
		_, proofErr := retainedContractExpiryUsage(retained)
		server.Raise(proofErr)
		// Ordinary CloseContract refuses a disputed row. This expiry owner may
		// finalize its existing checkpoints after their original proof commits,
		// retaining every byte count and the immutable proof. A failed financial
		// transaction rolls these flags back with its dispute clear; legacy work
		// commits them with its intent and keeps the dispute until settlement.
		server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET checkpoint=false WHERE contract_id=$1 AND checkpoint`, contractId))
		var legacy bool
		server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1 AND NOT redis_reserved)`, contractId).Scan(&legacy))
		if legacy {
			server.Raise(queueLegacySettlementInTx(ctx, tx, contractId, ContractOutcomeSettled, true))
			return
		}
		changed := server.RaisePgResult(tx.Exec(
			ctx,
			`
                    UPDATE transfer_contract
                    SET dispute = false, close_time = $2
                    WHERE contract_id = $1 AND dispute AND outcome IS NULL
                `,
			contractId,
			server.NowUtc(),
		))
		if changed.RowsAffected() == 0 {
			return
		}
		var err error
		posts, resolved, err = settleEscrowInTx(ctx, tx, contractId, ContractOutcomeSettled)
		server.Raise(err)
		if !resolved {
			panic(errors.New("contract remained non-final after force-close attempt"))
		}
	})
	if resolved {
		forceCloseContractCounter.WithLabelValues("dispute_both_sides").Inc()
		if glog.V(1) {
			glog.Infof("%ssettle contract dispute: both sides\n", tag)
		}
	}
	server.RunPosts(ctx, posts...)
}

func continueContractExpiry(ctx context.Context, tag string, openContract *contractExpiryState, scope *contractExpiryRepairScope) error {
	// Force close may synthesize a missing endpoint close. Its billing
	// outcome must not be mistaken for verified bilateral subnet usage.
	// Both preparation callers commit before continuing. Reuse that sticky
	// flag; states without the write-preparation witness keep the fallback.
	if !openContract.usageUnverifiedRetained {
		contractExpiryContinuationTx(ctx, openContract.contractId, scope, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_unverified=true WHERE contract_id=$1 AND outcome IS NULL`, openContract.contractId))
		})
	}
	if openContract.dispute {
		settleExpiredContractDispute(ctx, tag, openContract.contractId, scope)
		return nil
	}

	if openContract.sourceCloseTime == nil && openContract.destinationCloseTime == nil {
		// close with both sides 0
		recordForceCloseContract("both sides", tag)

		err := closeContractWithExpiryScope(
			ctx, scope,
			openContract.contractId,
			openContract.sourceId,
			ByteCount(0),
			false,
		)
		if err != nil {
			return err
		}

		err = closeContractWithExpiryScope(
			ctx, scope,
			openContract.contractId,
			openContract.destinationId,
			ByteCount(0),
			false,
		)
		if err != nil {
			return err
		}

	} else if openContract.sourceCloseTime == nil {
		// Source accepts destination. A lone destination checkpoint must
		// also be made final; adding the missing source close alone leaves
		// one checkpoint row and therefore cannot settle the contract.
		recordForceCloseContract("source accepts destination", tag)

		err := closeContractWithExpiryScope(
			ctx, scope,
			openContract.contractId,
			openContract.sourceId,
			*openContract.destinationUsedTransferByteCount,
			false,
		)
		if err != nil {
			return err
		}
		if *openContract.destinationCheckpoint {
			err = closeContractWithExpiryScope(
				ctx, scope,
				openContract.contractId,
				openContract.destinationId,
				ByteCount(0),
				false,
			)
			if err != nil {
				return err
			}
		}

	} else if openContract.destinationCloseTime == nil {
		// Destination accepts source. Mirror the checkpoint finalization
		// above so either one-sided orientation converges in one sweep.
		recordForceCloseContract("destination accepts source", tag)

		err := closeContractWithExpiryScope(
			ctx, scope,
			openContract.contractId,
			openContract.destinationId,
			*openContract.sourceUsedTransferByteCount,
			false,
		)
		if err != nil {
			return err
		}
		if *openContract.sourceCheckpoint {
			err = closeContractWithExpiryScope(
				ctx, scope,
				openContract.contractId,
				openContract.sourceId,
				ByteCount(0),
				false,
			)
			if err != nil {
				return err
			}
		}

	} else if *openContract.sourceCheckpoint || *openContract.destinationCheckpoint {
		// finalize one or more checkpoints

		if *openContract.sourceCheckpoint {
			recordForceCloseContract("finalize source checkpoint", tag)

			err := closeContractWithExpiryScope(
				ctx, scope,
				openContract.contractId,
				openContract.sourceId,
				ByteCount(0),
				false,
			)
			if err != nil {
				return err
			}
		}

		if *openContract.destinationCheckpoint {
			recordForceCloseContract("finalize destination checkpoint", tag)
			err := closeContractWithExpiryScope(
				ctx, scope,
				openContract.contractId,
				openContract.destinationId,
				ByteCount(0),
				false,
			)
			if err != nil {
				return err
			}
		}

	} else {
		// nothing to settle, just close the transaction
		var posts []func() any
		var err error
		contractExpiryContinuationTx(ctx, openContract.contractId, scope, func(tx server.PgTx) {
			posts, _, err = settleEscrowForegroundWithExpiryScopeInTx(ctx, tx, openContract.contractId, ContractOutcomeSettled, scope)
			if scope != nil {
				server.Raise(err)
			}
		})
		if err != nil {
			return err
		}
		server.RunPosts(ctx, posts...)
	}

	return nil
}
