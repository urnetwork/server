//go:build sidecar_pending

package controller

// §22.5's "declared sidecars get no Public contract, only sidecar_clients" (DESIGN.md
// 20.8, plan-QUOTA-small-readings #14): CreateContract refuses a declared no-egress source
// a Public contract in any network, from the owner row GetProvideRelationshipDetails reads
// in its one query, and never records the route as rejected. MG0 holds the owner row, so
// this runs under -tags sidecar_pending, through .stack/server-test.sh.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A client of networkId with an owner row of these marks.
func testingDeclaredClient(t testing.TB, ctx context.Context, networkId server.Id, strictExp bool, declaresNoEgress bool) server.Id {
	t.Helper()
	clientId := server.NewId()
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_client_owner (client_id, admin_refused, strict_exp, declares_no_egress)
				VALUES ($1, true, $2, $3)
			`,
			clientId,
			strictExp,
			declaresNoEgress,
		))
	})
	return clientId
}

// Asks for a contract to destinationId and returns its result.
func testingCreateContractResult(t testing.TB, ctx context.Context, sourceId server.Id, destinationId server.Id) *protocol.CreateContractResult {
	t.Helper()
	frames, err := CreateContract(ctx, sourceId, &protocol.CreateContract{DestinationId: destinationId.Bytes(), TransferByteCount: 1024 * 1024}, connect.DefaultContractManagerSettings())
	if err != nil || len(frames) != 1 {
		t.Fatalf("create contract: %d frames, %v", len(frames), err)
	}
	message, err := connect.FromFrame(frames[0])
	returnConnectControlFrames(frames)
	if err != nil {
		t.Fatal(err)
	}
	return message.(*protocol.CreateContractResult)
}

func TestDeclaredSidecarGetsNoPublicContract(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		if !server.SidecarMg0Linked() {
			t.Fatal("a pending build does not link MG0")
		}
		sidecarNetwork, providerNetwork := server.NewId(), server.NewId()
		for _, networkId := range []server.Id{sidecarNetwork, providerNetwork} {
			model.Testing_CreateNetwork(ctx, networkId, "declared-fixture-"+networkId.String(), server.NewId())
		}
		server.Raise(model.AddBasicTransferBalance(ctx, sidecarNetwork, 64*1024*1024, server.NowUtc(), server.NowUtc().Add(time.Hour)))
		declared := testingDeclaredClient(t, ctx, sidecarNetwork, true, true)
		withEgress := testingDeclaredClient(t, ctx, sidecarNetwork, true, false)
		provider := testingDeclaredClient(t, ctx, providerNetwork, false, false)
		model.SetProvide(ctx, provider, map[model.ProvideMode][]byte{model.ProvideModePublic: bytes.Repeat([]byte{42}, 32)})

		// the marks ride the relationship's one query: one acquisition, and no more
		attempts, stop := server.DenyPostgresForTest(t)
		permitted, revoke := server.PermitPostgresAcquisitionForTest(t, ctx)
		details := model.GetProvideRelationshipDetails(permitted, declared, provider)
		revoke()
		stop()
		connect.AssertEqual(t, attempts(), uint64(1))
		connect.AssertEqual(t, details.Mode, model.ProvideModePublic)
		connect.AssertEqual(t, details.SourceStrictExp, true)
		connect.AssertEqual(t, details.SourceDeclaresNoEgress, true)
		connect.AssertEqual(t, details.DestinationStrictExp, false)

		before := rejectionCount(t, "internal", "declared_no_egress", "false")
		for i := 0; i < 2; i += 1 {
			result := testingCreateContractResult(t, ctx, declared, provider)
			if result.Contract != nil || result.Error == nil || *result.Error != protocol.ContractError_NoPermission {
				t.Fatalf("attempt %d: a declared sidecar got %+v", i, result)
			}
		}
		// each refusal is counted, and none was recorded as the route's: the second
		// was decided again, not replayed
		connect.AssertEqual(t, rejectionCount(t, "internal", "declared_no_egress", "false"), before+2)
		connect.AssertEqual(t, model.GetOpenTransferByteCount(ctx, sidecarNetwork), model.ByteCount(0))

		// a sidecar with egress is no declared one: its Public contract is not refused
		// for the declaration
		result := testingCreateContractResult(t, ctx, withEgress, provider)
		if result.Error != nil && *result.Error == protocol.ContractError_NoPermission {
			t.Fatalf("a sidecar with egress was refused: %+v", result)
		}
		connect.AssertEqual(t, rejectionCount(t, "internal", "declared_no_egress", "false"), before+2)

		// within its own network a declared sidecar still contracts: Network mode
		sameNetwork := testingDeclaredClient(t, ctx, sidecarNetwork, false, false)
		model.SetProvide(ctx, sameNetwork, map[model.ProvideMode][]byte{model.ProvideModeNetwork: bytes.Repeat([]byte{43}, 32)})
		result = testingCreateContractResult(t, ctx, declared, sameNetwork)
		if result.Error != nil {
			t.Fatalf("a declared sidecar was refused its own network: %v", *result.Error)
		}
	})
}
