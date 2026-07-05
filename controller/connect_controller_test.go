package controller

import (
	"context"
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

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
	assert.Equal(t, allowed, true)
	assert.Equal(t, companion, true)
	assert.Equal(t, provideMode, model.ProvideModeStream)

	// Friends-and-family relationship, destination advertising only Stream: same
	// companion Stream fallback as the Network case (both are free NoEscrow modes
	// the older destination cannot advertise).
	provideMode, companion, allowed = resolveNonCompanionProvideMode(
		model.ProvideModeFriendsAndFamily,
		map[model.ProvideMode]bool{model.ProvideModeStream: true},
	)
	assert.Equal(t, allowed, true)
	assert.Equal(t, companion, true)
	assert.Equal(t, provideMode, model.ProvideModeStream)

	// Destination advertises the ideal (Network) mode: use it directly, no
	// companion, no fallback.
	provideMode, companion, allowed = resolveNonCompanionProvideMode(
		model.ProvideModeNetwork,
		map[model.ProvideMode]bool{model.ProvideModeNetwork: true, model.ProvideModeStream: true},
	)
	assert.Equal(t, allowed, true)
	assert.Equal(t, companion, false)
	assert.Equal(t, provideMode, model.ProvideModeNetwork)

	// Public relationship, destination advertises Public: use it directly.
	provideMode, companion, allowed = resolveNonCompanionProvideMode(
		model.ProvideModePublic,
		map[model.ProvideMode]bool{model.ProvideModePublic: true},
	)
	assert.Equal(t, allowed, true)
	assert.Equal(t, companion, false)
	assert.Equal(t, provideMode, model.ProvideModePublic)

	// Destination advertises both the relationship mode and Stream: the
	// relationship mode wins (no unnecessary companion fallback).
	provideMode, companion, allowed = resolveNonCompanionProvideMode(
		model.ProvideModePublic,
		map[model.ProvideMode]bool{model.ProvideModePublic: true, model.ProvideModeStream: true},
	)
	assert.Equal(t, allowed, true)
	assert.Equal(t, companion, false)
	assert.Equal(t, provideMode, model.ProvideModePublic)

	// Destination advertises neither the relationship mode nor Stream: not
	// allowed (caller rejects with NoPermission). The fallback must not
	// over-authorize.
	_, _, allowed = resolveNonCompanionProvideMode(
		model.ProvideModeNetwork,
		map[model.ProvideMode]bool{model.ProvideModePublic: true},
	)
	assert.Equal(t, allowed, false)

	// Destination advertises nothing: not allowed.
	_, _, allowed = resolveNonCompanionProvideMode(
		model.ProvideModeNetwork,
		map[model.ProvideMode]bool{},
	)
	assert.Equal(t, allowed, false)
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
			model.Testing_CreateNetwork(ctx, networkId, "test", userId)
			balanceCode, err := model.CreateBalanceCode(
				ctx,
				model.ByteCount(1024*1024*1024*1024),
				365*24*time.Hour,
				model.UsdToNanoCents(10.00),
				"", "", "",
			)
			assert.Equal(t, err, nil)
			_, err = model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
				Secret:    balanceCode.Secret,
				NetworkId: networkId,
			}, ctx)
			assert.Equal(t, err, nil)
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
			assert.Equal(t, err, nil)
			assert.Equal(t, len(frames), 1)
			message, err := connect.FromFrame(frames[0])
			assert.Equal(t, err, nil)
			result, ok := message.(*protocol.CreateContractResult)
			assert.Equal(t, ok, true)
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
			assert.Equal(t, err, nil)

			result := createReturnContract(provider, consumer)

			// must NOT be rejected, and must settle as a companion Stream contract
			assert.Equal(t, result.Error == nil, true)
			assert.Equal(t, result.Contract != nil, true)
			if result.Contract != nil {
				assert.Equal(t, result.Contract.ProvideMode, protocol.ProvideMode_Stream)
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

			assert.Equal(t, result.Error == nil, true)
			assert.Equal(t, result.Contract != nil, true)
			if result.Contract != nil {
				assert.Equal(t, result.Contract.ProvideMode, protocol.ProvideMode_Network)
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

			assert.Equal(t, result.Contract == nil, true)
			assert.Equal(t, result.Error != nil, true)
			if result.Error != nil {
				assert.Equal(t, *result.Error, protocol.ContractError_NoPermission)
			}
		}
	})
}
