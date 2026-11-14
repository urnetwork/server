package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
)

// §19.10's restore hold at CreateContract (plan-OPS-restore-hold; OPS-M3-03): while sidecar.yml holds, a request is
// answered Setup before any read, so this needs no store; the hold's cause is what the rejection counter carries.
func TestCreateContractAnswersSetupWhileRestoreHeld(t *testing.T) {
	defer server.Testing_PushSidecarConfig("sidecar:\n  restore_hold: true\n")()
	frames, err := CreateContract(
		context.Background(),
		server.NewId(),
		&protocol.CreateContract{DestinationId: server.NewId().Bytes(), TransferByteCount: 1024},
		connect.DefaultContractManagerSettings(),
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(frames), 1)
	message, err := connect.FromFrame(frames[0])
	connect.AssertEqual(t, err, nil)
	result, ok := message.(*protocol.CreateContractResult)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, result.Error != nil, true)
	connect.AssertEqual(t, *result.Error, protocol.ContractError_Setup)
	connect.AssertEqual(t, result.Contract == nil, true)
}
