//go:build !sidecar_pending

package model

import (
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A release leg until MG0's Gn-out: nothing can be marked, so a mark is refused
// and never dropped. A sidecar fails closed without admin_refused (§5.6), and
// nothing here may reach MG0's tables, which do not exist.
func TestMarkRequestIsRefusedWithoutMg0(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		connect.AssertEqual(t, server.SidecarMg0Linked(), false)

		for _, args := range []*AuthNetworkClientArgs{
			{Description: "sidecar", DeviceSpec: "test", AdminRefused: true},
			{Description: "sidecar", DeviceSpec: "test", StrictExp: true},
		} {
			result, err := AuthNetworkClient(args, self.root)
			connect.AssertEqual(t, err, nil)
			if result.Error == nil || result.Error.Message != clientMarksUnavailableMessage || result.ClientId != nil {
				t.Fatalf("auth-client = %+v, error = %+v", result, result.Error)
			}
		}

		// and every unmarked path works without the tables
		client := self.mint(t, self.root, &AuthNetworkClientArgs{Description: "device", DeviceSpec: "test"})
		connect.AssertEqual(t, client.AdminRefused, false)
		connect.AssertEqual(t, client.StrictExp, false)
		reissued := self.mint(t, self.root, &AuthNetworkClientArgs{ClientId: client.ClientId, Description: "device", DeviceSpec: "test"})
		connect.AssertEqual(t, reissued.StrictExp, false)
		connect.AssertEqual(t, ClientRefusesAdmin(self.ctx, self.networkId, *client.ClientId), false)

		removeReapedNetworkClientDependents(self.ctx, []server.Id{*client.ClientId}, nil)
		for _, step := range sweepOrphanNetworkClientSteps() {
			if step.table == "network_client_owner" {
				t.Fatal("the sweep reads a table this binary does not link")
			}
		}
	})
}
