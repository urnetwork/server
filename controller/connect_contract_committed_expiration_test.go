// Signing reuses the successful model result without a new pool acquisition.
package controller

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The public path has no optional identity lookup after its model result.
func committedExpirationTestClients(ctx context.Context, t testing.TB) (server.Id, server.Id, []byte) {
	t.Helper()
	_, providerId, _, payerId := companionOriginTestSetup(ctx, t)
	secret := []byte("synthetic-committed-expiration-key")
	model.SetProvide(ctx, providerId, map[model.ProvideMode][]byte{model.ProvideModePublic: secret})
	return payerId, providerId, secret
}

// The provider proof covers the exact result, including an already due deadline.
func requireCommittedExpirationReply(t testing.TB, frames []*protocol.Frame, escrow *model.TransferEscrow, secret []byte) {
	t.Helper()
	defer returnConnectControlFrames(frames)
	if len(frames) != 1 {
		t.Fatalf("reply frames=%d", len(frames))
	}
	message, err := connect.FromFrame(frames[0])
	if err != nil {
		t.Fatal(err)
	}
	result, ok := message.(*protocol.CreateContractResult)
	if !ok || result.Error != nil || result.Contract == nil {
		t.Fatal("missing successful contract reply")
	}
	var stored protocol.StoredContract
	if err := proto.Unmarshal(result.Contract.StoredContractBytes, &stored); err != nil {
		t.Fatal(err)
	}
	id, err := server.IdFromBytes(stored.ContractId)
	if err != nil || id != escrow.ContractId || stored.ExpirationTimeUnixMilli == nil ||
		*stored.ExpirationTimeUnixMilli != escrow.ExpirationTime.UnixMilli() {
		t.Fatal("signing changed the committed contract id or deadline", err)
	}
	if !connect.VerifyStoredContract(connect.DefaultContractManagerSettings(), secret,
		result.Contract.StoredContractBytes, result.Contract.StoredContractHmac) {
		t.Fatal("committed deadline is not covered by the provider proof")
	}
}

// Count after the real transaction and its joined posts return. The old final
// expiry lookup deterministically adds one acquisition after this boundary.
func TestCreateContractSignsCommittedExpirationWithoutRead(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		sourceId, destinationId, secret := committedExpirationTestClients(ctx, t)
		previous := createTransferEscrow
		defer func() { createTransferEscrow = previous }()
		var committed *model.TransferEscrow
		var committedAcquires float64
		createTransferEscrow = func(ctx context.Context, sourceNetworkId, sourceId, destinationNetworkId, destinationId server.Id, count model.ByteCount) (*model.TransferEscrow, error) {
			result, err := previous(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, count)
			if err == nil {
				committed = result
				committedAcquires = contractPairAcquireCount(t)
			}
			return result, err
		}
		frames, err := CreateContract(ctx, sourceId, &protocol.CreateContract{
			DestinationId: destinationId.Bytes(), TransferByteCount: uint64(MinContractTransferByteCount),
		}, connect.DefaultContractManagerSettings())
		if err != nil || committed == nil || committed.ExpirationTime.IsZero() {
			returnConnectControlFrames(frames)
			t.Fatal("creation did not return its committed deadline", err)
		}
		if delta := contractPairAcquireCount(t) - committedAcquires; delta != 0 {
			returnConnectControlFrames(frames)
			t.Fatalf("signing reacquired PostgreSQL after commit: %.0f", delta)
		}
		requireCommittedExpirationReply(t, frames, committed, secret)
	})
}

// A reused committed result keeps its original deadline. Force that deadline
// into the past without sleeping; response assembly must neither refresh it
// nor query the row again. This does not enable the dormant reuse policy.
func TestCreateContractReusesCommittedExpirationWithoutExtension(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		sourceId, destinationId, secret := committedExpirationTestClients(ctx, t)
		sourceNetwork, destinationNetwork := model.FindActiveClientPairNetworks(ctx, sourceId, destinationId)
		if sourceNetwork == nil || destinationNetwork == nil {
			t.Fatal("synthetic contract endpoints are not active")
		}
		committed, err := model.CreateTransferEscrow(ctx, *sourceNetwork, sourceId, *destinationNetwork, destinationId, MinContractTransferByteCount)
		if err != nil {
			t.Fatal(err)
		}
		committed.ExpirationTime = time.UnixMilli(1_000_000).UTC()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, committed.ContractId, committed.ExpirationTime))
		})
		previous := createTransferEscrow
		defer func() { createTransferEscrow = previous }()
		var committedAcquires float64
		createTransferEscrow = func(context.Context, server.Id, server.Id, server.Id, server.Id, model.ByteCount) (*model.TransferEscrow, error) {
			committedAcquires = contractPairAcquireCount(t)
			return committed, nil
		}
		for range 2 {
			frames, err := CreateContract(ctx, sourceId, &protocol.CreateContract{
				DestinationId: destinationId.Bytes(), TransferByteCount: uint64(MinContractTransferByteCount),
			}, connect.DefaultContractManagerSettings())
			if err != nil {
				returnConnectControlFrames(frames)
				t.Fatal(err)
			}
			if delta := contractPairAcquireCount(t) - committedAcquires; delta != 0 {
				returnConnectControlFrames(frames)
				t.Fatalf("reused result reacquired PostgreSQL: %.0f", delta)
			}
			requireCommittedExpirationReply(t, frames, committed, secret)
		}
	})
}

// Cancellation after commit still refuses the response, without asking a
// canceled pool acquisition to provide that boundary as an accidental side effect.
func TestCreateContractCommittedExpirationPreservesCancellation(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(model.WithProviderWorkSessionSource(t.Context(), nil))
		defer cancel()
		sourceId, destinationId, _ := committedExpirationTestClients(ctx, t)
		previous := createTransferEscrow
		defer func() { createTransferEscrow = previous }()
		createTransferEscrow = func(ctx context.Context, sourceNetworkId, sourceId, destinationNetworkId, destinationId server.Id, count model.ByteCount) (*model.TransferEscrow, error) {
			result, err := previous(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, count)
			cancel()
			return result, err
		}
		frames, err := CreateContract(ctx, sourceId, &protocol.CreateContract{
			DestinationId: destinationId.Bytes(), TransferByteCount: uint64(MinContractTransferByteCount),
		}, connect.DefaultContractManagerSettings())
		defer returnConnectControlFrames(frames)
		if !errors.Is(err, context.Canceled) || len(frames) != 0 {
			t.Fatal("canceled creation emitted a signed contract", err)
		}
	})
}
