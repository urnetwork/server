package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
	"bringyour.com/connect"
	"bringyour.com/protocol"
)

var ControlId = bringyour.Id(connect.ControlId)

var MinContractTransferByteCount = func() model.ByteCount {
	settings := connect.DefaultClientSettings()
	return max(
		model.ByteCount(32*1024),
		settings.SendBufferSettings.MinMessageByteCount,
		settings.ReceiveBufferSettings.MinMessageByteCount,
	)
}()

// allow the return contract to be created for up to this timeout after the source contract was closed
var OriginContractTimeout = 15 * time.Second

type ConnectControlArgs struct {
	Pack string `json:"pack"`
}

type ConnectControlResult struct {
	Pack  string               `json:"pack"`
	Error *ConnectControlError `json:"error"`
}

type ConnectControlError struct {
	Message string `json:"message"`
}

// the message is verified from source `clientId`
func ConnectControl(
	connectControl *ConnectControlArgs,
	clientSession *session.ClientSession,
) (*ConnectControlResult, error) {
	packBytes, err := base64.StdEncoding.DecodeString(connectControl.Pack)
	if err != nil {
		return nil, err
	}

	pack := &protocol.Pack{}
	err = proto.Unmarshal(packBytes, pack)
	if err != nil {
		return nil, err
	}

	resultFrames, resultErr := ConnectControlFrames(
		clientSession.Ctx,
		*clientSession.ByJwt.ClientId,
		pack.Frames,
	)

	resultPack := &protocol.Pack{
		Frames: resultFrames,
	}
	resultPackBytes, err := proto.Marshal(resultPack)
	if err != nil {
		return nil, err
	}

	result := &ConnectControlResult{
		Pack: base64.StdEncoding.EncodeToString(resultPackBytes),
	}
	if resultErr != nil {
		result.Error = &ConnectControlError{
			Message: resultErr.Error(),
		}
	}
	return result, nil
}

func ConnectControlFrames(
	ctx context.Context,
	clientId bringyour.Id,
	frames []*protocol.Frame,
) ([]*protocol.Frame, error) {
	netOutFrames := []*protocol.Frame{}

	for _, frame := range frames {
		message, err := connect.FromFrame(frame)
		if err != nil {
			return netOutFrames, err
		}

		var outFrames []*protocol.Frame
		err = nil

		switch v := message.(type) {
		case *protocol.CreateContract:
			outFrames, err = CreateContract(ctx, clientId, v)
		case *protocol.CloseContract:
			err = CloseContract(ctx, clientId, v)
		case *protocol.Provide:
			err = Provide(ctx, clientId, v)

		default:
			err = fmt.Errorf("Cannot handle oob control message: %T", message)
		}

		if err != nil {
			return netOutFrames, err
		}
		if 0 < len(outFrames) {
			netOutFrames = append(netOutFrames, outFrames...)
		}
	}

	return netOutFrames, nil
}

func GetProvideMode(ctx context.Context, destinationId bringyour.Id) model.ProvideMode {

	if destinationId == ControlId {
		return model.ProvideModeNetwork
	}

	provideMode, err := model.GetProvideMode(ctx, destinationId)
	if err != nil {
		return model.ProvideModeNone
	}
	return provideMode
}

// this is the "min" or most specific relationship
func GetProvideRelationship(ctx context.Context, sourceId bringyour.Id, destinationId bringyour.Id) model.ProvideMode {
	if sourceId == ControlId || destinationId == ControlId {
		return model.ProvideModeNetwork
	}

	if sourceId == destinationId {
		return model.ProvideModeNetwork
	}

	if sourceClient := model.GetNetworkClient(ctx, sourceId); sourceClient != nil {
		if destinationClient := model.GetNetworkClient(ctx, destinationId); destinationClient != nil {
			if sourceClient.NetworkId == destinationClient.NetworkId {
				return model.ProvideModeNetwork
			}
		}
	}

	// TODO network and friends-and-family not implemented yet
	// FIXME these exist in the association model now, can be added

	return model.ProvideModePublic
}

func CreateContract(
	ctx context.Context,
	clientId bringyour.Id,
	createContract *protocol.CreateContract,
) ([]*protocol.Frame, error) {
	bringyour.Logger().Printf("CONTROL CREATE CONTRACT (companion=%t)\n", createContract.Companion)

	destinationId := bringyour.RequireIdFromBytes(createContract.DestinationId)
	var provideMode model.ProvideMode

	if createContract.Companion {

		// companion contracts use `ProvideModeStream`
		provideMode = model.ProvideModeStream

	} else {

		minRelationship := GetProvideRelationship(ctx, clientId, destinationId)

		maxProvideMode := GetProvideMode(ctx, destinationId)
		if maxProvideMode < minRelationship {
			bringyour.Logger().Printf("CONTROL CREATE CONTRACT ERROR NO PERMISSION (%s->%s)\n", clientId.String(), destinationId.String())
			contractError := protocol.ContractError_NoPermission
			result := &protocol.CreateContractResult{
				Error: &contractError,
			}
			frame := connect.RequireToFrame(result)
			// self.client.Send(frame, connect.Id(self.clientId), nil)
			return []*protocol.Frame{frame}, nil
		}

		provideMode = minRelationship
	}

	provideSecretKey, err := model.GetProvideSecretKey(ctx, destinationId, provideMode)
	if err != nil {
		bringyour.Logger().Printf("CONTROL CREATE CONTRACT ERROR NO SECRET KEY\n")
		contractError := protocol.ContractError_NoPermission
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame := connect.RequireToFrame(result)
		// self.client.Send(frame, connect.Id(self.clientId), nil)
		return []*protocol.Frame{frame}, nil
	}

	contractId, transferByteCount, err := nextContract(ctx, clientId, createContract, provideMode)
	bringyour.Logger().Printf("CONTROL CREATE CONTRACT TRANSFER BYTE COUNT %d %d %d\n", model.ByteCount(createContract.TransferByteCount), transferByteCount, uint64(transferByteCount))

	if err != nil {
		bringyour.Logger().Printf("CONTROL CREATE CONTRACT ERROR: %s\n", err)
		contractError := protocol.ContractError_InsufficientBalance
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame := connect.RequireToFrame(result)
		// self.client.Send(frame, connect.Id(self.clientId), nil)
		return []*protocol.Frame{frame}, nil
	}

	storedContract := &protocol.StoredContract{
		ContractId:        contractId.Bytes(),
		TransferByteCount: uint64(transferByteCount),
		SourceId:          clientId.Bytes(),
		DestinationId:     destinationId.Bytes(),
	}
	storedContractBytes, _ := proto.Marshal(storedContract)

	mac := hmac.New(sha256.New, provideSecretKey)
	storedContractHmac := mac.Sum(storedContractBytes)

	result := &protocol.CreateContractResult{
		Contract: &protocol.Contract{
			StoredContractBytes: storedContractBytes,
			StoredContractHmac:  storedContractHmac,
			ProvideMode:         protocol.ProvideMode(provideMode),
		},
	}
	frame := connect.RequireToFrame(result)
	// self.client.Send(frame, connect.Id(self.clientId), nil)
	bringyour.Logger().Printf("CONTROL CREATE CONTRACT SENT\n")
	return []*protocol.Frame{frame}, nil
}

func nextContract(
	ctx context.Context,
	clientId bringyour.Id,
	createContract *protocol.CreateContract,
	provideMode model.ProvideMode,
) (bringyour.Id, model.ByteCount, error) {
	destinationId := bringyour.Id(createContract.DestinationId)

	// look for existing open contracts that the requestor does not have
	usedContractIds := map[bringyour.Id]bool{}
	for _, contractIdBytes := range createContract.UsedContractIds {
		if contractId, err := bringyour.IdFromBytes(contractIdBytes); err == nil {
			usedContractIds[contractId] = true
		}
	}
	contractIdTransferByteCounts := model.GetOpenTransferEscrowsOrderedByCreateTime(
		ctx,
		clientId,
		destinationId,
		model.ByteCount(createContract.TransferByteCount),
	)
	for contractId, transferByteCount := range contractIdTransferByteCounts {
		if !usedContractIds[contractId] {
			return contractId, transferByteCount, nil
		}
	}

	// new contract
	return newContract(
		ctx,
		clientId,
		destinationId,
		// companion contracts reply to an existing open contract
		createContract.Companion,
		model.ByteCount(createContract.TransferByteCount),
		provideMode,
	)
}

func newContract(
	ctx context.Context,
	sourceId bringyour.Id,
	destinationId bringyour.Id,
	companionContract bool,
	transferByteCount model.ByteCount,
	provideMode model.ProvideMode,
) (contractId bringyour.Id, contractTransferByteCount model.ByteCount, returnErr error) {
	sourceNetworkId, err := model.FindClientNetwork(ctx, sourceId)
	if err != nil {
		// the source is not a real client
		returnErr = err
		return
	}
	destinationNetworkId, err := model.FindClientNetwork(ctx, destinationId)
	if err != nil {
		// the destination is not a real client
		returnErr = err
		return
	}

	contractTransferByteCount = max(MinContractTransferByteCount, transferByteCount)

	if provideMode < model.ProvideModePublic {
		contractId, err = model.CreateContractNoEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferByteCount,
		)
		if err != nil {
			returnErr = err
			return
		}
	} else if companionContract {
		escrow, err := model.CreateCompanionTransferEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferByteCount,
			OriginContractTimeout,
		)
		if err != nil {
			returnErr = err
			return
		}
		contractId = escrow.ContractId
	} else {
		escrow, err := model.CreateTransferEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferByteCount,
		)
		if err != nil {
			returnErr = err
			return
		}
		contractId = escrow.ContractId
	}

	return
}

func Provide(
	ctx context.Context,
	clientId bringyour.Id,
	provide *protocol.Provide,
) error {
	secretKeys := map[model.ProvideMode][]byte{}
	for _, provideKey := range provide.Keys {
		secretKeys[model.ProvideMode(provideKey.Mode)] = provideKey.ProvideSecretKey
	}
	// bringyour.Logger().Printf("SET PROVIDE %s %v\n", sourceId.String(), secretKeys)
	model.SetProvide(ctx, clientId, secretKeys)
	// bringyour.Logger().Printf("SET PROVIDE COMPLETE %s %v\n", sourceId.String(), secretKeys)

	return nil
}

func CloseContract(
	ctx context.Context,
	clientId bringyour.Id,
	closeContract *protocol.CloseContract,
) error {
	contractId := bringyour.RequireIdFromBytes(closeContract.ContractId)
	usedTransferByteCount := model.ByteCount(closeContract.AckedByteCount)
	checkpoint := closeContract.Checkpoint

	err := model.CloseContract(ctx, contractId, clientId, usedTransferByteCount, checkpoint)
	return err
}
