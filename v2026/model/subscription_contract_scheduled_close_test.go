// Exact scheduled keys retain the existing report and financial owners.
package model

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Real public reports cover every valid partial shape. Their fresh timestamps
// are after the requested deadline and the stored expiration is later still.
func TestScheduledContractCloseCapsEveryFreeReportShape(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newFreeExpiryOwnerFixture(ctx)
		cases := []struct {
			name                                                                          string
			source, destination, sourceCheckpoint, destinationCheckpoint, disputed, final bool
		}{
			{name: "empty"},
			{name: "source checkpoint", source: true, sourceCheckpoint: true},
			{name: "source final", source: true},
			{name: "destination checkpoint", destination: true, destinationCheckpoint: true},
			{name: "destination final", destination: true},
			{name: "both checkpoints", source: true, destination: true, sourceCheckpoint: true, destinationCheckpoint: true},
			{name: "source checkpoint destination final", source: true, destination: true, sourceCheckpoint: true},
			{name: "source final destination checkpoint", source: true, destination: true, destinationCheckpoint: true},
			{name: "public dispute", source: true, sourceCheckpoint: true, disputed: true},
			{name: "interrupted public final", source: true, final: true},
		}
		for _, sample := range cases {
			id, err := CreateContractNoEscrow(ctx, f.networkId, f.sourceId, f.networkId, f.destinationId, 100)
			server.Raise(err)
			deadline := server.NowUtc().Add(-time.Minute)
			if sample.source {
				server.Raise(CloseContract(ctx, id, f.sourceId, 17, sample.sourceCheckpoint))
			}
			if sample.destination {
				server.Raise(CloseContract(ctx, id, f.destinationId, 17, sample.destinationCheckpoint))
			}
			if sample.disputed {
				SetContractDispute(ctx, id, true)
			}
			if sample.final {
				interruptFreeExpiryPublicClose(t, ctx, id, f.destinationId)
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, server.NowUtc().Add(3*time.Hour)))
			})
			before := readRedisExpiryRepairTestState(ctx, id)
			if owner, err := CloseContractAtDeadline(ctx, id, time.Time{}); err == nil || owner != nil || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
				t.Fatal("invalid scheduled close changed a live contract", sample.name, err)
			}
			owner, err := CloseContractAtDeadline(ctx, id, deadline)
			if err != nil || owner != nil {
				t.Fatal("due free close did not use its source owner", sample.name, err)
			}
			if sample.name == "empty" {
				server.Db(ctx, func(conn server.PgConn) {
					var exact bool
					server.Raise(conn.QueryRow(ctx, `SELECT outcome='settled' AND NOT dispute
						AND NOT EXISTS(SELECT 1 FROM contract_close WHERE contract_id=$1)
						AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1)
						AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
						AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
						FROM transfer_contract WHERE contract_id=$1`, id).Scan(&exact))
					if !exact {
						t.Fatal("reportless free deadline lost zero-use normal closure")
					}
				})
			} else {
				if closed, terminal := GetContractClose(ctx, id); !terminal || closed.Outcome != ContractOutcomeSettled {
					t.Fatal("free deadline did not commit closure", sample.name)
				}
			}
			proof, snapshot := readContractExpiryTestSnapshot(t, ctx, id)
			wantReports := 0
			if sample.source {
				wantReports++
			}
			if sample.destination || sample.final {
				wantReports++
			}
			if snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != wantReports {
				t.Fatal("scheduled close fabricated an authenticated original report", sample.name)
			}
			for _, report := range snapshot.Expiry.Reports {
				if report.ByteCount != 17 {
					t.Fatal("scheduled close changed original bytes", sample.name)
				}
			}
			before = readRedisExpiryRepairTestState(ctx, id)
			if _, err := CloseContractAtDeadline(ctx, id, deadline); err != nil || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
				t.Fatal("terminal scheduled replay changed custody", sample.name, err)
			}
			afterProof, _ := readContractExpiryTestSnapshot(t, ctx, id)
			if !bytes.Equal(proof, afterProof) {
				t.Fatal("scheduled replay rewrote proof", sample.name)
			}
		}
	})
}

// Both paid backends keep their real debit owner. Missing cached contract payer
// is resolved from retained legacy escrow; it never becomes free settlement.
func TestScheduledContractCloseKeepsRedisAndLegacyAccounting(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		for _, legacy := range []bool{false, true} {
			f := newNetEscrowOrderingTestFixture(t, ctx)
			var id server.Id
			if legacy {
				contract, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
				server.RunPosts(ctx, posts...)
				id = contract.ContractId
			} else {
				id = createRedisAdmissionTest(ctx, f, 100).ContractId
			}
			deadline := server.NowUtc().Add(-time.Minute)
			server.Raise(CloseContract(ctx, id, f.sourceId, 17, true))
			SetContractDispute(ctx, id, true)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2,
					payer_network_id=CASE WHEN $3 THEN NULL ELSE payer_network_id END WHERE contract_id=$1`, id, server.NowUtc().Add(3*time.Hour), legacy))
			})
			owner, err := CloseContractAtDeadline(ctx, id, deadline)
			if err != nil {
				t.Fatal("scheduled paid close failed", legacy, err)
			}
			if owner != nil {
				t.Fatal("deadline returned unfinished financial work")
			}
			applied, released, busy, err := flushTransferDebitBalance(ctx, f.balanceId)
			wantDebits := 0
			if !legacy {
				wantDebits = 1
			}
			if err != nil || applied != wantDebits || released != wantDebits || busy {
				t.Fatal("deadline did not preserve its original consumption owner", applied, released, busy, err)
			}
			requireLegacySettlementTestState(t, ctx, f, id, false, true, 983, 0)
			requireDeadlineProviderDurability(t, ctx, f.destinationNetworkId, id, 17)
			before := readRedisExpiryRepairTestState(ctx, id)
			if _, err := CloseContractAtDeadline(ctx, id, deadline); err != nil || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
				t.Fatal("scheduled paid replay repeated work", legacy, err)
			}
		}
	})
}

// A capped deadline leaves accepted intent/proof/report bytes untouched. Even
// a real report committed while C is held cannot delay its ordinary worker.
func TestScheduledContractCloseCapsHeldFreshExistingIntent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		id := contract.ContractId
		deadline := server.NowUtc().Add(-time.Minute)
		server.Raise(CloseContract(ctx, id, f.sourceId, 17, true))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, server.NowUtc().Add(3*time.Hour)))
			server.Raise(queueLegacySettlementInTx(ctx, tx, id, ContractOutcomeSettled, false))
		})
		// Hold C until the actual task is waiting, then commit a fresh public
		// report through its InTx owner. Lock observation, not a sleep, proves
		// the task captured its retirement authority after this report.
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		type outcome struct {
			owner *ContractCloseOwner
			err   error
		}
		finished := make(chan outcome, 1)
		started, joined := false, false
		defer func() {
			cancel()
			if started && !joined {
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Error("scheduled close did not join during cleanup")
				}
			}
		}()
		var expectedState string
		const stateSql = `SELECT jsonb_build_array(
			(SELECT jsonb_agg(row_to_json(r) ORDER BY party) FROM contract_close r WHERE contract_id=$1))::text
			FROM transfer_contract WHERE contract_id=$1`
		server.Tx(bounded, func(tx server.PgTx) {
			var holder int32
			var originalReport time.Time
			server.Raise(tx.QueryRow(bounded, `SELECT close_time FROM contract_close WHERE contract_id=$1 AND party='source'`, id).Scan(&originalReport))
			server.Raise(tx.QueryRow(bounded, `SELECT pg_backend_pid() FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, id).Scan(&holder))
			started = true
			go func() {
				owner, err := CloseContractAtDeadline(bounded, id, deadline)
				finished <- outcome{owner: owner, err: err}
			}()
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
			for {
				var blocked bool
				server.Raise(tx.QueryRow(bounded, `SELECT EXISTS(SELECT 1 FROM pg_locks
					WHERE NOT granted AND $1::integer=ANY(pg_blocking_pids(pid)))`, holder).Scan(&blocked))
				if blocked {
					break
				}
				select {
				case result := <-finished:
					joined = true
					t.Fatal("scheduled close escaped held contract", result.err)
				case <-bounded.Done():
					t.Fatal("scheduled close never reached the contract owner", bounded.Err())
				case <-ticker.C:
				}
			}
			applied, _, err := applyContractCloseReportInTx(bounded, tx, id, f.sourceId, 0, true, nil)
			server.Raise(err)
			var freshReport time.Time
			server.Raise(tx.QueryRow(bounded, `SELECT close_time FROM contract_close WHERE contract_id=$1 AND party='source'`, id).Scan(&freshReport))
			if !applied || !freshReport.After(originalReport) || !freshReport.After(deadline) {
				t.Fatal("held public checkpoint did not commit a fresh report after admission")
			}
			server.Raise(tx.QueryRow(bounded, stateSql, id).Scan(&expectedState))
		}, server.TxReadCommitted, server.OptNoRetry())
		var completed outcome
		select {
		case completed = <-finished:
			joined = true
		case <-bounded.Done():
			t.Fatal("scheduled close did not join after C release", bounded.Err())
		}
		owner, err := completed.owner, completed.err
		if err != nil || owner != nil {
			t.Fatal("scheduled close did not finish accepted intent", err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var current string
			var expiration time.Time
			server.Raise(conn.QueryRow(ctx, stateSql, id).Scan(&current))
			server.Raise(conn.QueryRow(ctx, `SELECT expiration_time FROM transfer_contract WHERE contract_id=$1`, id).Scan(&expiration))
			if current != expectedState || !expiration.Equal(deadline.Truncate(time.Microsecond)) {
				t.Fatal("due handoff changed accepted proof/reports or failed to cap its later expiration")
			}
		})
		server.Db(ctx, func(conn server.PgConn) {
			var unchanged bool
			server.Raise(conn.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(party='source' AND checkpoint AND used_transfer_byte_count=17) FROM contract_close WHERE contract_id=$1`, id).Scan(&unchanged))
			if !unchanged {
				t.Fatal("intent handoff changed reports before its owner")
			}
		})
		page, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 8)
		if err != nil || page.Completed != 0 || page.Failed != 0 {
			t.Fatal("due accepted intent was not retired with closure", page, err)
		}
		closed, terminal := GetContractClose(ctx, id)
		if !terminal || closed.Outcome != ContractOutcomeSettled {
			t.Fatal("scheduled close changed accepted adjudication")
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 983, 0)
	})
}

// An underfunded intent consumes available escrow and closes at its deadline.
func TestScheduledContractCloseReconcilesUnderfundedIntent(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		id := contract.ContractId
		server.Raise(CloseContract(ctx, id, f.sourceId, 300, true))
		server.Raise(SettleEscrow(ctx, id, ContractOutcomeSettled))
		owner, err := CloseContractAtDeadline(ctx, id, server.NowUtc().Add(-time.Minute))
		if err != nil || owner != nil {
			t.Fatal("underfunded intent did not finish", err)
		}
		page, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 8)
		if err != nil || page.Completed != 0 || page.Failed != 0 {
			t.Fatal("scheduled deadline left repeated accounting failure", page, err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 900, 0)
	})
}

// A canceled task cannot prepare expiry or make the accepted intent disappear.
func TestScheduledContractCloseCancellationKeepsCustody(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newFreeExpiryOwnerFixture(ctx)
		id, err := CreateContractNoEscrow(ctx, f.networkId, f.sourceId, f.networkId, f.destinationId, 100)
		server.Raise(err)
		server.Raise(CloseContract(ctx, id, f.sourceId, 17, true))
		before := readRedisExpiryRepairTestState(ctx, id)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := CloseContractAtDeadline(canceled, id, server.NowUtc().Add(-time.Minute)); err == nil || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
			t.Fatal("canceled scheduled close changed custody", err)
		}
	})
}

// A due cap cannot replace an accepted dispute outcome or supply its missing
// unselected report; adjudication stays with the existing payer continuation.
func TestScheduledContractClosePreservesExistingAdjudication(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		f := newNetEscrowOrderingTestFixture(t, ctx)
		contract, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		id := contract.ContractId
		server.Raise(CloseContract(ctx, id, f.sourceId, 17, true))
		SetContractDispute(ctx, id, true)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, server.NowUtc().Add(3*time.Hour)))
			server.Raise(queueLegacySettlementInTx(ctx, tx, id, ContractOutcomeDisputeResolvedToSource, true))
		})
		read := func() (raw string) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_array(
					(SELECT jsonb_agg(row_to_json(r) ORDER BY party) FROM contract_close r WHERE contract_id=$1))::text
					FROM transfer_contract WHERE contract_id=$1`, id).Scan(&raw))
			})
			return
		}
		before := read()
		owner, err := CloseContractAtDeadline(ctx, id, server.NowUtc().Add(-time.Minute))
		if err != nil || owner != nil || read() != before {
			t.Fatal("scheduled cap changed accepted adjudication", err)
		}
		page, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 8)
		if err != nil || page.Completed != 0 || page.Failed != 0 {
			t.Fatal("capped adjudication retained unfinished financial work", page, err)
		}
		closed, terminal := GetContractClose(ctx, id)
		if !terminal || closed.Outcome != ContractOutcomeDisputeResolvedToSource {
			t.Fatal("capped close replaced the accepted outcome")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT count(*)=1 AND bool_and(party='source' AND checkpoint AND used_transfer_byte_count=17) FROM contract_close WHERE contract_id=$1`, id).Scan(&exact))
			if !exact {
				t.Fatal("adjudication fabricated an unselected report")
			}
		})
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 983, 0)
	})
}
