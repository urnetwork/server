package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	// "github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

var ControlId = server.Id(connect.ControlId)

var MinContractTransferByteCount = func() model.ByteCount {
	settings := connect.DefaultClientSettings()
	return max(
		settings.ContractManagerSettings.InitialContractTransferByteCount,
		settings.SendBufferSettings.MinMessageByteCount,
		settings.ReceiveBufferSettings.MinMessageByteCount,
	)
}()

var MaxContractTransferByteCount = func() model.ByteCount {
	settings := connect.DefaultClientSettings()
	return max(
		2 * settings.ContractManagerSettings.StandardContractTransferByteCount,
	)
}()

// allow the return contract to be created for up to this timeout after the source contract was closed
var OriginContractLinger = 300 * time.Second

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
	packBytes, err := connect.DecodeBase64(base64.StdEncoding, connectControl.Pack)
	if err != nil {
		return nil, err
	}
	defer connect.MessagePoolReturn(packBytes)

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
		Pack: connect.EncodeBase64(base64.StdEncoding, resultPackBytes),
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
	clientId server.Id,
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

func GetProvideModes(ctx context.Context, destinationId server.Id) map[model.ProvideMode]bool {

	if destinationId == ControlId {
		return map[model.ProvideMode]bool{
			model.ProvideModeNetwork: true,
		}
	}

	provideModes, err := model.GetProvideModes(ctx, destinationId)
	if err != nil {
		return map[model.ProvideMode]bool{}
	}
	return provideModes
}

// this is the "min" or most specific relationship
func GetProvideRelationship(ctx context.Context, sourceId server.Id, destinationId server.Id) model.ProvideMode {
	if sourceId == ControlId || destinationId == ControlId {
		return model.ProvideModeNetwork
	}

	return model.GetProvideRelationship(ctx, sourceId, destinationId)
}

func CreateContract(
	ctx context.Context,
	clientId server.Id,
	createContract *protocol.CreateContract,
) ([]*protocol.Frame, error) {
	// server.Logger().Printf("CONTROL CREATE CONTRACT (companion=%t)\n", createContract.Companion)

	destinationId := server.RequireIdFromBytes(createContract.DestinationId)
	var provideMode model.ProvideMode

	if createContract.Companion {

		// companion contracts use `ProvideModeStream`
		provideMode = model.ProvideModeStream

	} else {
		provideRelationship := GetProvideRelationship(ctx, clientId, destinationId)

		if provideModes := GetProvideModes(ctx, destinationId); !provideModes[provideRelationship] {
			// server.Logger().Printf("CONTROL CREATE CONTRACT ERROR NO PERMISSION (%s->%s)\n", clientId.String(), destinationId.String())
			contractError := protocol.ContractError_NoPermission
			result := &protocol.CreateContractResult{
				Error: &contractError,
			}
			frame, err := connect.ToFrame(result, connect.DefaultProtocolVersion)
			// self.client.Send(frame, connect.Id(self.clientId), nil)
			if err != nil {
				return nil, err
			}
			return []*protocol.Frame{frame}, nil
		}

		provideMode = provideRelationship
	}

	provideSecretKey, err := model.GetProvideSecretKey(ctx, destinationId, provideMode)
	if err != nil {
		// server.Logger().Printf("CONTROL CREATE CONTRACT ERROR NO SECRET KEY\n")
		contractError := protocol.ContractError_NoPermission
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame, err := connect.ToFrame(result, connect.DefaultProtocolVersion)
		if err != nil {
			return nil, err
		}
		// self.client.Send(frame, connect.Id(self.clientId), nil)
		return []*protocol.Frame{frame}, nil
	}

	contractId, transferByteCount, priority, err := nextContract(ctx, clientId, createContract, provideMode)
	// server.Logger().Printf("CONTROL CREATE CONTRACT TRANSFER BYTE COUNT %d %d %d\n", model.ByteCount(createContract.TransferByteCount), transferByteCount, uint64(transferByteCount))

	if err != nil {
		// server.Logger().Printf("CONTROL CREATE CONTRACT ERROR: %s\n", err)
		contractError := protocol.ContractError_InsufficientBalance
		result := &protocol.CreateContractResult{
			Error: &contractError,
		}
		frame, err := connect.ToFrame(result, connect.DefaultProtocolVersion)
		if err != nil {
			return nil, err
		}
		// self.client.Send(frame, connect.Id(self.clientId), nil)
		return []*protocol.Frame{frame}, nil
	}

	storedContract := &protocol.StoredContract{
		ContractId:        contractId.Bytes(),
		TransferByteCount: uint64(transferByteCount),
		SourceId:          clientId.Bytes(),
		DestinationId:     destinationId.Bytes(),
		Priority:          &priority,
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
	frame, err := connect.ToFrame(result, connect.DefaultProtocolVersion)
	if err != nil {
		return nil, err
	}
	// self.client.Send(frame, connect.Id(self.clientId), nil)
	// server.Logger().Printf("CONTROL CREATE CONTRACT SENT\n")
	return []*protocol.Frame{frame}, nil
}

func nextContract(
	ctx context.Context,
	clientId server.Id,
	createContract *protocol.CreateContract,
	provideMode model.ProvideMode,
) (server.Id, model.ByteCount, model.Priority, error) {
	destinationId := server.Id(createContract.DestinationId)

	if 0 < len(createContract.UsedContractIds) {
		// look for existing open contracts that the requestor does not have
		usedContractIds := map[server.Id]bool{}
		for _, contractIdBytes := range createContract.UsedContractIds {
			if contractId, err := server.IdFromBytes(contractIdBytes); err == nil {
				usedContractIds[contractId] = true
			}
		}
		escrows := model.GetOpenTransferEscrowsOrderedByPriorityCreateTime(
			ctx,
			clientId,
			destinationId,
			model.ByteCount(createContract.TransferByteCount),
		)
		for _, escrow := range escrows {
			if !usedContractIds[escrow.ContractId] {
				return escrow.ContractId, escrow.TransferByteCount, escrow.Priority, nil
			}
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
	sourceId server.Id,
	destinationId server.Id,
	companionContract bool,
	transferByteCount model.ByteCount,
	provideMode model.ProvideMode,
) (contractId server.Id, contractTransferByteCount model.ByteCount, priority model.Priority, returnErr error) {
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

	contractTransferByteCount = min(
		max(MinContractTransferByteCount, transferByteCount),
		MaxContractTransferByteCount,
	)

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
		priority = model.TrustedPriority
	} else if companionContract {
		escrow, err := model.CreateCompanionTransferEscrow(
			ctx,
			sourceNetworkId,
			sourceId,
			destinationNetworkId,
			destinationId,
			contractTransferByteCount,
			OriginContractLinger,
		)
		if err != nil {
			returnErr = err
			return
		}
		contractId = escrow.ContractId
		priority = escrow.Priority
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
		priority = escrow.Priority
	}

	return
}

func Provide(
	ctx context.Context,
	clientId server.Id,
	provide *protocol.Provide,
) error {
	secretKeys := map[model.ProvideMode][]byte{}
	for _, provideKey := range provide.Keys {
		secretKeys[model.ProvideMode(provideKey.Mode)] = provideKey.ProvideSecretKey
	}
	// server.Logger().Printf("SET PROVIDE %s %v\n", sourceId.String(), secretKeys)
	model.SetProvide(ctx, clientId, secretKeys)
	// server.Logger().Printf("SET PROVIDE COMPLETE %s %v\n", sourceId.String(), secretKeys)

	return nil
}

func CloseContract(
	ctx context.Context,
	clientId server.Id,
	closeContract *protocol.CloseContract,
) error {
	contractId := server.RequireIdFromBytes(closeContract.ContractId)
	usedTransferByteCount := model.ByteCount(closeContract.AckedByteCount)
	checkpoint := closeContract.Checkpoint

	err := model.CloseContract(ctx, contractId, clientId, usedTransferByteCount, checkpoint)
	return err
}
