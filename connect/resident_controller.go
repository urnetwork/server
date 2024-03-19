package main

import (
	"context"
	// "sync"

	"crypto/hmac"
	"crypto/sha256"

	"google.golang.org/protobuf/proto"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/connect"
	"bringyour.com/protocol"
)


type residentController struct {
	ctx context.Context
    cancel context.CancelFunc

    clientId bringyour.Id
    client *connect.Client

	residentContractManager *residentContractManager
	settings *ExchangeSettings
}

func newResidentController(
	ctx context.Context,
    cancel context.CancelFunc,
    clientId bringyour.Id,
    client *connect.Client,
    residentContractManager *residentContractManager,
    settings *ExchangeSettings,
) *residentController {
	return &residentController{
		ctx: ctx,
		cancel: cancel,
		clientId: clientId,
		client: client,
		residentContractManager: residentContractManager,
		settings: settings,
	}
}

	
// the message is verified from source `clientId`
func (self *residentController) HandleControlMessage(message any) {

	switch v := message.(type) {
	case *protocol.Provide:
		self.handleProvide(v)

	case *protocol.CreateContract:
		self.handleCreateContract(v)

	case *protocol.CloseContract:
		self.handleCloseContract(v)
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

func (self *residentController) handleCreateContract(createContract *protocol.CreateContract) {
	bringyour.Logger().Printf("CONTROL CREATE CONTRACT (companion=%t)\n", createContract.Companion)


	destinationId := bringyour.Id(createContract.DestinationId)
	var relationship model.ProvideMode

	if createContract.Companion {

		// companion contracts use `ProvideModeStream`
 		relationship = model.ProvideModeStream

	} else {


		minRelationship := self.residentContractManager.GetProvideRelationship(self.clientId, destinationId)

		maxProvideMode := self.residentContractManager.GetProvideMode(destinationId)
		if maxProvideMode < minRelationship {
			bringyour.Logger().Printf("CONTROL CREATE CONTRACT ERROR NO PERMISSION\n")
			contractError := protocol.ContractError_NoPermission
			result := &protocol.CreateContractResult{
				Error: &contractError,
			}
			frame, err := connect.ToFrame(result)
			bringyour.Raise(err)
			self.client.Send(frame, connect.Id(self.clientId), nil)
			return
		}

		relationship = minRelationship
	}

	provideSecretKey, err := model.GetProvideSecretKey(self.ctx, destinationId, relationship)
	if err != nil {
		bringyour.Logger().Printf("CONTROL CREATE CONTRACT ERROR NO SECRET KEY\n")
		contractError := protocol.ContractError_NoPermission
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame, err := connect.ToFrame(result)
		bringyour.Raise(err)
		self.client.Send(frame, connect.Id(self.clientId), nil)
		return
	}

	// if `relationship < Public`, use CreateContractNoEscrow
	// else use CreateTransferEscrow or CreateCompanionTransferEscrow
	contractId, contractByteCount, err := self.residentContractManager.CreateContract(
		self.clientId,
		destinationId,
		// companion contracts reply to an existing open contract
		createContract.Companion,
		ByteCount(createContract.TransferByteCount),
		relationship,
	)
	bringyour.Logger().Printf("CONTROL CREATE CONTRACT TRANSFER BYTE COUNT %d %d %d\n", ByteCount(createContract.TransferByteCount), contractByteCount, uint64(contractByteCount))

	if err != nil {
		bringyour.Logger().Printf("CONTROL CREATE CONTRACT ERROR: %s\n", err)
		contractError := protocol.ContractError_InsufficientBalance
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame, err := connect.ToFrame(result)
		bringyour.Raise(err)
		self.client.Send(frame, connect.Id(self.clientId), nil)
		return
	}

	storedContract := &protocol.StoredContract{
		ContractId: contractId.Bytes(),
		TransferByteCount: uint64(contractByteCount),
		SourceId: self.clientId.Bytes(),
		DestinationId: destinationId.Bytes(),
	}
	storedContractBytes, err := proto.Marshal(storedContract)
	if err != nil {
		bringyour.Logger().Printf("CONTROL CREATE CONTRACT STORED ERROR\n")
		contractError := protocol.ContractError_Setup
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame, err := connect.ToFrame(result)
		bringyour.Raise(err)
		self.client.Send(frame, connect.Id(self.clientId), nil)
		return
	}
	mac := hmac.New(sha256.New, provideSecretKey)
	storedContractHmac := mac.Sum(storedContractBytes)

	result := &protocol.CreateContractResult{
		Contract: &protocol.Contract{
			StoredContractBytes: storedContractBytes,
			StoredContractHmac: storedContractHmac,
			ProvideMode: protocol.ProvideMode(relationship),
		},
	}
	frame, err := connect.ToFrame(result)
	bringyour.Raise(err)
	self.client.Send(frame, connect.Id(self.clientId), nil)
	bringyour.Logger().Printf("CONTROL CREATE CONTRACT SENT\n")
}

func (self *residentController) handleCloseContract(closeContract *protocol.CloseContract) {
	self.residentContractManager.CloseContract(
		bringyour.RequireIdFromBytes(closeContract.ContractId),
		self.clientId,
		ByteCount(closeContract.AckedByteCount),
	)
}