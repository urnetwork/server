//go:build !sidecar_pending

package model

// A release leg until MG0's Gn-out (DESIGN.md 20.8; FS-54): no owner table, so the
// profile, the create cap's and the peer valve's counts and the relationship read only
// network_client as they always did, nothing is a declared sidecar, and the sidecar
// gates pass with no I/O even with the limits enforced.

import (
	"context"
	"fmt"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestSidecarClientCountingWithoutMg0(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if server.SidecarMg0Linked() {
			t.Fatal("a release build links MG0")
		}
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("release%s", networkId), server.NewId())
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic", "synthetic")
		other := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), other, "synthetic", "synthetic")

		_, topLevel, category, peer, _ := GetNetworkPeerProfile(ctx, clientId)
		connect.AssertEqual(t, topLevel, true)
		connect.AssertEqual(t, peer != nil, true)
		connect.AssertEqual(t, category, NetworkPeerCategoryClient)

		var count int
		server.Tx(ctx, func(tx server.PgTx) {
			count = countNetworkActiveTopLevelClients(ctx, tx, networkId, 100)
		})
		connect.AssertEqual(t, count, 2)
		Testing_ClearNetworkPeersEnabledCache()
		connect.AssertEqual(t, NetworkPeersEnabled(ctx, networkId), true)

		details := GetProvideRelationshipDetails(ctx, clientId, other)
		connect.AssertEqual(t, details.Mode, ProvideModeNetwork)
		connect.AssertEqual(t, details.SourceStrictExp || details.DestinationStrictExp || details.SourceDeclaresNoEgress, false)
		connect.AssertEqual(t, details.DeclaredSourceRefused(ProvideModePublic), false)

		defer Testing_SetEnforceSidecarLimits(true)()
		connect.AssertEqual(t, SidecarClientCountingOn(), false)
		attempts, stop := server.DenyPostgresForTest(t)
		connect.AssertEqual(t, CanConnectSidecarClient(ctx, clientId), true)
		connect.AssertEqual(t, SidecarClientsExceeded(ctx, networkId) == nil, true)
		stop()
		connect.AssertEqual(t, attempts(), uint64(0))
	})
}
