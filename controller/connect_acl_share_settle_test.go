//go:build sidecar_pending

package controller

// §20.6's share row through a share contract's whole life on the private stack (owner decisions 10 and 11,
// Q-QUOTA-5): a cross-network share's FriendsAndFamily contract and its companion reserve from the dialer's network
// alone at CreateContract (connect_controller_share_test.go's TestCreateContractShareEscrowsTheDialerAlone, and
// connect_acl_share_test.go's share pair), and here each, closed by both its parties, settles the mean of their
// reports from the dialer's balances through transfer_debit_journal, which the debit flush applies before it releases
// the reservation, as Public's escrow does. No meter counts a share: nothing is journaled in service_debit_journal
// under either contract, so network_service_usage holds nothing for either network, and the relay meter's settle reads
// only a contract both of whose ends are one network's (model/relayed_contract_settle.go). The e2e it bridge share
// case reconciles the same ledgers across a share's real links (server/sidecare2e's it_bridge_test.go Reconcile). Run
// under -tags sidecar_pending through .stack/server-test.sh.

import (
	"context"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// a's share contract to b and b's companion back, each closed by both parties, debit only y's balances, by the mean of
// each contract's two reports, once the flush has applied them, and release every byte reserved past that; x's
// balances keep every byte, and neither contract leaves a row on any meter.
func TestCreateContractAclShareSettlesFromTheDialerOnNoMeter(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		states := sidecartest.NewFakeAclContractStates()
		pair := newAclShareTestPair(t, ctx, states)
		pair.x.install()
		pair.y.install()
		testingAclContractGate(t, ctx, states)

		contractId := func(result *protocol.CreateContractResult) server.Id {
			storedContract := &protocol.StoredContract{}
			if err := proto.Unmarshal(result.Contract.StoredContractBytes, storedContract); err != nil {
				t.Fatal(err)
			}
			return server.Id(storedContract.ContractId)
		}
		// what a network's balances started with and hold in Postgres, and what is available past every Redis
		// reservation on them
		type shareNetworkBalance struct {
			startByteCount     int64
			byteCount          int64
			availableByteCount int64
		}
		balance := func(networkId server.Id) shareNetworkBalance {
			var networkBalance shareNetworkBalance
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`
						SELECT COALESCE(SUM(start_balance_byte_count), 0), COALESCE(SUM(balance_byte_count), 0)
						FROM transfer_balance
						WHERE network_id = $1
					`,
					networkId,
				).Scan(&networkBalance.startByteCount, &networkBalance.byteCount))
			})
			networkBalance.availableByteCount = int64(model.GetActiveTransferBalanceByteCount(ctx, networkId))
			return networkBalance
		}
		// a contract's escrow rows by the network whose balance each draws: the bytes each reserves, what the settled
		// ones paid, and how many are not settled
		type shareContractEscrow struct {
			escrowByteCounts  map[server.Id]int64
			payoutByteCounts  map[server.Id]int64
			unsettledRowCount int
		}
		escrow := func(contractId server.Id) shareContractEscrow {
			contractEscrow := shareContractEscrow{
				escrowByteCounts: map[server.Id]int64{},
				payoutByteCounts: map[server.Id]int64{},
			}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
						SELECT
							transfer_balance.network_id,
							transfer_escrow.balance_byte_count,
							transfer_escrow.settled,
							COALESCE(transfer_escrow.payout_byte_count, 0)
						FROM transfer_escrow
						INNER JOIN transfer_balance ON transfer_balance.balance_id = transfer_escrow.balance_id
						WHERE transfer_escrow.contract_id = $1
					`,
					contractId,
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var networkId server.Id
						var escrowByteCount, payoutByteCount int64
						var settled bool
						server.Raise(result.Scan(&networkId, &escrowByteCount, &settled, &payoutByteCount))
						contractEscrow.escrowByteCounts[networkId] += escrowByteCount
						if settled {
							contractEscrow.payoutByteCounts[networkId] += payoutByteCount
						} else {
							contractEscrow.unsettledRowCount += 1
						}
					}
				})
			})
			return contractEscrow
		}
		// what transfer_debit_journal holds under a contract, by the network whose balance each row debits
		journaled := func(contractId server.Id) map[server.Id]int64 {
			networkByteCounts := map[server.Id]int64{}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
						SELECT transfer_balance.network_id, transfer_debit_journal.debit_byte_count
						FROM transfer_debit_journal
						INNER JOIN transfer_balance ON transfer_balance.balance_id = transfer_debit_journal.balance_id
						WHERE transfer_debit_journal.contract_id = $1
					`,
					contractId,
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var networkId server.Id
						var byteCount int64
						server.Raise(result.Scan(&networkId, &byteCount))
						networkByteCounts[networkId] += byteCount
					}
				})
			})
			return networkByteCounts
		}
		// the rows any meter holds for the contracts and the networks: service_debit_journal's under the contracts'
		// ids, which a relayed contract's settle keys by its id, and network_service_usage's of the networks
		meterRowCount := func(contractIds []server.Id, networkIds []server.Id) (journalRowCount int, usageRowCount int) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT COUNT(*) FROM service_debit_journal WHERE usage_id = ANY($1)`,
					contractIds,
				).Scan(&journalRowCount))
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT COUNT(*) FROM network_service_usage WHERE network_id = ANY($1)`,
					networkIds,
				).Scan(&usageRowCount))
			})
			return
		}
		networkIds := []server.Id{pair.x.networkId, pair.y.networkId}
		xStart, yStart := balance(pair.x.networkId), balance(pair.y.networkId)

		forward := testingAclCreateContract(t, ctx, pair.a, pair.b, false)
		if !testingAclContractCreated(forward) || forward.Contract.ProvideMode != protocol.ProvideMode_FriendsAndFamily {
			t.Fatalf("a→b: %+v", forward)
		}
		companion := testingAclCreateContract(t, ctx, pair.b, pair.a, true)
		if !testingAclContractCreated(companion) {
			t.Fatalf("b's companion to a: %+v", companion)
		}
		forwardId, companionId := contractId(forward), contractId(companion)
		contractIds := []server.Id{forwardId, companionId}

		// open: each reserves from y's balances alone, which Postgres has not debited yet
		reservedByteCount := int64(0)
		for _, id := range contractIds {
			contractEscrow := escrow(id)
			if len(contractEscrow.escrowByteCounts) != 1 || contractEscrow.escrowByteCounts[pair.y.networkId] <= 0 {
				t.Fatalf("contract %s reserves %v, where only y, the dialer, pays (owner decision 11)", id, contractEscrow.escrowByteCounts)
			}
			reservedByteCount += contractEscrow.escrowByteCounts[pair.y.networkId]
		}
		if x := balance(pair.x.networkId); x != xStart {
			t.Errorf("x's balances moved while the share's contracts are open: %+v from %+v", x, xStart)
		}
		if y := balance(pair.y.networkId); y.byteCount != yStart.byteCount || yStart.availableByteCount-reservedByteCount < y.availableByteCount {
			t.Errorf("y's balances while the share's contracts reserve %d B: %+v from %+v", reservedByteCount, y, yStart)
		}

		// each party's close report: the forward's source a and destination b, then the companion's source b and
		// destination a; the forward settles the mean, 3200 B, and the companion 1100 B
		closes := []struct {
			contractId server.Id
			clientId   server.Id
			byteCount  model.ByteCount
		}{
			{contractId: forwardId, clientId: pair.a, byteCount: 3000},
			{contractId: forwardId, clientId: pair.b, byteCount: 3400},
			{contractId: companionId, clientId: pair.b, byteCount: 1200},
			{contractId: companionId, clientId: pair.a, byteCount: 1000},
		}
		for _, report := range closes {
			if err := model.CloseContract(ctx, report.contractId, report.clientId, report.byteCount, false); err != nil {
				t.Fatalf("contract %s's close by %s: %v", report.contractId, report.clientId, err)
			}
		}
		settledByteCounts := map[server.Id]int64{forwardId: 3200, companionId: 1100}

		// settled: each debit journaled on y's balances alone, at the contract's settled count, and on no meter
		for _, id := range contractIds {
			if debits := journaled(id); len(debits) != 1 || debits[pair.y.networkId] != settledByteCounts[id] {
				t.Errorf("contract %s journals debits %v, want %d B on y's balances alone", id, debits, settledByteCounts[id])
			}
		}
		if journalRowCount, _ := meterRowCount(contractIds, networkIds); journalRowCount != 0 {
			t.Errorf("service_debit_journal holds %d rows under the share's contracts, which no meter counts (§20.6)", journalRowCount)
		}

		// the flush taskworker runs applies each debit to y's balances and releases what each contract reserved
		for shard := range model.TransferDebitShardCount {
			result, err := model.FlushTransferDebits(ctx, shard, nil, 64)
			if err != nil || 0 < result.Failed || 0 < result.Busy {
				t.Fatalf("shard %d's transfer debit flush: %+v, %v", shard, result, err)
			}
		}

		for _, id := range contractIds {
			contractEscrow := escrow(id)
			if contractEscrow.unsettledRowCount != 0 || len(contractEscrow.payoutByteCounts) != 1 ||
				contractEscrow.payoutByteCounts[pair.y.networkId] != settledByteCounts[id] {
				t.Errorf("contract %s's escrow paid %v with %d rows unsettled, want %d B from y's balances alone", id, contractEscrow.payoutByteCounts,
					contractEscrow.unsettledRowCount, settledByteCounts[id])
			}
			if debits := journaled(id); len(debits) != 0 {
				t.Errorf("contract %s keeps journal rows %v past the flush", id, debits)
			}
		}
		paidByteCount := settledByteCounts[forwardId] + settledByteCounts[companionId]
		if y := balance(pair.y.networkId); y.byteCount != yStart.byteCount-paidByteCount || y.availableByteCount != yStart.availableByteCount-paidByteCount {
			t.Errorf("y's balances: %+v from %+v, want %d B less in Postgres and available, nothing left reserved", y, yStart, paidByteCount)
		}
		if x := balance(pair.x.networkId); x != xStart {
			t.Errorf("x's balances: %+v from %+v, where the share's destination pays nothing", x, xStart)
		}
		if journalRowCount, usageRowCount := meterRowCount(contractIds, networkIds); journalRowCount != 0 || usageRowCount != 0 {
			t.Errorf("the meters hold %d service_debit_journal rows under the share's contracts and %d network_service_usage rows of its networks, "+
				"where no meter counts a share (§20.6)", journalRowCount, usageRowCount)
		}
	})
}
