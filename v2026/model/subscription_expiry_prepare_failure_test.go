// Distinguishes absent expiry work from an attempted proof transaction that rolled back.
package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/urnetwork/server/v2026"
)

// Both modes advance a fully visited raw page without abandoning its failed row.
func requireForceClosePreparationFailureAllowsTail(t *testing.T, legacy bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
		defer cancel()
		first := newForceCloseOperationalFixture(t, ctx, legacy)
		tail := newForceCloseOperationalFixture(t, ctx, legacy)
		fixtures := []*forceCloseDisputeFixture{first, tail}
		ids := []server.Id{first.contractId, tail.contractId}
		created := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
		server.Tx(ctx, func(tx server.PgTx) {
			for index, fixture := range fixtures {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract
					SET create_time=$2,expiration_time=NULL WHERE contract_id=$1`,
					fixture.contractId, created.Add(time.Duration(index)*time.Minute)))
				// A recent authenticated checkpoint must not extend the null deadline fallback.
				server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`,
					fixture.contractId, server.NowUtc()))
			}
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
				CREATE SEQUENCE synthetic_expiry_proof_attempts;
				CREATE FUNCTION synthetic_expiry_proof_failure() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					IF NEW.contract_id='%s'::uuid THEN
						PERFORM nextval('synthetic_expiry_proof_attempts');
						RAISE EXCEPTION USING ERRCODE='53200',MESSAGE='synthetic expiry proof resource failure';
					END IF;
					RETURN NEW;
				END;
				$$;
				CREATE TRIGGER synthetic_expiry_proof_failure
				BEFORE UPDATE OF usage_unverified ON transfer_contract
				FOR EACH ROW WHEN (NOT OLD.usage_unverified AND NEW.usage_unverified)
				EXECUTE FUNCTION synthetic_expiry_proof_failure();`, first.contractId)))
		})
		before := []forceCloseDisputeState{first.state(t, ctx), tail.state(t, ctx)}
		readOwners := func() (prepared, terminal, journals int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT
					count(*) FILTER(WHERE usage_unverified),count(*) FILTER(WHERE outcome IS NOT NULL),
					(SELECT count(*) FROM transfer_debit_journal WHERE contract_id=ANY($1::uuid[]))
					FROM transfer_contract WHERE contract_id=ANY($1::uuid[])`, ids).
					Scan(&prepared, &terminal, &journals))
			})
			return
		}
		// A legacy reservation settles inline and writes no debit journal.
		wantJournals := func(terminal int) int {
			if legacy {
				return 0
			}
			return terminal
		}
		var tailRetained forceCloseDisputeState
		for attempt := 1; attempt <= 2; attempt++ {
			count, cursor, err := forceCloseOpenContractIdsBudgetedPage(ctx, first.cutoff, 2, 1, 1, 0, nil, time.Minute, 1)
			var database *pgconn.PgError
			var accounting *ForceCloseAccountingError
			if count != 0 || cursor == nil || cursor.Open == nil || cursor.Open.ContractId != first.contractId ||
				!errors.As(err, &database) || database.Code != "53200" || errors.As(err, &accounting) {
				t.Fatalf("completed failed visit lost error or raw progress: mode=%t count=%d cursor=%+v error=%v", legacy, count, cursor, err)
			}
			var attempts int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT last_value FROM synthetic_expiry_proof_attempts`).Scan(&attempts))
			})
			if attempts != attempt {
				t.Fatal("failed proof was not visited exactly once per raw-page attempt", attempts, attempt)
			}
			if after := first.state(t, ctx); after != before[0] {
				t.Fatalf("failed proof changed financial custody: before=%+v after=%+v", before[0], after)
			}
			if attempt == 1 {
				if prepared, terminal, journals := readOwners(); prepared != 0 || terminal != 0 || journals != 0 || tail.state(t, ctx) != before[1] {
					t.Fatal("proof rollback or unvisited tail published a new owner", prepared, terminal, journals)
				}
			}
			// The fault remains installed. The next raw page must still reach
			// the healthy tail, then end the pass so the failed head can return.
			tailCount, next, tailErr := forceCloseOpenContractIdsBudgetedPage(ctx, first.cutoff, 3, 1, 1, 0, cursor, time.Minute, 1)
			wantTailCount := int64(0)
			if attempt == 1 {
				wantTailCount = 1
			}
			if tailErr != nil || tailCount != wantTailCount || next != nil {
				t.Fatal("persistent failed head blocked or duplicated the healthy tail", tailCount, next, tailErr)
			}
			prepared, terminal, journals := readOwners()
			if prepared != 1 || terminal != 1 || journals != wantJournals(1) {
				t.Fatal("healthy tail lost its own mode-specific settlement custody", prepared, terminal, journals)
			}
			if attempt == 1 {
				tailRetained = tail.state(t, ctx)
			} else if tail.state(t, ctx) != tailRetained {
				t.Fatal("returning to the unresolved head duplicated the healthy tail's financial work")
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_expiry_proof_failure ON transfer_contract`))
		})
		count, cursor, err := forceCloseOpenContractIdsBudgetedPage(ctx, first.cutoff, 3, 1, 1, 0, nil, time.Minute, 1)
		if err != nil || count != 1 || cursor != nil {
			t.Fatal("removing the proof fault did not recover the retained head", count, cursor, err)
		}
		prepared, terminal, journals := readOwners()
		if prepared != 2 || terminal != 2 || journals != wantJournals(2) {
			t.Fatal("healthy recovery used the wrong reservation-mode owner", prepared, terminal, journals)
		}
		recovered := make([]forceCloseDisputeState, len(fixtures))
		for index, fixture := range fixtures {
			recovered[index] = fixture.state(t, ctx)
			_, snapshot := readContractExpiryTestSnapshot(t, ctx, fixture.contractId)
			if snapshot.Expiry == nil || len(snapshot.Expiry.Reports) != 2 || snapshot.ByteCount != forceCloseOperationalUsage {
				t.Fatal("recovery lost the original authenticated report proof")
			}
			if recovered[index].providerEarnedByteCount != forceCloseOperationalUsage {
				t.Fatal("recovered outcome did not retain its exact earned payout")
			}
			if legacy {
				// The legacy debit, reservation release and escrow metadata
				// commit with the outcome.
				if recovered[index].payerBalanceByteCount != before[index].payerBalanceByteCount-forceCloseOperationalUsage ||
					recovered[index].netEscrowByteCount != 0 || !recovered[index].escrowSettled {
					t.Fatal("legacy expiry did not settle its reservation inline")
				}
			} else {
				if recovered[index].payerBalanceByteCount != before[index].payerBalanceByteCount ||
					recovered[index].netEscrowByteCount != before[index].netEscrowByteCount || recovered[index].escrowSettled {
					t.Fatal("expiry consumed or released the separate financial owner's reservation")
				}
				requireForceCloseDebitJournal(t, ctx, fixture, 1, forceCloseOperationalUsage)
			}
		}
		if replayCount, replayCursor, replayErr := forceCloseOpenContractIdsBudgetedPage(ctx, first.cutoff, 3, 1, 1, 0, nil, time.Minute, 1); replayErr != nil || replayCount != 0 || replayCursor != nil {
			t.Fatal("replay repeated work already owned by settlement", replayCount, replayCursor, replayErr)
		}
		for index, fixture := range fixtures {
			if after := fixture.state(t, ctx); after != recovered[index] {
				t.Fatal("replay duplicated or lost financial custody", index)
			}
		}
	})
}

// A visited legacy contract has no proof after preparation fails; its recovery
// settles the reservation inline.
func TestForceCloseLegacyPreparationFailureAllowsTailAndRevisitsHead(t *testing.T) {
	requireForceClosePreparationFailureAllowsTail(t, true)
}

// A Redis contract's recovery retains its outcome and debit journal.
func TestForceCloseRedisPreparationFailureAllowsTailAndRevisitsHead(t *testing.T) {
	requireForceClosePreparationFailureAllowsTail(t, false)
}
