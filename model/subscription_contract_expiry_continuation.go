// The ordinary expiry sweep and explicit-ID repair share the same report continuation.
package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
)

// A missing peer accepts the existing count for billing only. A checkpoint
// receives a zero increment, leaving its original byte contribution unchanged.
type contractExpiryReportContinuation struct {
	clientId              server.Id
	usedTransferByteCount ByteCount
	label                 string
}

// Ordinary expiry and an existing intent choose exactly the same report edits.
// An accepted adjudication finalizes only its selected, already present report.
func contractExpiryReportContinuations(state *contractExpiryState, outcome ContractOutcome) ([]contractExpiryReportContinuation, error) {
	sourceFinal := contractExpiryReportContinuation{clientId: state.sourceId, label: "finalize source checkpoint"}
	destinationFinal := contractExpiryReportContinuation{clientId: state.destinationId, label: "finalize destination checkpoint"}
	switch outcome {
	case ContractOutcomeDisputeResolvedToSource:
		if state.sourceCloseTime == nil {
			return nil, fmt.Errorf("adjudicated expiry lacks its source report")
		}
		if *state.sourceCheckpoint {
			return []contractExpiryReportContinuation{sourceFinal}, nil
		}
		return nil, nil
	case ContractOutcomeDisputeResolvedToDestination:
		if state.destinationCloseTime == nil {
			return nil, fmt.Errorf("adjudicated expiry lacks its destination report")
		}
		if *state.destinationCheckpoint {
			return []contractExpiryReportContinuation{destinationFinal}, nil
		}
		return nil, nil
	case ContractOutcomeSettled:
	default:
		return nil, fmt.Errorf("unknown expiry continuation outcome")
	}
	if state.sourceCloseTime == nil && state.destinationCloseTime == nil {
		sourceFinal.label = "both sides"
		destinationFinal.label = ""
		return []contractExpiryReportContinuation{sourceFinal, destinationFinal}, nil
	}
	if state.sourceCloseTime == nil {
		sourceFinal.usedTransferByteCount = *state.destinationUsedTransferByteCount
		sourceFinal.label = "source accepts destination"
		reports := []contractExpiryReportContinuation{sourceFinal}
		if *state.destinationCheckpoint {
			destinationFinal.label = ""
			reports = append(reports, destinationFinal)
		}
		return reports, nil
	}
	if state.destinationCloseTime == nil {
		destinationFinal.usedTransferByteCount = *state.sourceUsedTransferByteCount
		destinationFinal.label = "destination accepts source"
		reports := []contractExpiryReportContinuation{destinationFinal}
		if *state.sourceCheckpoint {
			sourceFinal.label = ""
			reports = append(reports, sourceFinal)
		}
		return reports, nil
	}
	reports := []contractExpiryReportContinuation{}
	if *state.sourceCheckpoint {
		reports = append(reports, sourceFinal)
	}
	if *state.destinationCheckpoint {
		reports = append(reports, destinationFinal)
	}
	return reports, nil
}

// The caller owns this contract and retained its original proof before any
// synthetic billing peer. All reports and financial work use the caller's tx.
func continueContractExpiryReportsInTx(ctx context.Context, tx server.PgTx, state *contractExpiryState, outcome ContractOutcome) error {
	if !state.usageUnverifiedRetained {
		return fmt.Errorf("expiry report continuation lacks retained ownership")
	}
	reports, err := contractExpiryReportContinuations(state, outcome)
	if err != nil {
		return err
	}
	for _, report := range reports {
		_, _, err := applyContractCloseReportWithExpiryInTx(ctx, tx, state.contractId, report.clientId,
			report.usedTransferByteCount, false, nil, state)
		if err != nil {
			return err
		}
	}
	return nil
}

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
		// Reload reports under this owner. Partial disputes need the same
		// missing-peer billing continuation as ordinary expiry; their retained
		// proof still records only the original reports.
		fresh, err := prepareContractExpiryInTx(ctx, tx, contractId, server.NowUtc())
		server.Raise(err)
		if fresh == nil {
			panic(errors.New("disputed expiry lost retained ownership"))
		}
		server.Raise(continueContractExpiryReportsInTx(ctx, tx, fresh, ContractOutcomeSettled))
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

	reports, err := contractExpiryReportContinuations(openContract, ContractOutcomeSettled)
	if err != nil {
		return err
	}
	if len(reports) > 0 {
		for _, report := range reports {
			if report.label != "" {
				recordForceCloseContract(report.label, tag)
			}
			if err := closeContractWithExpiryScope(ctx, scope, openContract.contractId, report.clientId,
				report.usedTransferByteCount, false); err != nil {
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
