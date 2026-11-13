// Network contracts, between two clients of one network, reserve escrow from
// that network's balance like any other contract (owner, 2026-11-10: "all
// intra-traffic should count against the network data balance"), and
// friends-and-family contracts from the dialing network's ("a yes this
// deducts from the sender's transfer balance"). Every kind of contract shares
// the payer's one transfer balance ("all traffic shares the same network
// transfer balance"). These go through the resident control dispatcher, as a
// client's requests do, and settle through the escrow path: the payer is
// debited for the bytes used, and a participant on the payer's network takes
// no provider earnings. SN usage is unchanged. Network contracts opened before
// the change carry no escrow and still settle free.
package controller

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A two-party fixture whose window network holds balanceByteCount of paid
// balance, or no balance at all when it is zero. With sameNetwork the provider
// is in the window's network and advertises the network mode.
func newNetworkEscrowFixture(t testing.TB, sameNetwork bool, balanceByteCount model.ByteCount) *twoPartyCloseFixture {
	t.Helper()
	ctx := t.Context()
	f := &twoPartyCloseFixture{
		ctx:               ctx,
		settings:          connect.DefaultContractManagerSettings(),
		windowNetworkId:   server.NewId(),
		windowId:          server.NewId(),
		providerNetworkId: server.NewId(),
		providerId:        server.NewId(),
	}
	model.Testing_CreateNetwork(ctx, f.windowNetworkId, "synthetic network escrow window "+f.windowNetworkId.String(), server.NewId())
	provideMode := model.ProvideModePublic
	if sameNetwork {
		f.providerNetworkId = f.windowNetworkId
		provideMode = model.ProvideModeNetwork
	} else {
		model.Testing_CreateNetwork(ctx, f.providerNetworkId, "synthetic network escrow provider "+f.providerNetworkId.String(), server.NewId())
	}
	model.Testing_CreateDevice(ctx, f.windowNetworkId, server.NewId(), f.windowId, "synthetic window", "window")
	model.Testing_CreateDevice(ctx, f.providerNetworkId, server.NewId(), f.providerId, "synthetic provider", "provider")
	model.SetProvide(ctx, f.providerId, map[model.ProvideMode][]byte{
		provideMode: []byte("synthetic-network-escrow-key0000"),
	})
	if 0 < balanceByteCount {
		now := server.NowUtc()
		model.AddTransferBalance(ctx, &model.TransferBalance{
			NetworkId:             f.windowNetworkId,
			StartTime:             now.Add(-time.Minute),
			EndTime:               now.Add(24 * time.Hour),
			StartBalanceByteCount: balanceByteCount,
			BalanceByteCount:      balanceByteCount,
			NetRevenue:            model.UsdToNanoCents(1),
		})
	}
	return f
}

// The signed contract of a CreateContract reply, or its refusal.
func (self *twoPartyCloseFixture) networkEscrowSigned(t testing.TB, clientId server.Id, createContract *protocol.CreateContract) (*protocol.StoredContract, *protocol.ContractError) {
	t.Helper()
	resultFrames := self.control(t, clientId, createContract)
	defer returnConnectControlFrames(resultFrames)
	if len(resultFrames) != 1 {
		t.Fatalf("create contract returned %d frames, want 1", len(resultFrames))
	}
	message, err := connect.FromFrame(resultFrames[0])
	if err != nil {
		t.Fatal(err)
	}
	result, ok := message.(*protocol.CreateContractResult)
	if !ok {
		t.Fatalf("create contract returned %T", message)
	}
	if result.Error != nil || result.Contract == nil {
		if result.Error == nil {
			t.Fatal("create contract returned neither a contract nor an error")
		}
		return nil, result.Error
	}
	storedContract := &protocol.StoredContract{}
	if err := proto.Unmarshal(result.Contract.StoredContractBytes, storedContract); err != nil {
		t.Fatal(err)
	}
	return storedContract, nil
}

func networkEscrowContractId(t testing.TB, storedContract *protocol.StoredContract) server.Id {
	t.Helper()
	contractId, err := server.IdFromBytes(storedContract.ContractId)
	if err != nil {
		t.Fatal(err)
	}
	return contractId
}

// One contract's funding, earnings and usage facts.
type networkEscrowState struct {
	payerNetworkId      *server.Id
	companion           bool
	usageOriginIsSource *bool
	transferByteCount   model.ByteCount
	priority            int64
	escrowCount         int
	escrowByteCount     model.ByteCount
	redisReserved       bool
	debitByteCount      model.ByteCount
	sweepCount          int
	sweepNetworkId      *server.Id
	sweepByteCount      model.ByteCount
	providerUsage       []byte
}

func (self *twoPartyCloseFixture) networkEscrowState(t testing.TB, contractId server.Id) networkEscrowState {
	t.Helper()
	var state networkEscrowState
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `
			SELECT
				payer_network_id,
				companion_contract_id IS NOT NULL,
				usage_origin_is_source,
				transfer_byte_count,
				priority,
				(SELECT count(*) FROM transfer_escrow WHERE contract_id=$1),
				(SELECT COALESCE(sum(balance_byte_count),0)::bigint FROM transfer_escrow WHERE contract_id=$1),
				(SELECT COALESCE(bool_and(redis_reserved),false) FROM transfer_escrow WHERE contract_id=$1),
				(SELECT COALESCE(sum(debit_byte_count),0)::bigint FROM transfer_debit_journal WHERE contract_id=$1),
				(SELECT count(*) FROM transfer_escrow_sweep WHERE contract_id=$1),
				(SELECT min(network_id::text)::uuid FROM transfer_escrow_sweep WHERE contract_id=$1),
				(SELECT COALESCE(sum(payout_byte_count),0)::bigint FROM transfer_escrow_sweep WHERE contract_id=$1),
				provider_usage
			FROM transfer_contract WHERE contract_id=$1
		`, contractId).Scan(
			&state.payerNetworkId,
			&state.companion,
			&state.usageOriginIsSource,
			&state.transferByteCount,
			&state.priority,
			&state.escrowCount,
			&state.escrowByteCount,
			&state.redisReserved,
			&state.debitByteCount,
			&state.sweepCount,
			&state.sweepNetworkId,
			&state.sweepByteCount,
			&state.providerUsage,
		))
	})
	return state
}

// The contract reserved its whole signed size from payerNetworkId's balance
// through Redis admission, at the escrow priority it was signed with.
func requireNetworkEscrowFunded(t testing.TB, stage string, state networkEscrowState, payerNetworkId server.Id, transferByteCount model.ByteCount, signedPriority *uint32) {
	t.Helper()
	if state.payerNetworkId == nil || *state.payerNetworkId != payerNetworkId {
		t.Fatalf("%s: payer is %v, want the network %s", stage, state.payerNetworkId, payerNetworkId)
	}
	if state.escrowCount == 0 || !state.redisReserved || state.escrowByteCount != transferByteCount || state.transferByteCount != transferByteCount {
		t.Fatalf("%s: escrow rows=%d redis=%t escrowed=%d contract=%d, want a Redis-reserved escrow of %d",
			stage, state.escrowCount, state.redisReserved, state.escrowByteCount, state.transferByteCount, transferByteCount)
	}
	if signedPriority == nil || int64(*signedPriority) != state.priority || *signedPriority == model.TrustedPriority {
		t.Fatalf("%s: signed priority %v, want the escrow priority %d", stage, signedPriority, state.priority)
	}
}

// The usage snapshot's provider rows, by client.
func networkEscrowProviderUsage(t testing.TB, data []byte) map[server.Id]model.ByteCount {
	t.Helper()
	var snapshot struct {
		ByteCount model.ByteCount `json:"byte_count"`
		Providers []struct {
			ClientId  server.Id       `json:"client_id"`
			ByteCount model.ByteCount `json:"byte_count"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatalf("usage snapshot %q: %v", data, err)
	}
	providers := map[server.Id]model.ByteCount{}
	for _, provider := range snapshot.Providers {
		providers[provider.ClientId] = provider.ByteCount
	}
	return providers
}

func networkEscrowBalanceByteCount(t testing.TB, f *twoPartyCloseFixture) model.ByteCount {
	t.Helper()
	var balanceByteCount model.ByteCount
	server.Db(f.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(f.ctx, `SELECT COALESCE(sum(balance_byte_count),0)::bigint FROM transfer_balance WHERE network_id=$1`, f.windowNetworkId).Scan(&balanceByteCount))
	})
	return balanceByteCount
}

// The zero escrow contracts created so far, of one kind.
func networkEscrowZeroEscrowCount(t testing.TB, kind string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_zero_escrow_contracts_total" {
			continue
		}
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "kind" && label.GetValue() == kind {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// A network contract reserves from the network's balance, shows in its
// Pending, and settles through the escrow path: the network is debited for the
// bytes used, and the receiving client, on the payer's network, takes no
// provider earnings. Its usage snapshot still lists the receiving client, as
// SN usage did before.
func TestNetworkContractEscrowsFromNetworkBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newNetworkEscrowFixture(t, true, 64*MinContractTransferByteCount)
		streamVersion := uint32(connect.DefaultStreamVersion)
		signed, refusal := f.networkEscrowSigned(t, f.windowId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
			StreamVersion:     &streamVersion,
		})
		if refusal != nil {
			t.Fatalf("funded network contract refused: %s", *refusal)
		}
		contractId := networkEscrowContractId(t, signed)
		state := f.networkEscrowState(t, contractId)
		requireNetworkEscrowFunded(t, "network contract", state, f.windowNetworkId, MinContractTransferByteCount, signed.Priority)
		if model.ByteCount(signed.TransferByteCount) != MinContractTransferByteCount {
			t.Fatalf("signed %d bytes, want the escrowed %d", signed.TransferByteCount, MinContractTransferByteCount)
		}
		if pending := model.GetOpenTransferByteCount(f.ctx, f.windowNetworkId); pending != MinContractTransferByteCount {
			t.Fatalf("network Pending is %d bytes, want the open contract's %d", pending, MinContractTransferByteCount)
		}

		const byteCount = model.ByteCount(16 * 1024)
		f.closeContract(t, f.windowId, contractId, byteCount, false, true)
		f.assertOpen(t, contractId, "sender final")
		f.closeContract(t, f.providerId, contractId, byteCount, false, true)
		f.assertReceiverFinalSettled(t, contractId, byteCount)

		state = f.networkEscrowState(t, contractId)
		if state.debitByteCount != byteCount {
			t.Fatalf("settlement debited the network %d bytes, want %d", state.debitByteCount, byteCount)
		}
		if state.sweepCount != 0 || state.sweepByteCount != 0 {
			t.Fatalf("same-network receiver earned: %d sweeps of %d bytes", state.sweepCount, state.sweepByteCount)
		}
		if providers := networkEscrowProviderUsage(t, state.providerUsage); len(providers) != 1 || providers[f.providerId] != byteCount {
			t.Fatalf("usage snapshot providers %v, want the receiving client with %d bytes", providers, byteCount)
		}
		if pending := model.GetOpenTransferByteCount(f.ctx, f.windowNetworkId); pending != 0 {
			t.Fatalf("network Pending is %d bytes after settlement, want 0", pending)
		}
	})
}

// A network contract larger than the network's balance shrinks to fit it, and
// the client is signed only the escrowed size.
func TestNetworkContractShrinksToFitNetworkBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		balanceByteCount := 3 * model.Mib
		f := newNetworkEscrowFixture(t, true, balanceByteCount)
		signed, refusal := f.networkEscrowSigned(t, f.windowId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(MaxContractTransferByteCount),
		})
		if refusal != nil {
			t.Fatalf("network contract refused with %d bytes left: %s", balanceByteCount, *refusal)
		}
		if model.ByteCount(signed.TransferByteCount) != balanceByteCount {
			t.Fatalf("signed %d bytes, want the %d the network's balance holds", signed.TransferByteCount, balanceByteCount)
		}
		requireNetworkEscrowFunded(t, "shrunk network contract", f.networkEscrowState(t, networkEscrowContractId(t, signed)),
			f.windowNetworkId, balanceByteCount, signed.Priority)
	})
}

// A network with nothing left is refused network contracts with the ordinary
// insufficient balance error. Only while the degradation valve is open does it
// get a free contract instead, which settles on the no-escrow path.
func TestNetworkContractRefusedWhenNetworkBalanceExhausted(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newNetworkEscrowFixture(t, true, 0)
		request := &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
		}
		signed, refusal := f.networkEscrowSigned(t, f.windowId, request)
		if refusal == nil || *refusal != protocol.ContractError_InsufficientBalance {
			t.Fatalf("network contract with no balance: contract=%t refusal=%v, want InsufficientBalance", signed != nil, refusal)
		}
		var contractCount int
		server.Db(f.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(f.ctx, `SELECT count(*) FROM transfer_contract WHERE source_id=$1 AND destination_id=$2`, f.windowId, f.providerId).Scan(&contractCount))
		})
		if contractCount != 0 {
			t.Fatalf("refused network request left %d contracts", contractCount)
		}

		defer model.Testing_SetZeroContractCost(f.ctx, true)()
		before := networkEscrowZeroEscrowCount(t, "network")
		signed, refusal = f.networkEscrowSigned(t, f.windowId, request)
		if refusal != nil {
			t.Fatalf("exhausted network refused while the valve is open: %s", *refusal)
		}
		if after := networkEscrowZeroEscrowCount(t, "network"); after != before+1 {
			t.Fatalf("zero escrow network contracts went from %.0f to %.0f, want one more", before, after)
		}
		contractId := networkEscrowContractId(t, signed)
		state := f.networkEscrowState(t, contractId)
		if state.payerNetworkId != nil || state.escrowCount != 0 {
			t.Fatalf("valve contract has payer=%v and %d escrows, want neither", state.payerNetworkId, state.escrowCount)
		}
		const byteCount = model.ByteCount(8 * 1024)
		f.closeContract(t, f.windowId, contractId, byteCount, false, true)
		f.closeContract(t, f.providerId, contractId, byteCount, false, true)
		f.assertReceiverFinalSettled(t, contractId, byteCount)
		if state := f.networkEscrowState(t, contractId); state.debitByteCount != 0 || state.sweepCount != 0 {
			t.Fatalf("valve contract settled with debit=%d sweeps=%d, want a free close", state.debitByteCount, state.sweepCount)
		}
	})
}

// A network contract and a public contract draw on the same network transfer
// balance: what a network contract reserves is not available to a public one.
func TestNetworkAndPublicContractsShareOneBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		balanceByteCount := 2 * model.Mib
		f := newNetworkEscrowFixture(t, true, balanceByteCount)
		publicNetworkId, publicProviderId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(f.ctx, publicNetworkId, "synthetic network escrow public "+publicNetworkId.String(), server.NewId())
		model.Testing_CreateDevice(f.ctx, publicNetworkId, server.NewId(), publicProviderId, "synthetic public provider", "provider")
		model.SetProvide(f.ctx, publicProviderId, map[model.ProvideMode][]byte{
			model.ProvideModePublic: []byte("synthetic-network-escrow-public0"),
		})

		signed, refusal := f.networkEscrowSigned(t, f.windowId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(MaxContractTransferByteCount),
		})
		if refusal != nil {
			t.Fatalf("network contract refused: %s", *refusal)
		}
		if model.ByteCount(signed.TransferByteCount) != balanceByteCount {
			t.Fatalf("network contract signed %d bytes, want the whole %d balance", signed.TransferByteCount, balanceByteCount)
		}
		signed, refusal = f.networkEscrowSigned(t, f.windowId, &protocol.CreateContract{
			DestinationId:     publicProviderId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
		})
		if refusal == nil || *refusal != protocol.ContractError_InsufficientBalance {
			t.Fatalf("public contract after a network contract reserved the balance: contract=%t refusal=%v, want InsufficientBalance", signed != nil, refusal)
		}
	})
}

// A same-network companion reply is normalized to a network contract. It is
// funded by its source, from the network's balance, and keeps its destination
// as its usage origin, so SN usage credits the replying client, never the
// consumer it serves.
func TestNormalizedNetworkCompanionEscrowsAndKeepsUsageOrigin(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newNetworkEscrowFixture(t, true, 64*MinContractTransferByteCount)
		// the window receives the reply in the network mode
		model.SetProvide(f.ctx, f.windowId, map[model.ProvideMode][]byte{
			model.ProvideModeNetwork: []byte("synthetic-network-escrow-window0"),
		})
		streamVersion := uint32(connect.DefaultStreamVersion)
		if _, refusal := f.networkEscrowSigned(t, f.windowId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
			StreamVersion:     &streamVersion,
		}); refusal != nil {
			t.Fatalf("forward network contract refused: %s", *refusal)
		}
		signed, refusal := f.networkEscrowSigned(t, f.providerId, &protocol.CreateContract{
			DestinationId:     f.windowId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
			Companion:         true,
			StreamVersion:     &streamVersion,
		})
		if refusal != nil {
			t.Fatalf("normalized companion refused: %s", *refusal)
		}
		contractId := networkEscrowContractId(t, signed)
		state := f.networkEscrowState(t, contractId)
		if state.companion {
			t.Fatal("same-network companion was not normalized to a network contract")
		}
		requireNetworkEscrowFunded(t, "normalized companion", state, f.windowNetworkId, MinContractTransferByteCount, signed.Priority)
		if state.usageOriginIsSource == nil || *state.usageOriginIsSource {
			t.Fatalf("normalized companion usage origin is source=%v, want its destination", state.usageOriginIsSource)
		}

		const byteCount = model.ByteCount(24 * 1024)
		f.closeContract(t, f.providerId, contractId, byteCount, false, true)
		f.closeContract(t, f.windowId, contractId, byteCount, false, true)
		f.assertReceiverFinalSettled(t, contractId, byteCount)
		state = f.networkEscrowState(t, contractId)
		if state.debitByteCount != byteCount || state.sweepCount != 0 {
			t.Fatalf("normalized companion settled with debit=%d sweeps=%d, want a %d byte debit and no earnings", state.debitByteCount, state.sweepCount, byteCount)
		}
		if providers := networkEscrowProviderUsage(t, state.providerUsage); len(providers) != 1 || providers[f.providerId] != byteCount {
			t.Fatalf("usage snapshot providers %v, want the replying client with %d bytes", providers, byteCount)
		}
	})
}

// A network contract opened before escrow took over has no payer and no
// escrow. It still settles free on the no-escrow path, across the deploy: no
// debit, no earnings, and the network's balance and Pending untouched. Its
// usage snapshot lists the receiving client as before.
func TestOpenNoEscrowNetworkContractSettlesUnchanged(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		balanceByteCount := 64 * MinContractTransferByteCount
		f := newNetworkEscrowFixture(t, true, balanceByteCount)
		// the creator network contracts had before the change
		contractId, err := model.CreateContractNoEscrow(f.ctx, f.windowNetworkId, f.windowId, f.windowNetworkId, f.providerId, MinContractTransferByteCount)
		if err != nil {
			t.Fatal(err)
		}
		if state := f.networkEscrowState(t, contractId); state.payerNetworkId != nil || state.escrowCount != 0 {
			t.Fatalf("legacy network contract has payer=%v and %d escrows", state.payerNetworkId, state.escrowCount)
		}
		if pending := model.GetOpenTransferByteCount(f.ctx, f.windowNetworkId); pending != 0 {
			t.Fatalf("legacy network contract shows %d bytes in Pending", pending)
		}

		const byteCount = model.ByteCount(16 * 1024)
		f.closeContract(t, f.windowId, contractId, byteCount, false, true)
		f.closeContract(t, f.providerId, contractId, byteCount, false, true)
		f.assertReceiverFinalSettled(t, contractId, byteCount)
		state := f.networkEscrowState(t, contractId)
		if state.payerNetworkId != nil || state.escrowCount != 0 || state.debitByteCount != 0 || state.sweepCount != 0 {
			t.Fatalf("legacy network contract settled with payer=%v escrows=%d debit=%d sweeps=%d, want a free close",
				state.payerNetworkId, state.escrowCount, state.debitByteCount, state.sweepCount)
		}
		if providers := networkEscrowProviderUsage(t, state.providerUsage); len(providers) != 1 || providers[f.providerId] != byteCount {
			t.Fatalf("usage snapshot providers %v, want the receiving client with %d bytes", providers, byteCount)
		}
		if after := networkEscrowBalanceByteCount(t, f); after != balanceByteCount {
			t.Fatalf("legacy network contract changed the balance from %d to %d", balanceByteCount, after)
		}
	})
}

// A friends-and-family contract reserves from the dialing network's balance
// and settles through the escrow path: the dialing network is debited, and
// the provider, on another network, earns its share as on a public contract.
// No relationship resolves to friends and family today, so the contract is
// created at newContract.
func TestFriendsAndFamilyContractEscrowsFromSenderBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newNetworkEscrowFixture(t, false, 64*MinContractTransferByteCount)
		contractId, transferByteCount, priority, _, _, err := newContract(f.ctx, f.windowId, f.providerId, nil, false, true,
			MinContractTransferByteCount, model.ProvideModeFriendsAndFamily, false, 0, f.settings)
		if err != nil {
			t.Fatalf("funded friends-and-family contract refused: %v", err)
		}
		if transferByteCount != MinContractTransferByteCount {
			t.Fatalf("signed %d bytes, want %d", transferByteCount, MinContractTransferByteCount)
		}
		requireNetworkEscrowFunded(t, "friends-and-family contract", f.networkEscrowState(t, contractId), f.windowNetworkId, MinContractTransferByteCount, &priority)

		const byteCount = model.ByteCount(32 * 1024)
		f.closeContract(t, f.windowId, contractId, byteCount, false, true)
		f.closeContract(t, f.providerId, contractId, byteCount, false, true)
		f.assertReceiverFinalSettled(t, contractId, byteCount)
		state := f.networkEscrowState(t, contractId)
		if state.debitByteCount != byteCount {
			t.Fatalf("settlement debited the dialing network %d bytes, want %d", state.debitByteCount, byteCount)
		}
		if state.sweepCount != 1 || state.sweepNetworkId == nil || *state.sweepNetworkId != f.providerNetworkId || state.sweepByteCount != byteCount {
			t.Fatalf("provider earnings: %d sweeps of %d bytes to %v, want one of %d bytes to %s",
				state.sweepCount, state.sweepByteCount, state.sweepNetworkId, byteCount, f.providerNetworkId)
		}
	})
}
