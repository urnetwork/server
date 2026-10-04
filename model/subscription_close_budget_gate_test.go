package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A PostgreSQL trigger holds each close below the worker deadline while the
// completed subpage crosses the cooperative budget. Later rows await replay.
func TestForceCloseBudgetLoadedSubpageReplay(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		network := server.NewId()
		source, destination := server.NewId(), server.NewId()
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{source: network, destination: network})
		aged := server.NowUtc().Add(-time.Hour)
		ids := make([]server.Id, 4)
		for i := range ids {
			id, err := CreateContractNoEscrow(ctx, network, source, network, destination, 100)
			if err != nil {
				t.Fatal(err)
			}
			ids[i] = id
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, id, aged.Add(time.Duration(i)*time.Millisecond)))
			})
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
                CREATE FUNCTION synthetic_close_budget_residence() RETURNS trigger LANGUAGE plpgsql AS $$
                BEGIN
                    PERFORM pg_sleep(0.15);
                    RETURN NEW;
                END $$;
                CREATE TRIGGER synthetic_close_budget_residence
                BEFORE UPDATE OF outcome ON transfer_contract
                FOR EACH ROW WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL)
                EXECUTE FUNCTION synthetic_close_budget_residence();`))
		})
		callCtx, callCancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer callCancel()
		first, cursor, err := forceCloseOpenContractIdsBudgetedPage(callCtx, server.NowUtc(), 4, 1, 0, 0, nil, 200*time.Millisecond, 2)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_close_budget_residence ON transfer_contract;
                DROP FUNCTION synthetic_close_budget_residence();`))
		})
		if err != nil || first != 2 || cursor == nil || cursor.Open == nil || cursor.Open.ContractId != ids[1] {
			t.Fatalf("budget failed to checkpoint complete prefix: count=%d cursor=%+v err=%v", first, cursor, err)
		}
		var committed int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[]) AND outcome IS NOT NULL`, ids).Scan(&committed))
		})
		if committed != 2 {
			t.Fatalf("unexpected committed prefix=%d", committed)
		}
		second, next, err := forceCloseOpenContractIdsBudgetedPage(ctx, server.NowUtc(), 4, 1, 0, 0, cursor, time.Second, 2)
		if err != nil || first+second != 4 || next != nil {
			t.Fatalf("retry skipped or duplicated interrupted subpage: first=%d second=%d next=%+v err=%v", first, second, next, err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[]) AND outcome IS NOT NULL`, ids).Scan(&committed))
		})
		if committed != 4 {
			t.Fatalf("retry left contracts open: %d", committed)
		}
	})
}

// A real Redis-backed reservation remains owned while the later contract's
// PostgreSQL finalization crosses the elapsed budget. Parent cancellation takes the same
// barrier but must stay an error rather than becoming a normal page yield.
func TestForceCloseBudgetRedisReservationAndParentCancel(t *testing.T) {
	for _, parentCancel := range []bool{false, true} {
		name := "owner budget"
		if parentCancel {
			name = "parent cancellation"
		}
		t.Run(name, func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				setupCtx, setupCancel := context.WithTimeout(t.Context(), 45*time.Second)
				defer setupCancel()
				firstFixture := newForceCloseDisputeFixture(t, setupCtx, true, true, 1024, 1024, 4096)
				secondFixture := newForceCloseDisputeFixture(t, setupCtx, true, true, 1024, 1024, 4096)
				server.Tx(setupCtx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(setupCtx, `UPDATE transfer_contract SET create_time=create_time + interval '1 millisecond' WHERE contract_id=$1`, secondFixture.contractId))
				})
				beforeSecond := secondFixture.state(t, setupCtx)
				server.Tx(setupCtx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(setupCtx, fmt.Sprintf(`
                    CREATE FUNCTION synthetic_close_budget_block() RETURNS trigger LANGUAGE plpgsql AS $$
                    BEGIN
                        IF NEW.contract_id = '%s'::uuid THEN
                            PERFORM pg_sleep(3);
                        END IF;
                        RETURN NEW;
                    END $$;
                    CREATE TRIGGER synthetic_close_budget_block
                    BEFORE UPDATE OF outcome ON transfer_contract
                    FOR EACH ROW WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL)
                    EXECUTE FUNCTION synthetic_close_budget_block();`, firstFixture.contractId)))
				})
				callCtx, cancel := context.WithCancel(setupCtx)
				if parentCancel {
					time.AfterFunc(time.Second, cancel)
				}
				budget := 100 * time.Millisecond
				if parentCancel {
					budget = 10 * time.Second
				}
				count, cursor, err := forceCloseOpenContractIdsBudgetedPage(callCtx, firstFixture.cutoff, 2, 1, 0, 0, nil, budget, 1)
				cancel()
				server.Tx(setupCtx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(setupCtx, `DROP TRIGGER synthetic_close_budget_block ON transfer_contract;
                    DROP FUNCTION synthetic_close_budget_block();`))
				})
				if parentCancel {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("parent cancellation lost: count=%d cursor=%+v err=%v", count, cursor, err)
					}
				} else if err != nil || count != 1 || cursor == nil || cursor.Open == nil || cursor.Open.ContractId != firstFixture.contractId {
					t.Fatalf("budget did not checkpoint first financial close: count=%d cursor=%+v err=%v", count, cursor, err)
				}
				if afterSecond := secondFixture.state(t, setupCtx); afterSecond != beforeSecond {
					t.Fatalf("interrupted contract's reserved financial state changed: before=%+v after=%+v", beforeSecond, afterSecond)
				}
				if !parentCancel {
					settled := firstFixture.state(t, setupCtx)
					if settled.outcome != ContractOutcomeSettled || !settled.escrowSettled || settled.providerPayoutByteCount != 1024 || settled.escrowPayoutByteCount != 1024 || settled.requestTokenByteCount != 1024 || settled.redisEscrowByteCount != 1024 || settled.streamFound {
						t.Fatalf("budget overrun lost required financial posts: %+v", settled)
					}
					applied, released, busy, debitErr := flushTransferDebitBalance(setupCtx, firstFixture.balanceId)
					if debitErr != nil || applied != 1 || released != 1 || busy {
						t.Fatalf("owner debit failed conservation: applied=%d released=%d busy=%t err=%v", applied, released, busy, debitErr)
					}
					final := firstFixture.state(t, setupCtx)
					if final.payerBalanceByteCount != forceCloseDisputeInitialBalance-1024 || final.requestTokenByteCount != 0 || final.redisEscrowByteCount != 0 || final.providerPayoutByteCount != 1024 {
						t.Fatalf("owner debit financial result: %+v", final)
					}
					if a, r, b, e := flushTransferDebitBalance(setupCtx, firstFixture.balanceId); a != 0 || r != 0 || b || e != nil || firstFixture.state(t, setupCtx) != final {
						t.Fatalf("repeat debit duplicated financial state: %d %d %t %v", a, r, b, e)
					}
					closed, next, retryErr := forceCloseOpenContractIdsBudgetedPage(setupCtx, firstFixture.cutoff, 2, 1, 0, 0, cursor, 5*time.Second, 1)
					if retryErr != nil || closed != 1 || next != nil {
						t.Fatalf("financial replay did not finish second contract: count=%d cursor=%+v err=%v", closed, next, retryErr)
					}
					if secondFixture.state(t, setupCtx).outcome == "" {
						t.Fatal("financial replay left second contract open")
					}
				}
			})
		})
	}
}

// Completed earlier subpages add verified progress without hiding later errors.
func TestForceCloseBudgetAccountingAndMixedErrors(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		t.Run(fmt.Sprintf("mixed=%t", mixed), func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx := t.Context()
				const escrow = ByteCount(32 * 1024 * 1024)
				good := newForceCloseDisputeFixture(t, ctx, true, true, 1024, 1024, escrow)
				bad := newForceCloseDisputeFixture(t, ctx, !mixed, !mixed, 0, 4*escrow, escrow)
				before := bad.state(t, ctx)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=create_time+interval '1 millisecond' WHERE contract_id=$1`, bad.contractId))
				})
				if mixed {
					server.Tx(ctx, func(tx server.PgTx) {
						server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`
      CREATE FUNCTION synthetic_budget_mixed_error() RETURNS trigger LANGUAGE plpgsql AS $$
      BEGIN RAISE EXCEPTION 'synthetic budget operational failure'; RETURN NEW; END $$; -- %s
      CREATE TRIGGER synthetic_budget_mixed_error BEFORE UPDATE OF outcome ON transfer_contract FOR EACH STATEMENT EXECUTE FUNCTION synthetic_budget_mixed_error();`, bad.contractId)))
					})
				}
				count, cursor, err := forceCloseOpenContractIdsBudgetedPage(ctx, good.cutoff, 4, 1, 0, 0, nil, time.Second, 1)
				if err == nil || count != 2 || !errors.Is(err, errContractInsufficientEscrow) {
					t.Fatalf("lost page error or selected count: count=%d cursor=%+v err=%v", count, cursor, err)
				}
				var accounting *ForceCloseAccountingError
				if mixed {
					if errors.As(err, &accounting) || !strings.Contains(err.Error(), "synthetic budget operational failure") {
						t.Fatalf("mixed error gained accounting authority: %v", err)
					}
					if cursor != nil {
						t.Fatalf("mixed failure skipped uncompleted page: %+v", cursor)
					}
				} else {
					if !errors.As(err, &accounting) || accounting.VerifiedCloseCount() != 1 || accounting.AccountingRejectionCount() != 1 {
						t.Fatalf("prior verified accounting progress lost: %v", err)
					}
				}
				after := bad.state(t, ctx)
				if after.escrowSettled || after.escrowPayoutByteCount != before.escrowPayoutByteCount || after.payerBalanceByteCount != before.payerBalanceByteCount || after.providerPayoutByteCount != before.providerPayoutByteCount || after.requestTokenByteCount != before.requestTokenByteCount {
					t.Fatalf("rejected money state changed: before=%+v after=%+v", before, after)
				}
				if state := good.state(t, ctx); !mixed && (state.providerPayoutByteCount != 1024 || !state.escrowSettled) {
					t.Fatalf("completed prefix missing financial posts: %+v", state)
				}
			})
		})
	}
}

func TestForceCloseBudgetZeroCloseRetainedHead(t *testing.T) {
	for _, disputed := range []bool{false, true} {
		name := "open"
		if disputed {
			name = "disputed"
		}
		t.Run(name, func(t *testing.T) {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
				defer cancel()
				f, pending := legacySettlementTestIntent(t, ctx)
				later, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 100)
				if err != nil {
					t.Fatal(err)
				}
				aged := server.NowUtc().Add(-2 * time.Hour)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,dispute=$3 WHERE contract_id=$1`, pending, aged, disputed))
					server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET close_time=$2 WHERE contract_id=$1`, pending, aged))
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,dispute=$3 WHERE contract_id=$1`, later, aged.Add(time.Minute), disputed))
					if disputed {
						// A no-escrow zero-usage dispute exercises the other selector
						// without adding a second legacy financial owner.
						server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
                            VALUES($1,'source',0,$2,false),($1,'destination',0,$2,false)`, later, aged))
					}
				})
				closed, cursor, err := forceCloseOpenContractIdsBudgetedPage(ctx, aged.Add(time.Hour), 10, 1, 0, 0, nil, time.Nanosecond, 1)
				if err != nil || closed != 0 || cursor == nil {
					t.Fatalf("owned head did not advance its bounded cursor: closed=%d cursor=%t err=%v", closed, cursor != nil, err)
				}
				closed, cursor, err = forceCloseOpenContractIdsBudgetedPage(ctx, aged.Add(time.Hour), 10, 1, 0, 0, cursor, time.Second, 1)
				if err != nil || closed != 1 {
					t.Fatalf("pending head starved later %s contract: verified=%d err=%v", name, closed, err)
				}
				requireLegacySettlementTestState(t, ctx, f, pending, true, false, 1000, 100)
				server.Db(ctx, func(conn server.PgConn) {
					var terminal bool
					server.Raise(conn.QueryRow(ctx, `SELECT outcome IS NOT NULL FROM transfer_contract WHERE contract_id=$1`, later).Scan(&terminal))
					if !terminal {
						t.Fatal("later ordinary contract did not reach a final outcome")
					}
				})
			})
		})
	}
}

// Equal creation timestamps need the contract-id tie-breaker. A pass also
// retains its upper time bound, so later arrivals cannot prevent completion.
