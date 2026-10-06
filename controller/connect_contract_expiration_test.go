// The signed control response and the accounting row share one absolute deadline.
package controller

import (
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Both legacy and current request shapes receive the persisted 60 minute
// deadline inside the provider HMAC. Removing or changing it breaks the proof.
func TestCreateContractSignsPersistedExpiration(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		_, providerId, payerNetworkId, payerId := companionOriginTestSetup(ctx, t)
		peerId := server.NewId()
		model.Testing_CreateDevice(ctx, payerNetworkId, server.NewId(), peerId, "expiration peer", "test")
		secret := []byte("synthetic-expiration-provider-key")
		model.SetProvide(ctx, providerId, map[model.ProvideMode][]byte{model.ProvideModePublic: secret})
		model.SetProvide(ctx, payerId, map[model.ProvideMode][]byte{model.ProvideModeStream: secret})
		model.SetProvide(ctx, peerId, map[model.ProvideMode][]byte{model.ProvideModeNetwork: secret})
		settings := connect.DefaultContractManagerSettings()
		version := uint32(connect.DefaultStreamVersion)
		for _, test := range []struct {
			name                string
			source, destination server.Id
			companion           bool
			streamVersion       *uint32
		}{
			{name: "legacy public", source: payerId, destination: providerId},
			{name: "current public", source: payerId, destination: providerId, streamVersion: &version},
			{name: "companion", source: providerId, destination: payerId, companion: true, streamVersion: &version},
			{name: "network", source: payerId, destination: peerId, streamVersion: &version},
		} {
			request := &protocol.CreateContract{DestinationId: test.destination.Bytes(), TransferByteCount: uint64(MinContractTransferByteCount),
				Companion: test.companion, StreamVersion: test.streamVersion}
			frames, err := CreateContract(ctx, test.source, request, settings)
			if err != nil || len(frames) != 1 {
				t.Fatalf("%s create: frames=%d err=%v", test.name, len(frames), err)
			}
			func() {
				defer returnConnectControlFrames(frames)
				message, err := connect.FromFrame(frames[0])
				if err != nil {
					t.Fatal(err)
				}
				result, ok := message.(*protocol.CreateContractResult)
				if !ok || result.Error != nil || result.Contract == nil {
					t.Fatalf("%s missing successful response", test.name)
				}
				if (result.CreateContract != nil) != (test.streamVersion != nil) {
					t.Fatal("legacy result echo shape changed")
				}
				var stored protocol.StoredContract
				if err := proto.Unmarshal(result.Contract.StoredContractBytes, &stored); err != nil {
					t.Fatal(err)
				}
				if stored.ExpirationTimeUnixMilli == nil {
					t.Fatalf("%s deadline absent from signed contract", test.name)
				}
				id, err := server.IdFromBytes(stored.ContractId)
				if err != nil {
					t.Fatal(err)
				}
				var created, persisted time.Time
				server.Db(ctx, func(conn server.PgConn) {
					server.Raise(conn.QueryRow(ctx, `SELECT create_time,expiration_time FROM transfer_contract WHERE contract_id=$1`, id).Scan(&created, &persisted))
				})
				if !persisted.Equal(time.UnixMilli(*stored.ExpirationTimeUnixMilli)) || persisted.Sub(created) != 60*time.Minute {
					t.Fatalf("%s signed deadline differs from 60 minute persisted deadline", test.name)
				}
				if !connect.VerifyStoredContract(settings, secret, result.Contract.StoredContractBytes, result.Contract.StoredContractHmac) {
					t.Fatal("expiration response signature failed")
				}
				stored.ExpirationTimeUnixMilli = nil
				withoutDeadline, err := proto.Marshal(&stored)
				if err != nil {
					t.Fatal(err)
				}
				if connect.VerifyStoredContract(settings, secret, withoutDeadline, result.Contract.StoredContractHmac) {
					t.Fatal("deadline was not covered by the provider HMAC")
				}
			}()
		}
	})
}
