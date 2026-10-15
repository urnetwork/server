// Free batches must prove current source/no-escrow authority before writing,
// and preserve ordinary refusal, busy heads, provenance and rollback custody.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"testing"
	"time"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"gopkg.in/yaml.v3"
)

// The queue intentionally retains an obsolete source hint. A batch reads the
// locked current header and actual escrow, refuses unchanged, then the ordinary
// singleton repairs its owner without attempting debit or free completion.
func TestLegacySourceBatchRefusesChangedSourcePayerAndHiddenEscrow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		for _, mode := range []string{"source", "payer", "legacy_escrow"} {
			f := legacySourceBatchSeed(t, ctx, 8)
			actualSource := f.owner.Id
			var actualPayer *server.Id
			var grant netEscrowOrderingTestFixture
			if mode == "legacy_escrow" {
				grant = newNetEscrowOrderingTestFixture(t, ctx)
				actualPayer = &grant.sourceNetworkId
			} else if mode == "payer" {
				actualPayer = &f.sourceNetworkId
			} else {
				actualSource = server.NewId()
			}
			server.Tx(ctx, func(tx server.PgTx) {
				switch mode {
				case "source":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET source_id=$2 WHERE contract_id=$1`, f.ids[0], actualSource))
				case "payer":
					server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=$2 WHERE contract_id=$1`, f.ids[0], actualPayer))
				case "legacy_escrow":
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
 VALUES($1,$2,100)`, f.ids[0], grant.balanceId))
				}
				server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL,source_client_id=$2 WHERE contract_id=$1`, f.ids[0], f.owner.Id))
			}, server.TxReadCommitted, server.OptNoRetry())
			wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
			owned := context.WithValue(ctx, legacySettlementCloseScopeKey{}, f.owner)
			before := contractClosedCounter.Snapshot()
			wire.enabled(true)
			attempts, err := flushLegacySourceSettlementBatch(owned, f.ids)
			wire.enabled(false)
			counts := wire.snapshot()[server.DefaultPgVaultResourceName]
			closeWire()
			if err != nil || len(attempts) != 1 || attempts[0].contractId != f.ids[0] || !attempts[0].fallback || attempts[0].completed || attempts[0].busy || attempts[0].financialWriteRollback {
				t.Fatal("changed authority was treated as free work", mode, attempts, err)
			}
			if counts["transactions_with_writes_committed"] != 0 || counts["transactions_with_writes_rolled_back"] != 0 || counts["rollback_commands_observed"] != 0 {
				t.Fatal("source authority refusal performed writes", mode, counts)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var untouched bool
				server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1::uuid[]))=8
 AND NOT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=ANY($1::uuid[]) AND outcome IS NOT NULL)
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=ANY($1::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=ANY($1::uuid[]))`, f.ids).Scan(&untouched))
				if !untouched {
					t.Fatal("source refusal changed original custody", mode)
				}
			}, server.OptNoRetry())
			completed, busy, gate, err := flushLegacySettlementWithGrantWait(owned, f.ids[0], nil)
			if err != nil || completed || !busy || gate != legacySettlementBusyAdmission {
				t.Fatal("source fallback bypassed ordinary owner rekey", mode, completed, busy, gate, err)
			}
			server.Db(ctx, func(conn server.PgConn) {
				var payer *server.Id
				var source server.Id
				server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id,source_client_id FROM legacy_settlement_intent WHERE contract_id=$1`, f.ids[0]).Scan(&payer, &source))
				if source != actualSource || (payer == nil) != (actualPayer == nil) || payer != nil && *payer != *actualPayer {
					t.Fatal("ordinary fallback rekeyed to a guessed owner", mode, payer, source)
				}
			}, server.OptNoRetry())
			if !bytes.Equal(f.reports, legacyFinancialCohortReports(ctx, f.ids)) {
				t.Fatal("source authority refusal changed original reports", mode)
			}
			after := contractClosedCounter.Snapshot()
			if !before.Stable || !after.Stable || before.Confirmed != after.Confirmed || before.Uncertain != after.Uncertain || before.Untracked != after.Untracked {
				t.Fatal("source refusal or owner repair claimed a close", mode, before, after)
			}
		}
	})
}

// One independent header owner stays held through a forward page and its head
// revisit. Later rows progress; the released head is recovered through EOF.
func TestLegacySourceBatchBusyHeaderKeepsHeadFairness(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacySourceBatchSeed(t, ctx, 17)
		locked, release, joined := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			var holdErr error
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					var id server.Id
					server.Raise(tx.QueryRow(ctx, `SELECT contract_id FROM transfer_contract WHERE contract_id=$1 FOR UPDATE`, f.ids[0]).Scan(&id))
					close(locked)
					select {
					case <-release:
					case <-ctx.Done():
					}
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { holdErr = err })
			joined <- holdErr
		}()
		released := false
		defer func() {
			if !released {
				cancel()
				<-joined
			}
		}()
		select {
		case <-locked:
		case err := <-joined:
			released = true
			t.Fatal("source fixture could not retain its header", err)
		}
		first, err := runLegacyCloseSettlementPages(ctx, f.owner, nil, 8, false)
		if err != nil || first.Visited != 8 || first.Completed != 7 || first.BusyOrGone != 1 || first.BusyContractUnavailable != 1 || first.Failed != 0 || !first.More || first.Cursor == nil || first.Cursor.ContractId != f.ids[7] {
			t.Fatal("source batch lost private header refusal or forward progress", first, err)
		}
		legacySourceBatchRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids[1:8]))
		second, err := runLegacyCloseSettlementPages(ctx, f.owner, first.Cursor, 8, false)
		if err != nil || second.Completed != 7 || second.HeadVisited != 1 || second.HeadBusyContractUnavailable != 1 || second.Failed != 0 || second.Cursor == nil || second.Cursor.ContractId != f.ids[14] || !second.PassEndTime.Equal(first.PassEndTime) {
			t.Fatal("source continuation starved the private predecessor or changed its cutoff", second, err)
		}
		legacySourceBatchRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids[1:15]))
		close(release)
		released = true
		if err := <-joined; err != nil {
			t.Fatal("source fixture header release failed", err)
		}
		resumed, err := runLegacyCloseSettlementPages(ctx, f.owner, second.Cursor, LegacySettlementPageLimit, true)
		if err != nil || resumed.Completed != 3 || resumed.HeadCompleted != 1 || resumed.Failed != 0 || resumed.BusyOrGone != 0 || resumed.More || resumed.Cursor != nil {
			t.Fatal("source head release did not restore exact EOF progress", resumed, err)
		}
		legacySourceBatchRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
	})
}

// The original request/reservation producer supplies signed evidence. Each
// free terminal row must retain its own signed outcome with the exact stored
// database clock, and a replay must leave those originals byte-for-byte intact.
func TestLegacySourceBatchRetainsEachSignedOutcomeOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkSessionFixture(t)
		requireContractCloseOwnerTestSchema(t, f.ctx)
		ids := make([]server.Id, 8)
		for index := range ids {
			ids[index] = f.contract(t)
		}
		server.Tx(f.ctx, func(tx server.PgTx) {
			for _, id := range ids {
				for _, party := range []server.Id{f.sourceId, f.destinationId} {
					_, _, err := applyContractCloseReportInTx(f.ctx, tx, id, party, 121, false, nil)
					server.Raise(err)
				}
			}
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
 SELECT id,get_byte(uuid_send(id),15)%16,'settled',timestamp '2010-01-01' FROM unnest($1::uuid[]) AS requested(id)`, ids))
		}, server.TxReadCommitted, server.OptNoRetry())
		owner := ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: f.sourceId}
		before := contractClosedCounter.Snapshot()
		result, err := runLegacyCloseSettlementPages(f.ctx, owner, nil, LegacySettlementPageLimit, true)
		if err != nil || result.Completed != len(ids) || result.BusyOrGone != 0 || result.Failed != 0 || result.More {
			t.Fatal("source batch failed signed terminal custody", result, err)
		}
		originals := map[server.Id][]byte{}
		server.Db(f.ctx, func(conn server.PgConn) {
			for _, id := range ids {
				var raw []byte
				var closedAt time.Time
				server.Raise(conn.QueryRow(f.ctx, `SELECT original,close_time FROM provider_work_outcome_original
 JOIN transfer_contract USING(contract_id) WHERE contract_id=$1`, id).Scan(&raw, &closedAt))
				receipt, err := protocol.DecodeProviderWorkReceipt(f.ctx, raw)
				if err != nil || receipt.Outcome == nil || receipt.Outcome.ContractId != id.String() || receipt.Outcome.Outcome != ContractOutcomeSettled ||
					receipt.Outcome.ClosedAtUnixMicro != closedAt.UnixMicro() || receipt.Outcome.SourceBytes != 121 || receipt.Outcome.DestinationBytes != 121 ||
					!receipt.Outcome.SourceComplete || !receipt.Outcome.DestinationComplete {
					t.Fatal("source batch mixed signed contract identity, reports or clock", receipt, err)
				}
				if err := protocol.VerifyProviderWorkReceiptAuthority(f.ctx, receipt, f.source.authority); err != nil {
					t.Fatal("source batch outcome signature failed", err)
				}
				originals[id] = bytes.Clone(raw)
			}
		}, server.OptNoRetry())
		replay, err := runLegacyCloseSettlementPages(f.ctx, owner, nil, LegacySettlementPageLimit, true)
		if err != nil || replay.Completed != 0 || replay.Visited != 0 || replay.More || replay.Cursor != nil {
			t.Fatal("signed source replay repeated work", replay, err)
		}
		server.Db(f.ctx, func(conn server.PgConn) {
			for id, original := range originals {
				var raw []byte
				server.Raise(conn.QueryRow(f.ctx, `SELECT original FROM provider_work_outcome_original WHERE contract_id=$1`, id).Scan(&raw))
				if !bytes.Equal(original, raw) {
					t.Fatal("source replay changed a signed original")
				}
			}
		}, server.OptNoRetry())
		after := contractClosedCounter.Snapshot()
		if !before.Stable || !after.Stable || after.Confirmed-before.Confirmed != uint64(len(ids)) || after.Uncertain != before.Uncertain || after.Untracked != before.Untracked {
			t.Fatal("source provenance repeated or lost a confirmed outcome", before, after)
		}
	})
}

// A real statement failure follows the acknowledged DELETE. The batch cannot
// retry that write-bearing transaction as an unchanged singleton prefix.
func TestLegacySourceBatchWriteFailureRestoresAllIntents(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := legacySourceBatchSeed(t, ctx, 8)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION source_batch_outcome_refusal() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN RAISE EXCEPTION 'synthetic source batch outcome refusal'; END $$;
 CREATE TRIGGER source_batch_outcome_refusal BEFORE UPDATE OF outcome ON transfer_contract
 FOR EACH ROW EXECUTE FUNCTION source_batch_outcome_refusal()`))
		}, server.TxReadCommitted, server.OptNoRetry())
		wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
		defer closeWire()
		owned := context.WithValue(ctx, legacySettlementCloseScopeKey{}, f.owner)
		before := contractClosedCounter.Snapshot()
		wire.enabled(true)
		attempts, err := flushLegacySourceSettlementBatch(owned, f.ids)
		wire.enabled(false)
		if err == nil || len(attempts) != 0 {
			t.Fatal("source write failure was converted into read-only fallback", attempts, err)
		}
		counts := wire.snapshot()[server.DefaultPgVaultResourceName]
		if counts["begin_commands_observed"] != 1 || counts["rollback_commands_observed"] != 1 || counts["commit_commands_observed"] != 0 || counts["transactions_with_writes_rolled_back"] != 1 {
			t.Fatal("source statement failure lost write-bearing rollback custody", counts)
		}
		legacySourceBatchRequire(t, ctx, f, map[server.Id]bool{})
		after := contractClosedCounter.Snapshot()
		if !before.Stable || !after.Stable || before.Confirmed != after.Confirmed || before.Uncertain != after.Uncertain || before.Untracked != after.Untracked {
			t.Fatal("rolled-back source batch claimed a close", before, after)
		}
	})
}

// Missing direction cannot satisfy the current immutable-usage guard. Detect
// it before queuing any outcome so healthy peers never pay for its rollback.
func TestLegacySourceBatchMissingDirectionRefusesBeforeWrites(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := legacySourceBatchSeed(t, ctx, 8)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_origin_is_source=NULL WHERE contract_id=$1`, f.ids[0]))
		}, server.TxReadCommitted, server.OptNoRetry())
		wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
		defer closeWire()
		owned := context.WithValue(ctx, legacySettlementCloseScopeKey{}, f.owner)
		wire.enabled(true)
		attempts, err := flushLegacySourceSettlementBatch(owned, f.ids)
		wire.enabled(false)
		if err != nil || len(attempts) != 1 || !attempts[0].fallback || attempts[0].contractId != f.ids[0] || attempts[0].completed || attempts[0].financialWriteRollback {
			t.Fatal("missing source direction entered outcome writes", attempts, err)
		}
		counts := wire.snapshot()[server.DefaultPgVaultResourceName]
		if counts["transactions_with_writes_committed"] != 0 || counts["transactions_with_writes_rolled_back"] != 0 || counts["rollback_commands_observed"] != 0 {
			t.Fatal("invalid usage rolled back healthy source peers", counts)
		}
		legacySourceBatchRequire(t, ctx, f, map[server.Id]bool{})
	})
}

// The real server commits and this fixture drops only its acknowledgement.
// Neither read-only fallback nor replay may reclassify the eight uncertain
// commit observations as acknowledged closes or mint another outcome.
func TestLegacySourceBatchUnknownCommitNeverFallsBack(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacySourceBatchSeed(t, ctx, 8)
		resource := server.Vault.RequireSimpleResource(server.DefaultPgVaultResourceName)
		proxy := newLegacyCohortCommitReplyProxy(t, ctx, resource.RequireString("authority"))
		defer proxy.close()
		values := maps.Clone(resource.Parse())
		values["authority"] = proxy.listener.Addr().String()
		encoded, err := yaml.Marshal(values)
		server.Raise(err)
		popPg := server.Vault.PushSimpleResource(server.DefaultPgVaultResourceName, encoded)
		server.PgReset()
		defer func() { server.PgReset(); popPg() }()
		before := contractClosedCounter.Snapshot()
		proxy.armed.Store(true)
		owned := context.WithValue(ctx, legacySettlementCloseScopeKey{}, f.owner)
		attempts, err := flushLegacySourceSettlementBatch(owned, f.ids)
		after := contractClosedCounter.Snapshot()
		if err == nil || len(attempts) != 0 || proxy.committed.Load() != 1 || proxy.idle.Load() != 1 {
			t.Fatal("source unknown commit became unchanged fallback", attempts, err)
		}
		if !before.Stable || !after.Stable || after.Confirmed != before.Confirmed || after.Uncertain-before.Uncertain != 8 || after.Untracked != before.Untracked {
			t.Fatal("source unknown acknowledgement changed exact commit custody", before, after)
		}
		legacySourceBatchRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		replay, err := runLegacyCloseSettlementPages(ctx, f.owner, nil, LegacySettlementPageLimit, true)
		if err != nil || replay.Visited != 0 || replay.Completed != 0 || replay.More || replay.Cursor != nil {
			t.Fatal("source unknown commit replay repeated outcome work", replay, err)
		}
		legacySourceBatchRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
		final := contractClosedCounter.Snapshot()
		if after.Confirmed != final.Confirmed || after.Uncertain != final.Uncertain || after.Untracked != final.Untracked {
			t.Fatal("source replay reclassified uncertain commit observations", after, final)
		}
	})
}

// One cohort may contain a retained expiry proof and an adjudicated dispute.
// Their immutable totals, original evidence and dispute clearing stay separate.
func TestLegacySourceBatchKeepsExpiryAndDisputeUsageSeparate(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := legacySourceBatchSeed(t, ctx, 2)
		retained := &contractUsageSnapshot{Version: 1, ByteCount: 4,
			Providers: []contractProviderUsage{{ClientId: f.destinationId, NetworkId: f.destinationNetworkId, ByteCount: 4}},
			Expiry: &contractUsageExpiry{Capacity: 10, Reports: map[ContractParty]contractUsageClose{
				ContractPartySource: {ByteCount: 4, Checkpoint: true}, ContractPartyDestination: {ByteCount: 7, Checkpoint: false},
			}},
		}
		raw, err := json.Marshal(retained)
		server.Raise(err)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET usage_unverified=true,provider_usage=$2::jsonb WHERE contract_id=$1`, f.ids[0], raw))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET dispute=true WHERE contract_id=$1`, f.ids[1]))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET clear_dispute=true,outcome=$2 WHERE contract_id=$1`, f.ids[1], ContractOutcomeDisputeResolvedToSource))
		}, server.TxReadCommitted, server.OptNoRetry())
		owned := context.WithValue(ctx, legacySettlementCloseScopeKey{}, f.owner)
		attempts, err := flushLegacySourceSettlementBatch(owned, f.ids)
		if err != nil || len(attempts) != 2 {
			t.Fatal("mixed source usage did not remain packed", attempts, err)
		}
		for _, attempt := range attempts {
			if !attempt.completed || attempt.busy || attempt.fallback {
				t.Fatal("source expiry or dispute lost its terminal owner", attempt)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT outcome='settled' AND usage_unverified AND provider_usage=$3::jsonb FROM transfer_contract WHERE contract_id=$1)
 AND (SELECT outcome='dispute_resolved_to_source' AND NOT dispute AND provider_usage->>'byte_count'='10'
  AND jsonb_array_length(provider_usage->'providers')=3 FROM transfer_contract WHERE contract_id=$2)
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id IN ($1,$2))
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id IN ($1,$2))
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id IN ($1,$2))
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id IN ($1,$2))`, f.ids[0], f.ids[1], raw).Scan(&exact))
			if !exact {
				t.Fatal("source batch mixed retained expiry, adjudication or financial custody")
			}
		}, server.OptNoRetry())
		if !bytes.Equal(f.reports, legacyFinancialCohortReports(ctx, f.ids)) {
			t.Fatal("source expiry/dispute rewrote original reports")
		}
	})
}
