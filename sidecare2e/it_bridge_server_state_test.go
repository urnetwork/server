//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// Every read and write of server state the it-bridge case makes outside the hosts and api's routes, in one file of its
// own beside world_server_state_test.go (the e2e conventions; QD-1028's layout move will move sidecar's model code):
// a link's window client's out-of-band control, which reaches server's controller in process as the world's sidecars'
// clients do, whether server holds the key a window client published in band, and the network's relayed contracts,
// whose close reports §20.6's relay meter debits (model/relayed_contract_settle.go), for the case's exact
// reconciliation of its transfer balances.

package sidecare2e

import (
	"bytes"
	"context"
	"fmt"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Out-of-band control as api's control route answers it, in process: each pack goes to server's controller for the
// client, on the case's context and off the caller's goroutine, as sidecart3's controllerOutOfBandControl carries a
// world sidecar's own client's. Once SendControl returns, the frames and the callback are this control's
// (connect/CODESTYLE.md's out-of-band boundary).
type itBridgeControl struct {
	ctx                     context.Context
	clientId                server.Id
	contractManagerSettings *connect.ContractManagerSettings
}

func (self *itBridgeControl) SendControl(frames []*protocol.Frame, callback connect.OobResultFunction) {
	go func() {
		defer func() {
			for _, frame := range frames {
				connect.MessagePoolReturn(frame.MessageBytes)
			}
		}()
		var resultFrames []*protocol.Frame
		var err error
		// a canceled case raises out of the db layer at teardown, which HandleError takes: the callback hears the raise
		// as its error, since a client counts a control in flight until its callback runs
		server.HandleError(func() {
			resultFrames, err = controller.ConnectControlFrames(self.ctx, self.clientId, frames, self.contractManagerSettings)
		}, func(raised error) {
			resultFrames = nil
			err = raised
		})
		defer func() {
			for _, frame := range resultFrames {
				connect.MessagePoolReturn(frame.MessageBytes)
			}
		}()
		if callback != nil {
			server.HandleError(func() {
				callback(resultFrames, err)
			})
		}
	}()
}

// Whether server holds publicKey as clientId's key, the one a contract to the client carries: a window client publishes
// it in band and nothing acks it.
func itBridgeClientKeyHeld(ctx context.Context, clientId server.Id, publicKey []byte) bool {
	held := false
	server.HandleError(func() {
		heldKey, err := model.GetClientPublicKey(ctx, clientId)
		held = err == nil && bytes.Equal(heldKey, publicKey)
	})
	return held
}

// One relayed contract of a network as server holds it (§20.6): two marked clients of one network on a Network
// contract, its grant reserved from the network's balances at CreateContract and debited from its close reports.
type ItBridgeRelayedContract struct {
	ContractId server.Id
	SourceId   server.Id
	// The bytes its signed grant allows.
	TransferByteCount int64
	// Whether an outcome is claimed, and which.
	Open    bool
	Outcome string
	// Each party's close report, by party.
	CloseReports map[string]int64
	// Whether it rode a P2P stream, which counts nothing.
	StreamBound bool
	// What service_debit_journal holds under the contract's id on meter relay, summed over its balances' rows.
	RelayDebitByteCount int64
	RelayDebitRowCount  int
}

// What server's settle counts for the contract (model's deadlineReportAmounts for an ordinary close, capped at its
// grant): the mean of both reports, or the one report there is, nothing for a stream-bound contract. A dispute's
// outcomes are not the case's, and read as the ordinary rule.
func (self *ItBridgeRelayedContract) SettledByteCount() int64 {
	if self.StreamBound {
		return 0
	}
	source, sourceOk := self.CloseReports[model.ContractPartySource]
	destination, destinationOk := self.CloseReports[model.ContractPartyDestination]
	used := int64(0)
	switch {
	case sourceOk && destinationOk:
		lower, upper := min(source, destination), max(source, destination)
		used = lower + (upper-lower)/2
	case sourceOk:
		used = source
	case destinationOk:
		used = destination
	}
	return min(max(0, used), self.TransferByteCount)
}

// networkId's relayed contracts: every Network contract both of whose ends are clients of the network with the relayed
// shape server's settle reads (payer_network_id the network, no escrow row, positive bytes, no companion origin), with
// their close reports and the relay meter's journal rows under each.
func itBridgeRelayedContracts(ctx context.Context, networkId server.Id) (contracts []*ItBridgeRelayedContract, returnErr error) {
	byId := map[server.Id]*ItBridgeRelayedContract{}
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT
						transfer_contract.contract_id,
						transfer_contract.source_id,
						transfer_contract.transfer_byte_count,
						transfer_contract.open,
						COALESCE(transfer_contract.outcome, ''),
						transfer_contract.stream_id IS NOT NULL
					FROM transfer_contract
					WHERE
						transfer_contract.payer_network_id = $1 AND
						transfer_contract.source_network_id = $1 AND
						transfer_contract.destination_network_id = $1 AND
						transfer_contract.companion_contract_id IS NULL AND
						0 < transfer_contract.transfer_byte_count AND
						NOT EXISTS (SELECT 1 FROM transfer_escrow WHERE transfer_escrow.contract_id = transfer_contract.contract_id)
					ORDER BY transfer_contract.contract_id
				`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					contract := &ItBridgeRelayedContract{CloseReports: map[string]int64{}}
					var transferByteCount model.ByteCount
					server.Raise(result.Scan(&contract.ContractId, &contract.SourceId, &transferByteCount, &contract.Open, &contract.Outcome, &contract.StreamBound))
					contract.TransferByteCount = int64(transferByteCount)
					byId[contract.ContractId] = contract
					contracts = append(contracts, contract)
				}
			})
			reports, err := conn.Query(
				ctx,
				`
					SELECT contract_close.contract_id, contract_close.party, contract_close.used_transfer_byte_count
					FROM contract_close
					INNER JOIN transfer_contract ON transfer_contract.contract_id = contract_close.contract_id
					WHERE transfer_contract.payer_network_id = $1
				`,
				networkId,
			)
			server.WithPgResult(reports, err, func() {
				for reports.Next() {
					var contractId server.Id
					var party string
					var byteCount model.ByteCount
					server.Raise(reports.Scan(&contractId, &party, &byteCount))
					if contract := byId[contractId]; contract != nil {
						contract.CloseReports[party] = int64(byteCount)
					}
				}
			})
			debits, err := conn.Query(
				ctx,
				`
					SELECT service_debit_journal.usage_id, service_debit_journal.debit_byte_count
					FROM service_debit_journal
					INNER JOIN transfer_balance ON transfer_balance.balance_id = service_debit_journal.balance_id
					WHERE transfer_balance.network_id = $1 AND service_debit_journal.meter = $2
				`,
				networkId,
				sidecar.QuotaMeterRelay,
			)
			server.WithPgResult(debits, err, func() {
				for debits.Next() {
					var usageId server.Id
					var byteCount int64
					server.Raise(debits.Scan(&usageId, &byteCount))
					if contract := byId[usageId]; contract != nil {
						contract.RelayDebitByteCount += byteCount
						contract.RelayDebitRowCount += 1
					}
				}
			})
		})
	}, func(err error) {
		returnErr = fmt.Errorf("the relayed contracts: %w", err)
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return contracts, nil
}
