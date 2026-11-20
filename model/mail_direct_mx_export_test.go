//go:build sidecar_pending

package model

// The direct-MX world for model_test's test through api's route (mail_direct_mx_probe_route_test.go), which imports
// api's handlers and so cannot be in this package.

import (
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/server"
)

// A Team network with mail on, its zone in a root of its own, and two receivers whose keys for mx are published, at
// invented public addresses §12.7's A rules take.
type Testing_MailDirectMxWorld struct {
	world         *testMailDirectMxWorld
	Root          string
	NetworkId     server.Id
	First         server.Id
	Second        server.Id
	FirstAddress  netip.Addr
	SecondAddress netip.Addr
}

func Testing_NewMailDirectMxWorld(t testing.TB, ctx context.Context) *Testing_MailDirectMxWorld {
	t.Helper()
	world := newTestMailDirectMxWorld(t, ctx)
	return &Testing_MailDirectMxWorld{
		world:         world,
		Root:          world.root,
		NetworkId:     world.networkId,
		First:         world.first,
		Second:        world.second,
		FirstAddress:  testDirectMxFirst,
		SecondAddress: testDirectMxSecond,
	}
}

// What the zone holds at mx's A, AAAA and TLSA, by type: kind and values each.
func (self *Testing_MailDirectMxWorld) MxRows(t testing.TB, ctx context.Context) map[uint16]string {
	t.Helper()
	return testDirectMxRows(t, ctx, self.world.testDnsRecordsWorld)
}
