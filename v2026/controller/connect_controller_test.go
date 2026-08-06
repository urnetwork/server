package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestContractFailureClassIsBounded(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("Insufficient balance (0)."), "insufficient_balance"},
		{fmt.Errorf("Missing origin contract for companion."), "missing_companion_origin"},
		{fmt.Errorf("Client does not exist."), "client_not_found"},
		{fmt.Errorf("postgres unavailable"), "other"},
	}
	for _, test := range tests {
		if got := contractFailureClass(test.err); got != test.want {
			t.Fatalf("contractFailureClass(%q) = %q, want %q", test.err, got, test.want)
		}
	}
}

// TestResolveNonCompanionProvideMode covers the provide-mode selection for
// non-companion contract requests, in particular the backward-compatibility
// fallback: when the destination does not advertise the ideal relationship mode
// but does provide Stream (older clients register only Stream), the contract
// falls back to a companion Stream contract instead of being rejected with
// NoPermission — which previously left such clients with a wedged return path.
func TestResolveNonCompanionProvideMode(t *testing.T) {
	// Same-network destination advertising only Stream (older client): the ideal
	// mode (Network) is unavailable, so fall back to a companion Stream contract
	// rather than rejecting.
	provideMode, companion, allowed := resolveNonCompanionProvideMode(
		model.ProvideModeNetwork,
		map[model.ProvideMode]bool{model.ProvideModeStream: true},
	)
	connect.AssertEqual(t, allowed, true)
	connect.AssertEqual(t, companion, true)
	connect.AssertEqual(t, provideMode, model.ProvideModeStream)

	// Friends-and-family relationship, destination advertising only Stream: same
	// companion Stream fallback as the Network case (both are free NoEscrow modes
	// the older destination cannot advertise).
	provideMode, companion, allowed = resolveNonCompanionProvideMode(
		model.ProvideModeFriendsAndFamily,
		map[model.ProvideMode]bool{model.ProvideModeStream: true},
	)
	connect.AssertEqual(t, allowed, true)
	connect.AssertEqual(t, companion, true)
	connect.AssertEqual(t, provideMode, model.ProvideModeStream)

	// Destination advertises the ideal (Network) mode: use it directly, no
	// companion, no fallback.
	provideMode, companion, allowed = resolveNonCompanionProvideMode(
		model.ProvideModeNetwork,
		map[model.ProvideMode]bool{model.ProvideModeNetwork: true, model.ProvideModeStream: true},
	)
	connect.AssertEqual(t, allowed, true)
	connect.AssertEqual(t, companion, false)
	connect.AssertEqual(t, provideMode, model.ProvideModeNetwork)

	// Public relationship, destination advertises Public: use it directly.
	provideMode, companion, allowed = resolveNonCompanionProvideMode(
		model.ProvideModePublic,
		map[model.ProvideMode]bool{model.ProvideModePublic: true},
	)
	connect.AssertEqual(t, allowed, true)
	connect.AssertEqual(t, companion, false)
	connect.AssertEqual(t, provideMode, model.ProvideModePublic)

	// Destination advertises both the relationship mode and Stream: the
	// relationship mode wins (no unnecessary companion fallback).
	provideMode, companion, allowed = resolveNonCompanionProvideMode(
		model.ProvideModePublic,
		map[model.ProvideMode]bool{model.ProvideModePublic: true, model.ProvideModeStream: true},
	)
	connect.AssertEqual(t, allowed, true)
	connect.AssertEqual(t, companion, false)
	connect.AssertEqual(t, provideMode, model.ProvideModePublic)

	// Destination advertises neither the relationship mode nor Stream: not
	// allowed (caller rejects with NoPermission). The fallback must not
	// over-authorize.
	_, _, allowed = resolveNonCompanionProvideMode(
		model.ProvideModeNetwork,
		map[model.ProvideMode]bool{model.ProvideModePublic: true},
	)
	connect.AssertEqual(t, allowed, false)

	// Destination advertises nothing: not allowed.
	_, _, allowed = resolveNonCompanionProvideMode(
		model.ProvideModeNetwork,
		map[model.ProvideMode]bool{},
	)
	connect.AssertEqual(t, allowed, false)
}

// TestCreateContractCompanionFallback exercises controller.CreateContract
// end-to-end against the database. It verifies the same-network return path for
// an older destination client that registers only ProvideModeStream: the
// provider requests a non-companion return contract (which resolves to the
// ProvideModeNetwork relationship), and the server must fall back to a companion
// Stream contract rather than rejecting with NoPermission — otherwise the older
// client's return traffic is silently blocked.
func TestCreateContractCompanionFallback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		streamKey := []byte("test-provide-secret-key-stream00")
		networkKey := []byte("test-provide-secret-key-network0")
		publicKey := []byte("test-provide-secret-key-public00")

		// newClient creates a device + network_client row in networkId and returns
		// the client id, so FindClientNetwork and GetProvideRelationship resolve.
		newClient := func(networkId server.Id) server.Id {
			clientId := server.NewId()
			deviceId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "test", "test")
			return clientId
		}

		// newFundedNetwork creates a network and gives it transfer balance, so
		// companion escrows (whose payer is the destination network) can settle.
		newFundedNetwork := func() server.Id {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("test-%s", networkId), userId)
			// unique purchase event id: balance codes reject a reused
			// purchase event, including the empty one
			balanceCode, err := model.CreateBalanceCode(
				ctx,
				model.ByteCount(1024*1024*1024*1024),
				365*24*time.Hour,
				model.UsdToNanoCents(10.00),
				server.NewId().String(), "", "",
			)
			connect.AssertEqual(t, err, nil)
			_, err = model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
				Secret:    balanceCode.Secret,
				NetworkId: networkId,
			}, ctx)
			connect.AssertEqual(t, err, nil)
			return networkId
		}

		// createReturnContract requests a non-companion return contract from
		// provider -> consumer (the shape a provider uses for return traffic) and
		// decodes the single result frame.
		createReturnContract := func(provider server.Id, consumer server.Id) *protocol.CreateContractResult {
			frames, err := CreateContract(ctx, provider, &protocol.CreateContract{
				DestinationId:     consumer.Bytes(),
				TransferByteCount: uint64(1024 * 1024),
			}, connect.DefaultContractManagerSettings())
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(frames), 1)
			message, err := connect.FromFrame(frames[0])
			connect.AssertEqual(t, err, nil)
			result, ok := message.(*protocol.CreateContractResult)
			connect.AssertEqual(t, ok, true)
			return result
		}

		// Scenario 1: companion fallback for an older client.
		// Same-network provider and consumer; the consumer (destination of the
		// return traffic) advertises ONLY ProvideModeStream. The same-network
		// return resolves to ProvideModeNetwork, which the consumer does not
		// advertise, so it must fall back to a companion Stream contract that
		// rides the forward (consumer -> provider) origin contract.
		{
			networkId := newFundedNetwork()
			provider := newClient(networkId)
			consumer := newClient(networkId)

			// older client: registers only Stream
			model.SetProvide(ctx, consumer, map[model.ProvideMode][]byte{
				model.ProvideModeStream: streamKey,
			})

			// forward origin (consumer -> provider) for the companion to ride
			_, err := model.CreateContractNoEscrow(ctx, networkId, consumer, networkId, provider, model.ByteCount(1024*1024))
			connect.AssertEqual(t, err, nil)

			result := createReturnContract(provider, consumer)

			// must NOT be rejected, and must settle as a companion Stream contract
			connect.AssertEqual(t, result.Error == nil, true)
			connect.AssertEqual(t, result.Contract != nil, true)
			if result.Contract != nil {
				connect.AssertEqual(t, result.Contract.ProvideMode, protocol.ProvideMode_Stream)
			}
		}

		// Scenario 2: the ideal relationship mode is used when advertised
		// (regression guard). The consumer advertises ProvideModeNetwork, so the
		// same-network return uses Network directly (NoEscrow) with no companion
		// fallback and no origin required.
		{
			networkId := newFundedNetwork()
			provider := newClient(networkId)
			consumer := newClient(networkId)

			model.SetProvide(ctx, consumer, map[model.ProvideMode][]byte{
				model.ProvideModeNetwork: networkKey,
				model.ProvideModeStream:  streamKey,
			})

			result := createReturnContract(provider, consumer)

			connect.AssertEqual(t, result.Error == nil, true)
			connect.AssertEqual(t, result.Contract != nil, true)
			if result.Contract != nil {
				connect.AssertEqual(t, result.Contract.ProvideMode, protocol.ProvideMode_Network)
			}
		}

		// Scenario 3: reject when the destination advertises neither the
		// relationship mode nor Stream (the fallback must not over-authorize). The
		// same-network relationship is Network; the consumer advertises only
		// Public, and there is no Stream to fall back to.
		{
			networkId := newFundedNetwork()
			provider := newClient(networkId)
			consumer := newClient(networkId)

			model.SetProvide(ctx, consumer, map[model.ProvideMode][]byte{
				model.ProvideModePublic: publicKey,
			})

			result := createReturnContract(provider, consumer)

			connect.AssertEqual(t, result.Contract == nil, true)
			connect.AssertEqual(t, result.Error != nil, true)
			if result.Error != nil {
				connect.AssertEqual(t, *result.Error, protocol.ContractError_NoPermission)
			}
		}
	})
}

// TestCreateContractCompanionStreamId verifies that a companion contract is
// marked with the origin flow's active stream id — the receive sequence on
// the other side inspects the contract to know the stream is active — even
// when the escrow-linked (earliest) origin contract is not the one carrying
// the stream. Also guards the stream-version gate: a version-0 request must
// not get a stream id.
func TestCreateContractCompanionStreamId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		streamKey := []byte("test-provide-secret-key-stream00")

		newClient := func(networkId server.Id) server.Id {
			clientId := server.NewId()
			deviceId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "test", "test")
			return clientId
		}

		newFundedNetwork := func() server.Id {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("test-%s", networkId), userId)
			balanceCode, err := model.CreateBalanceCode(
				ctx,
				model.ByteCount(1024*1024*1024*1024),
				365*24*time.Hour,
				model.UsdToNanoCents(10.00),
				server.NewId().String(), "", "",
			)
			connect.AssertEqual(t, err, nil)
			_, err = model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
				Secret:    balanceCode.Secret,
				NetworkId: networkId,
			}, ctx)
			connect.AssertEqual(t, err, nil)
			return networkId
		}

		createCompanionContract := func(source server.Id, destination server.Id, streamVersion *uint32) *protocol.CreateContractResult {
			frames, err := CreateContract(ctx, source, &protocol.CreateContract{
				DestinationId:     destination.Bytes(),
				TransferByteCount: uint64(1024 * 1024),
				Companion:         true,
				StreamVersion:     streamVersion,
			}, connect.DefaultContractManagerSettings())
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(frames), 1)
			message, err := connect.FromFrame(frames[0])
			connect.AssertEqual(t, err, nil)
			result, ok := message.(*protocol.CreateContractResult)
			connect.AssertEqual(t, ok, true)
			return result
		}

		storedContract := func(result *protocol.CreateContractResult) *protocol.StoredContract {
			connect.AssertEqual(t, result.Error == nil, true)
			connect.AssertEqual(t, result.Contract != nil, true)
			stored := &protocol.StoredContract{}
			connect.AssertEqual(t, proto.Unmarshal(result.Contract.StoredContractBytes, stored), nil)
			return stored
		}

		streamVersion1 := uint32(1)

		// the consumer advertises only Stream so the companion request settles
		// as a companion Stream contract (no network normalization)
		networkId := newFundedNetwork()
		provider := newClient(networkId)
		consumer := newClient(networkId)
		model.SetProvide(ctx, consumer, map[model.ProvideMode][]byte{
			model.ProvideModeStream: streamKey,
		})

		// the earliest origin (consumer -> provider) has NO stream; a newer
		// origin carries the active stream. The companion escrow links to the
		// earliest, and the marking must still resolve the stream.
		_, err := model.CreateContractNoEscrow(ctx, networkId, consumer, networkId, provider, model.ByteCount(1024*1024))
		connect.AssertEqual(t, err, nil)
		streamedOriginContractId, err := model.CreateContractNoEscrow(ctx, networkId, consumer, networkId, provider, model.ByteCount(1024*1024))
		connect.AssertEqual(t, err, nil)
		intermediaryId := server.NewId()
		streamId := model.AddToStream(ctx, streamedOriginContractId, consumer, provider, []server.Id{intermediaryId})

		result := createCompanionContract(provider, consumer, &streamVersion1)
		stored := storedContract(result)
		connect.AssertEqual(t, result.Contract.ProvideMode, protocol.ProvideMode_Stream)
		connect.AssertEqual(t, len(stored.StreamId) == 0, false)
		connect.AssertEqual(t, server.Id(stored.StreamId), streamId)

		// the companion joined the stream: it resolves the stream itself, and
		// keeps it alive when the streamed origin closes out
		companionContractId := server.Id(stored.ContractId)
		memberStreamId, _, ok := model.GetStream(ctx, companionContractId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, memberStreamId, streamId)
		model.RemoveFromStream(ctx, streamedOriginContractId)
		_, _, ok = model.GetStream(ctx, companionContractId)
		connect.AssertEqual(t, ok, true)

		// a stream-version-0 request never gets a stream id, even with the
		// stream active
		resultV0 := createCompanionContract(provider, consumer, nil)
		storedV0 := storedContract(resultV0)
		connect.AssertEqual(t, len(storedV0.StreamId), 0)

		// with no active stream for the flow, the companion stays unmarked
		model.RemoveFromStream(ctx, companionContractId)
		networkId2 := newFundedNetwork()
		provider2 := newClient(networkId2)
		consumer2 := newClient(networkId2)
		model.SetProvide(ctx, consumer2, map[model.ProvideMode][]byte{
			model.ProvideModeStream: streamKey,
		})
		_, err = model.CreateContractNoEscrow(ctx, networkId2, consumer2, networkId2, provider2, model.ByteCount(1024*1024))
		connect.AssertEqual(t, err, nil)
		result2 := createCompanionContract(provider2, consumer2, &streamVersion1)
		stored2 := storedContract(result2)
		connect.AssertEqual(t, len(stored2.StreamId), 0)
	})
}

// TestCreateContractNetworkNormalizedCompanionStreamId reproduces the
// 2026-07-20 same-network report: the streamed (force_stream) contract
// between the pair rode a stream and reported it, but the reply shipped
// without the stream id, so neither the provider send nor the client receive
// stats saw the stream — and, worse, the unstamped contract steered the
// reply traffic off the stream (`openContractMultiRouteWriter` follows the
// contract path once set). The reply request carries NO companion flag for
// the network relationship (`RemoteUserNatProvider` skips
// `CompanionContract()` for a network-mode source) and no stream request, so
// it is indistinguishable from a forward send: while the pair has an active
// stream, EVERY network contract between the pair must join it — the
// platform directing the client to switch to the stream. Covers the
// companion=true (older builds normalize) and companion=false (current
// builds) request shapes.
func TestCreateContractNetworkNormalizedCompanionStreamId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkKey := []byte("test-provide-secret-key-network0")
		streamKey := []byte("test-provide-secret-key-stream00")

		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("test-%s", networkId), userId)

		newClient := func() server.Id {
			clientId := server.NewId()
			deviceId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "test", "test")
			return clientId
		}
		// the app / original sender, and the provider replying
		client := newClient()
		provider := newClient()

		// both peers advertise the network mode (and Stream, like real
		// clients), so both directions settle as network no-escrow contracts
		for _, clientId := range []server.Id{client, provider} {
			model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
				model.ProvideModeNetwork: networkKey,
				model.ProvideModeStream:  streamKey,
			})
		}

		streamVersion1 := uint32(1)

		createContract := func(source server.Id, destination server.Id, companion bool, forceStream bool) *protocol.CreateContractResult {
			frames, err := CreateContract(ctx, source, &protocol.CreateContract{
				DestinationId:     destination.Bytes(),
				TransferByteCount: uint64(1024 * 1024),
				Companion:         companion,
				ForceStream:       &forceStream,
				StreamVersion:     &streamVersion1,
			}, connect.DefaultContractManagerSettings())
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(frames), 1)
			message, err := connect.FromFrame(frames[0])
			connect.AssertEqual(t, err, nil)
			result, ok := message.(*protocol.CreateContractResult)
			connect.AssertEqual(t, ok, true)
			return result
		}

		storedContract := func(result *protocol.CreateContractResult) *protocol.StoredContract {
			connect.AssertEqual(t, result.Error == nil, true)
			connect.AssertEqual(t, result.Contract != nil, true)
			stored := &protocol.StoredContract{}
			connect.AssertEqual(t, proto.Unmarshal(result.Contract.StoredContractBytes, stored), nil)
			return stored
		}

		// forward: the client requests a streamed network contract to the
		// provider (force_stream, the p2p shape)
		forwardResult := createContract(client, provider, false, true)
		forwardStored := storedContract(forwardResult)
		connect.AssertEqual(t, forwardResult.Contract.ProvideMode, protocol.ProvideMode_Network)
		connect.AssertEqual(t, len(forwardStored.StreamId) == 0, false)
		streamId := server.Id(forwardStored.StreamId)

		// reply, current build shape: NO companion flag, no stream request —
		// must still carry the SAME stream id and join the stream
		replyResult := createContract(provider, client, false, false)
		replyStored := storedContract(replyResult)
		connect.AssertEqual(t, replyResult.Contract.ProvideMode, protocol.ProvideMode_Network)
		connect.AssertEqual(t, len(replyStored.StreamId) == 0, false)
		connect.AssertEqual(t, server.Id(replyStored.StreamId), streamId)

		replyContractId := server.Id(replyStored.ContractId)
		memberStreamId, _, ok := model.GetStream(ctx, replyContractId)
		connect.AssertEqual(t, ok, true)
		connect.AssertEqual(t, memberStreamId, streamId)

		// reply, older build shape: companion request normalized to network —
		// same outcome
		replyNormalizedResult := createContract(provider, client, true, false)
		replyNormalizedStored := storedContract(replyNormalizedResult)
		connect.AssertEqual(t, replyNormalizedResult.Contract.ProvideMode, protocol.ProvideMode_Network)
		connect.AssertEqual(t, server.Id(replyNormalizedStored.StreamId), streamId)

		// the replies keep the stream alive when the streamed contract
		// closes out of it
		model.RemoveFromStream(ctx, server.Id(forwardStored.ContractId))
		_, _, ok = model.GetStream(ctx, replyContractId)
		connect.AssertEqual(t, ok, true)

		// a forward-direction network contract without an explicit stream
		// request also joins the pair's active stream — both directions of
		// an actively streaming pair ride the stream
		forward2Result := createContract(client, provider, false, false)
		forward2Stored := storedContract(forward2Result)
		connect.AssertEqual(t, server.Id(forward2Stored.StreamId), streamId)

		// a stream-version-0 request (older protocol build) must NOT be
		// steered onto the pair stream even while it is active — those
		// clients cannot handle a stream id in the contract
		v0Frames, err := CreateContract(ctx, provider, &protocol.CreateContract{
			DestinationId:     client.Bytes(),
			TransferByteCount: uint64(1024 * 1024),
			Companion:         false,
		}, connect.DefaultContractManagerSettings())
		connect.AssertEqual(t, err, nil)
		v0Message, err := connect.FromFrame(v0Frames[0])
		connect.AssertEqual(t, err, nil)
		v0Stored := storedContract(v0Message.(*protocol.CreateContractResult))
		connect.AssertEqual(t, len(v0Stored.StreamId), 0)
		_, _, ok = model.GetStream(ctx, server.Id(v0Stored.ContractId))
		connect.AssertEqual(t, ok, false)

		// with no active stream left for the pair, contracts stay unmarked
		// (and must not resurrect the dead stream)
		for _, contractId := range []server.Id{replyContractId, server.Id(replyNormalizedStored.ContractId), server.Id(forward2Stored.ContractId)} {
			model.RemoveFromStream(ctx, contractId)
		}
		reply2Result := createContract(provider, client, false, false)
		reply2Stored := storedContract(reply2Result)
		connect.AssertEqual(t, len(reply2Stored.StreamId), 0)
	})
}

// TestCreateContractEscrowForwardNoAutoJoin pins the escrow (cross-network)
// branch boundary of pair-stream steering: auto-join applies only to the
// network no-escrow branch. A cross-network forward contract WITHOUT an
// explicit stream request stays direct even while the pair has an active
// stream — cross-network senders choose streams explicitly via
// force_stream/intermediary_ids, and the cross-network reply joins through
// the companion escrow branch instead.
func TestCreateContractEscrowForwardNoAutoJoin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		publicKey := []byte("test-provide-secret-key-public00")

		newClient := func(networkId server.Id) server.Id {
			clientId := server.NewId()
			deviceId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "test", "test")
			return clientId
		}
		newFundedNetwork := func() server.Id {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("test-%s", networkId), userId)
			balanceCode, err := model.CreateBalanceCode(
				ctx,
				model.ByteCount(1024*1024*1024*1024),
				365*24*time.Hour,
				model.UsdToNanoCents(10.00),
				server.NewId().String(), "", "",
			)
			connect.AssertEqual(t, err, nil)
			_, err = model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
				Secret:    balanceCode.Secret,
				NetworkId: networkId,
			}, ctx)
			connect.AssertEqual(t, err, nil)
			return networkId
		}

		// cross-network: funded consumer network pays the forward escrow
		consumer := newClient(newFundedNetwork())
		provider := newClient(newFundedNetwork())
		model.SetProvide(ctx, provider, map[model.ProvideMode][]byte{
			model.ProvideModePublic: publicKey,
		})

		streamVersion1 := uint32(1)
		createContract := func(forceStream bool) *protocol.StoredContract {
			frames, err := CreateContract(ctx, consumer, &protocol.CreateContract{
				DestinationId:     provider.Bytes(),
				TransferByteCount: uint64(1024 * 1024),
				ForceStream:       &forceStream,
				StreamVersion:     &streamVersion1,
			}, connect.DefaultContractManagerSettings())
			connect.AssertEqual(t, err, nil)
			message, err := connect.FromFrame(frames[0])
			connect.AssertEqual(t, err, nil)
			result, ok := message.(*protocol.CreateContractResult)
			connect.AssertEqual(t, ok, true)
			connect.AssertEqual(t, result.Error == nil, true)
			connect.AssertEqual(t, result.Contract != nil, true)
			connect.AssertEqual(t, result.Contract.ProvideMode, protocol.ProvideMode_Public)
			stored := &protocol.StoredContract{}
			connect.AssertEqual(t, proto.Unmarshal(result.Contract.StoredContractBytes, stored), nil)
			return stored
		}

		// a streamed forward establishes the pair stream (escrow branch,
		// explicit force_stream)
		streamedStored := createContract(true)
		connect.AssertEqual(t, len(streamedStored.StreamId) == 0, false)

		// an escrow forward without an explicit stream request stays direct
		// while the pair stream is active
		directStored := createContract(false)
		connect.AssertEqual(t, len(directStored.StreamId), 0)
		_, _, ok := model.GetStream(ctx, server.Id(directStored.ContractId))
		connect.AssertEqual(t, ok, false)
	})
}

// TestCreateContractCompanionNetworkNormalization guards the boundaries of the
// companion -> network normalization: a companion request between same-network
// peers where the destination advertises the network mode settles as a
// non-companion network contract (no escrow), and nothing else does. A cross
// network companion must never normalize — that would hand strangers the
// no-escrow path — and a same-network destination that advertises only Stream
// keeps the companion fallback.
func TestCreateContractCompanionNetworkNormalization(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		streamKey := []byte("test-provide-secret-key-stream00")
		networkKey := []byte("test-provide-secret-key-network0")
		publicKey := []byte("test-provide-secret-key-public00")

		newClient := func(networkId server.Id) server.Id {
			clientId := server.NewId()
			deviceId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "test", "test")
			return clientId
		}

		newFundedNetwork := func() server.Id {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("test-%s", networkId), userId)
			err := model.AddBasicTransferBalance(
				ctx,
				networkId,
				model.ByteCount(1024*1024*1024*1024),
				server.NowUtc(),
				server.NowUtc().Add(365*24*time.Hour),
			)
			connect.AssertEqual(t, err, nil)
			return networkId
		}

		createCompanionContract := func(source server.Id, destination server.Id) *protocol.CreateContractResult {
			frames, err := CreateContract(ctx, source, &protocol.CreateContract{
				DestinationId:     destination.Bytes(),
				TransferByteCount: uint64(1024 * 1024),
				Companion:         true,
			}, connect.DefaultContractManagerSettings())
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(frames), 1)
			message, err := connect.FromFrame(frames[0])
			connect.AssertEqual(t, err, nil)
			result, ok := message.(*protocol.CreateContractResult)
			connect.AssertEqual(t, ok, true)
			return result
		}

		// Scenario 1: normalize. Same-network peers, destination advertises the
		// network mode. The companion request settles as a non-companion network
		// contract: no origin contract is needed (a real companion would reject
		// without one), the priority is trusted, and no escrow is opened.
		{
			networkId := newFundedNetwork()
			source := newClient(networkId)
			destination := newClient(networkId)

			model.SetProvide(ctx, destination, map[model.ProvideMode][]byte{
				model.ProvideModeNetwork: networkKey,
				model.ProvideModeStream:  streamKey,
			})

			openByteCount := model.GetOpenTransferByteCount(ctx, networkId)

			result := createCompanionContract(source, destination)

			connect.AssertEqual(t, result.Error == nil, true)
			connect.AssertEqual(t, result.Contract != nil, true)
			if result.Contract != nil {
				connect.AssertEqual(t, result.Contract.ProvideMode, protocol.ProvideMode_Network)

				storedContract := &protocol.StoredContract{}
				err := proto.Unmarshal(result.Contract.StoredContractBytes, storedContract)
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, storedContract.Priority != nil, true)
				if storedContract.Priority != nil {
					connect.AssertEqual(t, int(*storedContract.Priority), int(model.TrustedPriority))
				}
			}

			// the normalized contract is no-escrow: the payer network's open
			// escrow bytes are unchanged
			connect.AssertEqual(t, model.GetOpenTransferByteCount(ctx, networkId), openByteCount)
		}

		// Scenario 2: no normalization for a same-network destination that
		// advertises only Stream (older or provide-off client). The companion
		// request keeps the companion Stream path, riding the forward origin.
		{
			networkId := newFundedNetwork()
			source := newClient(networkId)
			destination := newClient(networkId)

			model.SetProvide(ctx, destination, map[model.ProvideMode][]byte{
				model.ProvideModeStream: streamKey,
			})

			// forward origin (destination -> source) for the companion to ride
			_, err := model.CreateContractNoEscrow(ctx, networkId, destination, networkId, source, model.ByteCount(1024*1024))
			connect.AssertEqual(t, err, nil)

			result := createCompanionContract(source, destination)

			connect.AssertEqual(t, result.Error == nil, true)
			connect.AssertEqual(t, result.Contract != nil, true)
			if result.Contract != nil {
				connect.AssertEqual(t, result.Contract.ProvideMode, protocol.ProvideMode_Stream)
			}
		}

		// Scenario 3: no normalization across networks, even when the destination
		// advertises the network mode. The relationship is Public, so the
		// companion request must keep the companion Stream path. Normalizing here
		// would grant strangers no-escrow contracts.
		{
			sourceNetworkId := newFundedNetwork()
			destinationNetworkId := newFundedNetwork()
			source := newClient(sourceNetworkId)
			destination := newClient(destinationNetworkId)

			model.SetProvide(ctx, destination, map[model.ProvideMode][]byte{
				model.ProvideModeNetwork: networkKey,
				model.ProvideModePublic:  publicKey,
				model.ProvideModeStream:  streamKey,
			})

			// forward origin (destination -> source) for the companion to ride
			_, err := model.CreateContractNoEscrow(ctx, destinationNetworkId, destination, sourceNetworkId, source, model.ByteCount(1024*1024))
			connect.AssertEqual(t, err, nil)

			result := createCompanionContract(source, destination)

			connect.AssertEqual(t, result.Error == nil, true)
			connect.AssertEqual(t, result.Contract != nil, true)
			if result.Contract != nil {
				connect.AssertEqual(t, result.Contract.ProvideMode, protocol.ProvideMode_Stream)
			}
		}
	})
}

// TestCreateContractIdentityStamping guards the identity privacy invariant:
// the source client's roles and principal are sealed into the stored contract
// only when the settled provide mode is network. Public and Stream contracts —
// including the same-network companion Stream fallback — must carry no
// identity, otherwise client identity metadata leaks to strangers.
func TestCreateContractIdentityStamping(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		streamKey := []byte("test-provide-secret-key-stream00")
		networkKey := []byte("test-provide-secret-key-network0")
		publicKey := []byte("test-provide-secret-key-public00")

		newClientWithIdentity := func(networkId server.Id, roles []string, principal string) server.Id {
			clientId := server.NewId()
			deviceId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "test", "test")
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						UPDATE network_client
						SET principal = $2
						WHERE client_id = $1
					`,
					clientId,
					principal,
				))
				for _, role := range roles {
					server.RaisePgResult(tx.Exec(
						ctx,
						`
							INSERT INTO network_client_role (client_id, role)
							VALUES ($1, $2)
						`,
						clientId,
						role,
					))
				}
			})
			return clientId
		}

		newFundedNetwork := func() server.Id {
			networkId := server.NewId()
			userId := server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("test-%s", networkId), userId)
			err := model.AddBasicTransferBalance(
				ctx,
				networkId,
				model.ByteCount(1024*1024*1024*1024),
				server.NowUtc(),
				server.NowUtc().Add(365*24*time.Hour),
			)
			connect.AssertEqual(t, err, nil)
			return networkId
		}

		createContract := func(source server.Id, destination server.Id, companion bool) *protocol.StoredContract {
			frames, err := CreateContract(ctx, source, &protocol.CreateContract{
				DestinationId:     destination.Bytes(),
				TransferByteCount: uint64(1024 * 1024),
				Companion:         companion,
			}, connect.DefaultContractManagerSettings())
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, len(frames), 1)
			message, err := connect.FromFrame(frames[0])
			connect.AssertEqual(t, err, nil)
			result, ok := message.(*protocol.CreateContractResult)
			connect.AssertEqual(t, ok, true)
			connect.AssertEqual(t, result.Error == nil, true)
			connect.AssertEqual(t, result.Contract != nil, true)
			storedContract := &protocol.StoredContract{}
			err = proto.Unmarshal(result.Contract.StoredContractBytes, storedContract)
			connect.AssertEqual(t, err, nil)
			return storedContract
		}

		roles := []string{"role1", "role2"}
		principal := "svc-a"

		// Scenario 1: same-network contract at the network mode carries the
		// source's identity (twice, to also cover the identity cache hit path)
		{
			networkId := newFundedNetwork()
			source := newClientWithIdentity(networkId, roles, principal)
			destination := newClientWithIdentity(networkId, nil, "")

			model.SetProvide(ctx, destination, map[model.ProvideMode][]byte{
				model.ProvideModeNetwork: networkKey,
				model.ProvideModeStream:  streamKey,
			})

			for range 2 {
				storedContract := createContract(source, destination, false)
				connect.AssertEqual(t, storedContract.Roles, roles)
				connect.AssertEqual(t, storedContract.Principal, principal)
			}
		}

		// Scenario 2: a cross-network public contract carries no identity even
		// though the source has roles and a principal
		{
			sourceNetworkId := newFundedNetwork()
			destinationNetworkId := newFundedNetwork()
			source := newClientWithIdentity(sourceNetworkId, roles, principal)
			destination := newClientWithIdentity(destinationNetworkId, nil, "")

			model.SetProvide(ctx, destination, map[model.ProvideMode][]byte{
				model.ProvideModePublic: publicKey,
				model.ProvideModeStream: streamKey,
			})

			storedContract := createContract(source, destination, false)
			connect.AssertEqual(t, len(storedContract.Roles), 0)
			connect.AssertEqual(t, storedContract.Principal, "")
		}

		// Scenario 3: the same-network companion Stream fallback (destination
		// advertises only Stream) carries no identity
		{
			networkId := newFundedNetwork()
			source := newClientWithIdentity(networkId, roles, principal)
			destination := newClientWithIdentity(networkId, nil, "")

			model.SetProvide(ctx, destination, map[model.ProvideMode][]byte{
				model.ProvideModeStream: streamKey,
			})

			// forward origin (destination -> source) for the companion to ride
			_, err := model.CreateContractNoEscrow(ctx, networkId, destination, networkId, source, model.ByteCount(1024*1024))
			connect.AssertEqual(t, err, nil)

			storedContract := createContract(source, destination, false)
			connect.AssertEqual(t, len(storedContract.Roles), 0)
			connect.AssertEqual(t, storedContract.Principal, "")
		}
	})
}
