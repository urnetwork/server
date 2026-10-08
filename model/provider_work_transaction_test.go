// Optional provenance shares the accounting owner without a nested rollback
// scope. Readiness, refusal and failure each have a distinct causal control.
package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

// Every method other than transaction ownership delegates to the real caller.
// The embedding also catches a replacement that still tries to release a scope.
type providerWorkSingleOwnerTx struct {
	server.PgTx
}

func (self *providerWorkSingleOwnerTx) Begin(context.Context) (pgx.Tx, error) {
	panic(errors.New("provider work began a nested transaction"))
}

func (self *providerWorkSingleOwnerTx) Commit(context.Context) error {
	panic(errors.New("provider work committed its caller's transaction"))
}

func (self *providerWorkSingleOwnerTx) Rollback(context.Context) error {
	panic(errors.New("provider work rolled back its caller's transaction"))
}

// This seam supplies only the catalog answer; any ownership method trips the
// same boundary as the native controls below.
type providerWorkReadyTx struct {
	providerWorkSingleOwnerTx
}

func (self *providerWorkReadyTx) QueryRow(context.Context, string, ...any) pgx.Row {
	return providerWorkReadyRow{}
}

type providerWorkReadyRow struct{}

func (providerWorkReadyRow) Scan(destinations ...any) error {
	*destinations[0].(*bool) = true
	return nil
}

// Cancellation belongs to the owner and preserves the operation's own cause;
// cleanup cannot manufacture a savepoint error or obscure the original one.
func TestProviderWorkOptionalSchemaCancellationPreservesCause(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	queryErr := errors.New("endpoint query canceled")
	tx := &providerWorkReadyTx{}
	var raised error
	server.HandleError(func() {
		providerWorkOptionalSchemaInTx(ctx, tx, func(supplied server.PgTx) error {
			if supplied != tx {
				t.Fatal("optional fence replaced the supplied transaction")
			}
			cancel()
			return queryErr
		})
	}, func(err error) { raised = err })
	if !errors.Is(raised, context.Canceled) || !errors.Is(raised, queryErr) {
		t.Fatal("cancellation replaced the operation's cause", raised)
	}
}

// The preflight and work see one backend and transaction, and only their
// external owner decides whether the callback's write survives.
func TestProviderWorkOptionalUsesCallerBackendAndRollback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE provider_work_owner_probe(value int NOT NULL)`))
		})
		for _, commit := range []bool{false, true} {
			server.Db(ctx, func(conn server.PgConn) {
				tx := server.RaisePgResult(conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}))
				defer rollbackCloseReportTestTransaction(ctx, tx)
				var beforePid, duringPid int32
				var beforeTx, duringTx int64
				server.Raise(tx.QueryRow(ctx, `SELECT pg_backend_pid(),txid_current()`).Scan(&beforePid, &beforeTx))
				owner := &providerWorkSingleOwnerTx{PgTx: tx}
				if !providerWorkOptionalSchemaInTx(ctx, owner, func(supplied server.PgTx) error {
					if supplied != owner {
						t.Fatal("optional work replaced its caller's transaction")
					}
					if err := supplied.QueryRow(ctx, `SELECT pg_backend_pid(),txid_current()`).Scan(&duringPid, &duringTx); err != nil {
						return err
					}
					_, err := supplied.Exec(ctx, `INSERT INTO provider_work_owner_probe VALUES(1)`)
					return err
				}) || beforePid != duringPid || beforeTx != duringTx {
					t.Fatal("optional work changed backend or transaction", beforePid, duringPid, beforeTx, duringTx)
				}
				if commit {
					server.Raise(tx.Commit(ctx))
				} else {
					server.Raise(tx.Rollback(ctx))
				}
				var count int
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM provider_work_owner_probe`).Scan(&count))
				if commit && count != 1 || !commit && count != 0 {
					t.Fatal("optional write escaped the owner's outcome", commit, count)
				}
			})
		}
	})
}

// Rename every rollout table independently. Installed endpoint fences still
// apply when only a receipt table is missing; no missing relation is queried.
func TestProviderWorkMissingSchemaKeepsContractAndInstalledFences(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		for _, table := range []string{
			"provider_work_session_head", "provider_work_session_event", "provider_work_session_receipt",
			"provider_work_reservation_original", "provider_work_stream_original",
			"provider_work_stream_contract", "provider_work_outcome_original",
		} {
			var contractId server.Id
			server.Tx(f.ctx, func(tx server.PgTx) {
				away := table + "_not_installed"
				server.RaisePgResult(tx.Exec(f.ctx, fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, pgx.Identifier{table}.Sanitize(), pgx.Identifier{away}.Sanitize())))
				owner := &providerWorkSingleOwnerTx{PgTx: tx}
				var err error
				contractId, _, err = createContractNoEscrowInTx(f.requestContext(t, nil), owner, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121, true)
				server.Raise(err)
				if table != "provider_work_session_head" {
					server.Raise(providerWorkRequireEndpointFencesInTx(f.ctx, owner, f.sourceId, f.destinationId))
				}
				server.RaisePgResult(tx.Exec(f.ctx, fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, pgx.Identifier{away}.Sanitize(), pgx.Identifier{table}.Sanitize())))
			}, server.TxReadCommitted, server.OptNoRetry())
			server.Db(f.ctx, func(conn server.PgConn) {
				var contract, original bool
				server.Raise(conn.QueryRow(f.ctx, `SELECT
 EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=$1),
 EXISTS(SELECT 1 FROM provider_work_reservation_original WHERE contract_id=$1)`, contractId).Scan(&contract, &original))
				if !contract || original {
					t.Fatal("missing schema changed accounting or certified incomplete evidence", table, contract, original)
				}
			})
		}
	})
}

// Missing rollout columns and functions are discovered before planning any
// statement that references them, just like wholly absent receipt tables.
func TestProviderWorkMissingSchemaMembersRefuseBeforeStatements(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		for _, statements := range [][2]string{
			{`ALTER TABLE provider_work_stream_original RENAME COLUMN original TO original_not_installed`, `ALTER TABLE provider_work_stream_original RENAME COLUMN original_not_installed TO original`},
			{`ALTER FUNCTION provider_work_session_append(uuid,uuid,text,uuid) RENAME TO provider_work_session_append_not_installed`, `ALTER FUNCTION provider_work_session_append_not_installed(uuid,uuid,text,uuid) RENAME TO provider_work_session_append`},
		} {
			server.Tx(f.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(f.ctx, statements[0]))
				if providerWorkOptionalInTx(f.ctx, &providerWorkSingleOwnerTx{PgTx: tx}, func(server.PgTx) error {
					t.Fatal("incomplete schema reached optional statements")
					return nil
				}) {
					t.Fatal("incomplete schema was ready")
				}
				server.RaisePgResult(tx.Exec(f.ctx, statements[1]))
			}, server.TxReadCommitted, server.OptNoRetry())
		}
	})
}

// A real receipt insert failure cannot be converted to optional evidence loss
// after the contract has been created in the same transaction.
func TestProviderWorkOriginalSqlFailureRollsBackContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		restore := forceStatementFailures(f.ctx, "provider_work_reservation_original", "INSERT")
		defer restore()
		failure := callWithForcedFailure(f.requestContext(t, nil), func(ctx context.Context) {
			server.Tx(ctx, func(tx server.PgTx) {
				_, _, err := createContractNoEscrowInTx(ctx, tx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121, true)
				server.Raise(err)
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if !isForcedFailure(failure, "P0001", "injected failure on INSERT provider_work_reservation_original") {
			t.Fatal("receipt SQL failure was swallowed or changed", failure)
		}
		server.Db(f.ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(f.ctx, `SELECT count(*) FROM transfer_contract WHERE source_id=$1 AND destination_id=$2`, f.sourceId, f.destinationId).Scan(&count))
			if count != 0 {
				t.Fatal("contract survived its receipt statement failure", count)
			}
		})
	})
}

// The live disconnect owner still fences and appends its journal event while
// optional receipts are unavailable. Restoring schema cannot heal that gap.
func TestProviderWorkMissingReceiptSchemaKeepsSessionMutation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		restore := forceTableUnavailable(f.ctx, "provider_work_session_receipt")
		server.Raise(DisconnectNetworkClient(f.ctx, f.sourceConnectionId))
		restore()
		server.Db(f.ctx, func(conn server.PgConn) {
			var connected bool
			var events, receipts int
			server.Raise(conn.QueryRow(f.ctx, `SELECT connected,
 (SELECT count(*) FROM provider_work_session_event WHERE client_id=$2),
 (SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$2)
 FROM network_client_connection WHERE connection_id=$1`, f.sourceConnectionId, f.sourceId).Scan(&connected, &events, &receipts))
			if connected || events != 3 || receipts != 2 {
				t.Fatal("missing receipts lost the session mutation or signed its gap", connected, events, receipts)
			}
		})
		id := f.contract(t)
		if providerWorkFixtureReservation(t, providerWorkFixtureReceipts(t, f.ctx, id), id).Reservation.Complete {
			t.Fatal("restored receipt schema healed an unsigned retirement")
		}
	})
}

// A receipt statement failure occurs after the real connection trigger has
// appended its event. Both must roll back, and a later owner can retry once.
func TestProviderWorkSessionReceiptSqlFailureRollsBackMutation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		restore := forceStatementFailures(f.ctx, "provider_work_session_receipt", "INSERT")
		failure := callWithForcedFailure(f.ctx, func(ctx context.Context) {
			server.Raise(DisconnectNetworkClient(ctx, f.sourceConnectionId))
		})
		if !isForcedFailure(failure, "P0001", "injected failure on INSERT provider_work_session_receipt") {
			t.Fatal("session receipt failure was swallowed or changed", failure)
		}
		restore()
		requireProviderWorkConnectionJournal(t, f, 2, 1)
		server.Raise(DisconnectNetworkClient(f.ctx, f.sourceConnectionId))
		requireProviderWorkConnectionJournal(t, f, 3, 0)
	})
}

// An absent fence is an ownership error, and a corrupt retained original is an
// integrity error. Neither may use the explicit expected-evidence refusal.
func TestProviderWorkOwnershipAndIntegrityFailuresRollbackOwner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		contractId := f.contract(t)
		streamId := server.NewId()
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `CREATE TABLE provider_work_owner_probe(value int NOT NULL)`))
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO provider_work_stream_original(stream_id,origin_contract_id,receipt_hash,original) VALUES($1,$2,$3,$4)`, streamId, contractId, make([]byte, 32), []byte("invalid retained original")))
		})
		for _, corrupt := range []bool{false, true} {
			var failure error
			server.HandleError(func() {
				server.Tx(f.ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO provider_work_owner_probe VALUES(1)`))
					if corrupt {
						providerWorkAttachStreamInTx(f.ctx, tx, contractId, streamId)
					} else {
						providerWorkRetainReservationInTx(f.ctx, tx, contractId)
					}
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { failure = err })
			if corrupt && !errors.Is(failure, protocol.ErrProviderWorkIntegrity) || !corrupt && (failure == nil || !strings.Contains(failure.Error(), "endpoint fence is absent")) {
				t.Fatal("ownership or integrity failure became optional", corrupt, failure)
			}
			server.Db(f.ctx, func(conn server.PgConn) {
				var count int
				server.Raise(conn.QueryRow(f.ctx, `SELECT count(*) FROM provider_work_owner_probe`).Scan(&count))
				if count != 0 {
					t.Fatal("ownership or integrity failure committed the owner", corrupt, count)
				}
			})
		}
	})
}

// Event timestamps straddle authority deterministically. A refused later event
// must not leave an earlier receipt behind; the successful control also proves
// that consecutive prepared receipts link without reading an uninserted row.
func TestProviderWorkSessionBatchPreparesBeforeWriting(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		at := server.NowUtc()
		for _, expired := range []bool{false, true} {
			clientId, networkId, connectionId := server.NewId(), server.NewId(), server.NewId()
			authority := f.source.authority
			if expired {
				authority.ThroughUnixMicro = at.Add(time.Microsecond).UnixMicro()
			}
			source, err := NewProviderWorkSessionSource(authority, f.source.key)
			server.Raise(err)
			ctx := WithProviderWorkSessionSource(f.ctx, source)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_session_head(client_id,sequence) VALUES($1,2)`, clientId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_work_session_event(client_id,sequence,network_id,connection_id,kind,observed_at,transaction_id) VALUES
 ($1,1,$2,NULL,'baseline',$4,txid_current()),($1,2,$2,$3,'admit',$5,txid_current())`, clientId, networkId, connectionId, at, at.Add(time.Microsecond)))
				providerWorkRetainSessionEventsInTx(ctx, &providerWorkSingleOwnerTx{PgTx: tx}, clientId)
			}, server.TxReadCommitted, server.OptNoRetry())
			server.Db(ctx, func(conn server.PgConn) {
				rows := server.RaisePgResult(conn.Query(ctx, `SELECT original FROM provider_work_session_receipt WHERE client_id=$1 ORDER BY sequence`, clientId))
				defer rows.Close()
				var receipts []protocol.ProviderWorkReceipt
				for rows.Next() {
					var raw []byte
					server.Raise(rows.Scan(&raw))
					receipt, err := protocol.DecodeProviderWorkReceipt(ctx, raw)
					server.Raise(err)
					receipts = append(receipts, receipt)
				}
				server.Raise(rows.Err())
				if expired {
					if len(receipts) != 0 {
						t.Fatal("later source refusal left a partial signed batch", len(receipts))
					}
				} else {
					states, err := protocol.ReplayProviderWorkEndpoint(ctx, authority, receipts)
					if err != nil || len(states) != 2 || states[1].ActiveConnections != 1 {
						t.Fatal("prepared receipt chain lost its predecessor", states, err)
					}
				}
			})
		}
	})
}

// Connection admission remains usable after optional authority ends or its
// bounded history is exhausted, and those gaps never acquire a later receipt.
func TestProviderWorkSessionEvidenceRefusalKeepsConnectionOwner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		for _, expired := range []bool{false, true} {
			clientId, networkId := server.NewId(), server.NewId()
			addContractPayoutTestClients(f.ctx, map[server.Id]server.Id{clientId: networkId})
			authority := f.source.authority
			if expired {
				authority.ThroughUnixMicro = server.NowUtc().Add(-time.Minute).UnixMicro()
			} else {
				authority.MaxEndpointEvents = 1
			}
			source := server.RaisePgResult(NewProviderWorkSessionSource(authority, f.source.key))
			ctx := WithProviderWorkSessionSource(f.ctx, source)
			connectionId, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, clientId, "192.0.2.31:13000", f.handlerId, 4)
			server.Raise(err)
			// A later source can retire the connection but must not backfill
			// either refused event from the preceding owner's transaction.
			server.Raise(DisconnectNetworkClient(f.ctx, connectionId))
			server.Db(f.ctx, func(conn server.PgConn) {
				var connected bool
				var events, receipts int
				server.Raise(conn.QueryRow(f.ctx, `SELECT connected,
 (SELECT count(*) FROM provider_work_session_event WHERE client_id=$2),
 (SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$2)
 FROM network_client_connection WHERE connection_id=$1`, connectionId, clientId).Scan(&connected, &events, &receipts))
				wantReceipts := 1
				if expired {
					wantReceipts = 0
				}
				if connected || events != 3 || receipts != wantReceipts {
					t.Fatal("evidence refusal changed lifecycle or healed a gap", expired, connected, events, receipts)
				}
			})
		}
	})
}

// A pre-journal live connection prevents an empty baseline. The first observed
// admission is a permanent unknown genesis, not a reason to stop traffic.
func TestProviderWorkSessionUnknownGenesisKeepsAdmission(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		clientId, networkId := server.NewId(), server.NewId()
		addContractPayoutTestClients(f.ctx, map[server.Id]server.Id{clientId: networkId})
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `ALTER TABLE network_client_connection DISABLE TRIGGER provider_work_session_mutation`))
		})
		unsigned := WithProviderWorkSessionSource(f.ctx, nil)
		_, _, _, _, err := ConnectNetworkClientWithIpFamily(unsigned, clientId, "192.0.2.32:13000", f.handlerId, 4)
		server.Raise(err)
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `ALTER TABLE network_client_connection ENABLE TRIGGER provider_work_session_mutation`))
		})
		_, _, _, _, err = ConnectNetworkClientWithIpFamily(f.ctx, clientId, "192.0.2.33:13001", f.handlerId, 4)
		server.Raise(err)
		server.Db(f.ctx, func(conn server.PgConn) {
			var connections, events, receipts int
			server.Raise(conn.QueryRow(f.ctx, `SELECT
 (SELECT count(*) FROM network_client_connection WHERE client_id=$1 AND connected),
 (SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
 (SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1)`, clientId).Scan(&connections, &events, &receipts))
			if connections != 2 || events != 1 || receipts != 0 {
				t.Fatal("unknown genesis stopped admission or fabricated a baseline", connections, events, receipts)
			}
		})
	})
}
