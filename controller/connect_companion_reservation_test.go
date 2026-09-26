// Contract responses must advertise the exact escrowed reservation after the
// internal prober's companion budget is applied.
package controller

import (
	"context"
	"encoding/base64"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Exercises the controller-to-model boundary used to construct the signed
// StoredContract; returning the original request would overpromise capacity.
func TestProberCompanionResponseUsesActualReservation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		providerNetworkId, providerId, proberNetworkId, proberId := companionOriginTestSetup(ctx, t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO prober_identity (singleton, network_id, client_id)
				VALUES (true, $1, $2)
				ON CONFLICT (singleton) DO UPDATE SET network_id = excluded.network_id, client_id = excluded.client_id
			`, proberNetworkId, proberId))
		})
		const opening model.ByteCount = 1024 * 1024
		if _, err := model.CreateTransferEscrow(ctx, proberNetworkId, proberId, providerNetworkId, providerId, opening); err != nil {
			t.Fatal(err)
		}
		request := companionCreateContract(providerId, proberId)
		request.TransferByteCount = 128 * uint64(opening)
		contractId, count, _, _, err := nextContract(ctx, providerId, request, true, model.ProvideModeStream, connect.DefaultContractManagerSettings())
		if err != nil {
			t.Fatal(err)
		}
		if count != opening {
			t.Errorf("controller would sign %d bytes, want actual reservation %d", count, opening)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var stored model.ByteCount
			server.Raise(conn.QueryRow(ctx, `SELECT transfer_byte_count FROM transfer_contract WHERE contract_id = $1`, contractId).Scan(&stored))
			if count != stored {
				t.Errorf("controller would sign %d bytes but persisted only %d", count, stored)
			}
		})
	})
}

// Both the resident frame dispatcher and API's encoded control boundary must
// sign the actual reservation, including legacy non-companion stream fallback.
func TestProberCompanionControlPathsSignActualReservation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		providerNetworkId, providerId, proberNetworkId, proberId := companionOriginTestSetup(ctx, t)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO prober_identity (singleton, network_id, client_id)
				VALUES (true, $1, $2)
				ON CONFLICT (singleton) DO UPDATE SET network_id = excluded.network_id, client_id = excluded.client_id
			`, proberNetworkId, proberId))
		})
		const opening model.ByteCount = 1024 * 1024
		if _, err := model.CreateTransferEscrow(ctx, proberNetworkId, proberId, providerNetworkId, providerId, opening); err != nil {
			t.Fatal(err)
		}
		key := []byte("synthetic-companion-reservation-test-key")
		model.SetProvide(ctx, proberId, map[model.ProvideMode][]byte{model.ProvideModeStream: key})
		settings := connect.DefaultContractManagerSettings()
		for _, test := range []struct {
			name      string
			http      bool
			companion bool
		}{
			{name: "resident companion", companion: true},
			{name: "resident fallback"},
			{name: "http companion", http: true, companion: true},
			{name: "http fallback", http: true},
		} {
			func() {
				request := companionCreateContract(providerId, proberId)
				request.TransferByteCount = 128 * uint64(opening)
				request.Companion = test.companion
				frame, err := connect.ToFrame(request, connect.DefaultProtocolVersion)
				if err != nil {
					t.Fatal(err)
				}
				defer connect.MessagePoolReturn(frame.MessageBytes)
				var frames []*protocol.Frame
				if test.http {
					pack, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
					if err != nil {
						t.Fatal(err)
					}
					byJwt := jwt.NewByJwt(providerNetworkId, server.NewId(), "synthetic provider", false, false).Client(server.NewId(), providerId)
					clientSession := session.Testing_CreateClientSession(ctx, byJwt)
					defer clientSession.Cancel()
					result, err := ConnectControl(&ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(pack)}, clientSession)
					if err != nil || result == nil || result.Error != nil {
						t.Fatalf("%s control failed: %v, result=%v", test.name, err, result)
					}
					wire, err := base64.StdEncoding.DecodeString(result.Pack)
					if err != nil {
						t.Fatal(err)
					}
					response := &protocol.Pack{}
					if err := proto.Unmarshal(wire, response); err != nil {
						t.Fatal(err)
					}
					frames = response.Frames
				} else {
					frames, err = ConnectControlFrames(ctx, providerId, []*protocol.Frame{frame}, settings)
					if err != nil {
						t.Fatalf("%s control failed: %v", test.name, err)
					}
					defer returnConnectControlFrames(frames)
				}
				if len(frames) != 1 {
					t.Fatalf("%s response frames = %d, want 1", test.name, len(frames))
				}
				message, err := connect.FromFrame(frames[0])
				if err != nil {
					t.Fatal(err)
				}
				result, ok := message.(*protocol.CreateContractResult)
				if !ok || result.Error != nil || result.Contract == nil {
					t.Fatalf("%s missing successful contract response", test.name)
				}
				stored := &protocol.StoredContract{}
				if err := proto.Unmarshal(result.Contract.StoredContractBytes, stored); err != nil {
					t.Fatal(err)
				}
				if stored.TransferByteCount != uint64(opening) {
					t.Errorf("%s signed %d bytes, want %d", test.name, stored.TransferByteCount, opening)
				}
				if !connect.VerifyStoredContract(settings, key, result.Contract.StoredContractBytes, result.Contract.StoredContractHmac) {
					t.Errorf("%s did not sign the actual stored contract", test.name)
				}
				contractId, err := server.IdFromBytes(stored.ContractId)
				if err != nil {
					t.Fatal(err)
				}
				server.Db(ctx, func(conn server.PgConn) {
					var reserved model.ByteCount
					server.Raise(conn.QueryRow(ctx, `SELECT sum(balance_byte_count) FROM transfer_escrow WHERE contract_id = $1`, contractId).Scan(&reserved))
					if reserved != model.ByteCount(stored.TransferByteCount) {
						t.Errorf("%s signed %d bytes with only %d reserved", test.name, stored.TransferByteCount, reserved)
					}
				})
			}()
		}
	})
}
