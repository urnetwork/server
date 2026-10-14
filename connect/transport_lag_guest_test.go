package connect

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The connect transport refuses a lag.dev guest's token (lag.dev/PLAN.md A1,
// session/AUTH.md §1) as a refused credential, before it reads any state, on
// both the h1 and the h3 path, which share connectClientAuthentication. Pure:
// nothing is read.
func TestConnectRefusesALagGuestToken(t *testing.T) {
	guest := session.NewByJwt(server.NewId(), server.NewId(), "lag-guest", false, false)
	guest.LagGuest = true
	byJwt := guest.Client(server.NewId(), server.NewId())

	_, status, err := connectClientAuthentication(context.Background(), byJwt, server.NewId().Bytes())
	if status != http.StatusForbidden || !errors.Is(err, errLagGuestConnect) {
		t.Fatalf("status = %d, err = %v; want 403 and the guest refusal", status, err)
	}
	if !connectCredentialRefused(status) {
		t.Fatal("the guest refusal is not a credential refusal")
	}
	if connectAuthDependencyUnavailable(err) {
		t.Fatal("the guest refusal reads as an outage, which a client retries")
	}
}
