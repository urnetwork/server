// Original-history reads belong to the close transaction, including its
// uncommitted history. Missing required schema aborts the complete owner.
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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urfoundation/sn/v2026/protocol"
	coreprotocol "github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/startifact"
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

// A missing deployed prerequisite refuses public admission and rolls back
// earlier caller writes. Restoring the schema admits the exact original once.
func originalCloseUnavailableSchemaTest(t testing.TB, remove, restore, sqlState string) {
	t.Helper()
	f, input, record, key := originalCloseAvailabilityFixture(t)
	domain, err := input.Domain.Digest()
	server.Raise(err)
	report := signCloseReportOriginal(t, f.report(), domain, key)
	server.Tx(f.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(f.ctx, `CREATE TABLE synthetic_close_schema_owner(value integer)`))
		server.RaisePgResult(tx.Exec(f.ctx, remove))
	}, server.TxReadCommitted, server.OptNoRetry())
	restored := false
	restoreSchema := func() {
		if !restored {
			server.Tx(f.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(f.ctx, restore))
			}, server.TxReadCommitted, server.OptNoRetry())
			restored = true
		}
	}
	defer restoreSchema()
	var sqlErr *pgconn.PgError
	if applied, err := CloseContractWithReport(f.ctx, report); applied || !errors.As(err, &sqlErr) || sqlErr.Code != sqlState {
		t.Fatal("missing required history schema became an accepted close", applied, err)
	}
	assertCloseReportCounts(t, f.ctx, f.contractId, 0, 0)
	assertCloseReportLegacyReceipts(t, f.ctx, f.contractId, 0)
	var probe *originalCloseTransactionProbe
	var ownerErr error
	server.HandleError(func() {
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO synthetic_close_schema_owner(value) VALUES(1)`))
			probe = &originalCloseTransactionProbe{PgTx: tx}
			server.Raise(tx.QueryRow(f.ctx, `SELECT pg_backend_pid()`).Scan(&probe.callerBackendPid))
			_, err := closeContractReportInTx(f.ctx, probe, report)
			server.Raise(err)
		}, server.TxReadCommitted, server.OptNoRetry())
	}, func(err error) { ownerErr = err })
	if probe.beginCalls != 0 || probe.historyReads != 1 || !errors.As(ownerErr, &sqlErr) || sqlErr.Code != sqlState {
		t.Fatalf("missing required schema did not abort caller: begin=%d reads=%d err=%v", probe.beginCalls, probe.historyReads, ownerErr)
	}
	server.Db(f.ctx, func(conn server.PgConn) {
		var writes int
		server.Raise(conn.QueryRow(f.ctx, `SELECT count(*) FROM synthetic_close_schema_owner`).Scan(&writes))
		if writes != 0 {
			t.Fatal("missing required schema committed an earlier caller write")
		}
	})
	assertCloseReportCounts(t, f.ctx, f.contractId, 0, 0)
	assertCloseReportLegacyReceipts(t, f.ctx, f.contractId, 0)
	restoreSchema()
	if applied, err := CloseContractWithReport(f.ctx, report); !applied || err != nil {
		t.Fatal("schema recovery failed to admit the exact original", applied, err)
	}
	if applied, err := CloseContractWithReport(f.ctx, report); applied || err != nil {
		t.Fatal("schema recovery repeated the admitted original", applied, err)
	}
	original, registration := retainedCloseOriginal(t, report)
	if !bytes.Equal(original, report.OriginalReport) || !bytes.Equal(registration, record.RegistrationBytes) || retainedCloseKeyIssue(t, report) != "" {
		t.Fatal("schema recovery lost the original or its exact registration")
	}
	assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	assertCloseReportLegacyReceipts(t, f.ctx, f.contractId, 1)
}

// The history relation is a migration prerequisite, not optional evidence.
func TestContractCloseOriginalMissingHistoryRelationRollsBackAndRecovers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		originalCloseUnavailableSchemaTest(t,
			`ALTER TABLE st_client_key_history RENAME TO synthetic_close_history_unavailable`,
			`ALTER TABLE synthetic_close_history_unavailable RENAME TO st_client_key_history`, "42P01")
	})
}

// An incomplete rollout has a relation but cannot satisfy the immutable read.
func TestContractCloseOriginalMissingHistoryColumnRollsBackAndRecovers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		originalCloseUnavailableSchemaTest(t,
			`ALTER TABLE st_client_key_history RENAME COLUMN registration_hash TO synthetic_unavailable_registration_hash`,
			`ALTER TABLE st_client_key_history RENAME COLUMN synthetic_unavailable_registration_hash TO registration_hash`, "42703")
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
