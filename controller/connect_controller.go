package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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

// urnetwork_connect_transfer_bytes counts bytes transferred on the connect
// path, summed from the acked byte counts of closed and checkpointed transfer
// contracts (see CloseContract). the acked byte count reported at each
// checkpoint is incremental (the contract_close table accumulates it with
// used_transfer_byte_count + $3), so adding it on every successful close is the
// running total of transferred bytes. exported to grafana via the default
// prometheus registry (see server/grafana.go StartStatsPusher)
var transferByteCounter = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "transfer_bytes",
		Help:      "Bytes transferred on the connect path, summed from closed and checkpointed transfer contracts",
	},
)

var contractFailureCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "contract_failures_total",
		Help:      "Create-contract failures partitioned by a bounded cause class and companion mode",
	},
	[]string{"cause", "companion"},
)

var controlFrameFailureCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "control_frame_failures_total",
		Help:      "Control frame failures partitioned by message type and a bounded cause class",
	},
	[]string{"message", "cause"},
)

func init() {
	prometheus.MustRegister(transferByteCounter, contractFailureCounter, controlFrameFailureCounter)
}

// controlFrameMessageLabel maps a control message to a bounded metric label.
// The label may never be derived from the message contents: a client controls
// what it sends, and an unbounded label set is a cardinality attack.
func controlFrameMessageLabel(message any) string {
	switch message.(type) {
	case *protocol.CreateContract:
		return "create_contract"
	case *protocol.CloseContract:
		return "close_contract"
	case *protocol.Provide:
		return "provide"
	case *protocol.EncryptedKey:
		return "encrypted_key"
	case *protocol.ClientKey:
		return "client_key"
	case *protocol.ControlPing:
		return "control_ping"
	case *protocol.ProvidePing:
		return "provide_ping"
	default:
		return "other"
	}
}

// controlFrameErrorClass buckets a control frame failure. The benign,
// client-driven classes are the ones that dominate in normal operation:
// contract_already_closed is a routine ControlSync re-send under transport
// churn, and contract_not_found is its cousin after a reap.
func controlFrameErrorClass(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "already closed with outcome"):
		return "contract_already_closed"
	case strings.Contains(message, "contract not found"):
		return "contract_not_found"
	case strings.Contains(message, "is not a party to the contract"):
		return "not_a_party"
	case strings.Contains(message, "contract in dispute"):
		return "contract_in_dispute"
	case strings.Contains(message, "cannot handle oob control message"):
		return "unhandled_message"
	case strings.Contains(message, "panicked"):
		return "panic"
	default:
		return "other"
	}
}

// recordControlFrameFailure counts a control frame failure and emits its
// detail at V(1). Control frames are client-supplied, so a per-occurrence log
// line at the default level is a way for any client — or any resend loop — to
// spam the logs; the counter is the lossless signal. Watch the `other` class
// for causes that are not yet classified.
func recordControlFrameFailure(message any, err error) {
	cause := controlFrameErrorClass(err)
	messageLabel := controlFrameMessageLabel(message)
	controlFrameFailureCounter.WithLabelValues(messageLabel, cause).Inc()
	if glog.V(1) {
		glog.Infof("[control][error] message=%s class=%s err = %v\n", messageLabel, cause, err)
	}
}

func contractFailureClass(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "insufficient balance"):
		return "insufficient_balance"
	case strings.Contains(message, "missing origin contract for companion"):
		return "missing_companion_origin"
	case strings.Contains(message, "client does not exist"):
		return "client_not_found"
	default:
		return "other"
	}
}

func recordContractFailure(
	clientId server.Id,
	destinationId server.Id,
	companion bool,
	transferByteCount model.ByteCount,
	err error,
) {
	cause := contractFailureClass(err)
	companionLabel := fmt.Sprintf("%t", companion)
	contractFailureCounter.WithLabelValues(cause, companionLabel).Inc()

	// A contract failure is driven by client-supplied request state (a
	// companion request racing its origin, an exhausted balance, an unknown
	// client), so it is counted rather than logged: a per-occurrence line at
	// the default level lets any client write to the logs, and this path
	// exceeded 1,000/minute in normal operation. The counter is the lossless
	// rate signal; watch the `other` class for causes that are not yet
	// classified, and enable V(1) for the per-failure detail.
	if glog.V(1) {
		glog.Infof(
			"[contract][error] class=%s %s->%s companion=%t transferByteCount=%d err = %v\n",
			cause,
			clientId,
			destinationId,
			companion,
			transferByteCount,
			err,
		)
	}
}

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

// Controller response frames own their pooled payloads until a caller copies
// them into its transport or deliberately drops them.
func returnConnectControlFrames(frames []*protocol.Frame) {
	for _, frame := range frames {
		connect.MessagePoolReturn(frame.MessageBytes)
	}
}

// the message is verified from source `clientId`
func ConnectControl(
	connectControl *ConnectControlArgs,
	clientSession *session.ClientSession,
) (*ConnectControlResult, error) {
	return connectControlObserved(connectControl, clientSession, nil)
}

// Runs the HTTP control boundary with an optional borrowed response-frame
// observer used by deterministic ownership tests. Nil is a production no-op.
func connectControlObserved(
	connectControl *ConnectControlArgs,
	clientSession *session.ClientSession,
	observeResultFrames func([]*protocol.Frame),
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
	if observeResultFrames != nil {
		observeResultFrames(resultFrames)
	}
	// The response pack owns a serialized copy. Release the controller's
	// pooled frame payloads after every marshal or error return.
	defer returnConnectControlFrames(resultFrames)

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
	defer func() {
		if recovered := recover(); recovered != nil {
			// A propagated cancellation panic does not return accumulated replies
			// to the caller. Release replies from earlier frames first.
			returnConnectControlFrames(netOutFrames)
			panic(recovered)
		}
	}()

	// Control frames in one pack are independent operations: the send side
	// coalesces queued control messages (e.g. a burst of CloseContract syncs)
	// into one pack, and the transfer layer acks the pack on delivery, so a
	// frame skipped here is never retried by the sender. Aborting the batch on
	// the first error therefore silently discarded every later operation — one
	// benign duplicate close (e.g. "already closed" from a resent sync) leaked
	// the remaining contracts' closes until the straggler reaper. Process every
	// frame and report the joined errors.
	var errs []error

	for _, frame := range frames {
		message, err := connect.FromFrame(frame)
		if err != nil {
			// a frame that will not decode is client-supplied bytes; `nil`
			// takes the bounded `other` message label
			recordControlFrameFailure(nil, err)
			errs = append(errs, err)
			continue
		}

		var outFrames []*protocol.Frame
		err = nil

		// The model layer raises db failures as panics (server.Raise). Two
		// classes reach here:
		// - a canceled caller context: propagate to the caller's lifecycle
		//   boundary rather than classifying cancellation as a live frame error.
		//   Resident controllers keep their context live until their admitted
		//   Client callback has joined, so transport teardown does not cancel an
		//   in-progress resident operation.
		// - a live-ctx failure that survived the db layer's own transient
		//   retries: treat as this frame's error so one poison frame cannot
		//   kill its batch siblings or resend-loop forever, and so it is
		//   REPORTED (observed: silent bulk destination-party contract leaks
		//   under chaos churn before this was surfaced).
		func() {
			defer func() {
				if r := recover(); r != nil {
					if ctx.Err() != nil {
						panic(r)
					}
					err = fmt.Errorf("control frame %T panicked: %v", message, r)
				}
			}()
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

			case *protocol.ControlPing, *protocol.ProvidePing:
				// keep-alives: the transfer-level ack is the only response the
				// sender waits for, and receipt already refreshes resident
				// activity. Older clients ping the control destination
				// constantly, so treating these as unhandled floods the log.

			default:
				err = fmt.Errorf("Cannot handle oob control message: %T", message)
			}
		}()

		if err != nil {
			// A handler may produce partial replies before reporting an error.
			// This API does not return those frames, so release them here.
			returnConnectControlFrames(outFrames)
			recordControlFrameFailure(message, err)
			errs = append(errs, err)
			continue
		}
		if 0 < len(outFrames) {
			netOutFrames = append(netOutFrames, outFrames...)
		}
	}

	return netOutFrames, errors.Join(errs...)
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

// resolveNonCompanionProvideMode selects the provide mode a non-companion
// contract is settled under, given the source->destination provideRelationship
// and the modes the destination advertises (provideModes). It returns
// companion=true when it falls back to a companion Stream contract, and
// allowed=false when the destination advertises neither the relationship mode
// nor Stream (the caller then rejects with NoPermission).
//
// The Stream fallback preserves backward compatibility with older clients. Such
// a client registers only ProvideModeStream, so a same-network return contract
// (which the provider requests under ProvideModeNetwork) would be rejected here
// outright, silently blocking its return traffic. Settling it as a companion
// Stream contract — the return path used before the ProvideModeNetwork
// optimization — keeps those clients working.
func resolveNonCompanionProvideMode(
	provideRelationship model.ProvideMode,
	provideModes map[model.ProvideMode]bool,
) (provideMode model.ProvideMode, companion bool, allowed bool) {
	switch {
	case provideModes[provideRelationship]:
		return provideRelationship, false, true
	case provideModes[model.ProvideModeStream]:
		return model.ProvideModeStream, true, true
	default:
		return provideRelationship, false, false
	}
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

	// V(2) diagnostic: log every contract request up front, including the
	// companion requests rejected below (which never reach [contract][cert]).
	glog.V(2).Infof("[contract][req]%s->%s companion=%t\n", clientId, destinationId, createContract.Companion)

	// companion tracks whether this contract is settled as a companion (reply)
	// contract. It starts from the request flag but may also be set below when we
	// fall back to a companion contract because the destination does not advertise
	// the ideal relationship mode.
	companion := createContract.Companion

	if companion {
		// companion contracts use `ProvideModeStream`
		provideMode = model.ProvideModeStream

		// network peers never fall back to stream: when the companion reply
		// is same-network and the destination advertises the network mode,
		// settle it as a non-companion network contract (the no-escrow path,
		// same as the forward direction between network peers)
		if GetProvideRelationship(ctx, clientId, destinationId) == model.ProvideModeNetwork &&
			GetProvideModes(ctx, destinationId)[model.ProvideModeNetwork] {
			glog.V(2).Infof("[contract][network-normalize]%s->%s companion settled as network\n", clientId, destinationId)
			provideMode = model.ProvideModeNetwork
			companion = false
		}
	} else {
		provideRelationship := GetProvideRelationship(ctx, clientId, destinationId)
		provideModes := GetProvideModes(ctx, destinationId)

		var allowed bool
		provideMode, companion, allowed = resolveNonCompanionProvideMode(provideRelationship, provideModes)
		if !allowed {
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
		if companion {
			glog.V(2).Infof("[contract][companion-fallback]%s->%s relationship=%d not provided; using companion Stream\n", clientId, destinationId, provideRelationship)
		}
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

	// Attach the destination's published cert chain, the chain signature, and
	// its public client key to the contract. The sender verifies the cert during
	// the per-peer TLS handshake (nil chain → skip), and cross-checks the public
	// key against the unauthenticated `/key/<client_id>` lookup to defeat a
	// man-in-the-middle platform that swaps both cert and key in lockstep.
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

	// V(2) diagnostic: confirm no companion contracts in symmetric mode, and
	// whether a destination cert is attached (the cert arms sender-side verification).
	glog.V(2).Infof(
		"[contract][cert]%s->%s companion=%t provideMode=%s certBlocks=%d certPemLen=%d clientKeySig=%d pubKey=%d\n",
		clientId, destinationId, createContract.Companion, provideMode,
		len(provideTlsCertificate), len(provideTlsCertificatePem),
		len(clientKeySignedTlsCertificate), len(destinationClientPublicKey),
	)

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	contractId, transferByteCount, priority, streamId, err := nextContract(ctx, clientId, createContract, companion, provideMode, contractManagerSettings)
	// server.Logger().Printf("CONTROL CREATE CONTRACT TRANSFER BYTE COUNT %d %d %d\n", model.ByteCount(createContract.TransferByteCount), transferByteCount, uint64(transferByteCount))

	if err != nil {
		// The client sees only InsufficientBalance, including unrelated
		// failures. Preserve the cause as a lossless bounded metric and a
		// rate-limited default-visible exemplar.
		recordContractFailure(
			clientId,
			destinationId,
			createContract.Companion,
			model.ByteCount(createContract.TransferByteCount),
			err,
		)
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
	// the source's roles and principal are sealed into the signed contract
	// bytes only when the provide mode is network. For all other provide
	// modes they are not set.
	if provideMode == model.ProvideModeNetwork {
		if identity := model.GetClientIdentity(ctx, clientId); identity != nil {
			storedContract.Roles = identity.Roles
			storedContract.Principal = identity.Principal
		}
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

// CompanionOriginWaitTimeout bounds how long a companion contract request
// waits for its origin contract to appear before the miss is answered as
// terminal. The race window is milliseconds (see the retry loop's comment);
// the bound only exists so a genuinely one-sided request cannot hold the
// control frame open indefinitely.
const CompanionOriginWaitTimeout = 3 * time.Second

// CompanionOriginWaitPollTimeout is the poll interval inside that wait.
const CompanionOriginWaitPollTimeout = 100 * time.Millisecond

func nextContract(
	ctx context.Context,
	clientId server.Id,
	createContract *protocol.CreateContract,
	companion bool,
	provideMode model.ProvideMode,
	contractManagerSettings *connect.ContractManagerSettings,
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
		companion,
		model.ByteCount(createContract.TransferByteCount),
		provideMode,
		forceStream,
		streamVersion,
		contractManagerSettings,
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
	contractManagerSettings *connect.ContractManagerSettings,
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
			if 0 < len(intermediaryIds) {
				streamId_ := model.AddToStream(ctx, contractId, sourceId, destinationId, intermediaryIds)
				streamId = &streamId_
			} else {
				// When the pair has an active stream, every network contract
				// between the pair joins it. Check the active pair before using
				// force_stream to create a direct stream: a provider reply keeps
				// the receiver-visible force-stream lane but cannot reconstruct
				// the sender's local intermediary list. Creating a direct stream
				// in that case forks the two directions onto different stream ids.
				streamId_, ok := model.AddContractToPairStream(
					ctx,
					contractId,
					sourceId,
					destinationId,
				)
				if ok {
					streamId = &streamId_
				} else if forceStream {
					streamId_ = model.AddToStream(ctx, contractId, sourceId, destinationId, nil)
					streamId = &streamId_
				}
			}
		}
	} else if companionContract {
		// A companion request that beats its origin contract is an ordering
		// race, not a refusal: at cold start both peers bring sessions up
		// simultaneously and the encryption control carrier asks for its
		// companion contract at session setup, frequently milliseconds before
		// the peer's origin lands. The client cannot tell this apart from a
		// real failure (every cause reaches it as InsufficientBalance), so
		// answering the race with an error sent the client into its blind
		// 30s CreateContractTimeout retry loop — a full sequence starve per
		// occurrence (~12 per test-suite run), the residual behind the
		// chaos-family first-attempt timeouts and the multiclient
		// dead-on-arrival window clients. Waiting out the race here costs
		// sub-second latency in the worst observed case (origins land within
		// ~111-661ms of the losing request) and nothing when the origin
		// already exists.
		var escrow *model.TransferEscrow
		var err error
		deadline := time.Now().Add(CompanionOriginWaitTimeout)
		for {
			escrow, err = model.CreateCompanionTransferEscrow(
				ctx,
				sourceNetworkId,
				sourceId,
				destinationNetworkId,
				destinationId,
				contractTransferByteCount,
				contractManagerSettings.OriginContractLinger,
			)
			if !errors.Is(err, model.ErrMissingCompanionOrigin) {
				break
			}
			if deadline.Before(time.Now()) {
				// genuinely no origin (a one-sided companion request):
				// answer the terminal cause as before
				break
			}
			select {
			case <-ctx.Done():
				returnErr = ctx.Err()
				return
			case <-time.After(CompanionOriginWaitPollTimeout):
			}
		}
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
			// when the origin flow has an active stream, the companion must
			// carry the stream id — the receive sequence on the other side
			// inspects the contract to know the stream is active — and join
			// the stream so it stays open while the reply is in flight
			originContractId := *escrow.CompanionContractId
			streamId_, ok := model.AddCompanionContractToStream(
				ctx,
				contractId,
				originContractId,
				sourceId,
				destinationId,
			)
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

// SetEncryptedKey validates the published TLS cert chain and stores it with the
// client's signature over it (by its long-lived identity key). The platform
// attaches both to every contract destined for this client; the sender verifies
// the signature against the destination's public key before trusting the chain.
// An empty chain clears it; a nil signature is allowed (older clients).
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

// SetClientKey stores the client's published long-lived public identity key
// (Ed25519, 32 bytes), keyed on `client_id` (rotation overwrites). Served by
// the unauthenticated `/key/<client_id>` API and attached to every contract
// destined for this client. An empty/nil key clears it.
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

// GetClientKeyArgs / GetClientKeyResult / GetClientKey back the unauthenticated
// `GET /key/<client_id>` route. A client that has never published a key returns
// `{"public_key": null}` with HTTP 200, so callers can tell "not yet published"
// from a network error without parsing status codes.
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

// concatenatePemBlocks joins the wire-level PEM chain into one byte slice
// (`pem.Decode` re-extracts each block in order). Returns nil for an empty chain.
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

// splitPemBlocks is the inverse of concatenatePemBlocks: it splits a concatenated
// PEM blob back into its individual blocks. Returns nil when the blob has none.
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
	if err == nil {
		// the acked byte count is incremental per checkpoint, so this sums to
		// the total transferred bytes (matching the contract_close accumulation)
		transferByteCounter.Add(float64(usedTransferByteCount))
	}
	return err
}
