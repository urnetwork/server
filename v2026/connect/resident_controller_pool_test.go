// This file verifies pooled response ownership when a resident deliberately
// drops controller replies that cannot travel on the in-band control path.
package connect

import (
	"context"
	"testing"

	clientconnect "github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// Handling an in-band request returns the pooled bytes for its dropped reply.
func TestResidentControllerReturnsDroppedResponseFrameOwnership(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		userId := server.NewId()
		sourceId := server.NewId()
		destinationId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "resident pool", userId)
		model.Testing_CreateDevice(
			ctx,
			networkId,
			server.NewId(),
			sourceId,
			"source",
			"source",
		)
		model.Testing_CreateDevice(
			ctx,
			networkId,
			server.NewId(),
			destinationId,
			"destination",
			"destination",
		)

		requestFrame, err := clientconnect.ToFrame(
			&protocol.CreateContract{
				DestinationId:     destinationId.Bytes(),
				TransferByteCount: 1024,
			},
			clientconnect.DefaultProtocolVersion,
		)
		if err != nil {
			t.Fatal(err)
		}
		defer clientconnect.MessagePoolReturn(requestFrame.MessageBytes)

		settings := DefaultExchangeSettings()
		func() {
			probeFrames, probeErr := controller.ConnectControlFrames(
				ctx,
				sourceId,
				[]*protocol.Frame{requestFrame},
				settings.ContractManagerSettings,
			)
			defer func() {
				for _, probeFrame := range probeFrames {
					clientconnect.MessagePoolReturn(probeFrame.MessageBytes)
				}
			}()
			if probeErr != nil {
				t.Fatal(probeErr)
			}
			if len(probeFrames) != 1 {
				t.Fatalf("controller response frame count=%d, want 1", len(probeFrames))
			}
		}()

		residentController := newResidentController(
			ctx,
			sourceId,
			nil,
			settings,
		)
		defer residentController.Close()
		responseWitnesses := make(chan []byte, 1)
		residentController.beforeDroppedResponseReturnForTest = func(responseBytes []byte) {
			responseWitness := clientconnect.MessagePoolShareReadOnly(responseBytes)
			select {
			case responseWitnesses <- responseWitness:
			default:
				clientconnect.MessagePoolReturn(responseWitness)
			}
		}
		if err := residentController.HandleControlFrames([]*protocol.Frame{requestFrame}); err != nil {
			t.Fatal(err)
		}
		select {
		case responseWitness := <-responseWitnesses:
			if !clientconnect.MessagePoolReturn(responseWitness) {
				t.Fatal("resident dropped-response owner outlived control handling")
			}
		default:
			t.Fatal("resident control did not expose its dropped response ownership")
		}
	})
}
