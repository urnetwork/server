// A close report holds its transaction's server only for work that needs the
// contract lock: no key-history read, no signature check, no repeated read.
package model

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server"
)

// Counts the exchanges a close makes with the server inside its caller's
// transaction. One statement or one batch is one round trip.
type closeReportRoundTripProbe struct {
	server.PgTx
	roundTrips   int
	historyReads int
}

func (self *closeReportRoundTripProbe) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	self.roundTrips++
	if strings.Contains(sql, "FROM st_client_key_history") {
		self.historyReads++
	}
	return self.PgTx.Query(ctx, sql, args...)
}

func (self *closeReportRoundTripProbe) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	self.roundTrips++
	return self.PgTx.QueryRow(ctx, sql, args...)
}

func (self *closeReportRoundTripProbe) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	self.roundTrips++
	return self.PgTx.Exec(ctx, sql, args...)
}

func (self *closeReportRoundTripProbe) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	self.roundTrips++
	return self.PgTx.SendBatch(ctx, batch)
}

// Runs one close in its own committed transaction behind a probe.
func closeReportRoundTrips(t testing.TB, ctx context.Context, close func(server.PgTx)) *closeReportRoundTripProbe {
	t.Helper()
	var probe *closeReportRoundTripProbe
	server.Tx(ctx, func(tx server.PgTx) {
		probe = &closeReportRoundTripProbe{PgTx: tx}
		close(probe)
	}, server.TxReadCommitted, server.OptNoRetry())
	return probe
}

// A new report takes the locked reads, the reservation and the increment, its
// receipt and the sender's order included. An exact retry takes the reads alone.
func TestContractCloseReportAdmissionRoundTrips(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := f.report()
		probe := closeReportRoundTrips(t, f.ctx, func(tx server.PgTx) {
			if applied, err := closeContractReportInTx(f.ctx, tx, report); !applied || err != nil {
				t.Fatal("new report was not admitted", applied, err)
			}
		})
		if probe.roundTrips != 3 || probe.historyReads != 0 {
			t.Fatalf("new report round trips=%d history reads=%d, want 3 and 0", probe.roundTrips, probe.historyReads)
		}
		probe = closeReportRoundTrips(t, f.ctx, func(tx server.PgTx) {
			if applied, err := closeContractReportInTx(f.ctx, tx, report); applied || err != nil {
				t.Fatal("exact retry was not acknowledged without work", applied, err)
			}
		})
		if probe.roundTrips != 1 {
			t.Fatalf("exact retry round trips=%d, want 1", probe.roundTrips)
		}
		// The shared owner publishes an identified receipt with its increment.
		identified := f.report()
		probe = closeReportRoundTrips(t, f.ctx, func(tx server.PgTx) {
			if applied, _, err := applyContractCloseReportInTx(f.ctx, tx, identified.ContractId, identified.ClientId, identified.AckedByteCount, identified.Checkpoint, &identified.ReportId); !applied || err != nil {
				t.Fatal("identified report was not applied", applied, err)
			}
		})
		if probe.roundTrips != 3 {
			t.Fatalf("identified report round trips=%d, want 3", probe.roundTrips)
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 40)
		assertCloseReportLegacyReceipts(t, f.ctx, f.contractId, 2)
	})
}

// An in-transaction caller still reads key history in its own transaction,
// once, after the locked reads.
func TestContractCloseReportSignedAdmissionRoundTrips(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, input, record, key := originalCloseAvailabilityFixture(t)
		domain, _ := input.Domain.Digest()
		report := signCloseReportOriginal(t, f.report(), domain, key)
		probe := closeReportRoundTrips(t, f.ctx, func(tx server.PgTx) {
			if applied, err := closeContractReportInTx(f.ctx, tx, report); !applied || err != nil {
				t.Fatal("signed report was not admitted", applied, err)
			}
		})
		if probe.roundTrips != 4 || probe.historyReads != 1 {
			t.Fatalf("signed report round trips=%d history reads=%d, want 4 and 1", probe.roundTrips, probe.historyReads)
		}
		if _, registration := retainedCloseOriginal(t, report); !bytes.Equal(registration, record.RegistrationBytes) {
			t.Fatal("signed report lost its exact registration")
		}
	})
}

// The outcome of one public close run beside a test.
type closeReportOutcome struct {
	applied bool
	err     error
}

// Starts the public close behind a synthetic history barrier and returns the
// pid of the backend reading key history once it waits on the barrier.
func startBlockedCloseHistoryRead(t testing.TB, ctx context.Context, holder pgx.Tx, holderPid int, lockKey int64, run func() closeReportOutcome) (readerPid int, finished <-chan closeReportOutcome) {
	t.Helper()
	outcomes := make(chan closeReportOutcome, 1)
	go func() {
		var outcome closeReportOutcome
		server.HandleError(func() { outcome = run() }, func(err error) { outcome.err = err })
		outcomes <- outcome
	}()
	for {
		var pids []int
		// Lock-manager state is live; it names the reader by its waiting lock.
		rows, err := holder.Query(ctx, `SELECT pid FROM pg_locks
		 WHERE locktype='advisory' AND NOT granted AND classid=0::oid
		 AND objid=$2::oid AND objsubid=1 AND $1::int=ANY(pg_blocking_pids(pid))`, holderPid, lockKey)
		if err == nil {
			pids, err = pgx.CollectRows(rows, pgx.RowTo[int])
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(pids) == 1 {
			return pids[0], outcomes
		}
		select {
		case outcome := <-outcomes:
			t.Fatal("close escaped the history barrier", outcome.applied, outcome.err)
		case <-ctx.Done():
			t.Fatal("close did not reach the history barrier", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

// A signed report reads key history before its transaction begins: while the
// read waits, its backend has no transaction id and the contract row is free.
func TestContractCloseReportSignedHistoryReadHoldsNoTransaction(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, input, record, key := originalCloseAvailabilityFixture(t)
		domain, _ := input.Domain.Digest()
		report := signCloseReportOriginal(t, f.report(), domain, key)
		const lockKey = 73472611
		restore := installCloseHistoryReadFunction(t, `BEGIN PERFORM pg_advisory_xact_lock(73472611); RETURN value; END`)
		defer restore()
		ctx, cancel := context.WithTimeout(f.ctx, 2*time.Minute)
		defer cancel()
		var finished <-chan closeReportOutcome
		server.Db(ctx, func(conn server.PgConn) {
			holder, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer rollbackCloseReportTestTransaction(ctx, holder)
			var holderPid int
			server.Raise(holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
			server.RaisePgResult(holder.Exec(ctx, `SELECT pg_advisory_xact_lock(73472611)`))
			var readerPid int
			readerPid, finished = startBlockedCloseHistoryRead(t, ctx, holder, holderPid, lockKey, func() closeReportOutcome {
				applied, err := CloseContractWithReport(ctx, report)
				return closeReportOutcome{applied: applied, err: err}
			})
			// A fresh statement reads current activity, not a cached snapshot.
			var readerXid *string
			server.Db(ctx, func(observer server.PgConn) {
				server.Raise(observer.QueryRow(ctx, `SELECT backend_xid::text FROM pg_stat_activity WHERE pid=$1`, readerPid).Scan(&readerXid))
			})
			if readerXid != nil {
				t.Fatal("key history read ran in a transaction that had already locked or written")
			}
			server.Tx(ctx, func(tx server.PgTx) {
				if _, err := tx.Exec(ctx, `SELECT 1 FROM transfer_contract WHERE contract_id=$1 FOR UPDATE NOWAIT`, f.contractId); err != nil {
					t.Fatal("key history read held the contract row lock", err)
				}
			}, server.TxReadCommitted, server.OptNoRetry())
			server.Raise(holder.Rollback(ctx))
		})
		select {
		case outcome := <-finished:
			if !outcome.applied || outcome.err != nil {
				t.Fatal("signed report was not admitted after its history read", outcome.applied, outcome.err)
			}
		case <-ctx.Done():
			t.Fatal("signed report did not finish", ctx.Err())
		}
		if _, registration := retainedCloseOriginal(t, report); !bytes.Equal(registration, record.RegistrationBytes) || retainedCloseKeyIssue(t, report) != "" {
			t.Fatal("history read before the transaction lost the exact registration")
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	})
}

// A caller that stops during the history read before BEGIN lets the read
// finish on its connection, then begins no transaction.
func TestContractCloseReportStoppedHistoryReadFinishesOnConnection(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f, input, record, key := originalCloseAvailabilityFixture(t)
		domain, _ := input.Domain.Digest()
		report := signCloseReportOriginal(t, f.report(), domain, key)
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE TABLE synthetic_close_history_finished(pid integer)`))
		})
		const lockKey = 73472612
		// A row reaches the marker table only if the read's own statement commits.
		restore := installCloseHistoryReadFunction(t, `BEGIN PERFORM pg_advisory_xact_lock(73472612);
		 INSERT INTO synthetic_close_history_finished(pid) VALUES (pg_backend_pid()); RETURN value; END`)
		defer restore()
		ctx, cancel := context.WithTimeout(f.ctx, 2*time.Minute)
		defer cancel()
		reportCtx, stopReport := context.WithCancel(ctx)
		defer stopReport()
		var finished <-chan closeReportOutcome
		server.Db(ctx, func(conn server.PgConn) {
			holder, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer rollbackCloseReportTestTransaction(ctx, holder)
			var holderPid int
			server.Raise(holder.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holderPid))
			server.RaisePgResult(holder.Exec(ctx, `SELECT pg_advisory_xact_lock(73472612)`))
			_, finished = startBlockedCloseHistoryRead(t, ctx, holder, holderPid, lockKey, func() closeReportOutcome {
				applied, err := CloseContractWithReport(reportCtx, report)
				return closeReportOutcome{applied: applied, err: err}
			})
			// Stop the caller while the read waits. pgx interrupts an unbounded
			// round trip at once; the bounded read keeps waiting on the barrier,
			// so the close cannot return before the barrier is released, well
			// inside server.PgStatementStopTimeout.
			stopReport()
			select {
			case outcome := <-finished:
				t.Fatal("caller's stop interrupted the history read's round trip", outcome.applied, outcome.err)
			case <-time.After(time.Second):
			}
			server.Raise(holder.Rollback(ctx))
		})
		select {
		case outcome := <-finished:
			if outcome.applied || !errors.Is(outcome.err, context.Canceled) {
				t.Fatal("stopped close admitted work or lost its stop", outcome.applied, outcome.err)
			}
		case <-ctx.Done():
			t.Fatal("stopped close did not finish", ctx.Err())
		}
		var reads int
		server.Db(f.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(f.ctx, `SELECT count(*) FROM synthetic_close_history_finished`).Scan(&reads))
		})
		if reads == 0 {
			t.Fatal("caller's stop interrupted the history read's round trip")
		}
		assertCloseReportCounts(t, f.ctx, f.contractId, 0, 0)
		restore()
		if applied, err := CloseContractWithReport(f.ctx, report); !applied || err != nil {
			t.Fatal("stopped original could not recover", applied, err)
		}
		if _, registration := retainedCloseOriginal(t, report); !bytes.Equal(registration, record.RegistrationBytes) {
			t.Fatal("recovered original lost its exact registration")
		}
	})
}

// An exact retry whose original already committed reads it without taking the
// contract lock, even while another owner holds that lock.
func TestContractCloseReportCommittedRetryTakesNoLock(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		report := f.report()
		if applied, err := CloseContractWithReport(f.ctx, report); !applied || err != nil {
			t.Fatal("original report was not admitted", applied, err)
		}
		changed := report
		changed.AckedByteCount++
		ctx, cancel := context.WithTimeout(f.ctx, 2*time.Minute)
		defer cancel()
		server.Db(ctx, func(conn server.PgConn) {
			holder, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer rollbackCloseReportTestTransaction(ctx, holder)
			server.RaisePgResult(holder.Exec(ctx, `SELECT 1 FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, f.contractId))
			for _, retry := range []struct {
				report ContractCloseReport
				err    error
			}{{report, nil}, {changed, ErrContractCloseReportConflict}} {
				server.Tx(ctx, func(tx server.PgTx) {
					// A lock wait fails at once instead of hanging the test.
					server.RaisePgResult(tx.Exec(ctx, `SET LOCAL lock_timeout = '1s'`))
					if applied, err := closeContractReportInTx(ctx, tx, retry.report); applied || !errors.Is(err, retry.err) {
						t.Fatal("committed identity did not answer without the contract lock", applied, err)
					}
				}, server.TxReadCommitted, server.OptNoRetry())
			}
		})
		assertCloseReportCounts(t, f.ctx, f.contractId, 1, 20)
	})
}

// The receiver's report follows the sender's final only when that final
// committed first; the accumulation statement's snapshot orders the two.
func TestContractCloseReportSenderFinalOrderFromAccumulation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportFixture(t)
		apply := func(contractId, clientId server.Id, checkpoint bool, reportId *server.Id) (applied, follows bool) {
			server.Tx(f.ctx, func(tx server.PgTx) {
				var err error
				applied, _, _, follows, err = applyContractCloseReportOrderInTx(f.ctx, tx, contractId, clientId, 20, checkpoint, reportId, nil)
				server.Raise(err)
			}, server.TxReadCommitted, server.OptNoRetry())
			return
		}
		admit := func(report ContractCloseReport) contractCloseReportAdmission {
			var admission contractCloseReportAdmission
			server.Tx(f.ctx, func(tx server.PgTx) {
				var err error
				admission, err = admitContractCloseReportInTx(f.ctx, tx, report, func() originalCloseKeyRead { return originalCloseKeyRead{} })
				server.Raise(err)
			}, server.TxReadCommitted, server.OptNoRetry())
			return admission
		}
		// The receiver checkpoint before the sender's final does not follow it,
		// and neither does the sender's own final.
		if applied, follows := apply(f.contractId, f.destinationId, true, nil); !applied || follows {
			t.Fatal("receiver checkpoint before the sender's final followed it", applied, follows)
		}
		if applied, follows := apply(f.contractId, f.sourceId, false, nil); !applied || follows {
			t.Fatal("sender's own final followed itself", applied, follows)
		}
		reportId := server.NewId()
		if applied, follows := apply(f.contractId, f.destinationId, true, &reportId); !applied || !follows {
			t.Fatal("receiver report after the sender's final did not follow it", applied, follows)
		}
		// A sender checkpoint is not a final; the identified admission sees
		// the final once it commits.
		if applied, follows := apply(f.otherContractId, f.sourceId, true, nil); !applied || follows {
			t.Fatal("sender checkpoint followed itself", applied, follows)
		}
		receiver := ContractCloseReport{ReportId: server.NewId(), ContractId: f.otherContractId, ClientId: f.destinationId, AckedByteCount: 20, Checkpoint: true}
		if admission := admit(receiver); !admission.applied || admission.followsSenderFinal {
			t.Fatal("receiver report after a sender checkpoint followed a final", admission)
		}
		if applied, follows := apply(f.otherContractId, f.sourceId, false, nil); !applied || follows {
			t.Fatal("sender's final followed itself", applied, follows)
		}
		receiver.ReportId = server.NewId()
		if admission := admit(receiver); !admission.applied || !admission.followsSenderFinal {
			t.Fatal("identified receiver report after the sender's final did not follow it", admission)
		}
		// A report that no longer applies publishes no receipt.
		late := server.NewId()
		if applied, follows := apply(f.contractId, f.sourceId, true, &late); applied || follows {
			t.Fatal("report after the party's final applied", applied, follows)
		}
		assertCloseReportLegacyReceipts(t, f.ctx, f.contractId, 1)
		assertCloseReportLegacyReceipts(t, f.ctx, f.otherContractId, 2)
	})
}
