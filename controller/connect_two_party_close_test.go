// A contract settles as soon as both parties report final through the resident
// control dispatcher, whichever escrow kind it holds. The receiver's final is
// recorded as a final, never a checkpoint, and is not refused or dropped after
// the sender's final. A receiver checkpoint alone keeps the contract open for
// the deadline closer unless it follows the sender's final (owner decision B,
// connect_covered_checkpoint_test.go). Legacy-escrow contracts record the same
// final and defer settlement to a durable intent; the
// subscription_legacy_settlement tests cover that path.
package controller

import (
	"context"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
)

// The window pays from a paid grant and is the sender of its origin contracts.
// The provider is the receiver of origin contracts and the sender of companions.
type twoPartyCloseFixture struct {
	ctx               context.Context
	settings          *connect.ContractManagerSettings
	windowNetworkId   server.Id
	windowId          server.Id
	providerNetworkId server.Id
	providerId        server.Id
}

// sameNetwork puts both clients in one network, which takes the no-escrow
// network-mode path. Otherwise the provider advertises public provide and the
// contract takes a Redis-admitted escrow.
func newTwoPartyCloseFixture(t testing.TB, sameNetwork bool) *twoPartyCloseFixture {
	t.Helper()
	ctx := t.Context()
	fixture := &twoPartyCloseFixture{
		ctx:               ctx,
		settings:          connect.DefaultContractManagerSettings(),
		windowNetworkId:   server.NewId(),
		windowId:          server.NewId(),
		providerNetworkId: server.NewId(),
		providerId:        server.NewId(),
	}
	// network names are unique; several fixtures may share one test database
	model.Testing_CreateNetwork(ctx, fixture.windowNetworkId, "synthetic two party window "+fixture.windowNetworkId.String(), server.NewId())
	if sameNetwork {
		fixture.providerNetworkId = fixture.windowNetworkId
	} else {
		model.Testing_CreateNetwork(ctx, fixture.providerNetworkId, "synthetic two party provider "+fixture.providerNetworkId.String(), server.NewId())
	}
	model.Testing_CreateDevice(ctx, fixture.windowNetworkId, server.NewId(), fixture.windowId, "synthetic window", "window")
	model.Testing_CreateDevice(ctx, fixture.providerNetworkId, server.NewId(), fixture.providerId, "synthetic provider", "provider")
	provideMode := model.ProvideModePublic
	if sameNetwork {
		provideMode = model.ProvideModeNetwork
	}
	model.SetProvide(ctx, fixture.providerId, map[model.ProvideMode][]byte{
		provideMode: []byte("synthetic-two-party-provide-key0"),
	})
	now := server.NowUtc()
	model.AddTransferBalance(ctx, &model.TransferBalance{
		NetworkId:             fixture.windowNetworkId,
		StartTime:             now.Add(-time.Minute),
		EndTime:               now.Add(24 * time.Hour),
		StartBalanceByteCount: 64 * MinContractTransferByteCount,
		BalanceByteCount:      64 * MinContractTransferByteCount,
		NetRevenue:            model.UsdToNanoCents(1),
	})
	return fixture
}

// Dispatch one frame exactly as the resident connect path does for clientId.
func (self *twoPartyCloseFixture) control(t testing.TB, clientId server.Id, message proto.Message) []*protocol.Frame {
	t.Helper()
	frame, err := connect.ToFrame(message, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer returnConnectControlFrames([]*protocol.Frame{frame})
	resultFrames, err := ConnectControlFrames(self.ctx, clientId, []*protocol.Frame{frame}, self.settings)
	if err != nil {
		returnConnectControlFrames(resultFrames)
		t.Fatalf("control frame from %s failed: %v", clientId, err)
	}
	return resultFrames
}

// Create through the real contract control message and return the signed id.
func (self *twoPartyCloseFixture) createContract(t testing.TB, clientId server.Id, createContract *protocol.CreateContract) server.Id {
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
	if !ok || result.Error != nil || result.Contract == nil {
		t.Fatalf("create contract did not return a contract: %v", message)
	}
	storedContract := &protocol.StoredContract{}
	if err := proto.Unmarshal(result.Contract.StoredContractBytes, storedContract); err != nil {
		t.Fatal(err)
	}
	contractId, err := server.IdFromBytes(storedContract.ContractId)
	if err != nil {
		t.Fatal(err)
	}
	return contractId
}

// Current clients identify every report; older clients send id-less reports.
func (self *twoPartyCloseFixture) closeContract(t testing.TB, clientId server.Id, contractId server.Id, ackedByteCount model.ByteCount, checkpoint bool, identified bool) {
	t.Helper()
	closeContract := &protocol.CloseContract{
		ContractId:     contractId.Bytes(),
		AckedByteCount: uint64(ackedByteCount),
		Checkpoint:     checkpoint,
	}
	if identified {
		closeContract.ReportId = server.NewId().Bytes()
	}
	returnConnectControlFrames(self.control(t, clientId, closeContract))
}

type twoPartyCloseState struct {
	outcome                          *model.ContractOutcome
	destinationFound                 bool
	destinationCheckpoint            bool
	destinationUsedTransferByteCount model.ByteCount
}

func (self *twoPartyCloseFixture) state(t testing.TB, contractId server.Id) twoPartyCloseState {
	t.Helper()
	var state twoPartyCloseState
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT outcome FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(&state.outcome))
		rows, err := conn.Query(self.ctx, `SELECT checkpoint,used_transfer_byte_count FROM contract_close WHERE contract_id=$1 AND party=$2`, contractId, model.ContractPartyDestination)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				state.destinationFound = true
				server.Raise(rows.Scan(&state.destinationCheckpoint, &state.destinationUsedTransferByteCount))
			}
		})
	})
	return state
}

func (self *twoPartyCloseFixture) assertOpen(t testing.TB, contractId server.Id, stage string) {
	t.Helper()
	if state := self.state(t, contractId); state.outcome != nil {
		t.Fatalf("%s: contract closed with outcome %s before both parties were final", stage, *state.outcome)
	}
}

// The receiver final must be recorded as a final with every reported byte, and
// the contract must settle in the same receiver close, without the closer.
func (self *twoPartyCloseFixture) assertReceiverFinalSettled(t testing.TB, contractId server.Id, receiverByteCount model.ByteCount) {
	t.Helper()
	state := self.state(t, contractId)
	if !state.destinationFound || state.destinationCheckpoint {
		t.Fatalf("receiver final not recorded as final: found=%t checkpoint=%t", state.destinationFound, state.destinationCheckpoint)
	}
	if state.destinationUsedTransferByteCount != receiverByteCount {
		t.Fatalf("receiver final recorded %d bytes, want %d", state.destinationUsedTransferByteCount, receiverByteCount)
	}
	if state.outcome == nil || *state.outcome != model.ContractOutcomeSettled {
		t.Fatalf("contract did not settle when both parties were final: outcome=%v", state.outcome)
	}
}

func (self *twoPartyCloseFixture) redisReserved(t testing.TB, contractId server.Id) (escrowCount int, redisReserved bool) {
	t.Helper()
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT count(*),COALESCE(bool_and(redis_reserved),false) FROM transfer_escrow WHERE contract_id=$1`, contractId).Scan(&escrowCount, &redisReserved))
	})
	return
}

// The production cross-network origin: sender final, then the receiver's
// checkpoint at receive-sequence exit, then its final for the same contract.
func TestTwoPartyFinalCloseSettlesRedisOriginContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newTwoPartyCloseFixture(t, false)
		contractId := f.createContract(t, f.windowId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
		})
		if escrowCount, redisReserved := f.redisReserved(t, contractId); escrowCount == 0 || !redisReserved {
			t.Fatalf("origin escrow is not Redis reserved: escrows=%d redis=%t", escrowCount, redisReserved)
		}
		const byteCount = model.ByteCount(64 * 1024)
		f.closeContract(t, f.windowId, contractId, byteCount, false, true)
		f.assertOpen(t, contractId, "sender final")
		f.closeContract(t, f.providerId, contractId, byteCount/2, true, true)
		f.assertOpen(t, contractId, "receiver checkpoint")
		if state := f.state(t, contractId); !state.destinationFound || !state.destinationCheckpoint {
			t.Fatalf("receiver checkpoint not recorded as a checkpoint: found=%t checkpoint=%t", state.destinationFound, state.destinationCheckpoint)
		}
		f.closeContract(t, f.providerId, contractId, byteCount-byteCount/2, false, true)
		f.assertReceiverFinalSettled(t, contractId, byteCount)
	})
}

// Older clients send id-less reports; the receiver final arrives first here.
func TestTwoPartyLegacyFinalCloseSettlesRedisOriginContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newTwoPartyCloseFixture(t, false)
		contractId := f.createContract(t, f.windowId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
		})
		const byteCount = model.ByteCount(48 * 1024)
		f.closeContract(t, f.providerId, contractId, byteCount, false, false)
		f.assertOpen(t, contractId, "receiver final")
		f.closeContract(t, f.windowId, contractId, byteCount, false, false)
		f.assertReceiverFinalSettled(t, contractId, byteCount)
	})
}

// The companion answers the window's origin. The provider sends it and the
// window, which also pays for it, is its receiver.
func TestTwoPartyFinalCloseSettlesCompanionContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newTwoPartyCloseFixture(t, false)
		// windows publish a stream key so providers can answer on a companion
		model.SetProvide(f.ctx, f.windowId, map[model.ProvideMode][]byte{
			model.ProvideModeStream: []byte("synthetic-two-party-stream-key00"),
		})
		f.createContract(t, f.windowId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
		})
		streamVersion := uint32(connect.DefaultStreamVersion)
		companionContractId := f.createContract(t, f.providerId, &protocol.CreateContract{
			DestinationId:     f.windowId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
			Companion:         true,
			StreamVersion:     &streamVersion,
		})
		var companion bool
		server.Db(f.ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(f.ctx, `SELECT companion_contract_id IS NOT NULL FROM transfer_contract WHERE contract_id=$1`, companionContractId).Scan(&companion))
		})
		if !companion {
			t.Fatal("reply contract was not created as a companion")
		}
		const byteCount = model.ByteCount(32 * 1024)
		f.closeContract(t, f.providerId, companionContractId, byteCount, false, true)
		f.assertOpen(t, companionContractId, "companion sender final")
		f.closeContract(t, f.windowId, companionContractId, byteCount, false, true)
		f.assertReceiverFinalSettled(t, companionContractId, byteCount)
	})
}

// A same-network contract carries no escrow and settles as a free close.
func TestTwoPartyFinalCloseSettlesNetworkContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newTwoPartyCloseFixture(t, true)
		contractId := f.createContract(t, f.windowId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
		})
		if escrowCount, _ := f.redisReserved(t, contractId); escrowCount != 0 {
			t.Fatalf("network contract unexpectedly holds %d escrows", escrowCount)
		}
		const byteCount = model.ByteCount(16 * 1024)
		f.closeContract(t, f.windowId, contractId, byteCount, false, true)
		f.assertOpen(t, contractId, "sender final")
		f.closeContract(t, f.providerId, contractId, byteCount, false, true)
		f.assertReceiverFinalSettled(t, contractId, byteCount)
	})
}

// A receiver that only checkpointed before the sender's final leaves the
// contract open after that final; the receiver's next report or the deadline
// closer owns it. Only a receiver report after the sender's final settles a
// checkpoint (owner decision B, connect_covered_checkpoint_test.go). Changing
// that rule is a settlement decision that must update this test.
func TestTwoPartyReceiverCheckpointLeavesContractForCloser(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newTwoPartyCloseFixture(t, false)
		contractId := f.createContract(t, f.windowId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
		})
		const byteCount = model.ByteCount(8 * 1024)
		f.closeContract(t, f.providerId, contractId, byteCount, true, true)
		f.closeContract(t, f.windowId, contractId, byteCount, false, true)
		f.assertOpen(t, contractId, "sender final after receiver checkpoint")
		if state := f.state(t, contractId); !state.destinationFound || !state.destinationCheckpoint {
			t.Fatalf("receiver checkpoint not retained: found=%t checkpoint=%t", state.destinationFound, state.destinationCheckpoint)
		}
	})
}
