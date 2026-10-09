// Optional provenance shares the accounting owner without a nested rollback
// scope. Optional evidence refusal and required SQL failure stay distinct.
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

// Cancellation belongs to the owner and preserves the operation's own cause;
// cleanup cannot manufacture a savepoint error or obscure the original one.
func TestProviderWorkOptionalCancellationPreservesCause(t *testing.T) {
	ctx, cancel := context.WithCancel(WithProviderWorkSessionSource(t.Context(), &ProviderWorkSessionSource{}))
	defer cancel()
	queryErr := errors.New("endpoint query canceled")
	tx := &providerWorkSingleOwnerTx{}
	var raised error
	server.HandleError(func() {
		providerWorkOptionalInTx(ctx, tx, func(supplied server.PgTx) error {
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

// The optional work sees one backend and transaction, and only its
// external owner decides whether the callback's write survives.
func TestProviderWorkOptionalUsesCallerBackendAndRollback(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), &ProviderWorkSessionSource{})
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
				if !providerWorkOptionalInTx(ctx, owner, func(supplied server.PgTx) error {
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

// Missing migrated objects fail at their required statement. Neither the
// contract nor a partial original may escape the owning transaction.
func TestProviderWorkMissingSchemaRollsBackRequiredReservation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		for _, table := range []string{
			"provider_work_session_head", "provider_work_session_receipt",
			"provider_work_reservation_original",
		} {
			failure := callWithForcedFailure(f.requestContext(t, nil), func(ctx context.Context) {
				server.Tx(ctx, func(tx server.PgTx) {
					away := table + "_not_installed"
					server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s RENAME TO %s`, pgx.Identifier{table}.Sanitize(), pgx.Identifier{away}.Sanitize())))
					_, _, err := createContractNoEscrowInTx(ctx, &providerWorkSingleOwnerTx{PgTx: tx}, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121, true)
					server.Raise(err)
					panic(errors.New("reservation accepted a missing required table"))
				}, server.TxReadCommitted, server.OptNoRetry())
			})
			if !isForcedFailure(failure, "42P01", "") {
				t.Fatal("missing table did not fail its required statement", table, failure)
			}
			server.Db(f.ctx, func(conn server.PgConn) {
				var contracts, originals int
				server.Raise(conn.QueryRow(f.ctx, `SELECT
 (SELECT count(*) FROM transfer_contract WHERE source_id=$1 AND destination_id=$2),
 (SELECT count(*) FROM provider_work_reservation_original WHERE source_id=$1 AND destination_id=$2)`, f.sourceId, f.destinationId).Scan(&contracts, &originals))
				if contracts != 0 || originals != 0 {
					t.Fatal("missing required schema committed partial work", table, contracts, originals)
				}
			})
		}
	})
}

// A required original column is exercised by the actual reservation insert,
// after accounting began; the same owner must roll all of it back.
func TestProviderWorkMissingReservationColumnRollsBackOwner(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		failure := callWithForcedFailure(f.requestContext(t, nil), func(ctx context.Context) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `ALTER TABLE provider_work_reservation_original RENAME COLUMN original TO original_not_installed`))
				_, _, err := createContractNoEscrowInTx(ctx, &providerWorkSingleOwnerTx{PgTx: tx}, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121, true)
				server.Raise(err)
				panic(errors.New("reservation accepted a missing required column"))
			}, server.TxReadCommitted, server.OptNoRetry())
		})
		if !isForcedFailure(failure, "42703", "") {
			t.Fatal("missing original column did not fail its insert", failure)
		}
		server.Db(f.ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(f.ctx, `SELECT count(*) FROM transfer_contract WHERE source_id=$1 AND destination_id=$2`, f.sourceId, f.destinationId).Scan(&count))
			if count != 0 {
				t.Fatal("contract survived its missing original column", count)
			}
		})
	})
}

// First admission calls the migrated append function directly. Its absence
// must leave neither a connection nor a partial signed genesis behind.
func TestProviderWorkMissingAppendFunctionRollsBackAdmission(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		failure := func() any {
			server.Tx(f.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(f.ctx, `ALTER FUNCTION provider_work_session_append(uuid,uuid,text,uuid) RENAME TO provider_work_session_append_not_installed`))
			})
			defer server.Tx(f.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(f.ctx, `ALTER FUNCTION provider_work_session_append_not_installed(uuid,uuid,text,uuid) RENAME TO provider_work_session_append`))
			})
			return callWithForcedFailure(f.ctx, func(ctx context.Context) {
				_, _, _, _, err := ConnectNetworkClientWithIpFamily(ctx, f.intermediaryId, "192.0.2.18:10009", f.handlerId, 4)
				server.Raise(err)
			})
		}()
		if !isForcedFailure(failure, "42883", "") {
			t.Fatal("missing append function did not fail its actual admission", failure)
		}
		server.Db(f.ctx, func(conn server.PgConn) {
			var connections, heads, events, receipts int
			server.Raise(conn.QueryRow(f.ctx, `SELECT
 (SELECT count(*) FROM network_client_connection WHERE client_id=$1),
 (SELECT count(*) FROM provider_work_session_head WHERE client_id=$1),
 (SELECT count(*) FROM provider_work_session_event WHERE client_id=$1),
 (SELECT count(*) FROM provider_work_session_receipt WHERE client_id=$1)`, f.intermediaryId).Scan(&connections, &heads, &events, &receipts))
			if connections != 0 || heads != 0 || events != 0 || receipts != 0 {
				t.Fatal("missing append function committed partial admission", connections, heads, events, receipts)
			}
		})
		_, _, _, _, err := ConnectNetworkClientWithIpFamily(f.ctx, f.intermediaryId, "192.0.2.18:10009", f.handlerId, 4)
		server.Raise(err)
	})
}

// The real stream attachment owns its accounting update, intermediary census
// and original reference together, even after Redis birth already succeeded.
func TestProviderWorkMissingStreamSchemaRollsBackAttachment(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, table := range []string{"provider_work_stream_original", "provider_work_stream_contract"} {
			f := newProviderWorkSessionFixture(t)
			intermediaries := []server.Id{f.intermediaryId}
			ctx := f.requestContext(t, intermediaries)
			contractId, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 121)
			server.Raise(err)
			streamId := AddToStream(ctx, contractId, f.sourceId, f.destinationId, intermediaries)
			failure := func() any {
				restore := forceTableUnavailable(ctx, table)
				defer restore()
				return callWithForcedFailure(ctx, func(callCtx context.Context) {
					server.Raise(SetContractStream(callCtx, contractId, streamId, intermediaries))
				})
			}()
			if !isForcedFailure(failure, "42P01", "") {
				t.Fatal("missing stream schema did not abort its attachment", table, failure)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var unattached bool
				var participants, references int
				server.Raise(conn.QueryRow(ctx, `SELECT stream_id IS NULL,
 (SELECT count(*) FROM contract_participant WHERE stream_id=$2),
 (SELECT count(*) FROM provider_work_stream_contract WHERE contract_id=$1)
 FROM transfer_contract WHERE contract_id=$1`, contractId, streamId).Scan(&unattached, &participants, &references))
				if !unattached || participants != 0 || references != 0 {
					t.Fatal("stream schema failure committed partial attachment", table, unattached, participants, references)
				}
			})
			server.Raise(SetContractStream(ctx, contractId, streamId, intermediaries))
			f.close(t, contractId)
			streamOriginals := 0
			for _, receipt := range providerWorkFixtureReceipts(t, ctx, contractId) {
				server.Raise(protocol.VerifyProviderWorkReceiptAuthority(ctx, receipt, f.source.authority))
				if receipt.Stream != nil {
					streamOriginals++
					if receipt.Stream.StreamId != streamId.String() {
						t.Fatal("attachment retry replaced the live birth original")
					}
				}
			}
			if streamOriginals != 1 {
				t.Fatal("attachment retry lost original stream custody", streamOriginals)
			}
		}
	})
}

// Close reports have their own committed owner. A missing or failed outcome
// original rolls back only the terminal decision, preserving exact retry input.
func TestProviderWorkOutcomeFailureRollsBackTerminalDecision(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		for _, missingSchema := range []bool{true, false} {
			contractId := f.contract(t)
			server.Raise(CloseContract(f.ctx, contractId, f.sourceId, 121, false))
			failure := func() any {
				var restore func()
				if missingSchema {
					restore = forceTableUnavailable(f.ctx, "provider_work_outcome_original")
				} else {
					restore = forceStatementFailures(f.ctx, "provider_work_outcome_original", "INSERT")
				}
				defer restore()
				return callWithForcedFailure(f.ctx, func(ctx context.Context) {
					server.Raise(CloseContract(ctx, contractId, f.destinationId, 121, false))
				})
			}()
			code := "P0001"
			if missingSchema {
				code = "42P01"
			}
			if !isForcedFailure(failure, code, "") {
				t.Fatal("outcome original failure did not abort its terminal owner", missingSchema, failure)
			}
			server.Db(f.ctx, func(conn server.PgConn) {
				var unclosed bool
				var reports, totalBytes, originals int
				server.Raise(conn.QueryRow(f.ctx, `SELECT outcome IS NULL AND close_time IS NULL AND provider_usage IS NULL,
 (SELECT count(*) FROM contract_close WHERE contract_id=$1),
 (SELECT sum(used_transfer_byte_count) FROM contract_close WHERE contract_id=$1),
 (SELECT count(*) FROM provider_work_outcome_original WHERE contract_id=$1)
 FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&unclosed, &reports, &totalBytes, &originals))
				if !unclosed || reports != 2 || totalBytes != 242 || originals != 0 {
					t.Fatal("failed terminal owner changed committed report custody", missingSchema, unclosed, reports, totalBytes, originals)
				}
			})
			server.Raise(CloseContract(f.ctx, contractId, f.destinationId, 121, false))
			outcomes := 0
			for _, receipt := range providerWorkFixtureReceipts(t, f.ctx, contractId) {
				server.Raise(protocol.VerifyProviderWorkReceiptAuthority(f.ctx, receipt, f.source.authority))
				if receipt.Outcome != nil {
					outcomes++
					if receipt.Outcome.SourceBytes != 121 || receipt.Outcome.DestinationBytes != 121 || receipt.Outcome.Outcome != ContractOutcomeSettled {
						t.Fatal("terminal retry changed retained report counters", receipt.Outcome)
					}
				}
			}
			if outcomes != 1 {
				t.Fatal("terminal retry did not retain exactly one original", outcomes)
			}
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

// Missing migrated objects fail the real disconnect after its journal
// mutation, and rollback retains the preceding signed connection state.
func TestProviderWorkMissingJournalSchemaRollsBackSessionMutation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, table := range []string{"provider_work_session_event", "provider_work_session_receipt"} {
			f := newProviderWorkSessionFixture(t)
			failure := func() any {
				restore := forceTableUnavailable(f.ctx, table)
				defer restore()
				return callWithForcedFailure(f.ctx, func(ctx context.Context) {
					server.Raise(DisconnectNetworkClient(ctx, f.sourceConnectionId))
				})
			}()
			if !isForcedFailure(failure, "42P01", "") {
				t.Fatal("missing journal schema did not abort its connection owner", table, failure)
			}
			requireProviderWorkConnectionJournal(t, f, 2, 1)
			server.Raise(DisconnectNetworkClient(f.ctx, f.sourceConnectionId))
			requireProviderWorkConnectionJournal(t, f, 3, 0)
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
