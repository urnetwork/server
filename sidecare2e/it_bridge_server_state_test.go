//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// Every read and write of server state the it-bridge case makes outside the hosts and api's routes, in one file of its
// own beside world_server_state_test.go (the e2e conventions; QD-1028's layout move will move sidecar's model code):
// a link's window client's out-of-band control, which reaches server's controller in process as the world's sidecars'
// clients do, whether server holds the key a window client published in band, how many sidecar clients server counts
// connected against the network's sidecar_clients, the provide modes a client provides as the controller reads them,
// §21.7's standing reads api's process installs at its start, and, for the case's exact reconciliation of its transfer
// balances, the network's relayed contracts, whose close reports §20.6's relay meter debits
// (model/relayed_contract_settle.go), and its share contracts, which §20.6's share row escrows from the dialer's
// network as Public and escrow's settle debits through transfer_debit_journal, which the case flushes as taskworker's
// FlushTransferDebits does.

package sidecare2e

import (
	"bytes"
	"context"
	"fmt"
	"slices"

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

// How many of networkId's sidecar clients server counts connected against sidecar_clients (§20.7, §20.8): the live
// entries of its connected-sidecar set, which each marked client's resident writes and refreshes.
func itBridgeConnectedSidecarClientCount(ctx context.Context, networkId server.Id) (count int64, returnErr error) {
	server.HandleError(func() {
		count = model.GetNetworkSidecarConnectedCount(ctx, networkId)
	}, func(err error) {
		returnErr = fmt.Errorf("the connected sidecar clients: %w", err)
	})
	return count, returnErr
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
	// Each party's close report, by party, and the parties whose report is a checkpoint rather than a final close.
	CloseReports      map[string]int64
	CheckpointReports map[string]bool
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
					contract := &ItBridgeRelayedContract{CloseReports: map[string]int64{}, CheckpointReports: map[string]bool{}}
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
					SELECT contract_close.contract_id, contract_close.party, contract_close.used_transfer_byte_count, contract_close.checkpoint
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
					var checkpoint bool
					server.Raise(reports.Scan(&contractId, &party, &byteCount, &checkpoint))
					if contract := byId[contractId]; contract != nil {
						contract.CloseReports[party] = int64(byteCount)
						if checkpoint {
							contract.CheckpointReports[party] = true
						}
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

// One share contract of a network as server holds it (§20.6's cross-network share row, §6.10.1 item 5, owner decision
// 11): a contract between a client of the network and a client of another network of the case, the FriendsAndFamily
// contract a share link's window client opens to its target's host or the companion the host answers on. Escrow
// reserves both directions from the dialer's network's balances, as for Public, at CreateContract, and settles each
// from both parties' close reports, its payout debiting those balances through transfer_debit_journal, which the flush
// applies before it releases the reservation. No service meter journals it, and the relay meter's settle reads only a
// contract both of whose ends are one network's (model/relayed_contract_settle.go).
type ItBridgeShareContract struct {
	ContractId           server.Id
	SourceId             server.Id
	DestinationId        server.Id
	SourceNetworkId      server.Id
	DestinationNetworkId server.Id
	// The forward contract a companion answers; nil for a forward contract.
	CompanionContractId *server.Id
	// The bytes its signed grant allows.
	TransferByteCount int64
	// Whether an outcome is claimed, and which.
	Open    bool
	Outcome string
	// Each party's close report, by party, and the parties whose report is a checkpoint rather than a final close.
	CloseReports      map[string]int64
	CheckpointReports map[string]bool
	// Its escrow rows by the network whose balance each draws: the bytes they reserve, and what the settled ones paid
	// (transfer_escrow's payout_byte_count); and how many rows are not settled yet.
	EscrowByteCounts  map[server.Id]int64
	PayoutByteCounts  map[server.Id]int64
	UnsettledRowCount int
	// transfer_debit_journal's rows under it, which the flush applies and deletes once their reservations are released.
	JournalRowCount int
	// What service_debit_journal holds under its id on any meter, summed over its balances' rows.
	ServiceDebitByteCount int64
	ServiceDebitRowCount  int
}

// The network that dialed the share, which pays both directions: a forward contract's source's, a companion's
// destination's.
func (self *ItBridgeShareContract) DialerNetworkId() server.Id {
	if self.CompanionContractId != nil {
		return self.DestinationNetworkId
	}
	return self.SourceNetworkId
}

// What escrow's settle counts for the contract (model's contractSettlementReportAmounts for an ordinary close, its
// deadline close's rule where one party reported), capped at its grant: the mean of both reports, or the one report
// there is. A dispute's outcomes are not the case's, and read as the ordinary rule.
func (self *ItBridgeShareContract) SettledByteCount() int64 {
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

// What its escrow paid, from every network's balances.
func (self *ItBridgeShareContract) PaidByteCount() int64 {
	paid := int64(0)
	for _, byteCount := range self.PayoutByteCounts {
		paid += byteCount
	}
	return paid
}

// What its escrow reserves, from every network's balances.
func (self *ItBridgeShareContract) EscrowedByteCount() int64 {
	escrowed := int64(0)
	for _, byteCount := range self.EscrowByteCounts {
		escrowed += byteCount
	}
	return escrowed
}

// Whether its settle is done and flushed: an outcome claimed, every escrow row settled, and no journal row left.
func (self *ItBridgeShareContract) Settled() bool {
	return !self.Open && self.UnsettledRowCount == 0 && self.JournalRowCount == 0
}

// The contract as one line, for a failure's message.
func (self *ItBridgeShareContract) Describe() string {
	direction := "forward"
	if self.CompanionContractId != nil {
		direction = fmt.Sprintf("companion of %s", *self.CompanionContractId)
	}
	return fmt.Sprintf("share contract %s (%s) from %s to %s, granted %d B, open %t, outcome %q, close reports %v, checkpoints %v, escrow by network %v, paid by network %v, %d escrow rows unsettled, %d journal rows",
		self.ContractId, direction, self.SourceId, self.DestinationId, self.TransferByteCount, self.Open, self.Outcome, self.CloseReports,
		self.CheckpointReports, self.EscrowByteCounts, self.PayoutByteCounts, self.UnsettledRowCount, self.JournalRowCount)
}

// networkId's share contracts: every contract one of whose ends is a client of networkId and the other a client of one
// of peerNetworkIds, the case's other networks, so no edge's Service contract is among them, with their close reports,
// their escrow rows by the network whose balance each draws, their transfer debit journal rows and any service debit
// journaled under their ids.
func itBridgeShareContracts(ctx context.Context, networkId server.Id, peerNetworkIds []server.Id) (contracts []*ItBridgeShareContract, returnErr error) {
	byId := map[server.Id]*ItBridgeShareContract{}
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT
						contract_id,
						source_id,
						destination_id,
						source_network_id,
						destination_network_id,
						companion_contract_id,
						transfer_byte_count,
						open,
						COALESCE(outcome, '')
					FROM transfer_contract
					WHERE
						(source_network_id = $1 AND destination_network_id = ANY($2)) OR
						(destination_network_id = $1 AND source_network_id = ANY($2))
					ORDER BY contract_id
				`,
				networkId,
				peerNetworkIds,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					contract := &ItBridgeShareContract{
						CloseReports:      map[string]int64{},
						CheckpointReports: map[string]bool{},
						EscrowByteCounts:  map[server.Id]int64{},
						PayoutByteCounts:  map[server.Id]int64{},
					}
					var transferByteCount model.ByteCount
					server.Raise(result.Scan(
						&contract.ContractId,
						&contract.SourceId,
						&contract.DestinationId,
						&contract.SourceNetworkId,
						&contract.DestinationNetworkId,
						&contract.CompanionContractId,
						&transferByteCount,
						&contract.Open,
						&contract.Outcome,
					))
					contract.TransferByteCount = int64(transferByteCount)
					byId[contract.ContractId] = contract
					contracts = append(contracts, contract)
				}
			})
			if len(contracts) == 0 {
				return
			}
			contractIds := []server.Id{}
			for _, contract := range contracts {
				contractIds = append(contractIds, contract.ContractId)
			}
			reports, err := conn.Query(
				ctx,
				`
					SELECT contract_id, party, used_transfer_byte_count, checkpoint
					FROM contract_close
					WHERE contract_id = ANY($1)
				`,
				contractIds,
			)
			server.WithPgResult(reports, err, func() {
				for reports.Next() {
					var contractId server.Id
					var party string
					var byteCount model.ByteCount
					var checkpoint bool
					server.Raise(reports.Scan(&contractId, &party, &byteCount, &checkpoint))
					if contract := byId[contractId]; contract != nil {
						contract.CloseReports[party] = int64(byteCount)
						if checkpoint {
							contract.CheckpointReports[party] = true
						}
					}
				}
			})
			escrows, err := conn.Query(
				ctx,
				`
					SELECT
						transfer_escrow.contract_id,
						transfer_balance.network_id,
						transfer_escrow.balance_byte_count,
						transfer_escrow.settled,
						COALESCE(transfer_escrow.payout_byte_count, 0)
					FROM transfer_escrow
					INNER JOIN transfer_balance ON transfer_balance.balance_id = transfer_escrow.balance_id
					WHERE transfer_escrow.contract_id = ANY($1)
				`,
				contractIds,
			)
			server.WithPgResult(escrows, err, func() {
				for escrows.Next() {
					var contractId server.Id
					var balanceNetworkId server.Id
					var escrowByteCount model.ByteCount
					var settled bool
					var payoutByteCount model.ByteCount
					server.Raise(escrows.Scan(&contractId, &balanceNetworkId, &escrowByteCount, &settled, &payoutByteCount))
					if contract := byId[contractId]; contract != nil {
						contract.EscrowByteCounts[balanceNetworkId] += int64(escrowByteCount)
						if settled {
							contract.PayoutByteCounts[balanceNetworkId] += int64(payoutByteCount)
						} else {
							contract.UnsettledRowCount += 1
						}
					}
				}
			})
			journal, err := conn.Query(
				ctx,
				`
					SELECT contract_id, COUNT(*)
					FROM transfer_debit_journal
					WHERE contract_id = ANY($1)
					GROUP BY contract_id
				`,
				contractIds,
			)
			server.WithPgResult(journal, err, func() {
				for journal.Next() {
					var contractId server.Id
					var rowCount int
					server.Raise(journal.Scan(&contractId, &rowCount))
					if contract := byId[contractId]; contract != nil {
						contract.JournalRowCount += rowCount
					}
				}
			})
			debits, err := conn.Query(
				ctx,
				`
					SELECT usage_id, SUM(debit_byte_count), COUNT(*)
					FROM service_debit_journal
					WHERE usage_id = ANY($1)
					GROUP BY usage_id
				`,
				contractIds,
			)
			server.WithPgResult(debits, err, func() {
				for debits.Next() {
					var usageId server.Id
					var byteCount int64
					var rowCount int
					server.Raise(debits.Scan(&usageId, &byteCount, &rowCount))
					if contract := byId[usageId]; contract != nil {
						contract.ServiceDebitByteCount += byteCount
						contract.ServiceDebitRowCount += rowCount
					}
				}
			})
		})
	}, func(err error) {
		returnErr = fmt.Errorf("the share contracts: %w", err)
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return contracts, nil
}

// Applies every transfer debit escrow's settles journaled to the payers' balances and releases each one's reservation,
// each shard's balances until none is left, pageBalanceCount a page (FlushTransferDebits' maxBalances, at most 64), as
// taskworker's FlushTransferDebits does on its schedule, which the World does not run: a share contract's settle
// debits its dialer's balances only through this journal (§20.6's share row), so the case's ledgers read after it. A
// balance another pass holds is busy and stays for the next call.
func itBridgeFlushTransferDebits(ctx context.Context, pageBalanceCount int) error {
	for shard := range model.TransferDebitShardCount {
		var after *server.Id
		for {
			result, err := model.FlushTransferDebits(ctx, shard, after, pageBalanceCount)
			if err != nil {
				return fmt.Errorf("shard %d's transfer debits: %w", shard, err)
			}
			if 0 < result.Failed {
				return fmt.Errorf("shard %d: %d balances' transfer debits failed to apply", shard, result.Failed)
			}
			if !result.More || result.LastBalanceId == nil {
				break
			}
			after = result.LastBalanceId
		}
	}
	return nil
}

// The provide modes server holds for clientId, by name, as the controller reads them when it settles a contract to it
// (controller.GetProvideModes): what a share link's FriendsAndFamily contract needs its host's client to provide.
func itBridgeProvideModes(ctx context.Context, clientId server.Id) []string {
	names := []string{}
	for mode, provided := range controller.GetProvideModes(ctx, clientId) {
		if provided {
			// model's modes align with connect's protocol.ProvideMode
			names = append(names, protocol.ProvideMode(mode).String())
		}
	}
	slices.Sort(names)
	return names
}

// §21.7's reads as api's process installs them at its start (controller.InstallAbuseStanding, which cli/api/main.go
// calls and the World's api does not): among them the standing an outbound share's filing asks
// (model.SetNetworkBridgeShareOutboundCheck), with none of which every invite is refused 403. ctx bounds the reads'
// loaders; the answer puts the previous reads back.
func itBridgeInstallAbuseStanding(ctx context.Context) func() {
	return controller.InstallAbuseStanding(ctx)
}

// What POST /acl/invite's three checks read for clientId of networkId (model.AclInvite), for an invite's refusal's
// message, since api's answer names no reason (I-13): the caller's identity, active or not; acl_network_shares in the
// network's limits, which a plan that leaves it 0 refuses where limits are enforced; and §21.7's standing as api's
// database holds it, whose limits give or keep outbound shares. The third reads Postgres, where api's own check reads
// its gate's memory, so a cold gate refuses what this reports allowed.
func itBridgeInviteFacts(ctx context.Context, networkId server.Id, clientId server.Id) string {
	identity := "none"
	if view := model.GetSidecarIdentityByClient(ctx, networkId, clientId); view != nil {
		identity = fmt.Sprintf("%v", view.Status)
	}
	shares := model.NetworkSidecarLimits(ctx, networkId).Limit("acl_network_shares")
	standing := "unknown"
	outbound := false
	if held, found := model.GetNetworkAbuseStanding(ctx, networkId, server.NowUtc()); found {
		standing = string(held.Standing)
		outbound = model.AbuseStandingLimits(held.Standing, model.DefaultAbuseSettings()).OutboundShares
	}
	return fmt.Sprintf("the caller's sidecar identity %s, acl_network_shares %d with limits enforced %t, standing %s giving outbound shares %t",
		identity, shares, model.Pro().EnforceSidecarLimits, standing, outbound)
}
