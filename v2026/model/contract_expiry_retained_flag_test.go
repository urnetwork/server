// Native statement observations distinguish retained proof from fallback states.
package model

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Observe actual flag-targeting statements and their committed transaction ids.
func installContractExpiryFlagObserver(ctx context.Context) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE test_expiry_flag_write (contract_id uuid, transaction_id xid8)`))
		server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION test_expiry_flag_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN INSERT INTO test_expiry_flag_write VALUES (NEW.contract_id, pg_current_xact_id()); RETURN NEW; END $$`))
		server.RaisePgResult(tx.Exec(ctx, `CREATE TRIGGER test_expiry_flag_write AFTER UPDATE OF usage_unverified ON transfer_contract
		FOR EACH ROW EXECUTE FUNCTION test_expiry_flag_write()`))
	})
}

// Read only synthetic rows belonging to the selected test contract.
func contractExpiryFlagWrites(ctx context.Context, id server.Id) (statements, transactions int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*), count(DISTINCT transaction_id) FROM test_expiry_flag_write WHERE contract_id=$1`, id).Scan(&statements, &transactions))
	})
	return
}

// Preserve the exact retained bytes, including old interrupted expiry's nil proof.
func contractExpiryFlagProof(ctx context.Context, id server.Id) (proof []byte) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT provider_usage FROM transfer_contract WHERE contract_id=$1`, id).Scan(&proof))
	})
	return
}

// Committed fresh, retained, and legacy-unconfirmed preparations need no flag tx.
func TestContractExpiryPreparedFlagAvoidsDuplicateWrite(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		installContractExpiryFlagObserver(ctx)
		for _, scenario := range []struct {
			name   string
			replay bool
			legacy bool
			scoped bool
		}{
			{name: "fresh"}, {name: "retained", replay: true},
			{name: "legacy_missing_proof", legacy: true}, {name: "scoped", scoped: true},
		} {
			id := newContractExpiryRepairTestContract(t, ctx, f, false)
			var scope *contractExpiryRepairScope
			if scenario.scoped {
				scope = &contractExpiryRepairScope{contractId: id, expectedPayer: f.sourceNetworkId}
			}
			if scenario.legacy {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_unverified=true, provider_usage=NULL WHERE contract_id=$1`, id))
				})
			}
			var state *contractExpiryState
			prepare := func() {
				contractExpiryContinuationTx(ctx, id, scope, func(tx server.PgTx) {
					var err error
					state, err = prepareContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute))
					server.Raise(err)
				})
				if state == nil {
					t.Fatalf("%s: preparation withdrew fixture", scenario.name)
				}
			}
			prepare()
			if scenario.replay {
				prepare()
			}
			beforeProof := contractExpiryFlagProof(ctx, id)
			beforeStatements, beforeTransactions := contractExpiryFlagWrites(ctx, id)
			steps := 0
			if scope != nil {
				scope.beforeTxForTest = func() { steps++ }
			}
			if err := continueContractExpiry(ctx, "[synthetic]", state, scope); err != nil {
				t.Fatal(err)
			}
			afterStatements, afterTransactions := contractExpiryFlagWrites(ctx, id)
			if afterStatements != beforeStatements || afterTransactions != beforeTransactions {
				t.Fatalf("%s: redundant usage flag statement/transaction: before=%d/%d after=%d/%d", scenario.name, beforeStatements, beforeTransactions, afterStatements, afterTransactions)
			}
			if scope != nil && steps != 2 {
				t.Fatalf("scoped continuation transactions=%d, want report and settlement only", steps)
			}
			afterProof := contractExpiryFlagProof(ctx, id)
			if !bytes.Equal(beforeProof, afterProof) {
				t.Fatalf("%s: continuation changed retained proof", scenario.name)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var intact bool
				server.Raise(conn.QueryRow(ctx, `SELECT usage_unverified AND outcome IS NULL
					AND EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
					AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
					AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
					FROM transfer_contract WHERE contract_id=$1`, id).Scan(&intact))
				if !intact {
					t.Fatalf("%s: continuation changed accounting custody", scenario.name)
				}
			})
		}
	})
}

// A preview never supplies write authority, even when its current flag is true.
func TestContractExpiryPreviewKeepsFlagFallback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newNetEscrowOrderingTestFixture(t, ctx)
		installContractExpiryFlagObserver(ctx)
		for _, retained := range []bool{false, true} {
			id := newContractExpiryRepairTestContract(t, ctx, f, false)
			if retained {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_unverified=true WHERE contract_id=$1`, id))
				})
			}
			var state *contractExpiryState
			server.Tx(ctx, func(tx server.PgTx) {
				var err error
				state, err = inspectContractExpiryInTx(ctx, tx, id, server.NowUtc().Add(-5*time.Minute), false)
				server.Raise(err)
			})
			if state == nil {
				t.Fatal("preview withdrew fixture")
			}
			beforeStatements, beforeTransactions := contractExpiryFlagWrites(ctx, id)
			if err := continueContractExpiry(ctx, "[synthetic]", state, nil); err != nil {
				t.Fatal(err)
			}
			afterStatements, afterTransactions := contractExpiryFlagWrites(ctx, id)
			if afterStatements != beforeStatements+1 || afterTransactions != beforeTransactions+1 {
				t.Fatalf("preview fallback missing: before=%d/%d after=%d/%d", beforeStatements, beforeTransactions, afterStatements, afterTransactions)
			}
		}
	})
}
