// Provider-selection callers use the portable fixture's documentation subnet,
// never an unspecified address requiring a production geography database.
package main

import (
	"context"
	"net/netip"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func simulationTestClientSession(ctx context.Context, jwt *session.ByJwt) *session.ClientSession {
	caller := session.Testing_CreateClientSession(ctx, jwt)
	caller.ClientAddress = "192.0.2.1:1234"
	return caller
}

func TestSimulationClientSessionUsesPortableDocumentationLocation(t *testing.T) {
	caller := simulationTestClientSession(context.Background(), session.NewByJwt(server.NewId(), server.NewId(), "synthetic-caller", false, false))
	defer caller.Cancel()
	address, port, err := caller.ParseClientIpPort()
	if err != nil || address.IsUnspecified() || !netip.MustParsePrefix("192.0.2.0/24").Contains(address) || port != 1234 {
		t.Fatal("simulation caller escaped the portable geography fixture", address, port, err)
	}
}
