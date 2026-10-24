// Close ownership selects retained client identity only when no financial
// payer exists, and keeps that identity separate from the payer namespace.
package model

import (
	"bytes"
	"testing"
	"time"

	"github.com/urnetwork/server"
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

// A source-paid companion retains real legacy escrow after its cached payer
// disappears. Its destination network must not become the settlement owner.
func TestContractCloseOwnerLegacyCompanionUsesEscrowPayer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
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
		})
		// The second final settles the companion in its own close: the actual
		// escrow payer's grant pays, and the anchor keeps its reservation.
		requireLegacySettlementTestState(t, ctx, fixture, companion.ContractId, true, 989, 100)
		requireLegacySettlementTestState(t, ctx, fixture, anchor.ContractId, false, 989, 100)
	})
}

// Retained endpoints suffice after client deletion: a free contract resolves
// its source-client owner without any live client row.
func TestContractCloseOwnerFreeSourceSurvivesMissingClientRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		contractId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		sourceNetworkId, destinationNetworkId := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count)
 VALUES($1,$2,$3,$4,$5,0)`, contractId, sourceNetworkId, sourceId, destinationNetworkId, destinationId))
			var absent bool
			server.Raise(tx.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM network_client WHERE client_id=ANY($1))`,
				[]server.Id{sourceId, destinationId}).Scan(&absent))
			if !absent {
				t.Fatal("fixture clients unexpectedly exist")
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
 AND NOT EXISTS(SELECT 1 FROM transfer_debit_journal WHERE contract_id=ANY($1::uuid[]))`, ids, len(cases)).Scan(&exact))
			if !exact {
				t.Fatal("free expiry manufactured a payer or left an unresolved close")
			}
		})
	})
}
