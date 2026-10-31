// Retryable legacy conflicts preserve atomic journal births and unsigned gaps.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// A successful earlier row/event must disappear when a later endpoint conflicts;
// the retry then appends exactly once without certifying the unsigned mutation.
func TestProviderWorkSessionLegacyConflictRollsBackEarlierJournalAndRetries(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
		defer cancel()
		type journalState struct {
			sequence uint64
			events   int
			receipts int
		}
		readState := func(tx server.PgTx, clientId server.Id) journalState {
			var state journalState
			server.Raise(tx.QueryRow(ctx, `SELECT sequence,
 (SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
 (SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1)
 FROM provider_work_session_head WHERE client_id=$1`, clientId).Scan(&state.sequence, &state.events, &state.receipts))
			return state
		}
		before := map[server.Id]journalState{}
		server.Db(ctx, func(conn server.PgConn) {
			owner, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer rollbackCloseReportTestTransaction(ctx, owner)
			for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
				before[clientId] = readState(owner, clientId)
			}
			// The held endpoint is the barrier: the legacy row trigger must
			// reject immediately instead of waiting while owning that row.
			providerWorkLockSessionMutationInTx(ctx, owner, f.destinationId)
			server.Db(ctx, func(other server.PgConn) {
				legacy, err := other.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
				server.Raise(err)
				defer rollbackCloseReportTestTransaction(ctx, legacy)
				server.RaisePgResult(legacy.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, f.sourceConnectionId, server.NowUtc()))
				written := readState(legacy, f.sourceId)
				if written.sequence != before[f.sourceId].sequence+1 || written.events != before[f.sourceId].events+1 || written.receipts != before[f.sourceId].receipts {
					t.Fatal("legacy first row did not append one unsigned event", written)
				}
				_, err = legacy.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, f.destinationConnectionId, server.NowUtc())
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
					t.Fatal("same-endpoint legacy conflict did not request transaction retry", err)
				}
				server.Raise(legacy.Rollback(ctx))
			})
			// NOWAIT proves both connection rows were released by the failed
			// transaction while the current owner still holds its fence.
			rows, err := owner.Query(ctx, `SELECT connection_id FROM network_client_connection WHERE connection_id=ANY($1) AND connected AND disconnect_time IS NULL FOR UPDATE NOWAIT`, []server.Id{f.sourceConnectionId, f.destinationConnectionId})
			server.Raise(err)
			count := 0
			for rows.Next() {
				count++
			}
			server.Raise(rows.Err())
			rows.Close()
			if count != 2 {
				t.Fatal("failed multirow transaction leaked a connection mutation", count)
			}
			for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
				if after := readState(owner, clientId); after != before[clientId] {
					t.Fatal("failed transaction leaked an event, receipt, or head gap", clientId, before[clientId], after)
				}
			}
			server.Raise(owner.Rollback(ctx))
		})
		server.Tx(ctx, func(tx server.PgTx) {
			for _, connectionId := range []server.Id{f.sourceConnectionId, f.destinationConnectionId} {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, connectionId, server.NowUtc()))
			}
		})
		server.Tx(ctx, func(tx server.PgTx) {
			for _, clientId := range []server.Id{f.sourceId, f.destinationId} {
				after := readState(tx, clientId)
				if after.sequence != before[clientId].sequence+1 || after.events != before[clientId].events+1 || after.receipts != before[clientId].receipts {
					t.Fatal("explicit retry duplicated an event or signed a legacy mutation", clientId, before[clientId], after)
				}
			}
		})
		id := f.contract(t)
		if providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, ctx, id), id).Reservation.Complete {
			t.Fatal("committed unsigned retry became complete signed attribution")
		}
	})
}

// A later cooperating cleanup and close cannot repair an already committed
// unsigned journal gap, and optional attribution must still allow traffic close.
func TestProviderWorkSessionUnsignedMutationSurvivesSignedCleanupAndClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, f.sourceConnectionId, server.NowUtc()))
		})
		connectionId, _, _, _, err := ConnectNetworkClientWithIpFamily(f.ctx, f.sourceId, "192.0.2.12:10003", f.handlerId, 4)
		if err != nil {
			t.Fatal("unsigned gap blocked ordinary signed admission", err)
		}
		if err := DisconnectNetworkClient(f.ctx, connectionId); err != nil {
			t.Fatal("unsigned gap blocked ordinary signed cleanup", err)
		}
		id := f.contract(t)
		receipts := providerWorkFixtureReceipts(t, f.ctx, id)
		if providerWorkFixtureReservation(t, receipts, id).Reservation.Complete {
			t.Fatal("later signed operations repaired unsigned original history")
		}
		f.close(t, id)
		foundOutcome := false
		for _, receipt := range providerWorkFixtureReceipts(t, f.ctx, id) {
			if receipt.Outcome != nil {
				foundOutcome = true
				if receipt.Outcome.SourceBytes != 121 || receipt.Outcome.DestinationBytes != 121 || receipt.Outcome.Outcome != ContractOutcomeSettled {
					t.Fatal("unsigned endpoint history changed ordinary settlement accounting", receipt.Outcome)
				}
			}
		}
		if !foundOutcome {
			t.Fatal("unsigned endpoint history suppressed the ordinary settlement original")
		}
		server.Db(f.ctx, func(conn server.PgConn) {
			var sequence uint64
			var events, originals int
			server.Raise(conn.QueryRow(f.ctx, `SELECT sequence,
 (SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
 (SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1)
 FROM provider_work_session_head WHERE client_id=$1`, f.sourceId).Scan(&sequence, &events, &originals))
			if sequence != 5 || events != 5 || originals != 2 {
				t.Fatal("signed cleanup reset or filled the unsigned journal gap", sequence, events, originals)
			}
		})
	})
}

// The production retry wrapper receives the real row-trigger error, then starts
// a fresh transaction only after an explicit first-conflict release barrier.
func TestProviderWorkSessionLegacyConflictUsesProductionTxRetry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
		defer cancel()
		ready := make(chan struct{})
		conflicted := make(chan struct{})
		released := make(chan error, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			var holderErr error
			server.HandleError(func() {
				server.Db(ctx, func(conn server.PgConn) {
					holder, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
					server.Raise(err)
					defer rollbackCloseReportTestTransaction(ctx, holder)
					server.Raise(providerWorkLockEndpointRowsInTx(ctx, holder, []server.Id{f.sourceId}))
					close(ready)
					select {
					case <-conflicted:
					case <-ctx.Done():
						server.Raise(ctx.Err())
					}
					server.Raise(holder.Rollback(ctx))
				})
			}, func(err error) { holderErr = err })
			released <- holderErr
		}()
		defer func() {
			cancel()
			select {
			case <-joined:
			case <-time.After(time.Minute):
				t.Error("retry endpoint holder did not join canceled cleanup")
			}
		}()
		select {
		case <-ready:
		case err := <-released:
			t.Fatal("endpoint holder failed before retry barrier", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		attempts := 0
		server.Tx(ctx, func(tx server.PgTx) {
			attempts++
			_, err := tx.Exec(ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, f.sourceConnectionId, server.NowUtc())
			if attempts == 1 {
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || pgErr.Code != "40001" {
					t.Fatal("first production attempt did not encounter held endpoint", err)
				}
				close(conflicted)
				select {
				case holderErr := <-released:
					server.Raise(holderErr)
				case <-ctx.Done():
					server.Raise(ctx.Err())
				}
			}
			server.Raise(err)
		}, server.TxReadCommitted)
		if attempts != 2 {
			t.Fatal("production retry did not rerun precisely once", attempts)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var connected bool
			var sequence uint64
			var events, originals int
			server.Raise(conn.QueryRow(ctx, `SELECT c.connected,h.sequence,
 (SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
 (SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1)
 FROM network_client_connection c JOIN provider_work_session_head h USING(client_id)
 WHERE c.connection_id=$2`, f.sourceId, f.sourceConnectionId).Scan(&connected, &sequence, &events, &originals))
			if connected || sequence != 3 || events != 3 || originals != 2 {
				t.Fatal("production retry leaked or duplicated a connection event", connected, sequence, events, originals)
			}
		})
	})
}

// A deferred foreign-key violation rejects commit after the row trigger has
// appended its event, proving the journal and connection remain one transaction.
func TestProviderWorkSessionCommitFailureRollsBackJournalMutation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		server.Db(f.ctx, func(conn server.PgConn) {
			tx, err := conn.BeginTx(f.ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
			server.Raise(err)
			defer rollbackCloseReportTestTransaction(f.ctx, tx)
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE TEMP TABLE provider_work_commit_parent (id int PRIMARY KEY) ON COMMIT DROP`))
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE TEMP TABLE provider_work_commit_child (parent_id int REFERENCES provider_work_commit_parent(id) DEFERRABLE INITIALLY DEFERRED) ON COMMIT DROP`))
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO provider_work_commit_child(parent_id) VALUES(1)`))
			server.RaisePgResult(tx.Exec(f.ctx, `UPDATE network_client_connection SET connected=false,disconnect_time=$2 WHERE connection_id=$1`, f.sourceConnectionId, server.NowUtc()))
			var pendingSequence uint64
			server.Raise(tx.QueryRow(f.ctx, `SELECT sequence FROM provider_work_session_head WHERE client_id=$1`, f.sourceId).Scan(&pendingSequence))
			if pendingSequence != 3 {
				t.Fatal("commit-failure fixture did not append its pending event", pendingSequence)
			}
			err = tx.Commit(f.ctx)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
				t.Fatal("deferred fault did not reject the commit", err)
			}
			var connected bool
			var sequence uint64
			var events, originals int
			server.Raise(conn.QueryRow(f.ctx, `SELECT c.connected,h.sequence,
 (SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
 (SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1)
 FROM network_client_connection c JOIN provider_work_session_head h USING(client_id)
 WHERE c.connection_id=$2`, f.sourceId, f.sourceConnectionId).Scan(&connected, &sequence, &events, &originals))
			if !connected || sequence != 2 || events != 2 || originals != 2 {
				t.Fatal("failed commit leaked a connection change or journal gap", connected, sequence, events, originals)
			}
		})
	})
}
