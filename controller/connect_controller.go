package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog"

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
		connect.DefaultContractManagerSettings(),
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
	contractManagerSettings *connect.ContractManagerSettings,
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
			outFrames, err = CreateContract(ctx, clientId, v, contractManagerSettings)
		case *protocol.CloseContract:
			err = CloseContract(ctx, clientId, v)
		case *protocol.Provide:
			err = Provide(ctx, clientId, v)
		case *protocol.EncryptedKey:
			err = SetEncryptedKey(ctx, clientId, v)
		case *protocol.ClientKey:
			err = SetClientKey(ctx, clientId, v)

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
	contractManagerSettings *connect.ContractManagerSettings,
) ([]*protocol.Frame, error) {
	// server.Logger().Printf("CONTROL CREATE CONTRACT (companion=%t)\n", createContract.Companion)

	destinationId := server.RequireIdFromBytes(createContract.DestinationId)
	var provideMode model.ProvideMode

	// V(2) diagnostic: log every contract request up front, including companion
	// requests that get rejected below (those never reach the success-path
	// [contract][cert] log). In symmetric mode we expect companion=false here.
	glog.V(2).Infof("[contract][req]%s->%s companion=%t\n", clientId, destinationId, createContract.Companion)

	if createContract.Companion {

		// companion contracts use `ProvideModeStream`
		provideMode = model.ProvideModeStream

	} else {
		provideRelationship := GetProvideRelationship(ctx, clientId, destinationId)

		if provideModes := GetProvideModes(ctx, destinationId); !provideModes[provideRelationship] {
			glog.V(2).Infof("[contract][reject]%s->%s no-permission (companion=%t relationship=%d)\n", clientId, destinationId, createContract.Companion, provideRelationship)
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
		// A companion request in symmetric mode lands here: provideMode=Stream(4)
		// has no secret key because the destination never provided Stream.
		glog.V(2).Infof("[contract][reject]%s->%s no-secret-key (companion=%t provideMode=%d err=%v)\n", clientId, destinationId, createContract.Companion, provideMode, err)
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

	// look up the destination's published TLS certificate chain (if any)
	// so the sender can verify it during the per-peer TLS handshake. The
	// certificate is keyed on `client_id` only (independent of provide
	// mode) — every encryption-enabled client publishes exactly one cert
	// via `EncryptedKey`, and that cert is the receiver-role identity for
	// every contract whose destination is this client. A nil chain means
	// the destination did not publish a cert; senders accept any cert
	// (skip verification) in that case — see Contract proto.
	//
	// Alongside the cert chain, return the destination's signature over
	// the chain by its long-lived client identity key
	// (`Contract.destination_client_key_signed_tls_certificate`) and the
	// destination's public client identity key
	// (`Contract.destination_client_public_key`). The sender uses these
	// for the long-lived-identity verification path (Option 1 + Option
	// 4): the public key value attached here is the platform's claim
	// about the destination's identity; the sender SHOULD cross-check
	// it against the unauthenticated `/key/<client_id>` lookup before
	// trusting it (defeats a MITM platform that substitutes both the
	// cert and the verifying key in lockstep).
	var provideTlsCertificatePem []byte
	var clientKeySignedTlsCertificate []byte
	var destinationClientPublicKey []byte

	var wg sync.WaitGroup
	wg.Add(2)
	go server.HandleError(func() {
		defer wg.Done()
		certPem, sig, err := model.GetClientTlsCertificateAndSignature(ctx, destinationId)
		if err == nil {
			provideTlsCertificatePem = certPem
			clientKeySignedTlsCertificate = sig
		}
	})
	go server.HandleError(func() {
		defer wg.Done()
		pub, err := model.GetClientPublicKey(ctx, destinationId)
		if err == nil {
			destinationClientPublicKey = pub
		}
	})
	wg.Wait()

	provideTlsCertificate := splitPemBlocks(provideTlsCertificatePem)

	// V(2) diagnostic: verify (a) no companion contracts are created in
	// symmetric mode, and (b) whether a destination TLS cert is attached to the
	// contract — the attached cert is what arms sender-side cert verification.
	glog.V(2).Infof(
		"[contract][cert]%s->%s companion=%t provideMode=%s certBlocks=%d certPemLen=%d clientKeySig=%d pubKey=%d\n",
		clientId, destinationId, createContract.Companion, provideMode,
		len(provideTlsCertificate), len(provideTlsCertificatePem),
		len(clientKeySignedTlsCertificate), len(destinationClientPublicKey),
	)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	contractId, transferByteCount, priority, streamId, err := nextContract(ctx, clientId, createContract, provideMode)
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
		ContractId:                               contractId.Bytes(),
		TransferByteCount:                        uint64(transferByteCount),
		SourceId:                                 clientId.Bytes(),
		DestinationId:                            destinationId.Bytes(),
		Priority:                                 &priority,
		ProvideTlsCertificate:                    provideTlsCertificate,
		DestinationClientPublicKey:               destinationClientPublicKey,
		DestinationClientKeySignedTlsCertificate: clientKeySignedTlsCertificate,
	}
	if streamId != nil {
		storedContract.StreamId = streamId.Bytes()
	}
	storedContractBytes, _ := proto.Marshal(storedContract)

	storedContractHmac := connect.SignStoredContract(contractManagerSettings, provideSecretKey, storedContractBytes)

	result := &protocol.CreateContractResult{
		Contract: &protocol.Contract{
			StoredContractBytes:                      storedContractBytes,
			StoredContractHmac:                       storedContractHmac,
			ProvideMode:                              protocol.ProvideMode(provideMode),
			ProvideTlsCertificate:                    provideTlsCertificate,
			DestinationClientPublicKey:               destinationClientPublicKey,
			DestinationClientKeySignedTlsCertificate: clientKeySignedTlsCertificate,
		},
	}
	streamVersion := 0
	if createContract.StreamVersion != nil {
		streamVersion = int(*createContract.StreamVersion)
	}
	switch streamVersion {
	case 0:
		// result CreateContract is unset
	default:
		result.CreateContract = createContract
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
) (server.Id, model.ByteCount, model.Priority, *server.Id, error) {
	destinationId := server.Id(createContract.DestinationId)

	/*
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
	*/

	var intermediaryIds []server.Id
	for _, intermediaryIdBytes := range createContract.IntermediaryIds {
		intermediaryId := server.Id(intermediaryIdBytes)
		intermediaryIds = append(intermediaryIds, intermediaryId)
	}

	forceStream := false
	if createContract.ForceStream != nil {
		forceStream = *createContract.ForceStream
	}
	streamVersion := 0
	if createContract.StreamVersion != nil {
		streamVersion = int(*createContract.StreamVersion)
	}
	// new contract
	return newContract(
		ctx,
		clientId,
		destinationId,
		intermediaryIds,
		// companion contracts reply to an existing open contract
		createContract.Companion,
		model.ByteCount(createContract.TransferByteCount),
		provideMode,
		forceStream,
		streamVersion,
	)
}

func newContract(
	ctx context.Context,
	sourceId server.Id,
	destinationId server.Id,
	intermediaryIds []server.Id,
	companionContract bool,
	transferByteCount model.ByteCount,
	provideMode model.ProvideMode,
	forceStream bool,
	streamVersion int,
) (contractId server.Id, contractTransferByteCount model.ByteCount, priority model.Priority, streamId *server.Id, returnErr error) {
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
	) * model.ByteCount(len(intermediaryIds)+1)

	if provideMode == model.ProvideModeNetwork || provideMode == model.ProvideModeFriendsAndFamily {
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

		switch streamVersion {
		case 0:
			// force stream is not supported
		default:
			if forceStream || 0 < len(intermediaryIds) {
				streamId_ := model.AddToStream(ctx, contractId, sourceId, destinationId, intermediaryIds)
				streamId = &streamId_
			}
		}
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

		switch streamVersion {
		case 0:
			// companion stream is not supported
		default:
			companionContractId := *escrow.CompanionContractId
			streamId_, _, ok := model.GetStream(ctx, companionContractId)
			if ok {
				streamId = &streamId_
			}
		}
	} else {
		// TODO store the intermediary ids on the contract so they can be rewarded in the payout
		// TODO the transfer should be equally divided amongst all the hops

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

		switch streamVersion {
		case 0:
			// force stream is not supported
		default:
			if forceStream || 0 < len(intermediaryIds) {
				streamId_ := model.AddToStream(ctx, contractId, sourceId, destinationId, intermediaryIds)
				streamId = &streamId_
			}
		}
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
	model.SetProvide(ctx, clientId, secretKeys)
	return nil
}

// SetEncryptedKey stores the client's published TLS certificate chain
// — and the client's signature over that chain by its long-lived
// identity key — so the platform can attach both to every contract
// whose destination is this client. Certificates are keyed on
// `client_id` only — a single cert per client regardless of provide
// mode. An empty chain clears the stored cert (e.g., when a client
// opts out of publishing).
//
// The signature is the value the receiving sender will verify against
// the destination's public client key (fetched out-of-band) before
// admitting the cert chain to the per-peer session's trusted set. If
// the publishing client doesn't include a signature, the chain is
// still stored but the signature column is left null.
func SetEncryptedKey(
	ctx context.Context,
	clientId server.Id,
	encryptedKey *protocol.EncryptedKey,
) error {
	for i, block := range encryptedKey.ProvideTlsCertificate {
		p, _ := pem.Decode(block)
		if p == nil {
			return fmt.Errorf("Invalid PEM in certificate chain at index %d", i)
		}
		if _, err := x509.ParseCertificate(p.Bytes); err != nil {
			return fmt.Errorf("Invalid X.509 certificate in chain at index %d: %w", i, err)
		}
	}
	tlsCertificatePem := concatenatePemBlocks(encryptedKey.ProvideTlsCertificate)
	model.SetClientTlsCertificateWithSignature(
		ctx,
		clientId,
		tlsCertificatePem,
		encryptedKey.ClientKeySignedTlsCertificate,
	)
	return nil
}

// SetClientKey stores the client's published long-lived public client
// identity key (Ed25519, 32 bytes). Keyed on `client_id`; rotation
// overwrites. The value is served by the unauthenticated
// `/key/<client_id>` API and is also attached to every contract whose
// destination is this client. An empty / nil key clears the stored
// value.
func SetClientKey(
	ctx context.Context,
	clientId server.Id,
	clientKey *protocol.ClientKey,
) error {
	if len(clientKey.PublicKey) != 0 && len(clientKey.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("Invalid client public key length: %d (expected %d)", len(clientKey.PublicKey), ed25519.PublicKeySize)
	}
	model.SetClientPublicKey(ctx, clientId, clientKey.PublicKey)
	return nil
}

// GetClientKeyArgs / GetClientKeyResult / GetClientKey back the
// unauthenticated `GET /key/<client_id>` route. The handler URL-routes
// the client id into `ClientId`; the controller looks it up in the
// `client_key` table. A client that has never published a key returns
// `{"public_key": null}` with HTTP 200, so callers can distinguish
// "not yet published" from a network error without parsing status
// codes.
type GetClientKeyArgs struct {
	ClientId server.Id `json:"client_id"`
}

type GetClientKeyResult struct {
	PublicKey []byte `json:"public_key"`
}

func GetClientKey(
	args *GetClientKeyArgs,
	clientSession *session.ClientSession,
) (*GetClientKeyResult, error) {
	pub, err := model.GetClientPublicKey(clientSession.Ctx, args.ClientId)
	if err != nil {
		return nil, err
	}
	return &GetClientKeyResult{
		PublicKey: pub,
	}, nil
}

// concatenatePemBlocks joins each PEM block in the wire-level chain into one
// byte slice. PEM is designed to be concatenable: tools like x509.ParseCertificate
// loop on `pem.Decode` and re-extract each block in order. Returns nil when
// no chain is provided.
func concatenatePemBlocks(chain [][]byte) []byte {
	if len(chain) == 0 {
		return nil
	}
	total := 0
	for _, block := range chain {
		total += len(block)
	}
	out := make([]byte, 0, total)
	for _, block := range chain {
		out = append(out, block...)
	}
	return out
}

// splitPemBlocks is the inverse of concatenatePemBlocks. Given a concatenated
// PEM bytes blob, returns the individual blocks (one entry per
// `-----BEGIN CERTIFICATE-----` ... `-----END CERTIFICATE-----`). Returns nil
// when the blob is empty or contains no PEM blocks.
func splitPemBlocks(blob []byte) [][]byte {
	if len(blob) == 0 {
		return nil
	}
	var out [][]byte
	rest := blob
	for len(rest) > 0 {
		block, next := pem.Decode(rest)
		if block == nil {
			break
		}
		out = append(out, pem.EncodeToMemory(block))
		rest = next
	}
	return out
}

func CloseContract(
	ctx context.Context,
	clientId server.Id,
	closeContract *protocol.CloseContract,
) error {
	contractId := server.RequireIdFromBytes(closeContract.ContractId)
	const maxByteCount = uint64(1<<63 - 1)
	if maxByteCount < closeContract.AckedByteCount {
		return fmt.Errorf("Invalid acked byte count %d (max %d)", closeContract.AckedByteCount, maxByteCount)
	}
	usedTransferByteCount := model.ByteCount(closeContract.AckedByteCount)
	checkpoint := closeContract.Checkpoint

	err := model.CloseContract(ctx, contractId, clientId, usedTransferByteCount, checkpoint)
	return err
}
