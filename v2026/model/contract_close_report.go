// Close-report custody joins one authenticated original request to one byte increment.
// A repeated exact report resumes settlement without adding work or changing history.
package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// The stable identity is scoped to the authenticated client, never supplied party text.
// Unacked bytes are retained exactly but do not contribute to completed-work credit.
type ContractCloseReport struct {
	ReportId          server.Id
	ContractId        server.Id
	ClientId          server.Id
	AckedByteCount    ByteCount
	UnackedByteCount  uint64
	Checkpoint        bool
	OriginalReport    []byte
	OriginalInventory []byte
}

// These typed refusals distinguish identity reuse from a missing or closed original.
var ErrContractCloseReportConflict = errors.New("contract close report identity has different original content")
var ErrContractCloseReportInvalid = errors.New("contract close report identity or byte count is invalid")
var ErrContractCloseReportClosed = errors.New("contract close report party is already terminal")

// A semantic refusal rolls back the whole transaction without changing database
// panic/retry ownership. Only this private envelope is consumed by the public API.
type contractCloseReportAbort struct{ cause error }

// Applied means this call committed a new logical report, including zero-byte
// finality and identified work already covered by a legacy checkpoint bound.
// Exact retries remain successful after settlement or contract-directory cleanup.
// Success acknowledges the report, not necessarily a terminal outcome:
// checkpoints, disputes and deferred legacy settlement can remain unfinished.
func CloseContractWithReport(ctx context.Context, report ContractCloseReport) (applied bool, returnErr error) {
	applied, _, returnErr = closeContractWithReportUsage(ctx, report)
	return
}

// Return the actual committed increment for transport metrics. In mixed legacy
// mode, a new identified report can be retained without increasing the bound.
// A later settlement error does not erase an already committed report delta.
func CloseContractWithReportUsage(ctx context.Context, report ContractCloseReport) (ByteCount, error) {
	_, added, err := closeContractWithReportUsage(ctx, report)
	return added, err
}

// Original evidence, the receipt and conservative accounting share one commit.
func closeContractWithReportUsage(ctx context.Context, report ContractCloseReport) (applied bool, addedByteCount ByteCount, returnErr error) {
	if err := ctx.Err(); err != nil {
		return false, 0, err
	}
	if report.ReportId == (server.Id{}) || report.ContractId == (server.Id{}) || report.ClientId == (server.Id{}) || report.AckedByteCount < 0 {
		return false, 0, ErrContractCloseReportInvalid
	}
	report.OriginalInventory = bytes.Clone(report.OriginalInventory)
	if len(report.OriginalInventory) == 0 {
		report.OriginalInventory = nil
	}
	report.OriginalReport = bytes.Clone(report.OriginalReport)
	if len(report.OriginalReport) == 0 {
		report.OriginalReport = nil
	}
	if _, err := validateContractCloseOriginal(report); err != nil {
		return false, 0, err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if refusal, ok := recovered.(contractCloseReportAbort); ok {
				applied = false
				addedByteCount = 0
				returnErr = refusal.cause
			} else {
				panic(recovered)
			}
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		// The transaction owner may retry a rolled-back attempt; never retain its result.
		applied = false
		addedByteCount = 0
		var err error
		applied, addedByteCount, err = closeContractReportUsageInTx(ctx, tx, report)
		if err != nil {
			panic(contractCloseReportAbort{cause: err})
		}
	}, server.TxReadCommitted)
	if !applied {
		// A retained report outlives the mutable contract and client directory.
		// Only an unfinished owner needs settlement resumed after a lost reply.
		var pending bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM transfer_contract
 WHERE contract_id=$1 AND outcome IS NULL AND NOT dispute)`, report.ContractId).Scan(&pending))
		})
		if !pending {
			return false, 0, nil
		}
	}
	// The original accepted transaction may have lost its reply before settlement.
	// Resume the idempotent original settlement even on an exact duplicate report.
	closed, err := settleContract(ctx, report.ContractId)
	if err != nil {
		return applied, addedByteCount, err
	}
	if closed {
		RemoveFromStream(ctx, report.ContractId)
	}
	return applied, addedByteCount, nil
}

// Read only immutable retained facts; no current client membership is consulted.
func matchContractCloseReportInTx(ctx context.Context, tx server.PgTx, report ContractCloseReport) (bool, error) {
	var contractId server.Id
	var acked ByteCount
	var unacked string
	var checkpoint bool
	var original, inventory []byte
	err := tx.QueryRow(ctx, `SELECT contract_id,acked_byte_count,unacked_byte_count::text,checkpoint,original_report,original_inventory
  FROM contract_close_report_evidence WHERE client_id=$1 AND report_id=$2`, report.ClientId, report.ReportId).Scan(&contractId, &acked, &unacked, &checkpoint, &original, &inventory)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	server.Raise(err)
	if contractId != report.ContractId || acked != report.AckedByteCount || unacked != strconv.FormatUint(report.UnackedByteCount, 10) || checkpoint != report.Checkpoint {
		return true, ErrContractCloseReportConflict
	}
	// Older intermediaries may omit evidence; their exact retry cannot erase it.
	// A previously unsigned admission cannot be backfilled as an original signature.
	if len(report.OriginalReport) > 0 && !bytes.Equal(report.OriginalReport, original) {
		return true, ErrContractCloseReportConflict
	}
	if len(report.OriginalInventory) > 0 && !bytes.Equal(report.OriginalInventory, inventory) {
		return true, ErrContractCloseReportConflict
	}
	return true, nil
}

// The contract row serializes admission with ordinary closes and settlement.
// The unique owner/report key also fences reuse against another contract row.
func closeContractReportInTx(ctx context.Context, tx server.PgTx, report ContractCloseReport) (bool, error) {
	applied, _, err := closeContractReportUsageInTx(ctx, tx, report)
	return applied, err
}

// Preserve exact original custody even when the accounting delta is conservative.
func closeContractReportUsageInTx(ctx context.Context, tx server.PgTx, report ContractCloseReport) (bool, ByteCount, error) {
	if err := ctx.Err(); err != nil {
		return false, 0, err
	}
	original, err := validateContractCloseOriginal(report)
	if err != nil {
		return false, 0, err
	}
	if found, err := matchContractCloseReportInTx(ctx, tx, report); found || err != nil {
		return false, 0, err
	}
	var sourceId, destinationId server.Id
	var outcome *ContractOutcome
	var dispute bool
	err = tx.QueryRow(ctx, `SELECT source_id,destination_id,outcome,dispute FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, report.ContractId).Scan(&sourceId, &destinationId, &outcome, &dispute)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, fmt.Errorf("Contract not found: %s", report.ContractId)
	}
	server.Raise(err)
	// A concurrent identical report may have committed while the row lock waited.
	if found, err := matchContractCloseReportInTx(ctx, tx, report); found || err != nil {
		return false, 0, err
	}
	var party ContractParty
	if report.ClientId == sourceId {
		party = ContractPartySource
	} else if report.ClientId == destinationId {
		party = ContractPartyDestination
	}
	if party == "" {
		return false, 0, fmt.Errorf("Client is not a party to the contract: %s", report.ContractId)
	}
	// A rolling older writer may already have accepted this exact increment.
	// It did not retain unacked bytes, a client signature or registration, so
	// acknowledge its receipt without counting or backfilling stronger evidence.
	var legacyAcked ByteCount
	var legacyCheckpoint bool
	err = tx.QueryRow(ctx, `SELECT used_transfer_byte_count,checkpoint FROM contract_close_report
  WHERE contract_id=$1 AND party=$2 AND report_id=$3`, report.ContractId, party, report.ReportId).Scan(&legacyAcked, &legacyCheckpoint)
	if err == nil {
		if legacyAcked != report.AckedByteCount || legacyCheckpoint != report.Checkpoint {
			return false, 0, ErrContractCloseReportConflict
		}
		return false, 0, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		server.Raise(err)
	}
	if outcome != nil {
		if *outcome == ContractOutcomeSettled {
			return false, 0, fmt.Errorf("%w: %s", errContractAlreadySettled, report.ContractId)
		}
		return false, 0, fmt.Errorf("Contract already closed with outcome %s: %s", *outcome, report.ContractId)
	}
	if dispute {
		return false, 0, fmt.Errorf("Contract in dispute: %s", report.ContractId)
	}
	registration, keyIssue, err := originalCloseKeyRegistrationInTx(ctx, tx, original)
	if err != nil {
		return false, 0, err
	}
	// Reserve before accumulating. A different-contract collision must never
	// commit an increment before discovering that its report key already exists.
	acceptedAt := server.NowUtc()
	tag := server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report_evidence
	  (client_id,report_id,contract_id,party,acked_byte_count,unacked_byte_count,checkpoint,accepted_at,original_report,original_key_registration,original_key_issue,original_inventory)
	  VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(client_id,report_id) DO NOTHING`,
		report.ClientId, report.ReportId, report.ContractId, party, report.AckedByteCount, strconv.FormatUint(report.UnackedByteCount, 10), report.Checkpoint, acceptedAt, report.OriginalReport, registration, keyIssue, report.OriginalInventory))
	if tag.RowsAffected() == 0 {
		found, err := matchContractCloseReportInTx(ctx, tx, report)
		if !found && err == nil {
			return false, 0, errors.New("original contract close report disappeared during admission")
		}
		return false, 0, err
	}
	// Use the current published transaction owner for both the old rolling
	// receipt and the byte increment. Any refusal rolls back our reservation too.
	applied, _, added, err := applyContractCloseReportUsageInTx(ctx, tx, report.ContractId, report.ClientId, report.AckedByteCount, report.Checkpoint, &report.ReportId, nil)
	if err != nil {
		return false, 0, err
	}
	if !applied {
		return false, 0, ErrContractCloseReportClosed
	}
	return true, added, nil
}
