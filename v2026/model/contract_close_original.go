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
	originalCloseKeyMissing     = "history_not_found"
	originalCloseKeyCapacity    = "history_capacity"
	originalCloseKeyUnavailable = "history_read_unavailable"
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

// Key history is independently immutable. Select an exact historical key from
// the report's own domain, never today's Redis projection or another policy.
// Missing schema/history leaves the component unknown. Once its schema is ready,
// any SQL error belongs to the caller's transaction and must abort that owner.
func originalCloseKeyRegistrationInTx(ctx context.Context, tx server.PgTx, original *coreprotocol.OriginalCloseReport) ([]byte, string, error) {
	if original == nil {
		return nil, "", nil
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	// Catalog reads can establish an incomplete rollout without issuing invalid
	// history SQL. The actual read keeps this exact caller's snapshot and writes.
	var ready bool
	if err := tx.QueryRow(ctx, `SELECT count(*)=7 FROM pg_catalog.pg_attribute
 WHERE attrelid=pg_catalog.to_regclass('st_client_key_history')
 AND attnum>0 AND NOT attisdropped
 AND attname=ANY(ARRAY['client_id','generation','domain_hash','registration_hash','registration','evidence_hash','evidence'])`).Scan(&ready); err != nil {
		return nil, "", errors.Join(ctx.Err(), fmt.Errorf("read original close key history schema: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	if !ready {
		return nil, originalCloseKeyUnavailable, nil
	}
	registration, readErr := readOriginalCloseKeyRegistration(ctx, tx, original)
	if ownerErr := ctx.Err(); ownerErr != nil {
		// The enclosing owner rolls back the whole transaction with bounded cleanup.
		return nil, "", errors.Join(ownerErr, readErr)
	}
	if readErr != nil {
		// Capacity alone is optional; a joined drain failure still aborts.
		if readErr == errOriginalCloseKeyCapacity {
			return nil, originalCloseKeyCapacity, nil
		}
		return nil, "", readErr
	}
	if len(registration) == 0 {
		return nil, originalCloseKeyMissing, nil
	}
	return registration, "", nil
}

// This bounded reader retains only an exact original registration. Capacity is
// optional only when row cleanup confirms no independent SQL/protocol failure.
func readOriginalCloseKeyRegistration(ctx context.Context, tx server.PgTx, original *coreprotocol.OriginalCloseReport) (registration []byte, returnErr error) {
	rows, err := tx.Query(ctx, `SELECT registration,registration_hash,evidence,evidence_hash
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
		var registration, registrationHash, evidence []byte
		var evidenceHash string
		if err := rows.Scan(&registration, &registrationHash, &evidence, &evidenceHash); err != nil {
			return nil, err
		}
		next := uint64(len(registration) + len(evidence))
		if next > MaxStClientKeyHistoryBytes-used {
			return nil, errOriginalCloseKeyCapacity
		}
		used += next
		record, err := decodeStClientKeyHistoryRecord(registration, registrationHash, evidence, evidenceHash)
		if err != nil {
			return nil, errors.Join(ErrContractCloseOriginalIntegrity, err)
		}
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
