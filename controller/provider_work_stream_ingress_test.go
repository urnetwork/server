// Exercise the original control-frame owner through actual Redis stream birth
// and terminal SQL custody, including a noncanonical but valid protobuf body.
package controller

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	snprotocol "github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"google.golang.org/protobuf/proto"
)

func TestProviderWorkOriginalIngressOwnsActualStreamAndClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		now := server.NowUtc()
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{117}, ed25519.SeedSize))
		authority := snprotocol.ProviderWorkSourceAuthority{DomainHash: [32]byte{118}, SourceId: server.NewId().String(), Generation: server.NewId().String(), PublicKey: [32]byte(key.Public().(ed25519.PublicKey)), FromUnixMicro: now.Add(-time.Hour).UnixMicro(), ThroughUnixMicro: now.Add(time.Hour).UnixMicro(), MaxEndpointEvents: 4096, MaxCohortMembers: 64, DirectoryPublicKeys: [][32]byte{}}
		source, err := model.NewProviderWorkSessionSource(authority, key)
		if err != nil {
			t.Fatal(err)
		}
		ctx := model.WithProviderWorkSessionSource(t.Context(), source)
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "original-stream.example", server.NewId())
		sourceId, destinationId, intermediaryId := server.NewId(), server.NewId(), server.NewId()
		for _, id := range []server.Id{sourceId, destinationId, intermediaryId} {
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), id, "synthetic", "synthetic")
		}
		model.SetProvide(ctx, destinationId, map[model.ProvideMode][]byte{model.ProvideModeNetwork: bytes.Repeat([]byte{119}, 32)})
		handlerId := model.CreateNetworkClientHandler(ctx)
		for index, id := range []server.Id{sourceId, destinationId} {
			address := []string{"192.0.2.20:12001", "192.0.2.21:12002"}[index]
			if _, _, _, _, err := model.ConnectNetworkClientWithIpFamily(ctx, id, address, handlerId, 4); err != nil {
				t.Fatal(err)
			}
		}
		version := uint32(1)
		request := &protocol.CreateContract{DestinationId: destinationId.Bytes(), TransferByteCount: 121, IntermediaryIds: [][]byte{intermediaryId.Bytes()}, StreamVersion: &version}
		body, err := proto.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		body = append(body, 0x10, 121)
		frame := &protocol.Frame{MessageType: protocol.MessageType_TransferCreateContract, MessageBytes: body}
		originalFrame, err := proto.Marshal(frame)
		if err != nil {
			t.Fatal(err)
		}
		expectedHash := sha256.Sum256(originalFrame)
		replies, err := ConnectControlFrames(ctx, sourceId, []*protocol.Frame{frame}, connect.DefaultContractManagerSettings())
		if err != nil {
			t.Fatal(err)
		}
		defer returnConnectControlFrames(replies)
		if len(replies) != 1 {
			t.Fatal("original ingress returned no contract", len(replies))
		}
		value, err := connect.FromFrame(replies[0])
		if err != nil {
			t.Fatal(err)
		}
		result, ok := value.(*protocol.CreateContractResult)
		if !ok || result.Error != nil || result.Contract == nil {
			t.Fatal("actual ingress refused synthetic stream", result)
		}
		var stored protocol.StoredContract
		if err := proto.Unmarshal(result.Contract.StoredContractBytes, &stored); err != nil {
			t.Fatal(err)
		}
		contractId := server.RequireIdFromBytes(stored.ContractId)
		if len(stored.StreamId) != 16 {
			t.Fatal("fixture missed actual stream birth")
		}
		for _, clientId := range []server.Id{sourceId, destinationId} {
			closeFrame, err := connect.ToFrame(&protocol.CloseContract{ContractId: stored.ContractId, AckedByteCount: 121, Checkpoint: false}, connect.DefaultProtocolVersion)
			if err != nil {
				t.Fatal(err)
			}
			closeReplies, err := ConnectControlFrames(ctx, clientId, []*protocol.Frame{closeFrame}, connect.DefaultContractManagerSettings())
			returnConnectControlFrames(closeReplies)
			if err != nil {
				t.Fatal(err)
			}
		}
		raws, err := model.ListProviderWorkOriginals(ctx, []server.Id{contractId})
		if err != nil {
			t.Fatal(err)
		}
		var cohort *snprotocol.ProviderWorkReceipt
		var outcome *snprotocol.ProviderWorkOutcome
		for _, raw := range raws {
			receipt, err := snprotocol.DecodeProviderWorkReceipt(ctx, raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := snprotocol.VerifyProviderWorkReceiptAuthority(ctx, receipt, authority); err != nil {
				t.Fatal(err)
			}
			if receipt.Stream != nil {
				cohort = &receipt
			}
			if receipt.Outcome != nil {
				outcome = receipt.Outcome
			}
		}
		if cohort == nil || outcome == nil || cohort.Stream.RequestFrameHash != expectedHash || len(cohort.Stream.Intermediaries) != 1 || cohort.Stream.Intermediaries[0].ClientId != intermediaryId.String() || cohort.Stream.Intermediaries[0].NetworkId != networkId.String() {
			t.Fatal("actual ingress substituted its original request or stream parties", cohort, outcome)
		}
		cohortHash, err := cohort.ContentHash(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if outcome.StreamHash != cohortHash || outcome.SourceBytes != 121 || outcome.DestinationBytes != 121 {
			t.Fatal("live close lost the original cohort or completed work", outcome)
		}
		changed := *cohort
		changedBody := *cohort.Stream
		changedBody.Intermediaries = append([]snprotocol.ProviderWorkParticipant(nil), cohort.Stream.Intermediaries...)
		changedBody.Intermediaries[0].NetworkId = server.NewId().String()
		changed.Stream = &changedBody
		if err := changed.Verify(ctx); !errors.Is(err, snprotocol.ErrProviderWorkIntegrity) {
			t.Fatal("mutated original provider network was accepted", err)
		}
	})
}
