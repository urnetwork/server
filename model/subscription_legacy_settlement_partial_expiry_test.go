// Existing intents cannot hide valid partial reports from the ordinary deadline.
package model

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Expected counts are explicit test inputs, independent of the continuation's
// action planner and of the settlement arithmetic used by production.
type legacyPartialExpiryTestCase struct {
	name                  string
	source                bool
	destination           bool
	sourceCheckpoint      bool
	destinationCheckpoint bool
	billing               ByteCount
	usage                 ByteCount
}

func legacyPartialExpiryTestCases() []legacyPartialExpiryTestCase {
	return []legacyPartialExpiryTestCase{
		{name: "source checkpoint and destination final", source: true, destination: true, sourceCheckpoint: true, billing: 200, usage: 100},
		{name: "source final", source: true, billing: 300},
		{name: "destination final", destination: true, billing: 100},
		{name: "source checkpoint", source: true, sourceCheckpoint: true, billing: 300},
		{name: "destination checkpoint", destination: true, destinationCheckpoint: true, billing: 100},
		{name: "both checkpoints", source: true, destination: true, sourceCheckpoint: true, destinationCheckpoint: true, billing: 200, usage: 100},
		{name: "source final and destination checkpoint", source: true, destination: true, destinationCheckpoint: true, billing: 200, usage: 100},
		{name: "no reports"},
	}
}

// Real report producers create the partial state. Only the deterministic clock
// and legacy NULL-direction migration state are fixture SQL; no report is edited.
func legacyPartialExpiryTestContract(t testing.TB, ctx context.Context, sample legacyPartialExpiryTestCase, unknownDirection bool) (netEscrowOrderingTestFixture, server.Id) {
	t.Helper()
	f := newNetEscrowOrderingTestFixture(t, ctx)
	contract, posts := createNetEscrowOrderingTestContract(ctx, f, 1000)
	server.RunPosts(ctx, posts...)
	id := contract.ContractId
	if sample.source {
		server.Raise(CloseContract(ctx, id, f.sourceId, 300, sample.sourceCheckpoint))
	}
	if sample.destination {
		server.Raise(CloseContract(ctx, id, f.destinationId, 100, sample.destinationCheckpoint))
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL,
            usage_origin_is_source=CASE WHEN $3 THEN NULL ELSE usage_origin_is_source END WHERE contract_id=$1`,
			id, server.NowUtc().Add(-61*time.Minute), unknownDirection))
	}, server.TxReadCommitted, server.OptNoRetry())
	requireLegacySettlementTestState(t, ctx, f, id, false, false, 1000, 1000)
	return f, id
}

// A synthetic billing peer never appears in the immutable original proof.
// Unknown direction remains zero-credit; known direction preserves both lower bounds.
func requireLegacyPartialExpiryTestProof(t testing.TB, ctx context.Context, f netEscrowOrderingTestFixture, id server.Id,
	sample legacyPartialExpiryTestCase, unknownDirection bool) []byte {
	t.Helper()
	data, proof := readContractExpiryTestSnapshot(t, ctx, id)
	if unknownDirection {
		if proof.ByteCount != 0 || len(proof.Providers) != 0 || proof.Expiry != nil || proof.ExcludedReason != "expired_unconfirmed" {
			t.Fatal("partial legacy expiry inferred unknown usage direction", sample.name, proof)
		}
	} else {
		if proof.Expiry == nil || proof.Expiry.Capacity != 1000 || proof.ByteCount != sample.usage {
			t.Fatal("partial expiry lost original usage bounds", sample.name, proof)
		}
		wantReports := 0
		for _, expected := range []struct {
			party      ContractParty
			present    bool
			count      ByteCount
			checkpoint bool
		}{
			{party: ContractPartySource, present: sample.source, count: 300, checkpoint: sample.sourceCheckpoint},
			{party: ContractPartyDestination, present: sample.destination, count: 100, checkpoint: sample.destinationCheckpoint},
		} {
			report, found := proof.Expiry.Reports[expected.party]
			if found != expected.present || found && (report.ByteCount != expected.count || report.Checkpoint != expected.checkpoint) {
				t.Fatal("expiry proof changed an original report or filled its missing peer", sample.name, expected.party, report)
			}
			if expected.present {
				wantReports++
			}
		}
		if len(proof.Expiry.Reports) != wantReports {
			t.Fatal("expiry proof gained an unrecognized party", sample.name)
		}
		if sample.source && sample.destination {
			if proof.ExcludedReason != "" || len(proof.Providers) != 1 || proof.Providers[0].ClientId != f.destinationId ||
				proof.Providers[0].NetworkId != f.destinationNetworkId || proof.Providers[0].ByteCount != sample.usage {
				t.Fatal("known original reports lost their provider attribution", sample.name, proof)
			}
		} else if len(proof.Providers) != 0 || proof.ExcludedReason != "expired_unconfirmed" {
			t.Fatal("one-sided expiry manufactured bilateral provider work", sample.name, proof)
		}
	}
	server.Db(ctx, func(conn server.PgConn) {
		var unverified, stillUnknown bool
		server.Raise(conn.QueryRow(ctx, `SELECT usage_unverified,usage_origin_is_source IS NULL FROM transfer_contract WHERE contract_id=$1`, id).Scan(&unverified, &stillUnknown))
		if !unverified || stillUnknown != unknownDirection {
			t.Fatal("expiry changed direction or omitted its retained proof", sample.name)
		}
	})
	return data
}

// Existing counts remain exact. A missing party receives only the ordinary
// billing fallback, and all settled reports become final without an increment.
func requireLegacyPartialExpiryTestReports(t testing.TB, ctx context.Context, id server.Id, sample legacyPartialExpiryTestCase) {
	t.Helper()
	sourceBytes, destinationBytes := ByteCount(0), ByteCount(0)
	if sample.source {
		sourceBytes, destinationBytes = 300, 300
	}
	if sample.destination {
		destinationBytes = 100
		if !sample.source {
			sourceBytes = 100
		}
	}
	server.Db(ctx, func(conn server.PgConn) {
		var exact bool
		server.Raise(conn.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(NOT checkpoint AND
            used_transfer_byte_count=CASE party WHEN 'source' THEN $2 WHEN 'destination' THEN $3 ELSE -1 END)
            FROM contract_close WHERE contract_id=$1`, id, sourceBytes, destinationBytes).Scan(&exact))
		if !exact {
			t.Fatal("expiry continuation changed report counts or left a checkpoint", sample.name)
		}
	})
}

// Both variants use the same public report producers. The no-intent control
// completes through the sweep first; the existing-intent variant must converge
// through its payer owner while recent reports pin the absolute 60-minute cause.
func TestLegacySettlementPartialExpiryMatchesOrdinaryPolicy(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, unknownDirection := range []bool{false, true} {
			for _, sample := range legacyPartialExpiryTestCases() {
				for _, existingIntent := range []bool{false, true} {
					f, id := legacyPartialExpiryTestContract(t, ctx, sample, unknownDirection)
					if existingIntent {
						server.Raise(SettleEscrow(ctx, id, ContractOutcomeSettled))
						requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 1000)
					}
					before := readRedisExpiryRepairTestState(ctx, id)
					count, _, err := ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
					if err != nil || count != 0 {
						t.Fatal("legacy partial sweep bypassed its financial owner", sample.name, existingIntent, count, err)
					}
					if existingIntent && !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
						t.Fatal("sweep bypassed existing partial intent custody", sample.name)
					}
					var retained []byte
					if !existingIntent {
						retained = requireLegacyPartialExpiryTestProof(t, ctx, f, id, sample, unknownDirection)
						requireLegacyPartialExpiryTestReports(t, ctx, id, sample)
					}
					page, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 8)
					if err != nil || page.Completed != 1 || page.Visited != 1 || page.Failed != 0 || page.BusyOrGone != 0 || page.More || page.Cursor != nil {
						t.Fatal("valid expired partial stayed behind its intent", sample.name, unknownDirection, existingIntent, page, err)
					}
					requireLegacySettlementTestState(t, ctx, f, id, false, true, 1000-sample.billing, 0)
					requireLegacyProviderDurability(t, ctx, f, id, int64(sample.billing))
					proof := requireLegacyPartialExpiryTestProof(t, ctx, f, id, sample, unknownDirection)
					if retained != nil && !bytes.Equal(retained, proof) {
						t.Fatal("settlement rewrote the ordinary expiry proof", sample.name)
					}
					requireLegacyPartialExpiryTestReports(t, ctx, id, sample)
					replay, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 8)
					if err != nil || replay.Completed != 0 || replay.Visited != 0 || replay.More || replay.Cursor != nil {
						t.Fatal("partial expiry replay repeated financial work", sample.name, replay, err)
					}
					if !bytes.Equal(proof, requireLegacyPartialExpiryTestProof(t, ctx, f, id, sample, unknownDirection)) {
						t.Fatal("partial expiry replay changed immutable usage", sample.name)
					}
					requireLegacySettlementTestState(t, ctx, f, id, false, true, 1000-sample.billing, 0)
					requireLegacyProviderDurability(t, ctx, f, id, int64(sample.billing))
					t.Logf("partial expiry policy name=%s unknown_direction=%t existing_intent=%t closed=1", sample.name, unknownDirection, existingIntent)
				}
			}
		}
	})
}

// A pending adjudication keeps its accepted outcome and selected original
// count. Expiry finalizes that checkpoint without inventing its missing peer.
func TestLegacySettlementPartialExpiryKeepsAdjudicatedAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, unknownDirection := range []bool{false, true} {
			for _, source := range []bool{true, false} {
				sample := legacyPartialExpiryTestCase{name: "adjudicated checkpoint", source: source, destination: !source,
					sourceCheckpoint: source, destinationCheckpoint: !source, billing: 300}
				outcome := ContractOutcomeDisputeResolvedToSource
				if !source {
					outcome, sample.billing = ContractOutcomeDisputeResolvedToDestination, 100
				}
				f, id := legacyPartialExpiryTestContract(t, ctx, sample, unknownDirection)
				server.Raise(SettleEscrow(ctx, id, outcome))
				page, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 8)
				if err != nil || page.Completed != 1 || page.Failed != 0 || page.BusyOrGone != 0 {
					t.Fatal("adjudicated checkpoint did not expire under its selected authority", source, unknownDirection, page, err)
				}
				requireLegacySettlementTestState(t, ctx, f, id, false, true, 1000-sample.billing, 0)
				requireLegacyProviderDurability(t, ctx, f, id, int64(sample.billing))
				requireLegacyPartialExpiryTestProof(t, ctx, f, id, sample, unknownDirection)
				server.Db(ctx, func(conn server.PgConn) {
					var exact bool
					server.Raise(conn.QueryRow(ctx, `SELECT outcome=$2 AND
                        (SELECT count(*)=1 AND bool_and(NOT checkpoint AND used_transfer_byte_count=$3)
                        FROM contract_close WHERE contract_id=$1)
                        FROM transfer_contract WHERE contract_id=$1`, id, outcome, sample.billing).Scan(&exact))
					if !exact {
						t.Fatal("expiry replaced the accepted outcome or supplied an adjudicated peer")
					}
				})
			}
		}
	})
}

// The expiry proof and synthesized peer share the rejected owner's rollback.
// Its reservation and original report remain available for ordinary recovery.
func TestLegacySettlementPartialExpiryAccountingRollbackKeepsReports(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		sample := legacyPartialExpiryTestCase{name: "underfunded source final", source: true, billing: 300}
		f, id := legacyPartialExpiryTestContract(t, ctx, sample, false)
		server.Raise(SettleEscrow(ctx, id, ContractOutcomeSettled))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_escrow SET balance_byte_count=299 WHERE contract_id=$1`, id))
		}, server.TxReadCommitted, server.OptNoRetry())
		refreshNetEscrow(ctx, []server.Id{f.balanceId})
		originalReports := legacyFinancialCohortReports(ctx, []server.Id{id})
		page, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 8)
		if err != nil || page.Completed != 0 || page.Failed != 1 || page.Visited != 1 {
			t.Fatal("partial expiry bypassed insufficient own escrow", page, err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 299)
		requireLegacyProviderDurability(t, ctx, f, id, 0)
		if !bytes.Equal(originalReports, legacyFinancialCohortReports(ctx, []server.Id{id})) {
			t.Fatal("failed partial expiry retained a synthetic peer")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT NOT c.usage_unverified AND c.provider_usage IS NULL AND
                i.failure_code='accounting' AND i.next_attempt_time>statement_timestamp() AT TIME ZONE 'UTC'
                FROM transfer_contract c JOIN legacy_settlement_intent i USING(contract_id) WHERE contract_id=$1`, id).Scan(&exact))
			if !exact {
				t.Fatal("failed partial expiry detached its proof or accounting custody")
			}
		})
		neighbor, posts := createNetEscrowOrderingTestContract(ctx, f, 100)
		server.RunPosts(ctx, posts...)
		server.Raise(CloseContract(ctx, neighbor.ContractId, f.sourceId, 11, false))
		server.Raise(CloseContract(ctx, neighbor.ContractId, f.destinationId, 11, false))
		tail, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 8)
		if err != nil || tail.Completed != 1 || tail.Failed != 0 || tail.More || tail.Cursor != nil {
			t.Fatal("refused partial expiry held a same-payer arrival after the pass", tail, err)
		}
		requireLegacySettlementTestState(t, ctx, f, neighbor.ContractId, false, true, 989, 299)
		requireLegacySettlementTestState(t, ctx, f, id, true, false, 989, 299)
		if !bytes.Equal(originalReports, legacyFinancialCohortReports(ctx, []server.Id{id})) {
			t.Fatal("healthy continuation changed the refused owner's original report")
		}
	})
}

// A partial member takes individual expiry continuation after the committed
// healthy prefix. Actual wire observations forbid rolling back healthy writes.
func TestLegacyFinancialCohortPartialExpiryPreservesPrefix(t *testing.T) {
	for _, unknownDirection := range []bool{false, true} {
		env := server.DefaultTestEnv()
		env.RerunCount = 0
		env.Run(t, func(t testing.TB) {
			ctx := t.Context()
			f := legacyFinancialCohortSeed(t, ctx, 8)
			id := f.ids[3]
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL,
                    usage_origin_is_source=CASE WHEN $3 THEN NULL ELSE usage_origin_is_source END WHERE contract_id=$1`,
					id, server.NowUtc().Add(-61*time.Minute), unknownDirection))
				server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET checkpoint=(party='source'),close_time=$2 WHERE contract_id=$1`, id, server.NowUtc()))
			}, server.TxReadCommitted, server.OptNoRetry())
			f.reports = legacyFinancialCohortReports(ctx, f.ids)
			legacyFinancialCohortRequire(t, ctx, f, map[server.Id]bool{})
			healthyIds := append(append([]server.Id{}, f.ids[:3]...), f.ids[4:]...)
			healthyReports := legacyFinancialCohortReports(ctx, healthyIds)
			wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
			defer closeWire()
			before := contractClosedCounter.Snapshot()
			wire.enabled(true)
			page, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, 8)
			wire.enabled(false)
			if err != nil || page.Completed != 8 || page.Visited != 8 || page.Failed != 0 || page.BusyOrGone != 0 ||
				page.FinancialCohortCompleted != 3 || page.FinancialCohortFallbacks != 1 || page.FinancialCohortWriteRollbacks != 0 {
				t.Fatal("partial expiry lost its healthy committed prefix", unknownDirection, page, err)
			}
			for route, counts := range wire.snapshot() {
				if counts["transactions_with_writes_rolled_back"] != 0 || counts["rollback_commands_observed"] != 0 ||
					counts["connections_closed_with_active_writes"] != 0 || counts["connections_closed_after_begin_without_end_command"] != 0 {
					t.Fatal("partial admission rolled back healthy writes", unknownDirection, route, counts)
				}
			}
			if !bytes.Equal(healthyReports, legacyFinancialCohortReports(ctx, healthyIds)) {
				t.Fatal("partial expiry changed a healthy sibling report")
			}
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT count(*)=2 AND bool_and(NOT checkpoint AND used_transfer_byte_count=3)
                    FROM contract_close WHERE contract_id=$1`, id).Scan(&exact))
				if !exact {
					t.Fatal("partial expiry rewrote a count or left a checkpoint")
				}
			})
			_, proof := readContractExpiryTestSnapshot(t, ctx, id)
			if unknownDirection {
				f.excludedUsage = map[server.Id]bool{id: true}
				if proof.Expiry != nil || proof.ByteCount != 0 || proof.ExcludedReason != "expired_unconfirmed" {
					t.Fatal("partial cohort inferred a legacy direction")
				}
			} else if proof.Expiry == nil || len(proof.Expiry.Reports) != 2 || proof.ByteCount != 3 ||
				!proof.Expiry.Reports[ContractPartySource].Checkpoint || proof.Expiry.Reports[ContractPartyDestination].Checkpoint {
				t.Fatal("partial cohort lost the original checkpoint proof")
			}
			// Only the asserted partial finalization advances this report oracle.
			f.reports = legacyFinancialCohortReports(ctx, f.ids)
			legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
			after := contractClosedCounter.Snapshot()
			if !before.Stable || !after.Stable || after.Confirmed-before.Confirmed != 8 ||
				after.Uncertain != before.Uncertain || after.Untracked != before.Untracked {
				t.Fatal("partial expiry changed per-contract commit custody", before, after)
			}
			replay, err := FlushLegacyPayerSettlements(ctx, f.payer.sourceNetworkId, nil, 8)
			if err != nil || replay.Visited != 0 || replay.Completed != 0 || replay.More || replay.Cursor != nil {
				t.Fatal("partial prefix replay repeated financial work", replay, err)
			}
			legacyFinancialCohortRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		})
	}
}

// Actual expiry eligibility owns report completion. Recent NULL rows and old
// rows with an explicit future deadline refuse before proof/report writes.
func TestLegacySettlementPartialExpiryKeepsFreshReports(t *testing.T) {
	for _, unknownDirection := range []bool{false, true} {
		for _, futureDeadline := range []bool{false, true} {
			env := server.DefaultTestEnv()
			env.RerunCount = 0
			env.Run(t, func(t testing.TB) {
				ctx := t.Context()
				sample := legacyPartialExpiryTestCase{name: "fresh checkpoint", source: true, destination: true, sourceCheckpoint: true, billing: 200, usage: 100}
				f, id := legacyPartialExpiryTestContract(t, ctx, sample, unknownDirection)
				now := server.NowUtc()
				created := now
				var deadline *time.Time
				if futureDeadline {
					created = now.Add(-2 * time.Hour)
					value := now.Add(time.Hour)
					deadline = &value
				}
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=$3 WHERE contract_id=$1`, id, created, deadline))
				}, server.TxReadCommitted, server.OptNoRetry())
				server.Raise(SettleEscrow(ctx, id, ContractOutcomeSettled))
				before := readRedisExpiryRepairTestState(ctx, id)
				wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
				defer closeWire()
				wire.enabled(true)
				completed, busy, _, err := flushLegacySettlement(ctx, id)
				wire.enabled(false)
				if err == nil || completed || busy || !bytes.Equal(before, readRedisExpiryRepairTestState(ctx, id)) {
					t.Fatal("partial intent overrode its fresh reports or future deadline", unknownDirection, futureDeadline, completed, busy, err)
				}
				for route, counts := range wire.snapshot() {
					if counts["transactions_with_writes_committed"] != 0 || counts["transactions_with_writes_rolled_back"] != 0 {
						t.Fatal("ineligible partial entered expiry writes", route, counts)
					}
				}
				requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 1000)
				requireLegacyProviderDurability(t, ctx, f, id, 0)
			})
		}
	}
}

// Disputed partials use the same retained proof and report owner. Legacy work
// keeps the dispute until settlement; Redis work retains its ordinary debit.
func TestContractExpirationDisputedPartialReportsContinue(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, redis := range []bool{false, true} {
			for _, sample := range []legacyPartialExpiryTestCase{
				{name: "disputed source final", source: true, billing: 300},
				{name: "disputed destination checkpoint", destination: true, destinationCheckpoint: true, billing: 100},
				{name: "disputed reportless"},
			} {
				f := newNetEscrowOrderingTestFixture(t, ctx)
				var contract *TransferEscrow
				if redis {
					contract = createRedisAdmissionTest(ctx, f, 1000)
				} else {
					var posts []func() any
					contract, posts = createNetEscrowOrderingTestContract(ctx, f, 1000)
					server.RunPosts(ctx, posts...)
				}
				id := contract.ContractId
				if sample.source {
					server.Raise(CloseContract(ctx, id, f.sourceId, 300, sample.sourceCheckpoint))
				}
				if sample.destination {
					server.Raise(CloseContract(ctx, id, f.destinationId, 100, sample.destinationCheckpoint))
				}
				SetContractDispute(ctx, id, true)
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL WHERE contract_id=$1`, id, server.NowUtc().Add(-61*time.Minute)))
				}, server.TxReadCommitted, server.OptNoRetry())
				count, _, err := ForceCloseOpenContractIdsPage(ctx, server.NowUtc().Add(-5*time.Minute), 32, 1, 0, 0, nil)
				want := int64(0)
				if redis {
					want = 1
				}
				if err != nil || count != want {
					t.Fatal("disputed partial expiry lost its financial continuation", sample.name, redis, count, err)
				}
				proof := requireLegacyPartialExpiryTestProof(t, ctx, f, id, sample, false)
				requireLegacyPartialExpiryTestReports(t, ctx, id, sample)
				if redis {
					page, err := FlushTransferDebits(ctx, int(f.balanceId[15])%TransferDebitShardCount, nil, 8)
					if err != nil || page.Failed != 0 {
						t.Fatal("disputed partial debit failed", sample.name, page, err)
					}
				} else {
					requireLegacySettlementTestState(t, ctx, f, id, true, false, 1000, 1000)
					server.Db(ctx, func(conn server.PgConn) {
						var retained bool
						server.Raise(conn.QueryRow(ctx, `SELECT c.dispute AND i.clear_dispute AND i.outcome='settled'
                            FROM transfer_contract c JOIN legacy_settlement_intent i USING(contract_id) WHERE contract_id=$1`, id).Scan(&retained))
						if !retained {
							t.Fatal("partial disputed expiry released custody before financial settlement")
						}
					})
					page, err := FlushLegacyPayerSettlements(ctx, f.sourceNetworkId, nil, 8)
					if err != nil || page.Completed != 1 || page.Failed != 0 || page.BusyOrGone != 0 {
						t.Fatal("disputed partial legacy intent did not settle", sample.name, page, err)
					}
				}
				requireLegacySettlementTestState(t, ctx, f, id, false, true, 1000-sample.billing, 0)
				if !bytes.Equal(proof, requireLegacyPartialExpiryTestProof(t, ctx, f, id, sample, false)) {
					t.Fatal("disputed partial settlement rewrote original proof")
				}
				server.Db(ctx, func(conn server.PgConn) {
					var exact bool
					server.Raise(conn.QueryRow(ctx, `SELECT outcome='settled' AND NOT dispute AND
                            COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=$1),0)=$2
                            FROM transfer_contract WHERE contract_id=$1`, id, sample.billing).Scan(&exact))
					if !exact {
						t.Fatal("disputed partial changed its accepted outcome or exact payout")
					}
				})
			}
		}
	})
}

// Retained source-owned intents carry report custody without money. Real
// partial reports need the same absolute expiry even when no grant exists.
func TestLegacySourceSettlementPartialExpiry(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, unknownDirection := range []bool{false, true} {
			for _, sample := range []legacyPartialExpiryTestCase{
				{name: "free source checkpoint and destination final", source: true, destination: true, sourceCheckpoint: true, usage: 100},
				{name: "free source final", source: true},
				{name: "free reportless"},
			} {
				f := newNetEscrowOrderingTestFixture(t, ctx)
				id, err := CreateContractNoEscrow(ctx, f.sourceNetworkId, f.sourceId, f.destinationNetworkId, f.destinationId, 1000)
				server.Raise(err)
				if sample.source {
					server.Raise(CloseContract(ctx, id, f.sourceId, 300, sample.sourceCheckpoint))
				}
				if sample.destination {
					server.Raise(CloseContract(ctx, id, f.destinationId, 100, sample.destinationCheckpoint))
				}
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2,expiration_time=NULL,
                        usage_origin_is_source=CASE WHEN $3 THEN NULL ELSE usage_origin_is_source END WHERE contract_id=$1`,
						id, server.NowUtc().Add(-61*time.Minute), unknownDirection))
					server.Raise(queueLegacySettlementInTx(ctx, tx, id, ContractOutcomeSettled, false))
				}, server.TxReadCommitted, server.OptNoRetry())
				owner := ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: f.sourceId}
				page, err := runLegacyCloseSettlementPages(ctx, owner, nil, 8, true)
				if err != nil || page.Completed != 1 || page.Failed != 0 || page.BusyOrGone != 0 || page.More || page.Cursor != nil {
					t.Fatal("source-owned partial did not expire", sample.name, unknownDirection, page, err)
				}
				proof := requireLegacyPartialExpiryTestProof(t, ctx, f, id, sample, unknownDirection)
				requireLegacyPartialExpiryTestReports(t, ctx, id, sample)
				server.Db(ctx, func(conn server.PgConn) {
					var exact bool
					server.Raise(conn.QueryRow(ctx, `SELECT outcome='settled'
                        AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
                        AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1)
                        AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
                        AND (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=1000
                        FROM transfer_contract WHERE contract_id=$1`, id, f.balanceId).Scan(&exact))
					if !exact {
						t.Fatal("source partial expiry gained money or retained an intent")
					}
				})
				requireLegacyProviderDurability(t, ctx, f, id, 0)
				replay, err := runLegacyCloseSettlementPages(ctx, owner, nil, 8, true)
				if err != nil || replay.Completed != 0 || replay.Visited != 0 || replay.More || replay.Cursor != nil ||
					!bytes.Equal(proof, requireLegacyPartialExpiryTestProof(t, ctx, f, id, sample, unknownDirection)) {
					t.Fatal("source partial replay changed custody or original proof", replay, err)
				}
			}
		}
	})
}
