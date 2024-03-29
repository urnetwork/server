package main

import (
	"context"
	// "sync"
	// "errors"
	"fmt"

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

	var replies []*protocol.Frame

	switch v := message.(type) {
	case *protocol.Provide:
		replies = self.handleProvide(v)

	case *protocol.CreateContract:
		replies = self.handleCreateContract(v)

	case *protocol.CloseContract:
		replies = self.handleCloseContract(v)
	}

	for _, frame := range replies {
		success := self.client.SendWithTimeout(
			frame,
			connect.Id(self.clientId),
			func(err error){},
			self.settings.WriteTimeout,
		)
		if success {
			fmt.Printf("CONTROLLER SENT TO CLIENT %s\n", self.clientId.String())
		} else {
			fmt.Printf("CONTROLLER COULD NOT SEND TO CLIENT %s\n", self.clientId.String())
		}
	}
}



func (self *residentController) handleProvide(provide *protocol.Provide) []*protocol.Frame {
	secretKeys := map[model.ProvideMode][]byte{}			
	for _, provideKey := range provide.Keys {
		secretKeys[model.ProvideMode(provideKey.Mode)] = provideKey.ProvideSecretKey	
	}
	// bringyour.Logger().Printf("SET PROVIDE %s %v\n", sourceId.String(), secretKeys)
	model.SetProvide(self.ctx, self.clientId, secretKeys)
	// bringyour.Logger().Printf("SET PROVIDE COMPLETE %s %v\n", sourceId.String(), secretKeys)

	return []*protocol.Frame{}
}

func (self *residentController) handleCreateContract(createContract *protocol.CreateContract) []*protocol.Frame {
	bringyour.Logger().Printf("CONTROL CREATE CONTRACT (companion=%t)\n", createContract.Companion)


	destinationId := bringyour.Id(createContract.DestinationId)
	var provideMode model.ProvideMode

	if createContract.Companion {

		// companion contracts use `ProvideModeStream`
 		provideMode = model.ProvideModeStream

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
			// self.client.Send(frame, connect.Id(self.clientId), nil)
			return []*protocol.Frame{frame}
		}

		provideMode = minRelationship
	}

	provideSecretKey, err := model.GetProvideSecretKey(self.ctx, destinationId, provideMode)
	if err != nil {
		bringyour.Logger().Printf("CONTROL CREATE CONTRACT ERROR NO SECRET KEY\n")
		contractError := protocol.ContractError_NoPermission
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame, err := connect.ToFrame(result)
		bringyour.Raise(err)
		// self.client.Send(frame, connect.Id(self.clientId), nil)
		return []*protocol.Frame{frame}
	}

	contractId, transferByteCount, err := self.nextContract(createContract, provideMode)
	bringyour.Logger().Printf("CONTROL CREATE CONTRACT TRANSFER BYTE COUNT %d %d %d\n", ByteCount(createContract.TransferByteCount), transferByteCount, uint64(transferByteCount))

	if err != nil {
		bringyour.Logger().Printf("CONTROL CREATE CONTRACT ERROR: %s\n", err)
		contractError := protocol.ContractError_InsufficientBalance
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame, err := connect.ToFrame(result)
		bringyour.Raise(err)
		// self.client.Send(frame, connect.Id(self.clientId), nil)
		return []*protocol.Frame{frame}
	}


	storedContract := &protocol.StoredContract{
		ContractId: contractId.Bytes(),
		TransferByteCount: uint64(transferByteCount),
		SourceId: self.clientId.Bytes(),
		DestinationId: destinationId.Bytes(),
	}
	storedContractBytes, _ := proto.Marshal(storedContract)
	
	mac := hmac.New(sha256.New, provideSecretKey)
	storedContractHmac := mac.Sum(storedContractBytes)

	result := &protocol.CreateContractResult{
		Contract: &protocol.Contract{
			StoredContractBytes: storedContractBytes,
			StoredContractHmac: storedContractHmac,
			ProvideMode: protocol.ProvideMode(provideMode),
		},
	}
	frame, err := connect.ToFrame(result)
	bringyour.Raise(err)
	// self.client.Send(frame, connect.Id(self.clientId), nil)
	bringyour.Logger().Printf("CONTROL CREATE CONTRACT SENT\n")
	return []*protocol.Frame{frame}
}

// FIXME test this
func (self *residentController) nextContract(createContract *protocol.CreateContract, provideMode model.ProvideMode) (bringyour.Id, ByteCount, error) {
	destinationId := bringyour.Id(createContract.DestinationId)

	// look for existing open contracts that the requestor does not have
	usedContractIds := map[bringyour.Id]bool{}
	for _, contractIdBytes := range createContract.UsedContractIds {
		if contractId, err := bringyour.IdFromBytes(contractIdBytes); err == nil {
			usedContractIds[contractId] = true
		}
	}
	contractIdTransferByteCounts := model.GetOpenTransferEscrowsOrderedByCreateTime(
		self.ctx,
		self.clientId,
		destinationId,
		ByteCount(createContract.TransferByteCount),
	)
	for contractId, transferByteCount := range contractIdTransferByteCounts {
		if !usedContractIds[contractId] {
			return contractId, transferByteCount, nil
		}
	}

	// new contract
	return self.residentContractManager.CreateContract(
		self.clientId,
		destinationId,
		// companion contracts reply to an existing open contract
		createContract.Companion,
		ByteCount(createContract.TransferByteCount),
		provideMode,
	)
}

func (self *residentController) handleCloseContract(closeContract *protocol.CloseContract) []*protocol.Frame {
	self.residentContractManager.CloseContract(
		bringyour.RequireIdFromBytes(closeContract.ContractId),
		self.clientId,
		ByteCount(closeContract.AckedByteCount),
	)

	return []*protocol.Frame{}
}