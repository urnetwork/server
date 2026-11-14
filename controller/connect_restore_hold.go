package controller

// §19.10's restore hold at CreateContract (plan-OPS-restore-hold, QD-076; OPS-M3-03): while sidecar.yml's
// restore_hold reads true, every CreateContract answers ContractError.Setup before it reads a row, so no contract is
// created from the restored rows before the api step has re-applied every removal since the older dump. Setup is the
// code every client retries on its own backoff (contractResultError's header), and open contracts, streams and
// sockets are untouched. api's routes hold beside it (api/restore_hold.go).

import (
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
)

// The rejection counter's cause for a contract the hold refused.
const contractRestoreHoldCause = "restore_hold"

// Whether sidecar.yml holds CreateContract now.
func contractRestoreHeld() bool {
	return server.CurrentSidecarConfig().RestoreHold()
}

// The Setup answer a held CreateContract gives.
func contractRestoreHoldFrames() ([]*protocol.Frame, error) {
	contractError := protocol.ContractError_Setup
	frame, err := connect.ToFrame(&protocol.CreateContractResult{Error: &contractError}, connect.DefaultProtocolVersion)
	if err != nil {
		return nil, err
	}
	return []*protocol.Frame{frame}, nil
}
