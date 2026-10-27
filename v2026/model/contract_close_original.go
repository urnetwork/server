// Original signatures and historical key registrations are retained in the same
// transaction as their report. Registration existence is not earning eligibility.
package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	coreprotocol "github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

var ErrContractCloseOriginalIntegrity = errors.New("original close signature conflicts with its authenticated report")

// These bounded retained diagnostics never grant registered-key authority.
const (
	originalCloseKeyMissing  = "history_not_found"
	originalCloseKeyCapacity = "history_capacity"
)

var errOriginalCloseKeyCapacity = errors.New("original close key history exceeds its finite admission bound")

// Verify exact caller-owned bytes before any increment. Missing optional bytes
// remain unsigned, while a present malformed envelope is a scoped refusal.
func validateContractCloseOriginal(report ContractCloseReport) (*coreprotocol.OriginalCloseReport, error) {
	if len(report.OriginalReport) == 0 {
		if len(report.OriginalInventory) != 0 {
			return nil, ErrContractCloseOriginalIntegrity
		}
		return nil, nil
	}
	original, err := coreprotocol.DecodeOriginalCloseReport(report.OriginalReport)
	if err != nil {
		return nil, errors.Join(ErrContractCloseOriginalIntegrity, err)
	}
	outer := &coreprotocol.CloseContract{ContractId: report.ContractId.Bytes(), ReportId: report.ReportId.Bytes(), AckedByteCount: uint64(report.AckedByteCount), UnackedByteCount: report.UnackedByteCount, Checkpoint: report.Checkpoint}
	if !original.Matches([16]byte(report.ClientId), outer) {
		return nil, ErrContractCloseOriginalIntegrity
	}
	if len(report.OriginalInventory) != 0 {
		inventory, err := coreprotocol.DecodeOriginalCloseInventory(report.OriginalInventory)
		if err != nil || !inventory.Matches(original) {
			return nil, errors.Join(ErrContractCloseOriginalIntegrity, err)
		}
	}
	return &original, nil
}

// One finished read of a report's original key registration. Admission
// consults it where it would otherwise read, so a refusal decided earlier
// keeps its precedence over a failed read. Its error aborts the report's
// transaction.
type originalCloseKeyRead struct {
	registration []byte
	issue        string
	err          error
}

// Key history is independently immutable. Select an exact historical key from
// the report's own domain, never today's Redis projection or another policy.
// Migrations own the required schema. Missing history remains unknown; any SQL
// error, including absent schema, aborts the report's transaction. An unsigned
// report reads nothing.
func readOriginalCloseKey(ctx context.Context, conn server.PgCanQuery, original *coreprotocol.OriginalCloseReport) originalCloseKeyRead {
	if original == nil {
		return originalCloseKeyRead{}
	}
	if err := ctx.Err(); err != nil {
		return originalCloseKeyRead{err: err}
	}
	registration, readErr := readOriginalCloseKeyRegistration(ctx, conn, original)
	if ownerErr := ctx.Err(); ownerErr != nil {
		// The enclosing owner rolls back the whole transaction with bounded cleanup.
		return originalCloseKeyRead{err: errors.Join(ownerErr, readErr)}
	}
	if readErr != nil {
		// Capacity alone is optional; a joined drain failure still aborts.
		if readErr == errOriginalCloseKeyCapacity {
			return originalCloseKeyRead{issue: originalCloseKeyCapacity}
		}
		return originalCloseKeyRead{err: readErr}
	}
	if len(registration) == 0 {
		return originalCloseKeyRead{issue: originalCloseKeyMissing}
	}
	return originalCloseKeyRead{registration: registration}
}

// This bounded reader retains only an exact original registration. Capacity is
// optional only when row cleanup confirms no independent SQL/protocol failure.
func readOriginalCloseKeyRegistration(ctx context.Context, conn server.PgCanQuery, original *coreprotocol.OriginalCloseReport) (registration []byte, returnErr error) {
	rows, err := conn.Query(ctx, `SELECT registration_hash,evidence,evidence_hash
 FROM st_client_key_history WHERE client_id=$1 AND domain_hash=$2
 ORDER BY generation DESC LIMIT $3`, server.Id(original.ClientId), original.DomainHash[:], MaxStClientKeyHistoryRegistrations+1)
	if err != nil {
		return nil, err
	}
	defer func() {
		rows.Close()
		if err := rows.Err(); err != nil {
			registration = nil
			returnErr = errors.Join(returnErr, fmt.Errorf("read original close key history: %w", err))
		}
	}()
	var used uint64
	for count := uint64(0); rows.Next(); count++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if count >= MaxStClientKeyHistoryRegistrations {
			return nil, errOriginalCloseKeyCapacity
		}
		var registrationHash, evidence []byte
		var evidenceHash string
		if err := rows.Scan(&registrationHash, &evidence, &evidenceHash); err != nil {
			return nil, err
		}
		// The budget also counts the registration copy the evidence payload
		// carries, known once the row decodes. The evidence alone goes first.
		if uint64(len(evidence)) > MaxStClientKeyHistoryBytes-used {
			return nil, errOriginalCloseKeyCapacity
		}
		record, err := decodeStClientKeyHistoryRecord(registrationHash, evidence, evidenceHash)
		if err != nil {
			return nil, errors.Join(ErrContractCloseOriginalIntegrity, err)
		}
		next := uint64(len(record.RegistrationBytes) + len(evidence))
		if next > MaxStClientKeyHistoryBytes-used {
			return nil, errOriginalCloseKeyCapacity
		}
		used += next
		domainHash, err := record.Registration.Domain.Digest()
		if err != nil || domainHash != original.DomainHash || record.Registration.ClientID != original.ClientId {
			return nil, errors.Join(ErrContractCloseOriginalIntegrity, err)
		}
		if record.Registration.Present && record.Registration.PublicKey == original.PublicKey {
			return bytes.Clone(record.RegistrationBytes), nil
		}
	}
	return nil, nil
}
