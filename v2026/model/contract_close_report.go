// Close-report custody joins one authenticated original request to one byte increment.
// A repeated exact report resumes settlement without adding work or changing history.
package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	coreprotocol "github.com/urnetwork/connect/v2026/protocol"
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
// Raised before BEGIN, it begins no transaction; a nil cause is then an exact
// retry the reads before BEGIN already answered.
type contractCloseReportAbort struct{ cause error }

// Applied means this call committed a new logical report, including zero-byte
// finality and identified work already covered by a legacy checkpoint bound.
// Exact retries remain successful after settlement or contract-directory cleanup.
// Success acknowledges the report, not necessarily a terminal outcome:
// checkpoints and disputes can remain unfinished.
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
	original, err := validateContractCloseOriginal(report)
	if err != nil {
		return false, 0, err
	}
	admission, err := admitContractCloseReport(ctx, report, original)
	if err != nil {
		return false, 0, err
	}
	if !admission.applied {
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
	// A duplicate resumes only the two-final rule: its original ordering against
	// the sender's final is not retained, so a contract whose checkpoint did not
	// complete waits for the receiver's next report or the expiry task.
	closed, completedCheckpoint, err := settleReportedContract(ctx, report.ContractId, admission.followsSenderFinal)
	if err != nil {
		return admission.applied, admission.addedByteCount, err
	}
	if closed && !completedCheckpoint {
		RemoveFromStream(ctx, report.ContractId)
	}
	return admission.applied, admission.addedByteCount, nil
}

// One transaction owns the original evidence, the receipt and the increment.
// Work that needs no lock finishes before BEGIN: validation and the original's
// signature check before this call and, for a signed report, the retained
// identity and key history reads. Between BEGIN and COMMIT the transaction
// makes three round trips: the locked reads, the reservation and the increment.
func admitContractCloseReport(ctx context.Context, report ContractCloseReport, original *coreprotocol.OriginalCloseReport) (admission contractCloseReportAdmission, returnErr error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if refusal, ok := recovered.(contractCloseReportAbort); ok {
				admission = contractCloseReportAdmission{}
				returnErr = refusal.cause
			} else {
				panic(recovered)
			}
		}
	}()
	txOptions := []any{server.TxReadCommitted}
	var keyRead originalCloseKeyRead
	if original != nil {
		// Every attempt reads on its own connection just before its BEGIN.
		txOptions = append(txOptions, server.TxReadBeforeBegin(func(conn server.PgCanQuery) {
			retained, read, stopErr := readSignedContractCloseReportBeforeBegin(ctx, conn, report, original)
			if stopErr != nil {
				// A stopped caller begins no transaction.
				panic(contractCloseReportAbort{cause: stopErr})
			}
			if retained != nil {
				// A retained report needs neither the lock nor the transaction.
				panic(contractCloseReportAbort{cause: retained.match(report)})
			}
			keyRead = read
		}))
	}
	server.Tx(ctx, func(tx server.PgTx) {
		// The transaction owner may retry a rolled-back attempt; never retain its result.
		admission = contractCloseReportAdmission{}
		var err error
		admission, err = admitContractCloseReportInTx(ctx, tx, report, func() originalCloseKeyRead { return keyRead })
		if err != nil {
			panic(contractCloseReportAbort{cause: err})
		}
	}, txOptions...)
	return
}

// A signed report reads on its transaction's connection before BEGIN, so no
// transaction holds a server across the key history read and its decode. The
// retained identity is read first: a retained report keeps the authority it
// was admitted with, so only a new one reads key history. Both reads see
// immutable rows, so they are the reads the transaction would have made had it
// begun earlier. As for the transaction's own statements (db_statement_stop.go),
// a caller's stop does not interrupt their round trips, which would destroy the
// pooled connection: the reads complete, or `server.PgStatementStopTimeout`
// interrupts them, and the stop is returned in place of their results.
func readSignedContractCloseReportBeforeBegin(ctx context.Context, conn server.PgCanQuery, report ContractCloseReport, original *coreprotocol.OriginalCloseReport) (*retainedContractCloseReport, originalCloseKeyRead, error) {
	if err := ctx.Err(); err != nil {
		// a stopped caller starts no read
		return nil, originalCloseKeyRead{}, err
	}
	readCtx, cancelRead := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRead()
	stopBound := context.AfterFunc(ctx, func() {
		select {
		case <-readCtx.Done():
		case <-time.After(server.PgStatementStopTimeout):
			cancelRead()
		}
	})
	defer stopBound()
	var key originalCloseKeyRead
	retained, err := readRetainedContractCloseReport(readCtx, conn, report)
	if err == nil && retained == nil {
		// A caller that stopped during the first read starts no second one.
		select {
		case <-ctx.Done():
		default:
			key = readOriginalCloseKey(readCtx, conn, original)
		}
	}
	if stopErr := ctx.Err(); stopErr != nil {
		return nil, originalCloseKeyRead{}, errors.Join(stopErr, err, key.err)
	}
	server.Raise(err)
	return retained, key, nil
}

// The retained original of one owner/report identity.
const contractCloseReportEvidenceSql = `SELECT contract_id,acked_byte_count,unacked_byte_count::text,checkpoint,original_report,original_inventory
  FROM contract_close_report_evidence WHERE client_id=$1 AND report_id=$2`

// Only immutable retained facts; no current client membership is consulted.
type retainedContractCloseReport struct {
	contractId server.Id
	acked      ByteCount
	unacked    string
	checkpoint bool
	original   []byte
	inventory  []byte
}

// The destinations of contractCloseReportEvidenceSql's columns.
func (self *retainedContractCloseReport) scanDestinations() []any {
	return []any{&self.contractId, &self.acked, &self.unacked, &self.checkpoint, &self.original, &self.inventory}
}

// A missing identity scans as nil.
func scanRetainedContractCloseReport(row pgx.Row) (*retainedContractCloseReport, error) {
	retained := &retainedContractCloseReport{}
	err := row.Scan(retained.scanDestinations()...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return retained, nil
}

// Reads a retained identity on any connection; a missing identity is nil.
func readRetainedContractCloseReport(ctx context.Context, conn server.PgCanQuery, report ContractCloseReport) (*retainedContractCloseReport, error) {
	rows, err := conn.Query(ctx, contractCloseReportEvidenceSql, report.ClientId, report.ReportId)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var retained *retainedContractCloseReport
	for rows.Next() {
		retained = &retainedContractCloseReport{}
		if err := rows.Scan(retained.scanDestinations()...); err != nil {
			return nil, err
		}
	}
	return retained, rows.Err()
}

// An exact retry repeats every retained fact.
func (self *retainedContractCloseReport) match(report ContractCloseReport) error {
	if self.contractId != report.ContractId || self.acked != report.AckedByteCount || self.unacked != strconv.FormatUint(report.UnackedByteCount, 10) || self.checkpoint != report.Checkpoint {
		return ErrContractCloseReportConflict
	}
	// Older intermediaries may omit evidence; their exact retry cannot erase it.
	// A previously unsigned admission cannot be backfilled as an original signature.
	if len(report.OriginalReport) > 0 && !bytes.Equal(report.OriginalReport, self.original) {
		return ErrContractCloseReportConflict
	}
	if len(report.OriginalInventory) > 0 && !bytes.Equal(report.OriginalInventory, self.inventory) {
		return ErrContractCloseReportConflict
	}
	return nil
}

// Read only immutable retained facts; no current client membership is consulted.
func matchContractCloseReportInTx(ctx context.Context, tx server.PgTx, report ContractCloseReport) (bool, error) {
	retained, err := scanRetainedContractCloseReport(tx.QueryRow(ctx, contractCloseReportEvidenceSql, report.ClientId, report.ReportId))
	server.Raise(err)
	if retained == nil {
		return false, nil
	}
	return true, retained.match(report)
}

// The contract row serializes admission with ordinary closes and settlement.
// The unique owner/report key also fences reuse against another contract row.
func closeContractReportInTx(ctx context.Context, tx server.PgTx, report ContractCloseReport) (bool, error) {
	applied, _, err := closeContractReportUsageInTx(ctx, tx, report)
	return applied, err
}

// Preserve exact original custody even when the accounting delta is conservative.
// An in-transaction caller reads key history in its own transaction, which may
// hold history it has not committed.
func closeContractReportUsageInTx(ctx context.Context, tx server.PgTx, report ContractCloseReport) (bool, ByteCount, error) {
	if err := ctx.Err(); err != nil {
		return false, 0, err
	}
	original, err := validateContractCloseOriginal(report)
	if err != nil {
		return false, 0, err
	}
	admission, err := admitContractCloseReportInTx(ctx, tx, report, func() originalCloseKeyRead {
		return readOriginalCloseKey(ctx, tx, original)
	})
	return admission.applied, admission.addedByteCount, err
}

// One report admitted under its contract lock.
type contractCloseReportAdmission struct {
	applied        bool
	addedByteCount ByteCount
	// the receiver applied this report after the sender's final had committed
	followsSenderFinal bool
}

// Admission takes three round trips: the locked reads, the reservation and the
// increment. readKey supplies the original key registration where admission
// consults it, after every refusal the locked reads decide.
func admitContractCloseReportInTx(ctx context.Context, tx server.PgTx, report ContractCloseReport, readKey func() originalCloseKeyRead) (contractCloseReportAdmission, error) {
	if err := ctx.Err(); err != nil {
		return contractCloseReportAdmission{}, err
	}
	locked := false
	var sourceId, destinationId server.Id
	var outcome *ContractOutcome
	var dispute bool
	var retained *retainedContractCloseReport
	legacyFound := false
	var legacyAcked ByteCount
	var legacyCheckpoint bool
	// One round trip carries the lock and both identity reads. Each statement
	// still takes its own read-committed snapshot once the one before it
	// finished, so both reads observe a report that committed while the lock
	// waited. An exact retry whose original already committed locks nothing.
	batch := &pgx.Batch{}
	batch.Queue(`SELECT source_id,destination_id,outcome,dispute FROM transfer_contract
 WHERE contract_id=$1 AND NOT EXISTS(SELECT 1 FROM contract_close_report_evidence WHERE client_id=$2 AND report_id=$3)
 FOR UPDATE`, report.ContractId, report.ClientId, report.ReportId).QueryRow(func(row pgx.Row) error {
		err := row.Scan(&sourceId, &destinationId, &outcome, &dispute)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		locked = err == nil
		return err
	})
	batch.Queue(contractCloseReportEvidenceSql, report.ClientId, report.ReportId).QueryRow(func(row pgx.Row) (err error) {
		retained, err = scanRetainedContractCloseReport(row)
		return
	})
	// A rolling older writer may already have accepted this exact increment
	// for the client's party.
	batch.Queue(`SELECT used_transfer_byte_count,checkpoint FROM contract_close_report
 WHERE contract_id=$1 AND report_id=$2 AND party=(SELECT CASE WHEN source_id=$3 THEN $4::text WHEN destination_id=$3 THEN $5::text END
  FROM transfer_contract WHERE contract_id=$1)`, report.ContractId, report.ReportId, report.ClientId, ContractPartySource, ContractPartyDestination).QueryRow(func(row pgx.Row) error {
		err := row.Scan(&legacyAcked, &legacyCheckpoint)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		legacyFound = err == nil
		return err
	})
	server.Raise(tx.SendBatch(ctx, batch).Close())
	// A retained identity decides first, so an exact retry succeeds even after
	// settlement or contract-directory cleanup.
	if retained != nil {
		return contractCloseReportAdmission{}, retained.match(report)
	}
	if !locked {
		return contractCloseReportAdmission{}, fmt.Errorf("Contract not found: %s", report.ContractId)
	}
	var party ContractParty
	if report.ClientId == sourceId {
		party = ContractPartySource
	} else if report.ClientId == destinationId {
		party = ContractPartyDestination
	}
	if party == "" {
		return contractCloseReportAdmission{}, fmt.Errorf("Client is not a party to the contract: %s", report.ContractId)
	}
	// The older writer did not retain unacked bytes, a client signature or
	// registration, so acknowledge its receipt without counting or backfilling
	// stronger evidence.
	if legacyFound {
		if legacyAcked != report.AckedByteCount || legacyCheckpoint != report.Checkpoint {
			return contractCloseReportAdmission{}, ErrContractCloseReportConflict
		}
		return contractCloseReportAdmission{}, nil
	}
	if outcome != nil {
		if *outcome == ContractOutcomeSettled {
			return contractCloseReportAdmission{}, fmt.Errorf("%w: %s", errContractAlreadySettled, report.ContractId)
		}
		return contractCloseReportAdmission{}, fmt.Errorf("Contract already closed with outcome %s: %s", *outcome, report.ContractId)
	}
	if dispute {
		return contractCloseReportAdmission{}, fmt.Errorf("Contract in dispute: %s", report.ContractId)
	}
	key := readKey()
	if key.err != nil {
		return contractCloseReportAdmission{}, key.err
	}
	// Reserve before accumulating. A different-contract collision must never
	// commit an increment before discovering that its report key already exists.
	acceptedAt := server.NowUtc()
	tag := server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close_report_evidence
	  (client_id,report_id,contract_id,party,acked_byte_count,unacked_byte_count,checkpoint,accepted_at,original_report,original_key_registration,original_key_issue,original_inventory)
	  VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(client_id,report_id) DO NOTHING`,
		report.ClientId, report.ReportId, report.ContractId, party, report.AckedByteCount, strconv.FormatUint(report.UnackedByteCount, 10), report.Checkpoint, acceptedAt, report.OriginalReport, key.registration, key.issue, report.OriginalInventory))
	if tag.RowsAffected() == 0 {
		found, err := matchContractCloseReportInTx(ctx, tx, report)
		if !found && err == nil {
			return contractCloseReportAdmission{}, errors.New("original contract close report disappeared during admission")
		}
		return contractCloseReportAdmission{}, err
	}
	// The lock above owns both the old rolling receipt and the byte increment.
	// Any refusal rolls back our reservation too.
	applied, added, followsSenderFinal := applyLockedContractCloseReportInTx(ctx, tx, report.ContractId, sourceId, destinationId, party, report.AckedByteCount, report.Checkpoint, &report.ReportId)
	if !applied {
		return contractCloseReportAdmission{}, ErrContractCloseReportClosed
	}
	return contractCloseReportAdmission{applied: true, addedByteCount: added, followsSenderFinal: followsSenderFinal}, nil
}
