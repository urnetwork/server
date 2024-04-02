package main

import (
	"context"
	// "sync"
	// "errors"
	"fmt"

	// "crypto/hmac"
	// "crypto/sha256"

	// "google.golang.org/protobuf/proto"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	// "bringyour.com/connect"
	"bringyour.com/protocol"
)


type residentController struct {
	ctx context.Context
    cancel context.CancelFunc

    clientId bringyour.Id

	residentContractManager *residentContractManager
	settings *ExchangeSettings
}

func newResidentController(
	ctx context.Context,
    cancel context.CancelFunc,
    clientId bringyour.Id,
    residentContractManager *residentContractManager,
    settings *ExchangeSettings,
) *residentController {
	return &residentController{
		ctx: ctx,
		cancel: cancel,
		clientId: clientId,
		residentContractManager: residentContractManager,
		settings: settings,
	}
}


// the message is verified from source `clientId`
// control messages are not allowed to have replies
// messages with replies must use resident_oob_controller in the api
func (self *residentController) HandleControlMessage(message any) {
	switch v := message.(type) {
	case *protocol.Provide:
		self.handleProvide(v)

	case *protocol.CreateContractHole:
		self.handleCreateContractHole(v)

	case *protocol.CloseContract:
		self.handleCloseContract(v)

	default:
		fmt.Printf("[resident]Unknown control message: %T", message)
	}
}



func (self *residentController) handleProvide(provide *protocol.Provide) {
	secretKeys := map[model.ProvideMode][]byte{}			
	for _, provideKey := range provide.Keys {
		secretKeys[model.ProvideMode(provideKey.Mode)] = provideKey.ProvideSecretKey	
	}
	// bringyour.Logger().Printf("SET PROVIDE %s %v\n", sourceId.String(), secretKeys)
	model.SetProvide(self.ctx, self.clientId, secretKeys)
	// bringyour.Logger().Printf("SET PROVIDE COMPLETE %s %v\n", sourceId.String(), secretKeys)
}

func (self *residentController) handleCreateContractHole(createContractHole *protocol.CreateContractHole) {
	self.residentContractManager.CreateContractHole(
		bringyour.RequireIdFromBytes(createContractHole.DestinationId),
		bringyour.RequireIdFromBytes(createContractHole.ContractId),
	)
}


func (self *residentController) handleCloseContract(closeContract *protocol.CloseContract) {
	self.residentContractManager.CloseContract(
		bringyour.RequireIdFromBytes(closeContract.ContractId),
		ByteCount(closeContract.AckedByteCount),
	)
}