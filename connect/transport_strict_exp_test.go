package connect

import (
	"context"
	"net/http"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

func newExpiredStrictExpToken() *session.ByJwt {
	network := session.NewByJwt(server.NewId(), server.NewId(), "strict-connect", false, false)
	client := network.Client(server.NewId(), server.NewId())
	client.SetStrictExp(true)
	client.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Hour))
	return client
}

// §22.5's §7 row: with reject_expired off, connect's transport auth refuses a
// strict_exp token past its exp (D-TEAM-14), on both the h1 and the h3 path, which
// share the verifier and connectClientAuthentication. A definitive refusal, before
// any state is read, so a sidecar re-enrolls and does not retry. Pure.
func TestConnectRefusesAnExpiredStrictExpToken(t *testing.T) {
	defer session.Testing_SetRejectExpired(false)()
	defer server.Testing_PushSidecarConfig("sidecar:\n  p0_enforce: {enabled: true}\n")()
	ctx := context.Background()

	byJwt := newExpiredStrictExpToken()
	if _, err := session.ParseByJwtForAudience(ctx, byJwt.Testing_Sign(), session.ByJwtAudienceConnect); err == nil {
		t.Fatal("the transport's verifier passed an expired strict_exp token")
	}

	// the final read, which is the check a token parsed just before its exp meets
	_, status, err := connectClientAuthentication(ctx, byJwt, server.NewId().Bytes())
	if err == nil || status != http.StatusUnauthorized {
		t.Fatalf("status = %d, err = %v; want 401", status, err)
	}
	if !connectCredentialRefused(status) {
		t.Fatal("the refusal is not a credential refusal")
	}
	if connectAuthDependencyUnavailable(err) {
		t.Fatal("the refusal reads as an outage, which a client retries")
	}
}

// Log-only: the transport passes it, as it passes any passed exp while
// reject_expired is off, and the verifier counts it.
func TestConnectAcceptsAnExpiredStrictExpTokenWhileLogOnly(t *testing.T) {
	defer session.Testing_SetRejectExpired(false)()
	defer server.Testing_PushSidecarConfig("")()

	byJwt := newExpiredStrictExpToken()
	if _, err := session.ParseByJwtForAudience(context.Background(), byJwt.Testing_Sign(), session.ByJwtAudienceConnect); err != nil {
		t.Fatalf("log-only refused: %s", err)
	}
}
