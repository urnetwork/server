package session

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The lag.dev guest claim (lag.dev/PLAN.md A1, AUTH.md §1): it survives every
// way a credential is derived and signed, and reads back unverified only as
// what the token says. Pure: tokens are signed with the local vault key.

func TestLagGuestClaimIsKept(t *testing.T) {
	network := NewByJwt(server.NewId(), server.NewId(), "lag-test", false, false)
	network.LagGuest = true

	client := network.Client(server.NewId(), server.NewId())
	connect.AssertEqual(t, client.LagGuest, true)
	connect.AssertEqual(t, client.Renew().LagGuest, true)
	connect.AssertEqual(t, network.Renew().LagGuest, true)

	signed := client.Testing_Sign()
	connect.AssertEqual(t, ByJwtLagGuestUnverified(signed), true)
	parsed, err := ParseByJwtForAudience(context.Background(), signed, ByJwtAudienceApi)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parsed.LagGuest, true)
	parsed, err = ParseByJwtForAudience(context.Background(), signed, ByJwtAudienceConnect)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parsed.LagGuest, true)

	// any other credential has no claim, and nothing that is not a token does
	ordinary := NewByJwt(server.NewId(), server.NewId(), "lag-test", false, false).Client(server.NewId(), server.NewId())
	connect.AssertEqual(t, ordinary.LagGuest, false)
	connect.AssertEqual(t, ByJwtLagGuestUnverified(ordinary.Testing_Sign()), false)
	connect.AssertEqual(t, ByJwtLagGuestUnverified("not a token"), false)
	connect.AssertEqual(t, ByJwtLagGuestUnverified(""), false)
}
