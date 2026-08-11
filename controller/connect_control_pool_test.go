// This file verifies ownership of pooled response frames at the HTTP control
// serialization boundary.
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

// Serializing a controller response returns its original pooled frame bytes.
func TestConnectControlReturnsResponseFrameOwnership(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		sourceDeviceId := server.NewId()
		sourceId := server.NewId()
		destinationId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "control pool", userId)
		model.Testing_CreateDevice(ctx, networkId, sourceDeviceId, sourceId, "source", "source")
		model.Testing_CreateDevice(
			ctx,
			networkId,
			server.NewId(),
			destinationId,
			"destination",
			"destination",
		)
		byJwt := jwt.NewByJwt(
			networkId,
			userId,
			"control pool",
			false,
			false,
		).Client(sourceDeviceId, sourceId)
		clientSession := session.Testing_CreateClientSession(ctx, byJwt)
		defer clientSession.Cancel()

		requestFrame, err := connect.ToFrame(
			&protocol.CreateContract{
				DestinationId:     destinationId.Bytes(),
				TransferByteCount: 1024,
			},
			connect.DefaultProtocolVersion,
		)
		if err != nil {
			t.Fatal(err)
		}
		requestBytes, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{requestFrame}})
		connect.MessagePoolReturn(requestFrame.MessageBytes)
		if err != nil {
			t.Fatal(err)
		}
		var responseWitnesses [][]byte
		result, err := connectControlObserved(
			&ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(requestBytes)},
			clientSession,
			func(responseFrames []*protocol.Frame) {
				for _, responseFrame := range responseFrames {
					responseWitnesses = append(
						responseWitnesses,
						connect.MessagePoolShareReadOnly(responseFrame.MessageBytes),
					)
				}
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		resultBytes, err := base64.StdEncoding.DecodeString(result.Pack)
		if err != nil {
			t.Fatal(err)
		}
		resultPack := &protocol.Pack{}
		if err := proto.Unmarshal(resultBytes, resultPack); err != nil {
			t.Fatal(err)
		}
		if len(resultPack.Frames) != 1 {
			t.Fatalf("control response frame count=%d, want 1", len(resultPack.Frames))
		}
		if len(responseWitnesses) != 1 {
			t.Fatalf("control response ownership witness count=%d, want 1", len(responseWitnesses))
		}
		if !connect.MessagePoolReturn(responseWitnesses[0]) {
			t.Fatal("serialized control response owner outlived HTTP control return")
		}
	})
}
