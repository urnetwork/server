//go:build sidecar_pending

package handlers

import (
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The stock connect installer reads the signed network log through API's real
// credential-protected handlers. Connect receives no metadata cluster.
func TestConnectHostLoadsTheSignedProjectionFromTheInternalApi(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		world := newInternalGateTestWorld(t, 2, 1)
		if _, err := model.AdvanceNetworkAclHead(tb.Context(), world.networkId, world.tip.GetVersion(), world.tip.GetChain(), true); err != nil {
			t.Fatal(err)
		}
		tb.Setenv(sidecarhost.ApiUrlEnvName, world.server.URL)
		tb.Cleanup(server.Testing_PushSidecarConfig("sidecar:\n  acl_contract_gate: {enabled: true}\n"))
		bearer := world.bearers[server.InternalGateCallerConnect]
		tb.Cleanup(server.Vault.PushSimpleResource(server.InternalGateCredentialFileName(server.InternalGateCallerConnect), []byte("current: "+bearer+"\n")))
		defer controller.SetAclContractGate(nil)()
		host := sidecartest.NewFakeHost(tb.Context(), world.world.Clock(), nil, nil, nil).ServiceHost()
		host.Blobs = nil
		stop := controller.InstallConnectAuthorization(tb.Context(), host)
		defer stop()
		gate := controller.ActiveAclContractGate()
		if gate == nil || !controller.AclContractGateInstalled() {
			t.Fatal("connect did not install its verified reader")
		}
		networkId := sidecar.Id(world.networkId)
		deadline := time.NewTimer(time.Minute)
		defer deadline.Stop()
		poll := time.NewTicker(25 * time.Millisecond)
		defer poll.Stop()
		for {
			view := gate.PeerView(networkId)
			if view.Listing == sidecar.AclPeerListingFiltered && view.Version == world.tip.GetVersion() {
				break
			}
			select {
			case <-deadline.C:
				t.Fatalf("connect did not verify the API tip: listing=%v version=%d", view.Listing, view.Version)
			case <-poll.C:
			}
		}
		if view := gate.PeerView(sidecar.NewId()); view.Listing != sidecar.AclPeerListingUnfiltered {
			t.Fatalf("the verified heads changed a network with no config: %v", view.Listing)
		}
		stop()
		if view := controller.ActiveAclContractGate().PeerView(networkId); view.Listing != sidecar.AclPeerListingUnavailable {
			t.Fatal("a removed connect host still publishes its former peer view")
		}
	})
}
