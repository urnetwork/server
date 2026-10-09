// Real source-owner pages prove bounded batching, per-contract usage, replay,
// and refusal before any free close can enter a financial path.
package model

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

type legacySourceBatchFixture struct {
	owner                ContractCloseOwner
	ids                  []server.Id
	sourceNetworkId      server.Id
	destinationNetworkId server.Id
	destinationId        server.Id
	streamId             server.Id
	participantId        server.Id
	participantNetworkId server.Id
	extenderId           server.Id
	extenderNetworkId    server.Id
	reports              []byte
}

// Endpoint identities intentionally have no live directory membership. Half
// the retained directions reverse service independently of the source owner.
func legacySourceBatchSeed(t testing.TB, ctx context.Context, count int) legacySourceBatchFixture {
	t.Helper()
	requireContractCloseOwnerTestSchema(t, ctx)
	f := legacySourceBatchFixture{
		owner: ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: server.NewId()},
		ids:   make([]server.Id, count), sourceNetworkId: server.NewId(),
		destinationNetworkId: server.NewId(), destinationId: server.NewId(), streamId: server.NewId(),
		participantId: server.NewId(), participantNetworkId: server.NewId(),
		extenderId: server.NewId(), extenderNetworkId: server.NewId(),
	}
	prefix := server.NewId()
	for index := range f.ids {
		f.ids[index] = legacyPayerTestContractId(prefix, uint32(index+1), index%LegacySettlementShardCount)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,usage_origin_is_source,stream_id)
 SELECT id,$2,$3,$4,$5,10,ordinal%2=1,$6 FROM unnest($1::uuid[]) WITH ORDINALITY AS requested(id,ordinal)`,
			f.ids, f.sourceNetworkId, f.owner.Id, f.destinationNetworkId, f.destinationId, f.streamId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close
 (contract_id,party,used_transfer_byte_count,checkpoint,close_time)
 SELECT id,party,amount,false,timestamp '2010-01-01' FROM unnest($1::uuid[]) AS requested(id)
 CROSS JOIN (VALUES ('source',11),('destination',9)) AS report(party,amount)`, f.ids))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_participant(stream_id,client_id,network_id) VALUES($1,$2,$3)`,
			f.streamId, f.participantId, f.participantNetworkId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_extender(contract_id,extender_id,party,client_id,network_id)
 SELECT id,$2,'source',$2,$3 FROM unnest($1::uuid[]) AS requested(id)`, f.ids, f.extenderId, f.extenderNetworkId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
 SELECT id,get_byte(uuid_send(id),15)%16,'settled',timestamp '2010-01-01' FROM unnest($1::uuid[]) AS requested(id)`, f.ids))
	}, server.TxReadCommitted, server.OptNoRetry())
	f.reports = legacyFinancialCohortReports(ctx, f.ids)
	return f
}

// Independent SQL reads check money, original reports, retained identities and
// immutable service allocation after a committed prefix and after its replay.
func legacySourceBatchRequire(t testing.TB, ctx context.Context, f legacySourceBatchFixture, completed map[server.Id]bool) {
	t.Helper()
	if !bytes.Equal(f.reports, legacyFinancialCohortReports(ctx, f.ids)) {
		t.Fatal("source batch changed original reports")
	}
	server.Db(ctx, func(conn server.PgConn) {
		var exact bool
		server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1::uuid[]))=$2
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=ANY($1::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=ANY($1::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=ANY($1::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM network_client WHERE client_id=$3)`, f.ids, len(f.ids)-len(completed), f.owner.Id).Scan(&exact))
		if !exact {
			t.Fatal("source batch lost intent custody, created financial records or required live source membership")
		}
		rows, err := conn.Query(ctx, `SELECT contract_id,outcome,close_time,provider_usage FROM transfer_contract
 WHERE contract_id=ANY($1::uuid[]) ORDER BY contract_id`, f.ids)
		seen := 0
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				var outcome *ContractOutcome
				var closedAt *time.Time
				var raw []byte
				server.Raise(rows.Scan(&id, &outcome, &closedAt, &raw))
				index := slices.Index(f.ids, id)
				if index < 0 {
					t.Fatal("source batch returned a foreign contract")
				}
				seen++
				if !completed[id] {
					if outcome != nil || len(raw) != 0 {
						t.Fatal("source batch wrote an unreturned suffix", index)
					}
					continue
				}
				if outcome == nil || *outcome != ContractOutcomeSettled || closedAt == nil {
					t.Fatal("source batch lost a completed outcome", index)
				}
				usage, err := decodeContractUsageSnapshot(raw)
				if err != nil || usage.ByteCount != 9 || len(usage.Providers) != 3 {
					t.Fatal("source batch changed per-contract usage", index, usage, err)
				}
				providers := map[server.Id]server.Id{f.participantId: f.participantNetworkId, f.extenderId: f.extenderNetworkId}
				if index%2 == 0 {
					providers[f.destinationId] = f.destinationNetworkId
				} else {
					providers[f.owner.Id] = f.sourceNetworkId
				}
				for _, provider := range usage.Providers {
					if network, present := providers[provider.ClientId]; !present || network != provider.NetworkId || provider.ByteCount != 3 {
						t.Fatal("source batch lost service direction or fresh retained participant", index, provider)
					}
					delete(providers, provider.ClientId)
				}
				if len(providers) != 0 {
					t.Fatal("source batch omitted a retained provider", index)
				}
			}
		})
		if seen != len(f.ids) {
			t.Fatal("source batch lost retained contracts")
		}
	}, server.OptNoRetry())
}

// This is the causal regression: the original runner emits 64 source singleton
// transactions. The new runner must emit exactly eight bounded source batches.
// Actual protocol counts compare the whole free page, including its EOF read.
func TestContractCloseOwnerFreePageUsesBoundedSourceBatches(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		var singletonReady int64
		for _, packed := range []bool{false, true} {
			f := legacySourceBatchSeed(t, ctx, 64)
			wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
			kinds := map[string]int{}
			observed := context.WithValue(ctx, legacyFinancialDiagnosticKey{}, func(value legacyFinancialDiagnosticObservation) { kinds[value.Kind]++ })
			cooldown := &legacyFinancialCohortCooldown{now: func() time.Duration { return 0 }}
			cooldown.deferProbe(int(f.owner.Id[15]) % LegacySettlementShardCount)
			observed = context.WithValue(observed, legacyFinancialCohortCooldownKey{}, cooldown)
			before := contractClosedCounter.Snapshot()
			wire.enabled(true)
			var result LegacyPayerSettlementResult
			var err error
			if packed {
				result, err = runLegacyCloseSettlementPages(observed, f.owner, nil, LegacySettlementPageLimit, true)
			} else {
				bounded, cancel := context.WithTimeoutCause(observed, 15*time.Second, errLegacySettlementPageBudget)
				result, err = flushLegacyCloseSettlementPages(observed, bounded, f.owner, nil, LegacySettlementPageLimit, true,
					func(parent, page context.Context, shard int, cursor *LegacySettlementCursor, limit int) (LegacySettlementFlushResult, error) {
						return flushLegacySettlementsPage(parent, page, shard, cursor, limit, flushLegacySettlementWithGrantWait)
					})
				cancel()
			}
			wire.enabled(false)
			counts := wire.snapshot()
			closeWire()
			if err != nil || result.Completed != len(f.ids) || result.Visited != len(f.ids) || result.BusyOrGone != 0 || result.Failed != 0 || result.More || result.Cursor != nil {
				t.Fatal("source page lost bounded progress or EOF", packed, result, err)
			}
			if result.FinancialCohortAttempts != 0 || result.FinancialCohortSelected != 0 || result.FinancialCohortCompleted != 0 ||
				result.FinancialCohortFallbacks != 0 || result.FinancialCohortWriteRollbacks != 0 {
				t.Fatal("source batch entered financial cohort accounting", result)
			}
			legacySourceBatchRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
			after := contractClosedCounter.Snapshot()
			if !before.Stable || !after.Stable || after.Confirmed-before.Confirmed != uint64(len(f.ids)) || after.Uncertain != before.Uncertain || after.Untracked != before.Untracked {
				t.Fatal("source page lost per-contract acknowledged commit custody", before, after)
			}
			ordinary := counts[server.DefaultPgVaultResourceName]
			if ordinary["rollback_commands_observed"] != 0 || ordinary["transactions_with_writes_rolled_back"] != 0 || ordinary["connections_closed_with_active_writes"] != 0 {
				t.Fatal("healthy source page rolled back or lost writes", counts)
			}
			wantTransactions := int64(64)
			if packed {
				wantTransactions = 8
			}
			if ordinary["begin_commands_observed"] != wantTransactions || ordinary["commit_commands_observed"] != wantTransactions || ordinary["transactions_with_writes_committed"] != wantTransactions {
				t.Fatal("source page changed its exact bounded transaction count", packed, counts)
			}
			if !packed {
				singletonReady = ordinary["ready_replies_observed"]
				if kinds["source_singleton"] != 64 || kinds["source_batch"] != 0 {
					t.Fatal("singleton protocol control changed", kinds)
				}
			} else {
				if kinds["source_batch"] != 8 || kinds["source_singleton"] != 0 || len(kinds) != 1 {
					t.Fatal("free page still pays one transaction per contract", kinds)
				}
				if ordinary["ready_replies_observed"]*5 >= singletonReady {
					t.Fatal("free page failed to amortize actual Ready exchanges by five", singletonReady, ordinary["ready_replies_observed"])
				}
			}
			replay, err := runLegacyCloseSettlementPages(ctx, f.owner, nil, LegacySettlementPageLimit, true)
			if err != nil || replay.Visited != 0 || replay.Completed != 0 || replay.More || replay.Cursor != nil {
				t.Fatal("source batch EOF replay repeated work", replay, err)
			}
			legacySourceBatchRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
			final := contractClosedCounter.Snapshot()
			if final.Confirmed != after.Confirmed || final.Uncertain != after.Uncertain || final.Untracked != after.Untracked {
				t.Fatal("source batch replay changed commit custody")
			}
			t.Logf("source_page_protocol packed=%t contracts=64 wire=%v", packed, counts)
		}
	})
}

// A separate sibling owner commits shared membership after the batch owns all
// headers. A real protocol barrier forces the later participant statement to
// observe the new retained identity without touching any locked header.
func TestLegacySourceBatchReadsParticipantsAfterHeaderOwnership(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		f := legacySourceBatchSeed(t, ctx, 8)
		newParticipantId, newParticipantNetworkId := server.NewId(), server.NewId()
		wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
		defer closeWire()
		mutate := make(chan struct{})
		mutated := make(chan error, 1)
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			select {
			case <-mutate:
			case <-ctx.Done():
				mutated <- ctx.Err()
				return
			}
			var mutationErr error
			server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_participant SET client_id=$2,network_id=$3 WHERE stream_id=$1`, f.streamId, newParticipantId, newParticipantNetworkId))
				}, server.TxReadCommitted, server.OptNoRetry())
			}, func(err error) { mutationErr = err })
			mutated <- mutationErr
		}()
		defer func() { cancel(); <-joined }()
		var selects atomic.Int64
		mutationOutcome := make(chan error, 1)
		wire.proxies[server.DefaultPgVaultResourceName].commandCompleteObservation.Store(&legacyCohortCommandCompleteObservation{observe: func(tag []byte) {
			if bytes.Equal(tag, []byte("SELECT 8\x00")) && selects.Add(1) == 2 {
				close(mutate)
				mutationOutcome <- <-mutated
			}
		}})
		owned := context.WithValue(ctx, legacySettlementCloseScopeKey{}, f.owner)
		wire.enabled(true)
		attempts, err := flushLegacySourceSettlementBatch(owned, f.ids)
		wire.enabled(false)
		wire.proxies[server.DefaultPgVaultResourceName].commandCompleteObservation.Store(nil)
		if err != nil || selects.Load() < 2 || len(attempts) != len(f.ids) {
			t.Fatal("source freshness barrier did not complete the owned batch", attempts, err, selects.Load())
		}
		if mutationErr := <-mutationOutcome; mutationErr != nil {
			t.Fatal("source freshness sibling owner failed", mutationErr)
		}
		for _, attempt := range attempts {
			if !attempt.completed || attempt.busy || attempt.fallback {
				t.Fatal("fresh source batch fell back", attempt)
			}
		}
		f.participantId, f.participantNetworkId = newParticipantId, newParticipantNetworkId
		legacySourceBatchRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
	})
}

// The identical prewrite clock boundary is tested before writes and after the
// real outcome acknowledgement. An admitted batch must never soft-roll back.
func legacySourceBatchBudgetControl(t *testing.T, afterWrite bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := legacySourceBatchSeed(t, ctx, 8)
		wire, closeWire := legacyFinancialRunProtocolBind(t, ctx)
		defer closeWire()
		var advanced atomic.Bool
		base := time.Now()
		observed := context.WithValue(ctx, legacySettlementCloseScopeKey{}, f.owner)
		observed = context.WithValue(observed, legacyFinancialCohortClockKey{}, func() time.Time {
			if advanced.Load() {
				return base.Add(time.Hour)
			}
			return base
		})
		wire.proxies[server.DefaultPgVaultResourceName].commandCompleteObservation.Store(&legacyCohortCommandCompleteObservation{observe: func(tag []byte) {
			// Two original reports and two retained intermediary/extender
			// memberships per contract each return 16 rows before writes.
			if afterWrite && bytes.Equal(tag, []byte("UPDATE 8\x00")) || !afterWrite && bytes.Equal(tag, []byte("SELECT 16\x00")) {
				advanced.Store(true)
			}
		}})
		before := contractClosedCounter.Snapshot()
		wire.enabled(true)
		attempts, err := flushLegacySourceSettlementBatch(observed, f.ids)
		wire.enabled(false)
		wire.proxies[server.DefaultPgVaultResourceName].commandCompleteObservation.Store(nil)
		if err != nil || !advanced.Load() || ctx.Err() != nil {
			t.Fatal("source budget control lost its real boundary or parent", attempts, err)
		}
		completed := map[server.Id]bool{}
		var commits, rollbacks int64
		if afterWrite {
			commits = 1
			completed = legacyFinancialCohortCompleted(f.ids)
			if len(attempts) != len(f.ids) {
				t.Fatal("admitted source writes did not remain packed", attempts)
			}
			for _, attempt := range attempts {
				if !attempt.completed || attempt.busy || attempt.fallback || attempt.financialWriteRollback {
					t.Fatal("soft boundary repeated admitted source work", attempt)
				}
			}
		} else {
			rollbacks = 1
			if len(attempts) != 1 || attempts[0].contractId != f.ids[0] || !attempts[0].fallback || !attempts[0].deadlineFallback || attempts[0].completed || attempts[0].financialWriteRollback {
				t.Fatal("source admission refusal lost its unchanged prefix", attempts)
			}
		}
		counts := wire.snapshot()[server.DefaultPgVaultResourceName]
		if counts["begin_commands_observed"] != 1 || counts["commit_commands_observed"] != commits || counts["rollback_commands_observed"] != rollbacks ||
			counts["transactions_with_writes_committed"] != commits || counts["transactions_with_writes_rolled_back"] != 0 {
			t.Fatal("source budget guard wrote before refusal or rolled back healthy writes", counts)
		}
		legacySourceBatchRequire(t, ctx, f, completed)
		after := contractClosedCounter.Snapshot()
		if !before.Stable || !after.Stable || after.Confirmed-before.Confirmed != uint64(len(completed)) || after.Uncertain != before.Uncertain || after.Untracked != before.Untracked {
			t.Fatal("source budget control changed commit custody", before, after)
		}
	})
}

func TestLegacySourceBatchRefusesBeforeWrites(t *testing.T) {
	legacySourceBatchBudgetControl(t, false)
}

func TestLegacySourceBatchAdmittedWritesSurviveSoftBoundary(t *testing.T) {
	legacySourceBatchBudgetControl(t, true)
}

// Cancellation occurs after an acknowledged eight-row batch, never after an
// elapsed sleep. Both owners retain that prefix and the original pass cutoff.
func legacySourceBatchCancellationControl(t *testing.T, cancelParent bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := legacySourceBatchSeed(t, ctx, 17)
		parent, stopParent := context.WithCancel(ctx)
		defer stopParent()
		bounded, stopPage := context.WithCancelCause(parent)
		defer stopPage(nil)
		calls := 0
		result, err := flushLegacyCloseSettlementPages(parent, bounded, f.owner, nil, LegacySettlementPageLimit, true,
			func(parent, page context.Context, shard int, cursor *LegacySettlementCursor, limit int) (LegacySettlementFlushResult, error) {
				return flushLegacySettlementsPage(parent, page, shard, cursor, limit, flushLegacySettlementWithGrantWait,
					func(call context.Context, ids []server.Id) ([]legacyFinancialCohortAttempt, error) {
						calls++
						attempts, err := flushLegacySourceSettlementBatch(call, ids)
						if err == nil {
							if cancelParent {
								stopParent()
							} else {
								stopPage(errLegacySettlementPageBudget)
							}
						}
						return attempts, err
					})
			})
		if cancelParent && !errors.Is(err, context.Canceled) && !errors.Is(err, server.DbContextDoneError) || !cancelParent && err != nil {
			t.Fatal("source cancellation changed parent/page error custody", cancelParent, err)
		}
		if calls != 1 || result.Completed != 8 || result.Visited != 8 || result.Failed != 0 || result.Cursor == nil || result.Cursor.ContractId != f.ids[7] || result.PassEndTime.IsZero() || !cancelParent && !result.More {
			t.Fatal("source cancellation lost its acknowledged prefix", calls, result)
		}
		legacySourceBatchRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids[:8]))
		resumed, err := runLegacyCloseSettlementPages(ctx, f.owner, result.Cursor, LegacySettlementPageLimit, true)
		if err != nil || resumed.Completed != 9 || resumed.Visited != 9 || resumed.BusyOrGone != 0 || resumed.Failed != 0 || resumed.More || resumed.Cursor != nil || !resumed.PassEndTime.Equal(result.PassEndTime) {
			t.Fatal("source continuation changed its fixed pass or EOF", resumed, err)
		}
		legacySourceBatchRequire(t, ctx, f, legacyFinancialCohortCompleted(f.ids))
	})
}

func TestLegacySourceBatchPageBudgetKeepsCommittedPrefix(t *testing.T) {
	legacySourceBatchCancellationControl(t, false)
}

func TestLegacySourceBatchParentCancellationKeepsError(t *testing.T) {
	legacySourceBatchCancellationControl(t, true)
}
