// Optional registration availability must not erase authenticated completed work.
package model

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/startifact"
)

// Read the diagnostic from the immutable admission, independently of API success.
func retainedCloseKeyIssue(t testing.TB, report ContractCloseReport) string {
	t.Helper()
	var issue string
	server.Db(t.Context(), func(conn server.PgConn) {
		server.Raise(conn.QueryRow(t.Context(), `SELECT original_key_issue FROM contract_close_report_evidence WHERE client_id=$1 AND report_id=$2`, report.ClientId, report.ReportId).Scan(&issue))
	})
	return issue
}

// The exact ordinary key producer supplies a valid cryptographic history row.
func originalCloseAvailabilityFixture(t testing.TB) (*closeReportFixture, StClientKeyRegistrationInput, *StClientKeyHistoryRecord, ed25519.PrivateKey) {
	t.Helper()
	f := newCloseReportFixture(t)
	input := newStClientKeyHistoryTestInput(t)
	input.ClientID = f.sourceId
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{61}, 32))
	input.PublicKey = key[32:]
	record, err := StoreStClientKeyRegistration(f.ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	return f, input, record, key
}

// A synthetic larger immutable source reaches the public reader's sentinel.
// Every row remains signed and linked; no malformed-row refusal masks capacity.
func TestContractCloseOriginalHistoryCapacityKeepsObligation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, input, first, _ := originalCloseAvailabilityFixture(t)
		previous := first.Registration
		domain, _ := input.Domain.Digest()
		server.Tx(f.ctx, func(tx server.PgTx) {
			for generation := uint64(2); generation <= MaxStClientKeyHistoryRegistrations+1; generation++ {
				next := previous
				next.Generation = generation
				var err error
				next.PreviousHash, err = previous.ContentHash()
				server.Raise(err)
				next.PublicKey = [32]byte(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(63 + generation%2)}, 32))[32:])
				next.EffectiveBoundary.Block++
				next.EffectiveBoundary.Hash = sha256.Sum256([]byte(fmt.Sprintf("synthetic-capacity-%d", generation)))
				server.Raise(protocol.SignClientKeyRegistration(&next, input.RootKey))
				server.Raise(next.Follows(&previous))
				registration, err := next.Bytes()
				server.Raise(err)
				hash := sha256.Sum256(registration)
				evidence, evidenceHash, err := startifact.SealClientKeyRegistrationEvidence(input.DeploymentID, next, input.ArtifactKey, input.CreatedAt)
				server.Raise(err)
				server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO st_client_key_history(client_id,generation,domain_hash,registration_hash,registration,evidence_hash,evidence) VALUES($1,$2,$3,$4,$5,$6,$7)`, f.sourceId, generation, domain[:], hash[:], registration, evidenceHash, evidence))
				previous = next
			}
		})
		// This report's independently valid key is absent, so the reader must visit
		// the sentinel instead of succeeding at an early matching registration.
		report := signCloseReportOriginal(t, f.report(), domain, ed25519.NewKeyFromSeed(bytes.Repeat([]byte{62}, 32)))
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || !applied {
			t.Fatal("optional history capacity blocked authenticated completed work", applied, err)
		}
		original, registration := retainedCloseOriginal(t, report)
		if !bytes.Equal(original, report.OriginalReport) || len(registration) != 0 || retainedCloseKeyIssue(t, report) != originalCloseKeyCapacity {
			t.Fatal("history overflow lost the original or invented registration authority")
		}
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || applied {
			t.Fatal("overcapacity original retry repeated its increment", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	})
}

// Replace only the optional read relation in the isolated test database. The
// actual original table, immutable triggers and head foreign keys keep custody.
func installCloseHistoryReadFunction(t testing.TB, functionBody string) func() {
	t.Helper()
	ctx := t.Context()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE st_client_key_history RENAME TO synthetic_close_history_original;
   CREATE FUNCTION synthetic_close_history_read(value bytea) RETURNS bytea LANGUAGE plpgsql VOLATILE AS $$ `+functionBody+` $$;
   CREATE VIEW st_client_key_history AS SELECT client_id,generation,domain_hash,registration_hash,synthetic_close_history_read(registration) AS registration,evidence_hash,evidence FROM synthetic_close_history_original;`))
	})
	restored := false
	restore := func() {
		if restored {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		server.Tx(cleanupCtx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(cleanupCtx, `DROP VIEW st_client_key_history; ALTER TABLE synthetic_close_history_original RENAME TO st_client_key_history; DROP FUNCTION synthetic_close_history_read(bytea);`))
		})
		restored = true
	}
	return restore
}

// A real PostgreSQL read error aborts its savepoint, not the admitted byte
// increment. Recovery cannot backfill evidence into the first original report.
func TestContractCloseOriginalSqlReadFailureKeepsOriginalAndRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, input, record, key := originalCloseAvailabilityFixture(t)
		domain, _ := input.Domain.Digest()
		report := signCloseReportOriginal(t, f.report(), domain, key)
		restore := installCloseHistoryReadFunction(t, `BEGIN RAISE EXCEPTION 'synthetic optional history read unavailable'; END`)
		defer restore()
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || !applied {
			t.Fatal("optional SQL read error poisoned the accounting transaction", applied, err)
		}
		original, registration := retainedCloseOriginal(t, report)
		if !bytes.Equal(original, report.OriginalReport) || len(registration) != 0 || retainedCloseKeyIssue(t, report) != originalCloseKeyUnavailable {
			t.Fatal("optional SQL refusal lost original bytes or acquired registration")
		}
		restore()
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || applied {
			t.Fatal("recovered optional history repeated original work", applied, err)
		}
		_, registration = retainedCloseOriginal(t, report)
		if len(registration) != 0 || retainedCloseKeyIssue(t, report) != originalCloseKeyUnavailable {
			t.Fatal("later history was backfilled as original registration")
		}
		sibling := signCloseReportOriginal(t, f.report(), domain, key)
		if applied, err := CloseContractWithReport(f.ctx, sibling); err != nil || !applied {
			t.Fatal(applied, err)
		}
		_, registration = retainedCloseOriginal(t, sibling)
		if !bytes.Equal(registration, record.RegistrationBytes) || retainedCloseKeyIssue(t, sibling) != "" {
			t.Fatal("healthy later owner did not admit its own original registration")
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 2, 40)
	})
}

// The public close is canceled only after PostgreSQL proves its optional read
// is waiting. Its exact original can then recover under a fresh healthy owner.
func TestContractCloseOriginalCanceledHistoryReadRollsBackThenRecovers(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, input, record, key := originalCloseAvailabilityFixture(t)
		domain, _ := input.Domain.Digest()
		report := signCloseReportOriginal(t, f.report(), domain, key)
		restore := installCloseHistoryReadFunction(t, `BEGIN PERFORM pg_advisory_xact_lock(73472601); RETURN value; END`)
		defer restore()
		ctx, cancel := context.WithTimeout(f.ctx, 2*time.Minute)
		defer cancel()
		reportCtx, cancelReport := context.WithCancel(ctx)
		defer cancelReport()
		type result struct {
			applied bool
			err     error
		}
		finished := make(chan result, 1)
		joined := make(chan struct{})
		started := false
		defer func() {
			cancelReport()
			if started {
				select {
				case <-joined:
				case <-time.After(time.Minute):
					t.Error("canceled optional reader did not join cleanup")
				}
			}
		}()
		server.Db(ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer rollbackCloseReportTestTransaction(ctx, tx)
			var holderPid int
			server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
			server.RaisePgResult(tx.Exec(ctx, `SELECT pg_advisory_xact_lock(73472601)`))
			// Force the failed ordering: PostgreSQL caches this activity snapshot
			// before the worker starts, even in a read-committed transaction.
			const activitySql = `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)) AND query LIKE '%FROM st_client_key_history%')`
			var cachedBlocked bool
			server.Raise(tx.QueryRow(ctx, activitySql, holderPid).Scan(&cachedBlocked))
			if cachedBlocked {
				t.Fatal("optional reader existed before its owner started")
			}
			started = true
			go func() {
				defer close(joined)
				var observed result
				server.HandleError(func() { observed.applied, observed.err = CloseContractWithReport(reportCtx, report) }, func(err error) { observed.err = err })
				finished <- observed
			}()
			for {
				var blocked bool
				// Lock-manager state is live and identifies the optional reader's
				// exact one-bigint advisory lock without cached query text.
				server.Raise(tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks
				 WHERE locktype='advisory' AND NOT granted AND classid=0::oid
				 AND objid=73472601::oid AND objsubid=1
				 AND $1::int=ANY(pg_blocking_pids(pid)))`, holderPid).Scan(&blocked))
				if blocked {
					break
				}
				select {
				case value := <-finished:
					t.Fatal("optional reader escaped the actual database barrier", value.applied, value.err)
				case <-ctx.Done():
					t.Fatal("original optional reader did not reach actual database barrier", ctx.Err())
				case <-time.After(time.Millisecond):
				}
			}
			server.Raise(tx.QueryRow(ctx, activitySql, holderPid).Scan(&cachedBlocked))
			if cachedBlocked {
				t.Fatal("activity snapshot did not preserve the forced pre-worker ordering")
			}
			cancelReport()
			select {
			case value := <-finished:
				if value.applied || !errors.Is(value.err, context.Canceled) || errors.Is(value.err, ErrContractCloseOriginalIntegrity) {
					t.Fatal("owned read cancellation admitted work or became integrity", value.applied, value.err)
				}
			case <-ctx.Done():
				t.Fatal("canceled optional reader did not join", ctx.Err())
			}
			server.Raise(tx.Rollback(ctx))
		})
		assertCloseReportCounts(t, f.ctx, f.contractId, 0, 0)
		restore()
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || !applied {
			t.Fatal("fresh close owner could not recover the exact signed original", applied, err)
		}
		_, registration := retainedCloseOriginal(t, report)
		if !bytes.Equal(registration, record.RegistrationBytes) || retainedCloseKeyIssue(t, report) != "" {
			t.Fatal("recovery manufactured or lost registration authority")
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	})
}

// Returned contradictory source bytes remain a refusal, even though unavailable
// optional history is allowed. A healthy retry proves only this admission rolled back.
func TestContractCloseOriginalContradictoryHistoryRefusesScopedAdmission(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, input, record, key := originalCloseAvailabilityFixture(t)
		domain, _ := input.Domain.Digest()
		report := signCloseReportOriginal(t, f.report(), domain, key)
		restore := installCloseHistoryReadFunction(t, `BEGIN RETURN set_byte(value,octet_length(value)-1,get_byte(value,octet_length(value)-1) # 1); END`)
		defer restore()
		if applied, err := CloseContractWithReport(f.ctx, report); applied || !errors.Is(err, ErrContractCloseOriginalIntegrity) {
			t.Fatal("contradictory original registration became optional absence", applied, err)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 0, 0)
		sibling := f.report()
		if applied, err := CloseContractWithReport(f.ctx, sibling); err != nil || !applied {
			t.Fatal("unrelated unsigned close inherited registration refusal", applied, err)
		}
		restore()
		if applied, err := CloseContractWithReport(f.ctx, report); err != nil || !applied {
			t.Fatal("correct original registration could not recover", applied, err)
		}
		_, registration := retainedCloseOriginal(t, report)
		if !bytes.Equal(registration, record.RegistrationBytes) || retainedCloseKeyIssue(t, report) != "" {
			t.Fatal("recovered registration differs")
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 2, 40)
	})
}
