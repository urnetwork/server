// Every closer removes the closed contract's queued scheduled close in its own
// commit, except a row a live claim leases or another transaction holds.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// The queued close's stored function; the model only matches its run-once key.
const syntheticScheduledCloseFunctionName = "github.com/urnetwork/server/taskworker/work.CloseScheduledContract"

// Stores a queued close in the startup pass's shape. A zero claim time is an
// unclaimed row; otherwise the claim leases it until claim plus the task lease.
func insertSyntheticScheduledClose(ctx context.Context, contractId server.Id, deadline time.Time, claimTime time.Time) {
	releaseTime := time.Time{}
	if !claimTime.IsZero() {
		releaseTime = claimTime.Add(task.TaskLeaseTimeout)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO pending_task (
			task_id,function_name,args_json,client_address,client_address_port,run_at,run_once_key,
			run_priority,run_max_time_seconds,claim_time,release_time)
			VALUES($1,$2,$3,'',0,$4,$5,$6,30,$7,$8)`,
			server.NewId(), syntheticScheduledCloseFunctionName,
			`{"_private_task_arguments":true,"contract_id":"`+contractId.String()+`","deadline":"`+deadline.Format(time.RFC3339Nano)+`"}`,
			deadline, ScheduledContractCloseRunOnce(contractId).String(), task.DefaultPriority, claimTime, releaseTime))
	})
}

// The contracts whose queued close is still pending.
func readSyntheticScheduledCloses(ctx context.Context, contractIds []server.Id) map[server.Id]bool {
	keyContractIds := map[string]server.Id{}
	keys := []string{}
	for _, contractId := range contractIds {
		key := ScheduledContractCloseRunOnce(contractId).String()
		keyContractIds[key] = contractId
		keys = append(keys, key)
	}
	pending := map[server.Id]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT run_once_key FROM pending_task WHERE run_once_key=ANY($1)`, keys)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var key string
				server.Raise(rows.Scan(&key))
				pending[keyContractIds[key]] = true
			}
		})
	})
	return pending
}

// A deadline close and an ordinary settlement each remove their own queued
// close. A leased row and a row another transaction holds stay for their
// claim, and the close does not wait for the holder. Before closers removed
// queued closes, every row outlived its contract.
func TestContractCloseRemovesItsQueuedScheduledClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newFreeExpiryOwnerFixture(ctx)
		ids := make([]server.Id, 5)
		for index := range ids {
			id, err := CreateContractNoEscrow(ctx, f.networkId, f.sourceId, f.networkId, f.destinationId, 100)
			server.Raise(err)
			ids[index] = id
		}
		deadlineClosed, settled, leased, held, open := ids[0], ids[1], ids[2], ids[3], ids[4]
		server.Tx(ctx, func(tx server.PgTx) {
			// Queued closes exist only for legacy contracts without a stored deadline.
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=NULL WHERE contract_id=ANY($1)`, ids))
		})
		deadline := server.NowUtc().Add(-time.Hour).Truncate(time.Microsecond)
		for _, id := range ids {
			claimTime := time.Time{}
			if id == leased {
				claimTime = server.NowUtc()
			}
			insertSyntheticScheduledClose(ctx, id, deadline, claimTime)
		}

		// Another transaction holds the row lock until the closes have returned.
		holding, release, holderDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(holderDone)
			server.Tx(t.Context(), func(tx server.PgTx) {
				var found bool
				server.Raise(tx.QueryRow(t.Context(), `SELECT true FROM pending_task WHERE run_once_key=$1 FOR UPDATE`,
					ScheduledContractCloseRunOnce(held).String()).Scan(&found))
				close(holding)
				<-release
			})
		}()
		<-holding
		released := false
		defer func() {
			if !released {
				close(release)
			}
			<-holderDone
		}()

		for _, id := range []server.Id{deadlineClosed, leased, held} {
			if closed, err := ReconcileContractAtDeadline(ctx, id, deadline); err != nil || closed.AlreadyClosed || closed.Missing {
				t.Fatal("deadline close did not close its contract", err)
			}
		}
		server.Raise(CloseContract(ctx, settled, f.sourceId, 17, false))
		server.Raise(CloseContract(ctx, settled, f.destinationId, 17, false))
		if _, terminal := GetContractClose(ctx, settled); !terminal {
			t.Fatal("bilateral final reports did not settle the contract")
		}
		// Every close returned while the row lock was still held.
		pending := readSyntheticScheduledCloses(ctx, ids)
		close(release)
		released = true
		<-holderDone
		if pending[deadlineClosed] || pending[settled] {
			t.Fatal("a closer left its contract's queued scheduled close", pending[deadlineClosed], pending[settled])
		}
		if !pending[leased] || !pending[held] || !pending[open] {
			t.Fatal("a closer removed a leased, held or open contract's queued close", pending[leased], pending[held], pending[open])
		}
		if after := readSyntheticScheduledCloses(ctx, ids); !after[held] || len(after) != 3 {
			t.Fatal("the held queued close did not survive its holder", after)
		}
	})
}

// The cohort settlement closes many legacy contracts in one batch; each one's
// queued close leaves with it.
func TestLegacyFinancialCohortRemovesQueuedScheduledCloses(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacyFinancialCohortSeed(t, ctx, 8)
		deadline := server.NowUtc().Add(-time.Hour).Truncate(time.Microsecond)
		for _, id := range f.ids {
			insertSyntheticScheduledClose(ctx, id, deadline, time.Time{})
		}
		page, err := FlushLegacySettlements(ctx, 1, nil, 8)
		if err != nil || page.Visited != 8 || page.Completed != 8 || page.Failed != 0 {
			t.Fatal("public cohort did not complete", page, err)
		}
		legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		if pending := readSyntheticScheduledCloses(ctx, f.ids); len(pending) != 0 {
			t.Fatal("cohort settlement left queued scheduled closes", len(pending))
		}
	})
}
