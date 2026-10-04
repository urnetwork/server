// Close-report custody joins one authenticated original request to one byte increment.
// A repeated exact report resumes settlement without adding work or changing history.
package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

// The stable identity is scoped to the authenticated client, never supplied party text.
// Unacked bytes are retained exactly but do not contribute to completed-work credit.
type ContractCloseReport struct {
	ReportId         server.Id
	ContractId       server.Id
	ClientId         server.Id
	AckedByteCount   ByteCount
	UnackedByteCount uint64
	Checkpoint       bool
}

// These typed refusals distinguish identity reuse from a missing or closed original.
var ErrContractCloseReportConflict = errors.New("contract close report identity has different original content")
var ErrContractCloseReportInvalid = errors.New("contract close report identity or byte count is invalid")
var ErrContractCloseReportClosed = errors.New("contract close report party is already terminal")

// A semantic refusal rolls back the whole transaction without changing database
// panic/retry ownership. Only this private envelope is consumed by the public API.
type contractCloseReportAbort struct{ cause error }

// Applied means this call committed new completed bytes, including zero-byte finality.
// Exact retries remain successful after settlement or contract-directory cleanup.
func CloseContractWithReport(ctx context.Context, report ContractCloseReport) (applied bool, returnErr error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if report.ReportId == (server.Id{}) || report.ContractId == (server.Id{}) || report.ClientId == (server.Id{}) || report.AckedByteCount < 0 {
		return false, ErrContractCloseReportInvalid
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			if refusal, ok := recovered.(contractCloseReportAbort); ok {
				applied = false
				returnErr = refusal.cause
			} else {
				panic(recovered)
			}
		}
	}()
	server.Tx(ctx, func(tx server.PgTx) {
		// The transaction owner may retry a rolled-back attempt; never retain its result.
		applied = false
		var err error
		applied, err = closeContractReportInTx(ctx, tx, report)
		if err != nil {
			panic(contractCloseReportAbort{cause: err})
		}
	}, server.TxReadCommitted)
	// The original accepted transaction may have lost its reply before settlement.
	// Resume the idempotent original settlement even on an exact duplicate report.
	closed, err := settleContract(ctx, report.ContractId)
	if err != nil {
		return applied, err
	}
	if closed {
		RemoveFromStream(ctx, report.ContractId)
	}
	return applied, nil
}

// Read only immutable retained facts; no current client membership is consulted.
func matchContractCloseReportInTx(ctx context.Context, tx server.PgTx, report ContractCloseReport) (bool, error) {
	var contractId server.Id
	var acked ByteCount
	var unacked string
	var checkpoint bool
	err := tx.QueryRow(ctx, `SELECT contract_id,acked_byte_count,unacked_byte_count::text,checkpoint
  FROM contract_close_report WHERE client_id=$1 AND report_id=$2`, report.ClientId, report.ReportId).Scan(&contractId, &acked, &unacked, &checkpoint)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	server.Raise(err)
	if contractId != report.ContractId || acked != report.AckedByteCount || unacked != strconv.FormatUint(report.UnackedByteCount, 10) || checkpoint != report.Checkpoint {
		return true, ErrContractCloseReportConflict
	}
	return true, nil
}

// The contract row serializes admission with ordinary closes and settlement.
// The unique owner/report key also fences reuse against another contract row.
func closeContractReportInTx(ctx context.Context, tx server.PgTx, report ContractCloseReport) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if found, err := matchContractCloseReportInTx(ctx, tx, report); found || err != nil {
		return false, err
	}
	var sourceId, destinationId server.Id
	var outcome *ContractOutcome
	var dispute bool
	err := tx.QueryRow(ctx, `SELECT source_id,destination_id,outcome,dispute FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, report.ContractId).Scan(&sourceId, &destinationId, &outcome, &dispute)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("Contract not found: %s", report.ContractId)
	}
	server.Raise(err)
	// A concurrent identical report may have committed while the row lock waited.
	if found, err := matchContractCloseReportInTx(ctx, tx, report); found || err != nil {
		return false, err
	}
	var party ContractParty
	if report.ClientId == sourceId {
		party = ContractPartySource
	} else if report.ClientId == destinationId {
		party = ContractPartyDestination
	}
	if party == "" {
		return false, fmt.Errorf("Client is not a party to the contract: %s", report.ContractId)
	}
	if outcome != nil {
		if *outcome == ContractOutcomeSettled {
			return false, fmt.Errorf("%w: %s", errContractAlreadySettled, report.ContractId)
		}
		return false, fmt.Errorf("Contract already closed with outcome %s: %s", *outcome, report.ContractId)
	}
	if dispute {
		return false, fmt.Errorf("Contract in dispute: %s", report.ContractId)
	}
	// Reserve before accumulating. A different-contract collision must never
	// commit an increment before discovering that its report key already exists.
	acceptedAt := server.NowUtc()
	tag := server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report
  (client_id,report_id,contract_id,party,acked_byte_count,unacked_byte_count,checkpoint,accepted_at)
  VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(client_id,report_id) DO NOTHING`,
		report.ClientId, report.ReportId, report.ContractId, party, report.AckedByteCount, strconv.FormatUint(report.UnackedByteCount, 10), report.Checkpoint, acceptedAt))
	if tag.RowsAffected() == 0 {
		found, err := matchContractCloseReportInTx(ctx, tx, report)
		if !found && err == nil {
			return false, errors.New("original contract close report disappeared during admission")
		}
		return false, err
	}
	tag = server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
  (contract_id,party,used_transfer_byte_count,close_time,checkpoint) VALUES($1,$2,$3,$4,$5)
  ON CONFLICT(contract_id,party) DO UPDATE SET
   used_transfer_byte_count=contract_close.used_transfer_byte_count+EXCLUDED.used_transfer_byte_count,
   close_time=EXCLUDED.close_time,checkpoint=EXCLUDED.checkpoint
  WHERE contract_close.checkpoint=true`, report.ContractId, party, report.AckedByteCount, acceptedAt, report.Checkpoint))
	if tag.RowsAffected() != 1 {
		return false, ErrContractCloseReportClosed
	}
	return true, nil
}
