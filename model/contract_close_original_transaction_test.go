// Original-history reads belong to the close transaction, including its
// uncommitted history. Missing optional schema must not issue invalid SQL.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/protocol"
	coreprotocol "github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/startifact"
)

// A real PostgreSQL owner supplies all SQL; the probe refuses savepoints and
// verifies the history query stays on that owner's backend and wrapper.
type originalCloseTransactionProbe struct {
	server.PgTx
	callerBackendPid int
	historyReads     int
	beginCalls       int
}

// Any Begin is a deterministic reproduction of the forbidden nesting.
func (self *originalCloseTransactionProbe) Begin(context.Context) (pgx.Tx, error) {
	self.beginCalls++
	return nil, errors.New("original close attempted a nested transaction")
}

// Uncommitted history and this observed backend jointly establish ownership.
func (self *originalCloseTransactionProbe) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if strings.Contains(sql, "FROM st_client_key_history") {
		self.historyReads++
		var backendPid int
		if err := self.PgTx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&backendPid); err != nil {
			return nil, err
		}
		if backendPid != self.callerBackendPid {
			return nil, errors.New("original close history left the caller backend")
		}
	}
	return self.PgTx.Query(ctx, sql, args...)
}

// The matching generation exists only inside the caller when the production
// close reads it. A fresh transaction cannot see it, and Begin is a tripwire.
func TestContractCloseOriginalHistoryUsesCallerTransactionWithoutSavepoint(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, input, previous, _ := originalCloseAvailabilityFixture(t)
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{71}, 32))
		next := previous.Registration
		next.Generation++
		var err error
		next.PreviousHash, err = previous.Registration.ContentHash()
		server.Raise(err)
		next.PublicKey = [32]byte(key[32:])
		next.EffectiveBoundary.Block++
		next.EffectiveBoundary.Hash[0]++
		server.Raise(protocol.SignClientKeyRegistration(&next, input.RootKey))
		server.Raise(next.Follows(&previous.Registration))
		registration, err := next.Bytes()
		server.Raise(err)
		hash := sha256.Sum256(registration)
		evidence, evidenceHash, err := startifact.SealClientKeyRegistrationEvidence(input.DeploymentID, next, input.ArtifactKey, input.CreatedAt)
		server.Raise(err)
		domain, err := input.Domain.Digest()
		server.Raise(err)
		report := signCloseReportOriginal(t, f.report(), domain, key)
		var probe *originalCloseTransactionProbe
		var applied bool
		var closeErr error
		server.Tx(f.ctx, func(tx server.PgTx) {
			probe = &originalCloseTransactionProbe{PgTx: tx}
			server.Raise(tx.QueryRow(f.ctx, `SELECT pg_backend_pid()`).Scan(&probe.callerBackendPid))
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO st_client_key_history(client_id,generation,domain_hash,registration_hash,registration,evidence_hash,evidence) VALUES($1,$2,$3,$4,$5,$6,$7)`, f.sourceId, next.Generation, domain[:], hash[:], registration, evidenceHash, evidence))
			applied, closeErr = closeContractReportInTx(f.ctx, probe, report)
		}, server.TxReadCommitted, server.OptNoRetry())
		if probe.beginCalls != 0 || probe.historyReads != 1 || closeErr != nil || !applied {
			t.Fatalf("original close did not reuse caller transaction: begin=%d reads=%d applied=%v err=%v", probe.beginCalls, probe.historyReads, applied, closeErr)
		}
		_, retained := retainedCloseOriginal(t, report)
		if !bytes.Equal(retained, registration) || retainedCloseKeyIssue(t, report) != "" {
			t.Fatal("original close missed the caller's uncommitted matching history")
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	})
}

// Schema mutation and recovery share the same owner as the close, proving a
// missing optional relation/column neither issues invalid SQL nor poisons it.
func originalCloseUnavailableSchemaTest(t testing.TB, remove, restore string) {
	t.Helper()
	f, input, _, key := originalCloseAvailabilityFixture(t)
	domain, err := input.Domain.Digest()
	server.Raise(err)
	report := signCloseReportOriginal(t, f.report(), domain, key)
	var probe *originalCloseTransactionProbe
	var applied bool
	var closeErr error
	server.Tx(f.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(f.ctx, remove))
		probe = &originalCloseTransactionProbe{PgTx: tx}
		server.Raise(tx.QueryRow(f.ctx, `SELECT pg_backend_pid()`).Scan(&probe.callerBackendPid))
		applied, closeErr = closeContractReportInTx(f.ctx, probe, report)
		server.RaisePgResult(tx.Exec(f.ctx, restore))
	}, server.TxReadCommitted, server.OptNoRetry())
	if probe.beginCalls != 0 || probe.historyReads != 0 || closeErr != nil || !applied {
		t.Fatalf("missing optional schema did not preserve caller: begin=%d reads=%d applied=%v err=%v", probe.beginCalls, probe.historyReads, applied, closeErr)
	}
	original, registration := retainedCloseOriginal(t, report)
	if !bytes.Equal(original, report.OriginalReport) || len(registration) != 0 || retainedCloseKeyIssue(t, report) != originalCloseKeyUnavailable {
		t.Fatal("missing optional schema lost the original or acquired registration")
	}
	if applied, err := CloseContractWithReport(f.ctx, report); applied || err != nil {
		t.Fatal("schema recovery repeated an admitted original", applied, err)
	}
	_, registration = retainedCloseOriginal(t, report)
	if len(registration) != 0 || retainedCloseKeyIssue(t, report) != originalCloseKeyUnavailable {
		t.Fatal("schema recovery backfilled original registration authority")
	}
	assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
}

// Catalog absence is optional; the nonexistent relation is never queried.
func TestContractCloseOriginalMissingHistoryRelationKeepsCallerTransaction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		originalCloseUnavailableSchemaTest(t,
			`ALTER TABLE st_client_key_history RENAME TO synthetic_close_history_unavailable`,
			`ALTER TABLE synthetic_close_history_unavailable RENAME TO st_client_key_history`)
	})
}

// An incomplete rollout has a relation but cannot satisfy the immutable read.
func TestContractCloseOriginalMissingHistoryColumnKeepsCallerTransaction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		originalCloseUnavailableSchemaTest(t,
			`ALTER TABLE st_client_key_history RENAME COLUMN registration_hash TO synthetic_unavailable_registration_hash`,
			`ALTER TABLE st_client_key_history RENAME COLUMN synthetic_unavailable_registration_hash TO registration_hash`)
	})
}

// A protocol error can become visible only when the owner drains/closes rows.
type originalCloseDrainErrorRows struct {
	pgx.Rows
	failure error
	closed  bool
}

func (self *originalCloseDrainErrorRows) Next() bool { return false }
func (self *originalCloseDrainErrorRows) Close()     { self.closed = true }
func (self *originalCloseDrainErrorRows) Err() error {
	if self.closed {
		return self.failure
	}
	return nil
}

// The caller receives one owned row result with a deferred protocol failure.
type originalCloseDrainErrorTx struct {
	server.PgTx
	rows *originalCloseDrainErrorRows
}

func (self *originalCloseDrainErrorTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return self.rows, nil
}

// A completed scan is not success until result cleanup has joined its error.
func TestContractCloseOriginalHistoryReaderPreservesDrainFailure(t *testing.T) {
	failure := errors.New("synthetic original history drain failure")
	rows := &originalCloseDrainErrorRows{failure: failure}
	tx := &originalCloseDrainErrorTx{rows: rows}
	registration, err := readOriginalCloseKeyRegistration(t.Context(), tx, &coreprotocol.OriginalCloseReport{})
	if !rows.closed || !errors.Is(err, failure) || len(registration) != 0 {
		t.Fatal("original history drain failure became absence", rows.closed, registration, err)
	}
}
