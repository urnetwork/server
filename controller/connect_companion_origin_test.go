package controller

// A companion contract request that arrives before its origin contract is an
// ORDERING RACE, not a refusal. At cold start both peers bring their
// encryption sessions up simultaneously, and the EncryptedControl carrier
// requests its companion contract at session setup — frequently milliseconds
// before the peer's origin contract lands. The platform used to answer the
// race with an error; the client sees every contract failure collapsed into
// InsufficientBalance, so it retried blind for its whole 30s
// CreateContractTimeout and the sequence starved — observed ~12 times per
// full test-suite run as the chaos-family first-attempt Timeouts, and as the
// mechanism manufacturing dead-on-arrival multiclient window clients.
// nextContract now waits out the race, bounded by CompanionOriginWaitTimeout.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// companionOriginTestSetup builds two networks with a balance on the payer
// side. The companion request runs sourceId -> destinationId; its origin is
// the OPPOSITE direction (destinationId -> sourceId), and the payer for both
// is the destination network.
func companionOriginTestSetup(ctx context.Context, t testing.TB) (
	sourceNetworkId server.Id,
	sourceId server.Id,
	destinationNetworkId server.Id,
	destinationId server.Id,
) {
	sourceNetworkId = server.NewId()
	sourceUserId := server.NewId()
	sourceId = server.NewId()
	destinationNetworkId = server.NewId()
	destinationUserId := server.NewId()
	destinationId = server.NewId()

	model.Testing_CreateNetwork(ctx, sourceNetworkId, "cmpo-src", sourceUserId)
	model.Testing_CreateNetwork(ctx, destinationNetworkId, "cmpo-dst", destinationUserId)
	// nextContract resolves each client's network through network_client
	model.Testing_CreateDevice(ctx, sourceNetworkId, server.NewId(), sourceId, "cmpo src device", "test")
	model.Testing_CreateDevice(ctx, destinationNetworkId, server.NewId(), destinationId, "cmpo dst device", "test")

	// the destination network pays for the origin it creates AND for the
	// companion that answers it
	balanceCode, err := model.CreateBalanceCode(
		ctx,
		model.ByteCount(1024*1024*1024*1024),
		365*24*time.Hour,
		model.UsdToNanoCents(10.00),
		"",
		"",
		"",
	)
	connect.AssertEqual(t, err, nil)
	model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{
		Secret:    balanceCode.Secret,
		NetworkId: destinationNetworkId,
	}, ctx)

	return
}

func companionCreateContract(sourceId server.Id, destinationId server.Id) *protocol.CreateContract {
	streamVersion := uint32(connect.DefaultStreamVersion)
	return &protocol.CreateContract{
		DestinationId:     destinationId.Bytes(),
		TransferByteCount: 4096,
		Companion:         true,
		StreamVersion:     &streamVersion,
	}
}

// TestCompanionContractWaitsForRacedOrigin is the regression test for the 30s
// companion-contract starve: a companion request that fires before the origin
// exists must succeed once the origin lands moments later, NOT come back as
// an error the client cannot classify.
//
// Without the bounded wait in nextContract the request errors immediately —
// the origin arrives ~300ms too late — and this fails.
func TestCompanionContractWaitsForRacedOrigin(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		sourceNetworkId, sourceId, destinationNetworkId, destinationId := companionOriginTestSetup(ctx, t)
		_ = sourceNetworkId

		type companionResult struct {
			contractId server.Id
			err        error
		}
		resultC := make(chan companionResult, 1)
		start := time.Now()
		go func() {
			contractId, _, _, _, err := nextContract(
				ctx,
				sourceId,
				companionCreateContract(sourceId, destinationId),
				true,
				// public: the escrowed path — ProvideModeNetwork routes to
				// CreateContractNoEscrow and never consults the origin
				model.ProvideModePublic,
				connect.DefaultContractManagerSettings(),
			)
			resultC <- companionResult{contractId: contractId, err: err}
		}()

		// the losing side of the race: the origin (destination -> source)
		// lands a beat after the companion request fired
		time.Sleep(300 * time.Millisecond)
		originEscrow, err := model.CreateTransferEscrow(
			ctx,
			destinationNetworkId,
			destinationId,
			sourceNetworkId,
			sourceId,
			model.ByteCount(1024*1024),
		)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, originEscrow, nil)

		select {
		case result := <-resultC:
			elapsed := time.Since(start)
			if result.err != nil {
				t.Fatalf(
					"a companion request racing its origin must succeed once the origin lands (after %s): %s — an error here reaches the client as InsufficientBalance and starves the sequence for its whole CreateContractTimeout",
					elapsed, result.err,
				)
			}
			if result.contractId == (server.Id{}) {
				t.Fatal("companion succeeded without a contract id")
			}
			// well inside the client's blind retry budget: the whole point
			if CompanionOriginWaitTimeout <= elapsed {
				t.Fatalf("companion resolved but only after the full wait bound (%s)", elapsed)
			}
		case <-time.After(CompanionOriginWaitTimeout + 5*time.Second):
			t.Fatal("companion request neither succeeded nor failed within the wait bound")
		}
	})
}

// TestCompanionContractMissingOriginIsBounded pins the terminal half: a
// companion request whose origin never arrives must come back as
// ErrMissingCompanionOrigin after roughly CompanionOriginWaitTimeout — the
// wait must not become a hang, and the cause must stay identifiable.
func TestCompanionContractMissingOriginIsBounded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		_, sourceId, _, destinationId := companionOriginTestSetup(ctx, t)

		start := time.Now()
		_, _, _, _, err := nextContract(
			ctx,
			sourceId,
			companionCreateContract(sourceId, destinationId),
			true,
			// public: the escrowed path (see above)
			model.ProvideModePublic,
			connect.DefaultContractManagerSettings(),
		)
		elapsed := time.Since(start)

		if !errors.Is(err, model.ErrMissingCompanionOrigin) {
			t.Fatalf("a companion request with no origin must report the missing origin (got %v)", err)
		}
		if elapsed < CompanionOriginWaitTimeout {
			t.Fatalf("the miss was answered in %s, before the wait bound — the race window was not actually waited out", elapsed)
		}
		if CompanionOriginWaitTimeout+2*time.Second < elapsed {
			t.Fatalf("the wait must be bounded: took %s", elapsed)
		}
	})
}
