package model

// The lingering origins' side table (contract_origin_linger.go): what its
// trigger writes on each path that closes a contract, that the origin reads
// through it stay exact without transfer_contract_open_source_id, that the
// reads keep their previous statements until the side table is complete, and
// that the sweep deletes only rows past their prune time, in bounded batches.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// The contract's side row's prune time, nil without one.
func lingerTestPruneTime(t testing.TB, ctx context.Context, contractId server.Id) (pruneTime *time.Time) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT prune_time FROM contract_origin_linger WHERE contract_id = $1`, contractId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&pruneTime))
			}
		})
	})
	return
}

// The contract's deadline as every origin read computes it.
func lingerTestDeadline(t testing.TB, ctx context.Context, contractId server.Id) (deadline time.Time) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COALESCE(expiration_time, create_time + interval '60 minutes') FROM transfer_contract WHERE contract_id = $1`,
			contractId,
		).Scan(&deadline))
	})
	return
}

// The side row exists with the contract's deadline as its prune time.
func lingerTestRequireRow(t testing.TB, ctx context.Context, contractId server.Id, path string) {
	t.Helper()
	pruneTime := lingerTestPruneTime(t, ctx, contractId)
	if pruneTime == nil {
		t.Fatalf("%s: the early close wrote no side row", path)
	}
	if deadline := lingerTestDeadline(t, ctx, contractId); !pruneTime.Equal(deadline) {
		t.Fatalf("%s: the side row prunes at %s, want the deadline %s", path, pruneTime, deadline)
	}
}

// Inserts a contract between the clients with the given clock offsets from
// the database clock; a nil offset is a null column.
func lingerTestInsertContract(
	t testing.TB,
	ctx context.Context,
	clients escrowSelectionTestClients,
	createOffset time.Duration,
	expirationOffset *time.Duration,
	closeOffset *time.Duration,
	outcome *ContractOutcome,
	dispute bool,
) server.Id {
	t.Helper()
	contractId := server.NewId()
	var expirationSeconds, closeSeconds *float64
	if expirationOffset != nil {
		seconds := expirationOffset.Seconds()
		expirationSeconds = &seconds
	}
	if closeOffset != nil {
		seconds := closeOffset.Seconds()
		closeSeconds = &seconds
	}
	// an outcome carries its usage proof, as the settlement guard requires
	var usage *string
	if outcome != nil {
		proof := `{"version":1,"byte_count":0,"providers":[]}`
		usage = &proof
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `
			INSERT INTO transfer_contract (
				contract_id, source_network_id, source_id, destination_network_id, destination_id,
				transfer_byte_count, payer_network_id, create_time, expiration_time, close_time,
				outcome, dispute, provider_usage
			)
			SELECT
				$1, $2, $3, $4, $5, 1024, $2,
				now_utc + $6 * interval '1 second',
				now_utc + $7 * interval '1 second',
				now_utc + $8 * interval '1 second',
				$9, $10, $11::jsonb
			FROM (SELECT clock_timestamp() AT TIME ZONE 'UTC' AS now_utc) AS clock
		`, contractId, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId,
			createOffset.Seconds(), expirationSeconds, closeSeconds, outcome, dispute, usage))
	})
	return contractId
}

// An early close on the settlement path writes the side row with the
// contract's deadline; a contract that is open, or only checkpointed, writes
// none.
func TestContractOriginLingerCapturesASettlementBeforeItsDeadline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		clients := newEscrowSelectionTestClients(t, ctx)
		addContractPayoutTestBalance(ctx, clients.payerNetworkId, 8192)
		origin, err := clients.create(ctx, 1024, false)
		if err != nil {
			t.Fatal(err)
		}
		if pruneTime := lingerTestPruneTime(t, ctx, origin.ContractId); pruneTime != nil {
			t.Fatalf("an open contract has a side row pruned at %s", pruneTime)
		}
		if err := CloseContract(ctx, origin.ContractId, clients.payerId, 512, true); err != nil {
			t.Fatal(err)
		}
		if pruneTime := lingerTestPruneTime(t, ctx, origin.ContractId); pruneTime != nil {
			t.Fatalf("a checkpoint wrote a side row pruned at %s", pruneTime)
		}
		if err := CloseContract(ctx, origin.ContractId, clients.payerId, 512, false); err != nil {
			t.Fatal(err)
		}
		if err := CloseContract(ctx, origin.ContractId, clients.providerId, 512, false); err != nil {
			t.Fatal(err)
		}
		lingerTestRequireRow(t, ctx, origin.ContractId, "settlement")
	})
}

// The force close of a quiet contract before its deadline writes the side row.
func TestContractOriginLingerCapturesAForceCloseBeforeItsDeadline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		clients := newEscrowSelectionTestClients(t, ctx)
		contractId, err := CreateContractNoEscrow(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, 1024)
		if err != nil {
			t.Fatal(err)
		}
		// every contract quiet since an hour from now is due
		if _, err := ForceCloseOpenContractIds(ctx, server.NowUtc().Add(time.Hour), 10, 1, 0, 0); err != nil {
			t.Fatal(err)
		}
		lingerTestRequireRow(t, ctx, contractId, "force close")
	})
}

// A dispute closes the contract and writes the side row; resolving it back to
// open leaves the row, which the reads' own open condition sends to the open
// arm.
func TestContractOriginLingerCapturesADisputeAndItsReopenStaysExact(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		clients := newEscrowSelectionTestClients(t, ctx)
		addContractPayoutTestBalance(ctx, clients.payerNetworkId, 8192)
		addContractPayoutTestBalance(ctx, clients.providerNetworkId, 8192)
		origin, err := clients.create(ctx, 1024, false)
		if err != nil {
			t.Fatal(err)
		}
		SetContractDispute(ctx, origin.ContractId, true)
		lingerTestRequireRow(t, ctx, origin.ContractId, "dispute")

		SetContractDispute(ctx, origin.ContractId, false)
		var open bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT open FROM transfer_contract WHERE contract_id = $1`, origin.ContractId).Scan(&open))
		})
		if !open {
			t.Fatal("the resolved dispute did not reopen the contract")
		}
		// the open arm, not the side row, anchors the companion
		companion, err := clients.create(ctx, 1024, true)
		if err != nil || companion == nil || companion.CompanionContractId == nil || *companion.CompanionContractId != origin.ContractId {
			t.Fatalf("a companion on the reopened origin = %+v, %v", companion, err)
		}
	})
}

// A claim with a retirement time lowers the deadline to it: a retirement still
// ahead keeps the close early and the side row prunes at it; a retirement
// already past leaves no side row.
func TestContractOriginLingerFollowsARetirementClaim(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		clients := newEscrowSelectionTestClients(t, ctx)
		usage := `{"version":1,"byte_count":0,"providers":[]}`
		for _, retirementOffset := range []time.Duration{20 * time.Minute, -time.Minute} {
			contractId, err := CreateContractNoEscrow(ctx, clients.payerNetworkId, clients.payerId, clients.providerNetworkId, clients.providerId, 1024)
			if err != nil {
				t.Fatal(err)
			}
			retirement := server.NowUtc().Add(retirementOffset)
			server.Tx(ctx, func(tx server.PgTx) {
				claimed, err := claimContractOutcomeWithUsageValueInTx(ctx, tx, contractId, ContractOutcomeSettled, usage, false, &retirement)
				if err != nil || !claimed {
					t.Fatalf("the retirement claim = %t, %v", claimed, err)
				}
			})
			pruneTime := lingerTestPruneTime(t, ctx, contractId)
			if 0 < retirementOffset {
				lingerTestRequireRow(t, ctx, contractId, "retirement claim")
				if pruneTime.Sub(retirement).Abs() > time.Millisecond {
					t.Fatalf("the side row prunes at %s, want the retirement %s", pruneTime, retirement)
				}
			} else if pruneTime != nil {
				t.Fatalf("a claim retired before its close wrote a side row pruned at %s", pruneTime)
			}
		}
	})
}

// The trigger keeps exactly the closes the closed arm can read: an insert or
// update that closes before the deadline, by the close time or by the database
// clock when an application clock stamped the close late; not a close at or
// after the deadline, a close without a close time, or an open contract.
func TestContractOriginLingerCapturesOnlyClosesBeforeTheirDeadline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		settled := ContractOutcomeSettled
		duration := func(d time.Duration) *time.Duration { return &d }
		for _, c := range []struct {
			name       string
			create     time.Duration
			expiration *time.Duration
			close      *time.Duration
			outcome    *ContractOutcome
			dispute    bool
			captured   bool
		}{
			{name: "settled early", create: -10 * time.Minute, expiration: duration(50 * time.Minute), close: duration(-time.Minute), outcome: &settled, captured: true},
			{name: "disputed early", create: -10 * time.Minute, expiration: duration(50 * time.Minute), close: duration(-time.Minute), dispute: true, captured: true},
			{name: "legacy deadline from creation", create: -10 * time.Minute, close: duration(-time.Minute), outcome: &settled, captured: true},
			{name: "close stamped past the deadline ahead of the database clock", create: -59 * time.Minute, expiration: duration(time.Minute), close: duration(2 * time.Minute), dispute: true, captured: true},
			{name: "settled at the deadline", create: -70 * time.Minute, expiration: duration(-10 * time.Minute), close: duration(-10 * time.Minute), outcome: &settled},
			{name: "settled after the deadline", create: -70 * time.Minute, expiration: duration(-10 * time.Minute), close: duration(-time.Minute), outcome: &settled},
			{name: "legacy deadline past", create: -90 * time.Minute, close: duration(-time.Minute), outcome: &settled},
			{name: "disputed without a close time", create: -10 * time.Minute, expiration: duration(50 * time.Minute), dispute: true},
			{name: "open with a close time", create: -10 * time.Minute, expiration: duration(50 * time.Minute), close: duration(-time.Minute)},
		} {
			contractId := lingerTestInsertContract(t, ctx, clients, c.create, c.expiration, c.close, c.outcome, c.dispute)
			if captured := lingerTestPruneTime(t, ctx, contractId) != nil; captured != c.captured {
				t.Fatalf("%s: captured %t, want %t", c.name, captured, c.captured)
			}
			if c.captured {
				lingerTestRequireRow(t, ctx, contractId, c.name)
			}
		}

		// a deadline lowered while closed moves the prune time with it
		contractId := lingerTestInsertContract(t, ctx, clients, -10*time.Minute, duration(50*time.Minute), duration(-time.Minute), &settled, false)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time = expiration_time - interval '20 minutes' WHERE contract_id = $1`, contractId))
		})
		lingerTestRequireRow(t, ctx, contractId, "lowered deadline")
	})
}

// No update that leaves a contract open, or that changes none of the
// trigger's columns, enters the capture function: with the function replaced
// by one that raises, those updates succeed, and a close raises.
func TestContractOriginLingerOtherUpdatesNeverEnterTheFunction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clients := newEscrowSelectionTestClients(t, ctx)
		settled := ContractOutcomeSettled
		fifty := 50 * time.Minute
		closeOffset := -time.Minute
		openId := lingerTestInsertContract(t, ctx, clients, -10*time.Minute, &fifty, nil, nil, false)
		closedId := lingerTestInsertContract(t, ctx, clients, -10*time.Minute, &fifty, &closeOffset, &settled, false)
		lingerTestRequireRow(t, ctx, closedId, "fixture close")

		setFunction := func(body string) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `CREATE OR REPLACE FUNCTION contract_origin_linger_capture()
					RETURNS trigger LANGUAGE plpgsql AS $capture$`+body+`$capture$`))
			})
		}
		setFunction(`BEGIN RAISE EXCEPTION 'contract_origin_linger_capture entered'; END`)
		defer setFunction(server.ContractOriginLingerCaptureBodySql())

		for _, update := range []string{
			`UPDATE transfer_contract SET stream_id = gen_random_uuid() WHERE contract_id = ANY($1)`,
			`UPDATE transfer_contract SET usage_unverified = true WHERE contract_id = ANY($1)`,
			`UPDATE transfer_contract SET reap_time = clock_timestamp() AT TIME ZONE 'UTC' WHERE contract_id = ANY($1)`,
			// the trigger's columns, on a contract that stays open
			`UPDATE transfer_contract SET expiration_time = expiration_time - interval '1 minute', close_time = clock_timestamp() AT TIME ZONE 'UTC' WHERE contract_id = ANY($1) AND open`,
		} {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, update, []server.Id{openId, closedId}))
			})
		}

		var closeErr error
		server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute = true WHERE contract_id = $1`, openId))
			}, server.OptNoRetry())
		}, func(err error) {
			closeErr = err
		})
		if closeErr == nil || !strings.Contains(closeErr.Error(), "contract_origin_linger_capture entered") {
			t.Fatalf("a close did not enter the capture function: %v", closeErr)
		}
	})
}

// The root cause: transfer_contract_open_source_id (323 GiB on Main) served
// only the closed origin arms. With it dropped, every companion origin read
// still finds an origin that closed early within the linger, through either
// creation path, and still refuses an origin past its deadline or one whose
// close is older than the linger.
func TestCompanionOriginReadsStayExactWithoutTheClosedPairIndex(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `DROP INDEX IF EXISTS transfer_contract_open_source_id`))
		}, server.OptReadWrite(), server.OptNoRetry())
		contractOriginLingerReads.forget()
		defer contractOriginLingerReads.forget()

		settled := ContractOutcomeSettled
		deadline := 30 * time.Minute
		for _, c := range []struct {
			name       string
			create     time.Duration
			expiration time.Duration
			close      time.Duration
			linger     time.Duration
			found      bool
		}{
			{name: "closed early within the linger", create: -30 * time.Minute, expiration: deadline, close: -20 * time.Minute, linger: time.Hour, found: true},
			{name: "close older than the linger", create: -30 * time.Minute, expiration: deadline, close: -20 * time.Minute, linger: 10 * time.Minute},
			{name: "past its deadline", create: -70 * time.Minute, expiration: -10 * time.Minute, close: -40 * time.Minute, linger: time.Hour},
		} {
			for _, zeroEscrow := range []bool{false, true} {
				clients := newEscrowSelectionTestClients(t, ctx)
				// the companion's destination, the origin's payer, pays it
				addContractPayoutTestBalance(ctx, clients.payerNetworkId, 8192)
				expiration, closeOffset := c.expiration, c.close
				originId := lingerTestInsertContract(t, ctx, clients, c.create, &expiration, &closeOffset, &settled, false)
				if exists := CompanionOriginExists(ctx, clients.providerId, clients.payerId, c.linger); exists != c.found {
					t.Fatalf("%s: the existence read = %t, want %t", c.name, exists, c.found)
				}
				var companion *TransferEscrow
				var err error
				if zeroEscrow {
					companion, err = CreateZeroEscrowCompanionContract(ctx, clients.providerNetworkId, clients.providerId,
						clients.payerNetworkId, clients.payerId, 1024, c.linger)
				} else {
					companion, err = CreateCompanionTransferEscrow(ctx, clients.providerNetworkId, clients.providerId,
						clients.payerNetworkId, clients.payerId, 1024, c.linger)
				}
				if c.found {
					if err != nil || companion == nil || companion.CompanionContractId == nil || *companion.CompanionContractId != originId {
						t.Fatalf("%s zero escrow %t: the companion = %+v, %v, want one on the lingering origin", c.name, zeroEscrow, companion, err)
					}
				} else if companion != nil || !errors.Is(err, ErrMissingCompanionOrigin) {
					t.Fatalf("%s zero escrow %t: the companion = %+v, %v, want no origin", c.name, zeroEscrow, companion, err)
				}
			}
		}
	})
}

// A lingering companion (a chained reply carrier's anchor) is found through
// the side table too, with the index dropped.
func TestCompanionChainedOriginReadsALingeringCompanionWithoutTheClosedPairIndex(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `DROP INDEX IF EXISTS transfer_contract_open_source_id`))
		}, server.OptReadWrite(), server.OptNoRetry())
		clients := newEscrowSelectionTestClients(t, ctx)
		addContractPayoutTestBalance(ctx, clients.payerNetworkId, 8192)
		addContractPayoutTestBalance(ctx, clients.providerNetworkId, 8192)
		origin, err := clients.create(ctx, 1024, false)
		if err != nil {
			t.Fatal(err)
		}
		// the provider's return companion, payer to provider reversed
		reply, err := clients.create(ctx, 1024, true)
		if err != nil || reply == nil {
			t.Fatalf("the reply companion = %+v, %v", reply, err)
		}
		// the origin goes past its deadline; the reply closes early
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time = create_time - interval '1 second' WHERE contract_id = $1`, origin.ContractId))
		})
		SetContractDispute(ctx, reply.ContractId, true)
		lingerTestRequireRow(t, ctx, reply.ContractId, "reply dispute")
		// a companion on the reply's direction chains to it
		chained, err := CreateCompanionTransferEscrow(ctx, clients.payerNetworkId, clients.payerId,
			clients.providerNetworkId, clients.providerId, 1024, time.Hour)
		if err != nil || chained == nil || chained.CompanionContractId == nil || *chained.CompanionContractId != reply.ContractId {
			t.Fatalf("a companion chained to the lingering reply = %+v, %v", chained, err)
		}
	})
}

// The reads keep their previous statements until the side table is complete:
// an origin that closed while the trigger did not exist is found by them, and
// would be missed by the side table. Without the state row they keep them too.
// Each decision is counted.
func TestContractOriginLingerReadsWaitForTheCompleteTime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := WithProviderWorkSessionSource(t.Context(), nil)
		contractOriginLingerReads.forget()
		defer contractOriginLingerReads.forget()
		setCompleteTime := func(sql string) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, sql))
			})
			contractOriginLingerReads.forget()
		}
		if !contractOriginLingerReads.ready(ctx) {
			t.Fatal("a new database's side table is not complete")
		}

		clients := newEscrowSelectionTestClients(t, ctx)
		settled := ContractOutcomeSettled
		expiration, closeOffset := 40*time.Minute, -10*time.Minute
		// closed while the trigger did not exist
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE transfer_contract DISABLE TRIGGER transfer_contract_origin_linger`))
		}, server.OptReadWrite(), server.OptNoRetry())
		originId := lingerTestInsertContract(t, ctx, clients, -20*time.Minute, &expiration, &closeOffset, &settled, false)
		server.MaintenanceDb(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ALTER TABLE transfer_contract ENABLE TRIGGER transfer_contract_origin_linger`))
		}, server.OptReadWrite(), server.OptNoRetry())
		if pruneTime := lingerTestPruneTime(t, ctx, originId); pruneTime != nil {
			t.Fatal("the fixture's close wrote a side row")
		}

		observed, statements, closeScope := observePgStatements(t, ctx)
		defer closeScope()
		existsBy := func(want bool, sideTable bool, phase string) {
			t.Helper()
			legacyBefore := testutil.ToFloat64(contractOriginLingerLegacyReads)
			sideBefore := testutil.ToFloat64(contractOriginLingerSideTableReads)
			if exists := CompanionOriginExists(observed, clients.providerId, clients.payerId, time.Hour); exists != want {
				t.Fatalf("%s: the existence read = %t, want %t", phase, exists, want)
			}
			reads := statements.take()
			if len(reads) != 1 || strings.Contains(reads[0], "contract_origin_linger") != sideTable {
				t.Fatalf("%s: the existence read used %q, want the side table %t", phase, reads, sideTable)
			}
			legacy := testutil.ToFloat64(contractOriginLingerLegacyReads) - legacyBefore
			side := testutil.ToFloat64(contractOriginLingerSideTableReads) - sideBefore
			if sideTable && (side != 1 || legacy != 0) || !sideTable && (legacy != 1 || side != 0) {
				t.Fatalf("%s: counted %g side table and %g legacy reads", phase, side, legacy)
			}
		}

		setCompleteTime(`UPDATE contract_origin_linger_state SET complete_time = clock_timestamp() AT TIME ZONE 'UTC' + interval '75 minutes'`)
		existsBy(true, false, "warming up")
		setCompleteTime(`DELETE FROM contract_origin_linger_state`)
		existsBy(true, false, "no state row")
		setCompleteTime(`INSERT INTO contract_origin_linger_state (singleton, complete_time) VALUES (true, clock_timestamp() AT TIME ZONE 'UTC' - interval '1 second')`)
		// complete: the side table misses a close from before the trigger,
		// which is why the reads wait out the warmup
		existsBy(false, true, "complete")
	})
}

// A decision before the first read of the state waits for it, and a read
// failure never turns a process to the side table.
func TestContractOriginLingerReadsNeverGuessComplete(t *testing.T) {
	completion := newContractOriginLingerCompletion(DefaultContractOriginLingerSettings())
	completion.reading = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if completion.ready(ctx) {
		t.Fatal("a decision before the first read guessed complete")
	}
}

// Inserts side rows pruned at each offset from the database clock.
func lingerTestInsertRows(t testing.TB, ctx context.Context, offsets []time.Duration) []server.Id {
	t.Helper()
	contractIds := []server.Id{}
	server.Tx(ctx, func(tx server.PgTx) {
		for _, offset := range offsets {
			contractId := server.NewId()
			contractIds = append(contractIds, contractId)
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO contract_origin_linger (source_id, destination_id, contract_id, prune_time)
				VALUES (gen_random_uuid(), gen_random_uuid(), $1, clock_timestamp() AT TIME ZONE 'UTC' + $2 * interval '1 second')
			`, contractId, offset.Seconds()))
		}
	})
	return contractIds
}

func lingerTestRowCount(t testing.TB, ctx context.Context, contractIds []server.Id) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_origin_linger WHERE contract_id = ANY($1)`, contractIds).Scan(&count))
	})
	return
}

// The sweep deletes the rows past their prune time, oldest first, in batches
// its settings bound, and keeps every row still ahead of it. A run that spends
// its batches says so, reports the lag it left, and the next run finishes.
func TestRemoveExpiredContractOriginLingerDeletesOnlyRowsPastTheirPruneTime(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		expired := lingerTestInsertRows(t, ctx, []time.Duration{
			-3 * time.Hour, -2 * time.Hour, -time.Hour, -30 * time.Minute, -10 * time.Minute, -time.Minute, -time.Second,
		})
		kept := lingerTestInsertRows(t, ctx, []time.Duration{time.Minute, time.Hour})
		settings := DefaultContractOriginLingerSweepSettings()
		settings.BatchSize = 2
		settings.MaxBatches = 2
		deletedBefore := testutil.ToFloat64(retentionDeletedRowsCounter.WithLabelValues(RetentionTableContractOriginLinger))

		report, err := RemoveExpiredContractOriginLinger(ctx, settings)
		if err != nil || report.Deleted != 4 || report.Batches != 2 || report.Stop != ContractOriginLingerSweepStopBatches || !report.More() {
			t.Fatalf("the bounded run = %+v, %v; want 4 rows in 2 batches and more left", report, err)
		}
		// the oldest went first: the row due 10 minutes ago is the oldest left
		if report.Lag < 8*time.Minute || 12*time.Minute < report.Lag {
			t.Fatalf("the bounded run left a lag of %s, want about 10 minutes", report.Lag)
		}
		if remaining := lingerTestRowCount(t, ctx, expired[:4]); remaining != 0 {
			t.Fatalf("the oldest rows were not deleted first: %d left", remaining)
		}
		if lag := testutil.ToFloat64(contractOriginLingerSweepLagGauge); lag < 8*60 || 12*60 < lag {
			t.Fatalf("the lag gauge reads %g s, want about 600", lag)
		}

		report, err = RemoveExpiredContractOriginLinger(ctx, settings)
		if err != nil || report.Deleted != 3 || report.Stop != ContractOriginLingerSweepStopCaughtUp || report.More() || report.Lag != 0 {
			t.Fatalf("the second run = %+v, %v; want the last 3 rows and caught up", report, err)
		}
		if remaining := lingerTestRowCount(t, ctx, expired); remaining != 0 {
			t.Fatalf("%d rows past their prune time are left", remaining)
		}
		if remaining := lingerTestRowCount(t, ctx, kept); remaining != len(kept) {
			t.Fatalf("the sweep deleted a row still ahead of its prune time: %d of %d left", remaining, len(kept))
		}
		if moved := testutil.ToFloat64(retentionDeletedRowsCounter.WithLabelValues(RetentionTableContractOriginLinger)) - deletedBefore; moved != 7 {
			t.Fatalf("the deleted row counter moved by %g, want 7", moved)
		}
		if lag := testutil.ToFloat64(contractOriginLingerSweepLagGauge); lag != 0 {
			t.Fatalf("the lag gauge reads %g s after catching up", lag)
		}
	})
}

// The sweep's run budget bounds it: a spent budget deletes nothing and says
// there is more.
func TestRemoveExpiredContractOriginLingerStopsAtItsBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		expired := lingerTestInsertRows(t, ctx, []time.Duration{-time.Hour})
		settings := DefaultContractOriginLingerSweepSettings()
		settings.RunBudget = 0
		report, err := RemoveExpiredContractOriginLinger(ctx, settings)
		if err != nil || report.Deleted != 0 || report.Stop != ContractOriginLingerSweepStopBudget || !report.More() {
			t.Fatalf("a spent budget = %+v, %v", report, err)
		}
		if remaining := lingerTestRowCount(t, ctx, expired); remaining != 1 {
			t.Fatal("a spent budget deleted a row")
		}
	})
}
