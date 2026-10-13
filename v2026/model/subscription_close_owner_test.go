// Close scheduling selects retained client identity only when no financial
// payer exists, and keeps that identity separate from the payer namespace.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
)

// A missing payer cache does not turn a legacy reservation into free work.
func TestContractCloseOwnerSelectsFinancialPayerOrRetainedSource(t *testing.T) {
	sourceId, payerId, otherPayerId := server.NewId(), server.NewId(), server.NewId()
	emptyPayerId := server.Id{}
	for _, test := range []struct {
		name             string
		sourceId         server.Id
		payerNetworkId   *server.Id
		escrowNetworkIds []server.Id
		hasEscrow        bool
		want             ContractCloseOwner
		wantErr          bool
	}{
		{
			name: "retained payer without escrow", sourceId: sourceId, payerNetworkId: &payerId,
			want: ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: payerId},
		},
		{
			name: "retained payer remains authoritative", sourceId: sourceId, payerNetworkId: &payerId,
			escrowNetworkIds: []server.Id{otherPayerId}, hasEscrow: true,
			want: ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: payerId},
		},
		{
			name: "legacy escrow resolves missing payer", sourceId: sourceId,
			escrowNetworkIds: []server.Id{payerId}, hasEscrow: true,
			want: ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: payerId},
		},
		{
			name: "free work retains source client", sourceId: sourceId,
			want: ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: sourceId},
		},
		{
			name: "equal payer and source ids remain payer work", sourceId: payerId,
			escrowNetworkIds: []server.Id{payerId}, hasEscrow: true,
			want: ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: payerId},
		},
		{
			name: "missing legacy grant is not free", sourceId: sourceId, hasEscrow: true,
			wantErr: true,
		},
		{
			name: "ambiguous financial owner is not free", sourceId: sourceId,
			escrowNetworkIds: []server.Id{payerId, otherPayerId}, hasEscrow: true, wantErr: true,
		},
		{
			name: "empty grant owner is not free", sourceId: sourceId,
			escrowNetworkIds: []server.Id{{}}, hasEscrow: true, wantErr: true,
		},
		{
			name: "empty retained payer is not free", sourceId: sourceId,
			payerNetworkId: &emptyPayerId, wantErr: true,
		},
		{
			name: "missing source cannot own free work", wantErr: true,
		},
		{
			name: "missing retained source is rejected for paid work", payerNetworkId: &payerId, wantErr: true,
		},
	} {
		got, err := selectContractCloseOwner(test.sourceId, test.payerNetworkId, test.escrowNetworkIds, test.hasEscrow)
		if (err != nil) != test.wantErr || got != test.want {
			t.Fatalf("%s: owner=%+v err=%v, want=%+v error=%t", test.name, got, err, test.want, test.wantErr)
		}
	}
}

// Equal uuid bytes must not merge independent client and payer task owners.
func TestContractCloseOwnerNamespacesKeepPaidRunOnceIdentity(t *testing.T) {
	id := server.NewId()
	payer := ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: id}
	source := ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: id}
	if payer.runOnce().String() != task.RunOnce("flush_legacy_payer_settlements", id).String() {
		t.Fatal("paid close owner changed the existing run-once identity")
	}
	if payer.runOnce().String() == source.runOnce().String() {
		t.Fatal("source client and payer network with equal ids share a task owner")
	}
}

// Old paid arguments retain their payer identity; new source arguments cannot
// carry a paid alias even when both namespaces use the same UUID bytes.
func TestContractCloseOwnerTaskArgumentsPreserveLegacyPaidScope(t *testing.T) {
	id := server.NewId()
	payer := ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: id}
	source := ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: id}
	for _, test := range []struct {
		name    string
		args    *LegacyPayerSettlementArgs
		want    ContractCloseOwner
		wantErr bool
	}{
		{name: "legacy payer", args: &LegacyPayerSettlementArgs{Private: true, PayerNetworkId: id}, want: payer},
		{name: "typed payer", args: &LegacyPayerSettlementArgs{Private: true, PayerNetworkId: id, Owner: &payer}, want: payer},
		{name: "typed source", args: &LegacyPayerSettlementArgs{Private: true, Owner: &source}, want: source},
		{name: "equal uuid cannot alias payer", args: &LegacyPayerSettlementArgs{Private: true, PayerNetworkId: id, Owner: &source}, wantErr: true},
		{name: "missing private marker", args: &LegacyPayerSettlementArgs{Owner: &source}, wantErr: true},
		{name: "missing owner", args: &LegacyPayerSettlementArgs{Private: true}, wantErr: true},
		{name: "nil arguments", wantErr: true},
	} {
		got, err := legacyCloseTaskOwner(test.args)
		if (err != nil) != test.wantErr || got != test.want {
			t.Fatal(test.name, "changed typed task scope", got, err)
		}
	}
}

// Test setup must install the proposal through the candidate migration list;
// a missing retained-source column is a deployment failure, not a runtime gate.
func requireContractCloseOwnerTestSchema(t testing.TB, ctx context.Context) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		var present bool
		server.Raise(conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_attribute
 WHERE attrelid='legacy_settlement_intent'::regclass AND attname='source_client_id' AND NOT attisdropped)`).Scan(&present))
		if !present {
			t.Fatal("candidate close-owner source schema is missing")
		}
	})
}

// Actual durable publication preserves both namespaces and each first cursor
// while later wake requests retain the minimum requested deadline.
func TestContractCloseOwnerSchedulingKeepsNamespacesAndEarliestRunAt(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		id := server.NewId()
		owners := []ContractCloseOwner{
			{Kind: ContractCloseOwnerPayerNetwork, Id: id},
			{Kind: ContractCloseOwnerSourceClient, Id: id},
		}
		keys := make([]server.PgOwnershipKey, len(owners))
		for index, owner := range owners {
			keys[index] = task.RunOnceOwnershipKey(owner.runOnce())
		}
		due := time.Date(2020, time.January, 1, 0, 0, 0, 0, time.UTC)
		cursor := &LegacySettlementCursor{NextAttemptTime: due, ContractId: server.NewId(), PassEndTime: due}
		server.OwnedTx(ctx, keys, func(tx server.PgTx) {
			for _, owner := range owners {
				ScheduleLegacyCloseSettlementsInTx(clientSession, tx, owner, cursor, due.Add(time.Hour))
				ScheduleLegacyCloseSettlementsInTx(clientSession, tx, owner, nil, due)
				QueueLegacyCloseSettlementsInTx(clientSession, tx, owner)
			}
			var count int
			server.Raise(tx.QueryRow(ctx, `SELECT count(*) FROM pending_task WHERE run_once_key=ANY($1)`,
				[]string{owners[0].runOnce().String(), owners[1].runOnce().String()}).Scan(&count))
			if count != 2 {
				t.Fatal("equal-id owner namespaces did not retain independent pending rows", count)
			}
			for _, owner := range owners {
				var runAt time.Time
				var raw []byte
				var functionName string
				server.Raise(tx.QueryRow(ctx, `SELECT run_at,args_json,function_name FROM pending_task WHERE run_once_key=$1`,
					owner.runOnce().String()).Scan(&runAt, &raw, &functionName))
				var args LegacyPayerSettlementArgs
				server.Raise(json.Unmarshal(raw, &args))
				got := ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: args.PayerNetworkId}
				if args.Owner != nil {
					got = *args.Owner
				}
				if !runAt.Equal(due) || got != owner || !args.Private || args.Cursor == nil || args.Cursor.ContractId != cursor.ContractId {
					t.Fatalf("coalesced close owner lost earliest deadline or original scope: owner=%+v due=%v args=%+v", owner, runAt, args)
				}
				expectedFunctionName := NewLegacyPayerSettlementTaskTarget().TargetFunctionName()
				if owner.Kind == ContractCloseOwnerSourceClient {
					expectedFunctionName = NewLegacySourceSettlementTaskTarget().TargetFunctionName()
				}
				if functionName != expectedFunctionName || (owner.Kind == ContractCloseOwnerSourceClient && functionName == NewLegacyPayerSettlementTaskTarget().TargetFunctionName()) {
					t.Fatal("source task could be claimed by a rolling payer-only target", owner, functionName)
				}
			}
		}, server.TxReadCommitted, server.OptNoRetry())
	})
}

// A source-paid companion retains real legacy escrow after its cached payer
// disappears. Its destination network must not become the scheduling owner.
func TestContractCloseOwnerLegacyCompanionUsesEscrowPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireContractCloseOwnerTestSchema(t, ctx)
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		var anchor, companion *TransferEscrow
		var posts []func() any
		server.Tx(ctx, func(tx server.PgTx) {
			var nextPosts []func() any
			var err error
			anchor, nextPosts, err = createTransferEscrowInTx(ctx, tx,
				fixture.destinationNetworkId, fixture.destinationId, fixture.sourceNetworkId, fixture.sourceId,
				fixture.sourceNetworkId, 100, nil)
			server.Raise(err)
			posts = append(posts, nextPosts...)
			companion, nextPosts, err = createTransferEscrowInTx(ctx, tx,
				fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId,
				fixture.sourceNetworkId, 100, &anchor.ContractId)
			server.Raise(err)
			posts = append(posts, nextPosts...)
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=NULL WHERE contract_id=$1`, companion.ContractId))
		}, server.TxReadCommitted, server.OptNoRetry())
		server.RunPosts(ctx, posts...)
		server.Raise(CloseContract(ctx, companion.ContractId, fixture.sourceId, 11, false))
		server.Raise(CloseContract(ctx, companion.ContractId, fixture.destinationId, 11, false))
		server.Db(ctx, func(conn server.PgConn) {
			owner, sourceId, err := readContractCloseOwnerInConn(ctx, conn, companion.ContractId)
			if err != nil || owner != (ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: fixture.sourceNetworkId}) || sourceId != fixture.sourceId {
				t.Fatal("legacy companion was assigned to its endpoint fallback", owner, sourceId, err)
			}
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id=$2 AND source_client_id=$3
 FROM legacy_settlement_intent WHERE contract_id=$1`, companion.ContractId, fixture.sourceNetworkId, fixture.sourceId).Scan(&exact))
			if !exact {
				t.Fatal("foreground legacy queue did not retain the actual payer and source client")
			}
		})
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		wrong, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: fixture.destinationNetworkId}, clientSession)
		if err != nil || wrong.Completed != 0 {
			t.Fatal("destination fallback executed a source-funded legacy close", wrong, err)
		}
		requireLegacySettlementTestState(t, ctx, fixture, companion.ContractId, true, false, 1000, 200)
		right, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: fixture.sourceNetworkId}, clientSession)
		if err != nil || right.Completed != 1 || right.Failed != 0 {
			t.Fatal("actual escrow payer did not settle its retained legacy close", right, err)
		}
		requireLegacySettlementTestState(t, ctx, fixture, companion.ContractId, false, true, 989, 100)
		requireLegacyProviderDurability(t, ctx, fixture, companion.ContractId, 11)
		requireLegacySettlementTestState(t, ctx, fixture, anchor.ContractId, false, false, 989, 100)
	})
}

// An already-populated legacy endpoint hint must be repaired before its old
// queue owner can debit the actual payer's grant.
func TestContractCloseOwnerWorkerRepairsStalePayerBeforeFinancialWork(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireContractCloseOwnerTestSchema(t, ctx)
		fixture, contractId := legacySettlementTestIntent(t, ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET payer_network_id=NULL WHERE contract_id=$1`, contractId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
 SET payer_network_id=$2,source_client_id=NULL WHERE contract_id=$1`, contractId, fixture.destinationNetworkId))
		}, server.TxReadCommitted, server.OptNoRetry())
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		wrong, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: fixture.destinationNetworkId}, clientSession)
		if err != nil || wrong.Completed != 0 || wrong.Failed != 0 {
			t.Fatal("stale endpoint owner executed the actual payer's financial work", wrong, err)
		}
		requireLegacySettlementTestState(t, ctx, fixture, contractId, true, false, 1000, 100)
		server.Db(ctx, func(conn server.PgConn) {
			var repaired bool
			server.Raise(conn.QueryRow(ctx, `SELECT payer_network_id=$2 AND source_client_id=$3
 FROM legacy_settlement_intent WHERE contract_id=$1`, contractId, fixture.sourceNetworkId, fixture.sourceId).Scan(&repaired))
			if !repaired {
				t.Fatal("stale worker did not leave discoverable metadata for the actual payer")
			}
		})
		right, err := ApplyLegacyPayerSettlements(&LegacyPayerSettlementArgs{Private: true, PayerNetworkId: fixture.sourceNetworkId}, clientSession)
		if err != nil || right.Completed != 1 || right.Failed != 0 {
			t.Fatal("repaired actual payer did not retain the close", right, err)
		}
		requireLegacySettlementTestState(t, ctx, fixture, contractId, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, fixture, contractId, 11)
	})
}

// Retained endpoints suffice after client deletion. A rolling old writer's
// explicit network fallback must also be replaced by the source-client owner.
func TestContractCloseOwnerFreeSourceSurvivesMissingClientRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireContractCloseOwnerTestSchema(t, ctx)
		contractId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		sourceNetworkId, destinationNetworkId := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count)
 VALUES($1,$2,$3,$4,$5,0)`, contractId, sourceNetworkId, sourceId, destinationNetworkId, destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent
 (contract_id,shard,outcome,payer_network_id) VALUES($1,$2,'settled',$3)`, contractId, int(contractId[15])%LegacySettlementShardCount, sourceNetworkId))
			var exact bool
			server.Raise(tx.QueryRow(ctx, `SELECT payer_network_id IS NULL AND source_client_id=$2
 AND NOT EXISTS(SELECT 1 FROM network_client WHERE client_id=ANY($3))
 FROM legacy_settlement_intent WHERE contract_id=$1`, contractId, sourceId, []server.Id{sourceId, destinationId}).Scan(&exact))
			if !exact {
				t.Fatal("rolling free intent retained a network owner or required live clients")
			}
			owner, retainedSourceId, err := readContractCloseOwnerInConn(ctx, tx, contractId)
			if err != nil || owner != (ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: sourceId}) || retainedSourceId != sourceId {
				t.Fatal("free close did not resolve its retained source client", owner, retainedSourceId, err)
			}
		}, server.TxReadCommitted, server.OptNoRetry())
	})
}

// Ordinary free closes keep their report and usage lifecycle after endpoint
// deletion. A normalized companion changes usage direction, never close owner.
func TestContractCloseOwnerOrdinaryFreeReportsRetainSource(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireContractCloseOwnerTestSchema(t, ctx)
		for _, test := range []struct {
			name                string
			byteCount           ByteCount
			usageOriginIsSource bool
			clientsAbsent       bool
		}{
			{name: "zero byte", byteCount: 0, usageOriginIsSource: true},
			{name: "public positive", byteCount: 11, usageOriginIsSource: true},
			{name: "normalized companion positive", byteCount: 11, usageOriginIsSource: false},
			{name: "retained absent endpoints", byteCount: 11, usageOriginIsSource: true, clientsAbsent: true},
		} {
			sourceId, destinationId := server.NewId(), server.NewId()
			sourceNetworkId, destinationNetworkId := server.NewId(), server.NewId()
			if !test.usageOriginIsSource {
				destinationNetworkId = sourceNetworkId
			}
			var contractId server.Id
			if test.clientsAbsent {
				contractId = server.NewId()
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,usage_origin_is_source)
 VALUES($1,$2,$3,$4,$5,100,$6)`, contractId, sourceNetworkId, sourceId, destinationNetworkId, destinationId, test.usageOriginIsSource))
				}, server.TxReadCommitted, server.OptNoRetry())
			} else {
				addContractPayoutTestClients(ctx, map[server.Id]server.Id{sourceId: sourceNetworkId, destinationId: destinationNetworkId})
				var err error
				contractId, err = CreateContractNoEscrowWithUsageOrigin(ctx, sourceNetworkId, sourceId,
					destinationNetworkId, destinationId, 100, test.usageOriginIsSource)
				server.Raise(err)
			}
			server.Raise(CloseContract(ctx, contractId, sourceId, test.byteCount, true))
			if _, closed := GetContractClose(ctx, contractId); closed {
				t.Fatal(test.name, "source checkpoint completed a free contract")
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client WHERE client_id=ANY($1::uuid[])`, []server.Id{sourceId, destinationId}))
			}, server.TxReadCommitted, server.OptNoRetry())
			server.Raise(CloseContract(ctx, contractId, sourceId, 0, false))
			if _, closed := GetContractClose(ctx, contractId); closed {
				t.Fatal(test.name, "one final report completed a free contract")
			}
			server.Raise(CloseContract(ctx, contractId, destinationId, test.byteCount, false))
			close, closed := GetContractClose(ctx, contractId)
			if !closed || close.Outcome != string(ContractOutcomeSettled) {
				t.Fatal(test.name, "ordinary free close failed after client deletion", close)
			}
			data, usage := readContractExpiryTestSnapshot(t, ctx, contractId)
			providerId, providerNetworkId := destinationId, destinationNetworkId
			if !test.usageOriginIsSource {
				providerId, providerNetworkId = sourceId, sourceNetworkId
			}
			if usage.ByteCount != test.byteCount || usage.Expiry != nil ||
				(test.byteCount > 0 && (len(usage.Providers) != 1 || usage.Providers[0].ClientId != providerId || usage.Providers[0].NetworkId != providerNetworkId)) {
				t.Fatalf("%s: ordinary free usage lost retained orientation: %s", test.name, data)
			}
			server.Db(ctx, func(conn server.PgConn) {
				owner, retainedSourceId, err := readContractCloseOwnerInConn(ctx, conn, contractId)
				if err != nil || owner != (ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: sourceId}) || retainedSourceId != sourceId {
					t.Fatal(test.name, "ordinary free close selected an endpoint network or usage provider", owner, retainedSourceId, err)
				}
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=$1)
 AND NOT EXISTS(SELECT 1 FROM network_client WHERE client_id=ANY($2::uuid[]))
 AND (SELECT count(*) FROM contract_close WHERE contract_id=$1 AND NOT checkpoint AND used_transfer_byte_count=$3)=2`,
					contractId, []server.Id{sourceId, destinationId}, test.byteCount).Scan(&exact))
				if !exact {
					t.Fatal(test.name, "ordinary free close changed reports or manufactured financial work")
				}
			})
		}
	})
}

// Real expiry retains the original partial proof for free public and companion
// contracts, including an absent peer, without consulting deleted clients.
func TestContractCloseOwnerFreeExpiryRetainsOriginalPartialReports(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		requireContractCloseOwnerTestSchema(t, ctx)
		type expiryCase struct {
			name                string
			reports             map[ContractParty]contractUsageClose
			usageOriginIsSource bool
			wantBytes           ByteCount
			contractId          server.Id
			sourceId            server.Id
			providerId          server.Id
		}
		cases := []expiryCase{
			{name: "zero reports", reports: map[ContractParty]contractUsageClose{}, usageOriginIsSource: true},
			{name: "source checkpoint only", reports: map[ContractParty]contractUsageClose{
				ContractPartySource: {ByteCount: 17, Checkpoint: true},
			}, usageOriginIsSource: true},
			{name: "destination final only", reports: map[ContractParty]contractUsageClose{
				ContractPartyDestination: {ByteCount: 11, Checkpoint: false},
			}, usageOriginIsSource: true},
			{name: "companion bilateral checkpoints", reports: map[ContractParty]contractUsageClose{
				ContractPartySource:      {ByteCount: 17, Checkpoint: true},
				ContractPartyDestination: {ByteCount: 11, Checkpoint: true},
			}, usageOriginIsSource: false, wantBytes: 11},
		}
		ids := make([]server.Id, len(cases))
		for index := range cases {
			test := &cases[index]
			sourceId, destinationId := server.NewId(), server.NewId()
			sourceNetworkId, destinationNetworkId := server.NewId(), server.NewId()
			if !test.usageOriginIsSource {
				destinationNetworkId = sourceNetworkId
			}
			addContractPayoutTestClients(ctx, map[server.Id]server.Id{sourceId: sourceNetworkId, destinationId: destinationNetworkId})
			contractId, err := CreateContractNoEscrowWithUsageOrigin(ctx, sourceNetworkId, sourceId,
				destinationNetworkId, destinationId, 100, test.usageOriginIsSource)
			server.Raise(err)
			test.contractId, test.sourceId, test.providerId = contractId, sourceId, destinationId
			if !test.usageOriginIsSource {
				test.providerId = sourceId
			}
			ids[index] = contractId
			for _, party := range []ContractParty{ContractPartySource, ContractPartyDestination} {
				if report, ok := test.reports[party]; ok {
					clientId := sourceId
					if party == ContractPartyDestination {
						clientId = destinationId
					}
					server.Raise(CloseContract(ctx, contractId, clientId, report.ByteCount, report.Checkpoint))
				}
			}
			if _, closed := GetContractClose(ctx, contractId); closed {
				t.Fatal(test.name, "partial reports completed before expiry")
			}
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client WHERE client_id=ANY($1::uuid[])`, []server.Id{sourceId, destinationId}))
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		cutoff := server.NowUtc().Add(time.Hour)
		count, err := ForceCloseOpenContractIds(ctx, cutoff, len(cases), 2, 0, 0)
		if err != nil || count != int64(len(cases)) {
			t.Fatal("free expiry lost eligible contracts", count, err)
		}
		retained := make([][]byte, len(cases))
		for index, test := range cases {
			data, usage := readContractExpiryTestSnapshot(t, ctx, test.contractId)
			retained[index] = data
			if usage.ByteCount != test.wantBytes || usage.Expiry == nil || len(usage.Expiry.Reports) != len(test.reports) ||
				(test.wantBytes == 0 && usage.ExcludedReason != "expired_unconfirmed") ||
				(test.wantBytes > 0 && (len(usage.Providers) != 1 || usage.Providers[0].ClientId != test.providerId)) {
				t.Fatalf("%s: expiry changed original free proof: %s", test.name, data)
			}
			for party, report := range test.reports {
				if retainedReport, ok := usage.Expiry.Reports[party]; !ok || retainedReport != report {
					t.Fatal(test.name, "expiry reconstructed a report", party, usage.Expiry.Reports)
				}
			}
			server.Db(ctx, func(conn server.PgConn) {
				owner, _, err := readContractCloseOwnerInConn(ctx, conn, test.contractId)
				if err != nil || owner != (ContractCloseOwner{Kind: ContractCloseOwnerSourceClient, Id: test.sourceId}) {
					t.Fatal(test.name, "expiry lost retained source owner", owner, err)
				}
			})
		}
		count, err = ForceCloseOpenContractIds(ctx, cutoff, len(cases), 2, 0, 0)
		if err != nil || count != 0 {
			t.Fatal("completed free expiry repeated work", count, err)
		}
		for index, test := range cases {
			data, _ := readContractExpiryTestSnapshot(t, ctx, test.contractId)
			if !bytes.Equal(retained[index], data) {
				t.Fatal(test.name, "expiry replay rewrote retained usage")
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[]) AND outcome='settled')=$2
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=ANY($1::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=ANY($1::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=ANY($1::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1::uuid[]))`, ids, len(cases)).Scan(&exact))
			if !exact {
				t.Fatal("free expiry manufactured a payer or left an unresolved close")
			}
		})
	})
}

// All 2,048 contracts are pending together under 32 typed owners. Equal UUIDs
// span both namespaces and every free source shares one endpoint network. The
// explicitly held paid balance is a contention control, not a throughput claim.
func TestContractCloseOwnerMixed2048DispatchAndWorkerPreserveTypedOwners(t *testing.T) {
	runContractCloseOwnerMixed2048(t, false)
}

// Both profiles share the original raw reports, dispatcher, registered worker
// targets and accounting oracles. Only the explicit contention control differs.
func runContractCloseOwnerMixed2048(t *testing.T, healthyJitter bool) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		if healthyJitter {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, 5*time.Minute)
			defer cancel()
		}
		requireContractCloseOwnerTestSchema(t, ctx)
		const ownerPairCount = 16
		const contractsPerOwner = 64
		const totalContracts = ownerPairCount * 2 * contractsPerOwner
		due := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
		freeNetworkId := server.NewId()
		fixtures := make([]netEscrowOrderingTestFixture, ownerPairCount)
		paidIds := make([][]server.Id, ownerPairCount)
		freeIds := make([][]server.Id, ownerPairCount)
		freeSourceIds := make([]server.Id, ownerPairCount)
		allIds := make([]server.Id, 0, totalContracts)
		allFreeIds := make([]server.Id, 0, totalContracts/2)
		owners := make([]ContractCloseOwner, 0, ownerPairCount*2)
		expectedOwnerKVs := map[ContractCloseOwner]bool{}
		for index := range fixtures {
			fixture, ids := seedLegacyPayerTurnIntents(t, ctx, contractsPerOwner)
			fixtures[index], paidIds[index] = fixture, ids
			freeSourceIds[index] = fixture.sourceNetworkId
			allIds = append(allIds, ids...)
			prefix := server.NewId()
			freeIds[index] = make([]server.Id, contractsPerOwner)
			for contractIndex := range freeIds[index] {
				freeIds[index][contractIndex] = legacyPayerTestContractId(prefix, uint32(contractIndex+1), contractIndex%LegacySettlementShardCount)
			}
			allIds = append(allIds, freeIds[index]...)
			allFreeIds = append(allFreeIds, freeIds[index]...)
			for _, kind := range []ContractCloseOwnerKind{ContractCloseOwnerPayerNetwork, ContractCloseOwnerSourceClient} {
				owner := ContractCloseOwner{Kind: kind, Id: fixture.sourceNetworkId}
				owners = append(owners, owner)
				expectedOwnerKVs[owner] = true
			}
			server.Tx(ctx, func(tx server.PgTx) {
				// Retained source clients intentionally have no live membership.
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,usage_origin_is_source)
 SELECT id,$2,$3,$4,$5,2,$6 FROM unnest($1::uuid[]) AS requested(id)`,
					freeIds[index], freeNetworkId, fixture.sourceNetworkId, fixture.destinationNetworkId, fixture.destinationId, index%2 == 0))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
 SELECT id,party,1,timestamp '2010-01-01',false FROM unnest($1::uuid[]) AS requested(id)
 CROSS JOIN (VALUES ('source'),('destination')) AS report(party)`, freeIds[index]))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,next_attempt_time)
 SELECT id,get_byte(uuid_send(id),15)%16,'settled',$2 FROM unnest($1::uuid[]) AS requested(id)`, freeIds[index], due))
				if index == 0 {
					// A populated historical fallback also needs classification.
					server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent
 SET payer_network_id=$2,source_client_id=NULL WHERE contract_id=ANY($1::uuid[])`, freeIds[index], freeNetworkId))
				}
			}, server.TxReadCommitted, server.OptNoRetry())
		}
		originalReports := legacyFinancialCohortReports(ctx, allIds)
		var jitter *contractCloseOwnerJitterObservation
		if healthyJitter {
			var cleanup func()
			ctx, jitter, cleanup = newContractCloseOwnerJitterObservation(t, ctx)
			defer cleanup()
			defer jitter.report(t)
		}
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		seenOwnerKVs := map[ContractCloseOwner]bool{}
		for shard := range LegacySettlementShardCount {
			var cursor *LegacySettlementCursor
			var payerCursor, sourceCursor *LegacySettlementPayerCursor
			roundOwnerKVs := map[ContractCloseOwner]bool{}
			completedRounds := 0
			complete := false
			for page := 0; page < 2*(ownerPairCount+1); page++ {
				result, err := DispatchLegacySettlementCloseOwners(ctx, shard, cursor, payerCursor, sourceCursor)
				if err != nil || result.RegistrationFailed || result.Probes > legacySettlementPayerProbeLimit || result.SourceProbes > legacySettlementPayerProbeLimit {
					t.Fatal("mixed owner discovery lost bounded custody", shard, page, result, err)
				}
				keys, err := LegacyCloseSettlementQueueOwnershipKeys(result.PayerNetworkIds, result.SourceClientIds)
				server.Raise(err)
				if len(keys) > 0 {
					server.OwnedTx(ctx, keys, func(tx server.PgTx) {
						for _, family := range []struct {
							kind ContractCloseOwnerKind
							ids  []server.Id
						}{
							{kind: ContractCloseOwnerPayerNetwork, ids: result.PayerNetworkIds},
							{kind: ContractCloseOwnerSourceClient, ids: result.SourceClientIds},
						} {
							for _, id := range family.ids {
								owner := ContractCloseOwner{Kind: family.kind, Id: id}
								if !expectedOwnerKVs[owner] {
									t.Fatal("dispatcher published an endpoint fallback or foreign owner", owner)
								}
								if roundOwnerKVs[owner] {
									t.Fatal("dispatcher restarted an exhausted family inside a shared round", owner)
								}
								roundOwnerKVs[owner] = true
								seenOwnerKVs[owner] = true
								ScheduleLegacyCloseSettlementsInTx(clientSession, tx, owner, nil, due)
							}
						}
					}, server.TxReadCommitted, server.OptNoRetry())
				}
				// Persisted cursor encoding is part of the recurring task seam.
				encoded, err := json.Marshal(result)
				server.Raise(err)
				var continued LegacySettlementDispatchResult
				server.Raise(json.Unmarshal(encoded, &continued))
				cursor, payerCursor, sourceCursor = continued.Cursor, continued.PayerCursor, continued.SourceCursor
				if !continued.More {
					completedRounds++
					// Registration after a family's end belongs to the next
					// recurring round, even if the other family is unfinished.
					if completedRounds == 2 {
						complete = true
						break
					}
					roundOwnerKVs = map[ContractCloseOwner]bool{}
				}
			}
			if !complete {
				t.Fatal("mixed owner cursors did not finish their finite round", shard)
			}
		}
		if len(seenOwnerKVs) != len(expectedOwnerKVs) {
			t.Fatal("shared endpoint network or equal UUIDs hid an owner", len(seenOwnerKVs), len(expectedOwnerKVs))
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE legacy_settlement_intent`))
		}, server.OptNoRetry())
		const sourceIndex = "legacy_settlement_intent_source_due"
		checkLegacySettlementPayerPlan(t, ctx, legacySourceOwnerSql(legacySettlementPayerBeginSql), []any{0}, 1, sourceIndex)
		checkLegacySettlementPayerPlan(t, ctx, legacySourceOwnerIntentSql, []any{0, freeSourceIds[0], server.NowUtc()}, 1, sourceIndex)
		maximumSource := freeSourceIds[0]
		for _, id := range freeSourceIds[1:] {
			if maximumSource.Cmp(id) < 0 {
				maximumSource = id
			}
		}
		checkLegacySettlementPayerPlan(t, ctx, legacySourceOwnerSql(legacySettlementPayerNextSql)+` AND source_client_id>$3 ORDER BY source_client_id LIMIT 1`,
			[]any{0, maximumSource, maximumSource}, 0, sourceIndex)
		runOnceKeys := make([]string, len(owners))
		for index, owner := range owners {
			runOnceKeys[index] = owner.runOnce().String()
		}
		server.Db(ctx, func(conn server.PgConn) {
			var exact bool
			var pendingContracts int
			server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM pending_task WHERE run_once_key=ANY($1::text[]))=$2
 AND (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($3::uuid[]) AND payer_network_id IS NULL AND source_client_id IS NOT NULL)=$4
 AND NOT EXISTS(SELECT 1 FROM network_client WHERE client_id=ANY($5::uuid[]))
 AND (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($6::uuid[]) AND outcome IS NULL)=$7,
 (SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($6::uuid[]))`,
				runOnceKeys, len(owners), allFreeIds, len(allFreeIds), freeSourceIds, allIds, totalContracts).Scan(&exact, &pendingContracts))
			if jitter != nil {
				jitter.pendingContracts = pendingContracts
			}
			if !exact || pendingContracts != totalContracts {
				t.Fatal("dispatch coalescing merged typed owners or failed historical free registration")
			}
		})

		var releaseHeld chan struct{}
		var heldDone <-chan error
		heldJoined := false
		if !healthyJitter {
			// The held-grant control alone starts this separate actor. The
			// healthy jitter profile holds no database connection or grant.
			heldReady := make(chan error, 1)
			releaseHeld = make(chan struct{})
			heldDone = runPaymentModelTest(func() error {
				conn, err := server.AcquireMaintenanceDbConn(ctx)
				if err != nil {
					heldReady <- err
					return err
				}
				defer conn.Release()
				if _, err := conn.Exec(ctx, `SET default_transaction_read_only=off`); err != nil {
					heldReady <- err
					return err
				}
				held, err := conn.Begin(ctx)
				if err != nil {
					heldReady <- err
					return err
				}
				defer held.Rollback(context.Background())
				if _, err := held.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, fixtures[0].balanceId); err != nil {
					heldReady <- err
					return err
				}
				heldReady <- nil
				select {
				case <-releaseHeld:
					return held.Rollback(ctx)
				case <-ctx.Done():
					return ctx.Err()
				}
			})
			defer func() {
				if !heldJoined {
					close(releaseHeld)
					if err := <-heldDone; err != nil {
						t.Error("held grant actor cleanup", err)
					}
				}
			}()
			select {
			case err := <-heldReady:
				server.Raise(err)
			case err := <-heldDone:
				heldJoined = true
				close(releaseHeld)
				if err == nil {
					t.Fatal("held grant actor ended before its ready barrier")
				}
				server.Raise(err)
			}
		}
		settings := task.DefaultTaskWorkerSettings()
		settings.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, settings)
		defer worker.Close()
		worker.AddTargets(NewLegacyPayerSettlementTaskTarget(), NewLegacySourceSettlementTaskTarget())
		if jitter != nil {
			jitter.workerCalls++
		}
		finished, retried, postRetried, err := worker.EvalTasks(len(owners))
		if jitter != nil {
			jitter.finished = len(finished)
			jitter.taskRetries = len(retried)
			jitter.postRetries = len(postRetried)
			jitter.taskCallFailed = err != nil
		}
		if err != nil || len(finished) != len(owners) || len(retried)+len(postRetried) != 0 {
			t.Fatal("actual mixed owner task batch did not finish", finished, retried, postRetried, err)
		}
		heldOwner := ContractCloseOwner{Kind: ContractCloseOwnerPayerNetwork, Id: fixtures[0].sourceNetworkId}
		finishedOwnerKVs := map[ContractCloseOwner]bool{}
		finishedTasks := task.GetFinishedTasks(ctx, finished...)
		if jitter != nil {
			// Retain every joined result before a failed per-owner assertion
			// can stop validation. Failure evidence must not report a prefix.
			for _, done := range finishedTasks {
				var result LegacyPayerSettlementResult
				server.Raise(json.Unmarshal([]byte(done.ResultJson), &result))
				jitter.results = append(jitter.results, result)
			}
		}
		for _, done := range finishedTasks {
			if healthyJitter && (done.RescheduleError != "" || done.PostError != "" || !done.PostCompleted) {
				t.Fatal("healthy close task retained a task or post error", done.RescheduleError, done.PostError, done.PostCompleted)
			}
			var args LegacyPayerSettlementArgs
			var result LegacyPayerSettlementResult
			server.Raise(json.Unmarshal([]byte(done.ArgsJson), &args))
			server.Raise(json.Unmarshal([]byte(done.ResultJson), &result))
			owner, err := legacyCloseTaskOwner(&args)
			if err != nil || !expectedOwnerKVs[owner] || finishedOwnerKVs[owner] {
				t.Fatal("actual worker changed or duplicated close owner", owner, err)
			}
			finishedOwnerKVs[owner] = true
			if owner.Kind == ContractCloseOwnerSourceClient &&
				(result.FinancialCohortAttempts != 0 || result.FinancialCohortSelected != 0 ||
					result.FinancialCohortCompleted != 0 || result.FinancialCohortFallbacks != 0 || result.FinancialCohortWriteRollbacks != 0) {
				t.Fatal("free source entered an unsupported financial cohort", result)
			}
			if healthyJitter && owner.Kind == ContractCloseOwnerPayerNetwork &&
				(result.FinancialCohortCompleted != contractsPerOwner ||
					result.FinancialCohortAttempts != contractsPerOwner/legacyFinancialCohortLimit ||
					result.FinancialCohortSelected != contractsPerOwner || result.FinancialCohortFallbacks != 0 ||
					result.FinancialCohortWriteRollbacks != 0) {
				t.Fatal("healthy payer work repeated or bypassed packed financial cohorts", result)
			}
			if !healthyJitter && owner == heldOwner {
				if result.Completed != 0 || result.BusyOrGone == 0 || result.Failed != 0 {
					t.Fatal("held paid balance did not produce the explicit busy control", result)
				}
			} else if result.Completed != contractsPerOwner || result.BusyOrGone != 0 || result.Failed != 0 {
				t.Fatal("independent healthy owner did not complete exact retained work", owner, result)
			}
		}
		if len(finishedOwnerKVs) != len(owners) {
			t.Fatal("finished task rows lost a typed close owner", len(finishedOwnerKVs), len(owners))
		}
		if !healthyJitter {
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[]) AND outcome='settled')=$2
 AND NOT EXISTS(SELECT 1 FROM transfer_contract WHERE contract_id=ANY($3::uuid[]) AND outcome IS NOT NULL)
 AND (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($4::uuid[]) AND outcome='settled')=$5`,
					allIds, totalContracts-contractsPerOwner, paidIds[0], freeIds[0], contractsPerOwner).Scan(&exact))
				if !exact {
					t.Fatal("busy financial owner suppressed its equal-id free source or another owner")
				}
			})
			close(releaseHeld)
			heldErr := <-heldDone
			heldJoined = true
			server.Raise(heldErr)
			server.OwnedTx(ctx, []server.PgOwnershipKey{task.RunOnceOwnershipKey(heldOwner.runOnce())}, func(tx server.PgTx) {
				ScheduleLegacyCloseSettlementsInTx(clientSession, tx, heldOwner, nil, due)
			}, server.TxReadCommitted, server.OptNoRetry())
			finished, retried, postRetried, err = worker.EvalTasks(1)
			if err != nil || len(finished) != 1 || len(retried)+len(postRetried) != 0 {
				t.Fatal("released paid grant did not finish its durable continuation", finished, retried, postRetried, err)
			}
			var resumed LegacyPayerSettlementResult
			server.Raise(json.Unmarshal([]byte(task.GetFinishedTasks(ctx, finished...)[finished[0]].ResultJson), &resumed))
			if resumed.Completed != contractsPerOwner || resumed.BusyOrGone != 0 || resumed.Failed != 0 {
				t.Fatal("released paid owner did not complete exact retained work", resumed)
			}
		}

		assertCompleted := func() {
			t.Helper()
			if !bytes.Equal(originalReports, legacyFinancialCohortReports(ctx, allIds)) {
				t.Fatal("mixed settlement rewrote original reports")
			}
			server.Db(ctx, func(conn server.PgConn) {
				var exact bool
				server.Raise(conn.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($1::uuid[]) AND outcome='settled' AND (provider_usage->>'byte_count')::bigint=1)=$2
 AND NOT EXISTS(SELECT 1 FROM legacy_settlement_intent WHERE contract_id=ANY($1::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=ANY($1::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow WHERE contract_id=ANY($3::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM transfer_escrow_sweep WHERE contract_id=ANY($3::uuid[]))
 AND NOT EXISTS(SELECT 1 FROM pending_task WHERE run_once_key=ANY($4::text[]))`, allIds, totalContracts, allFreeIds, runOnceKeys).Scan(&exact))
				if !exact {
					t.Fatal("mixed close lost terminal usage, retained an intent, or charged free work")
				}
				for index, fixture := range fixtures {
					server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
 SELECT allocation FROM pending_task CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
 WHERE function_name=$4 AND NOT COALESCE((args_json::jsonb->>'applied')::boolean,false)
 AND (allocation->>'network_id')::uuid=$3
 ) SELECT
 (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$2)=4096-$5
 AND (SELECT count(*) FROM transfer_escrow WHERE contract_id=ANY($1::uuid[]) AND settled AND payout_byte_count=1)=$5
 AND (SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id=ANY($1::uuid[]))=$5
 AND (SELECT sum(payout_net_revenue_nano_cents) FROM transfer_escrow_sweep WHERE contract_id=ANY($1::uuid[]))=$5
 AND COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$3),0)
     +COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0)=$5
 AND COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$3),0)
     +COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)=$5`,
						paidIds[index], fixture.balanceId, fixture.destinationNetworkId,
						task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName(), contractsPerOwner).Scan(&exact))
					if !exact {
						t.Fatal("mixed close changed exact paid debit or durable provider custody", index)
					}
				}
			})
		}
		assertCompleted()
		for _, owner := range owners {
			args := &LegacyPayerSettlementArgs{Private: true, Owner: &owner}
			apply := ApplyLegacyPayerSettlements
			if owner.Kind == ContractCloseOwnerSourceClient {
				apply = ApplyLegacySourceSettlements
			}
			result, err := apply(args, clientSession)
			if err != nil || result.Visited != 0 || result.Completed != 0 || result.BusyOrGone != 0 || result.Failed != 0 || result.More {
				t.Fatal("mixed typed owner replay repeated work", owner, result, err)
			}
		}
		assertCompleted()
		if jitter != nil {
			jitter.accountingAndReplayVerified = true
			jitter.requireHealthy(t)
		} else {
			t.Log("2,048 pending logical contracts, 32 typed owners: 1,984 completed with one paid grant held; remaining 64 completed after explicit release; no throughput or zero-contention claim")
		}
	})
}
