package controller

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"google.golang.org/protobuf/proto"
)

// Exercise the actual derived credential and encoded control boundary: the
// signed reply promises only the private anchor's ramp, and retirement still
// invalidates both the credential and a stale in-process request.
func TestProberShardCompanionReplyControlKeepsPrivateAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
		defer cancel()
		owner, err := model.BeginProberShard(ctx, model.ProberShardKey{TaskId: server.NewId(), Epoch: server.NewId(), ShardCount: 1}, 65536, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := model.ProberShardIdentity(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		parentClaims, err := session.ParseByJwt(ctx, identity.ByClientJwt)
		if err != nil {
			t.Fatal(err)
		}
		parentSession := session.Testing_CreateClientSession(ctx, parentClaims)
		defer parentSession.Cancel()
		child, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{SourceClientId: &owner.ClientId,
			Description: "synthetic private reply", DeviceSpec: "synthetic"}, parentSession)
		if err != nil || child == nil || child.Error != nil || child.ClientId == nil || child.ByClientJwt == nil {
			t.Fatal("derived private credential failed", err)
		}
		claims, err := session.ParseByJwt(ctx, *child.ByClientJwt)
		if err != nil {
			t.Fatal(err)
		}
		if claims.NetworkId != owner.NetworkId || session.ValidateByJwtState(ctx, claims, true) != nil {
			t.Fatal("derived credential lost current private ownership")
		}
		childSession := session.Testing_CreateClientSession(ctx, claims)
		defer childSession.Cancel()
		peerNetwork, peerID := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, peerNetwork, "synthetic-private-reply-peer", server.NewId())
		model.Testing_CreateDevice(ctx, peerNetwork, server.NewId(), peerID, "synthetic peer", "synthetic")
		if _, err := model.CreateTransferEscrow(ctx, owner.NetworkId, *child.ClientId, peerNetwork, peerID, 1024); err != nil {
			t.Fatal(err)
		}
		back, err := model.CreateCompanionTransferEscrow(ctx, peerNetwork, peerID, owner.NetworkId, *child.ClientId, 4096, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		key := []byte("synthetic-private-reply-key")
		model.SetProvide(ctx, peerID, map[model.ProvideMode][]byte{model.ProvideModeStream: key})
		request := companionCreateContract(*child.ClientId, peerID)
		request.TransferByteCount = 32768
		frame, err := connect.ToFrame(request, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		defer connect.MessagePoolReturn(frame.MessageBytes)
		pack, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
		if err != nil {
			t.Fatal(err)
		}
		args := &ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(pack)}
		readResult := func() (*protocol.CreateContractResult, error) {
			result, err := ConnectControl(args, childSession)
			if err != nil {
				return nil, err
			}
			if result == nil || result.Error != nil {
				t.Fatal("control returned no protocol result")
			}
			wire, err := base64.StdEncoding.DecodeString(result.Pack)
			if err != nil {
				return nil, err
			}
			decoded := &protocol.Pack{}
			if err := proto.Unmarshal(wire, decoded); err != nil {
				return nil, err
			}
			if len(decoded.Frames) != 1 {
				t.Fatal("control returned an unexpected frame count")
			}
			message, err := connect.FromFrame(decoded.Frames[0])
			if err != nil {
				return nil, err
			}
			created, ok := message.(*protocol.CreateContractResult)
			if !ok {
				t.Fatal("control returned an unexpected frame type")
			}
			return created, nil
		}
		created, err := readResult()
		if err != nil || created.Error != nil || created.Contract == nil {
			t.Fatal("private reply control failed", err)
		}
		stored := &protocol.StoredContract{}
		if err := proto.Unmarshal(created.Contract.StoredContractBytes, stored); err != nil {
			t.Fatal(err)
		}
		if stored.TransferByteCount != 1024 || !connect.VerifyStoredContract(connect.DefaultContractManagerSettings(), key,
			created.Contract.StoredContractBytes, created.Contract.StoredContractHmac) {
			t.Fatal("private reply signed a different promise")
		}
		contractID, err := server.IdFromBytes(stored.ContractId)
		if err != nil {
			t.Fatal(err)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var payer, balance, anchor server.Id
			var reserved int64
			server.Raise(conn.QueryRow(ctx, `SELECT c.payer_network_id,e.balance_id,c.companion_contract_id,e.balance_byte_count
				FROM transfer_contract c JOIN transfer_escrow e USING(contract_id) WHERE c.contract_id=$1`, contractID).Scan(&payer, &balance, &anchor, &reserved))
			if payer != owner.NetworkId || balance != owner.BalanceId || anchor != back.ContractId || reserved != 1024 {
				t.Fatal("encoded control changed private payer, anchor or escrow")
			}
		})
		server.Raise(model.DrainProberShard(ctx, owner.Key))
		if session.ValidateByJwtState(ctx, claims, true) == nil {
			t.Fatal("retired derived credential remained valid")
		}
		created, err = readResult()
		if err == nil && created.Error == nil {
			t.Fatal("stale controller admitted a retired private reply")
		}
		server.Db(ctx, func(conn server.PgConn) {
			var count int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE payer_network_id=$1`, owner.NetworkId).Scan(&count))
			if count != 3 {
				t.Fatalf("retired request changed contract count: %d", count)
			}
		})
	})
}
